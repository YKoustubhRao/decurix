"""
A request done with a tool the worker wrote for itself, with no engine.

playbooks.py keeps, for a kind of request this desktop has done by hand, one
script that does it - written by the hosted model from the commands that
worked, and proven to reproduce the checked result before it was kept. Given
that tool, the engine still spent two turns on a request of the kind: one to
write the command line, one to say what it printed. Measured live, about
8,300 tokens against 15,000 by hand - most of it the engine re-reading its
own instructions twice.

Nothing in those two turns needs an engine. So once a tool has also done a
later request under the engine's eye (DIRECT_AFTER), the next one is done
here: one small model call reads this request into the tool's arguments, the
tool is run, and its result goes through the same final check as any file
work before it is reported. No operator turn at all.

What keeps it honest is what keeps a taught task honest:
  - the model's reading is checked with no model: as many arguments as the
    tool takes, each one found in the request's own words (a file by its
    name), or the engine does the request;
  - the model may say the request is not what the tool does - it asks for
    more, or for something else - and the engine does the request;
  - a tool that fails, or whose result the check finds wrong, hands the
    request to the engine with what happened, and counts against the tool;
  - a result is reported as done only when the check has read it back.

The watcher dispatches it before the engine, like a web question or a replay;
it reports as engine "tool".
"""

import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import traceback
from pathlib import Path

from decurix_agent import (attachments, config, guidance, llm_client, playbooks, produced, progress, script_sandbox, state,
                           verify)
from decurix_agent.logging_utils import log
from decurix_agent.messaging import write_final_reply

ENGINE = "tool"

# How many requests a tool must have done under the engine's eye, checked,
# before it is run with no engine. 0 runs a tool directly from the moment it
# is proven.
try:
    DIRECT_AFTER = max(0, int(os.environ.get("DECURIX_TOOL_DIRECT_AFTER", "1")))
except ValueError:
    DIRECT_AFTER = 1

RUN_SECONDS = int(os.environ.get("DECURIX_TOOL_SECONDS", "300"))
MIN_CONFIDENCE = 0.8
MAX_OUTPUT_CHARS = 6000
# A tool run directly that fails this many times running is not run directly again.
FAILURES_BEFORE_ENGINE_ONLY = 2

ARGS_SYSTEM = (
    "A worker has a script it wrote for one kind of request. You read a new request into the script's command-line "
    "arguments, or say the request is not what the script does.\n"
    "- The script does exactly what its description says. If the request asks for anything more (a second job "
    "before or after, another layout, another calculation, an email), or for something else, answer fits: false - "
    "the worker's general engine will do the request.\n"
    "- Every argument comes from the request's own words: a file by the name the request gives it, a value as the "
    "request writes it. Keep the form of the example's arguments (a bare file name stays a bare file name; a path "
    "stays a path). Never invent an argument, and never reuse the example's value for this request.\n"
    "- The same number of arguments as the example, in the same order.\n"
    "Reply with JSON only: {\"fits\": true|false, \"args\": [\"...\"], \"why\": \"one sentence\", \"confidence\": 0.0-1.0}"
)


def _ready(book):
    """Whether a playbook's tool has earned a run with no engine."""

    tool = book.get("tool") or {}
    if not tool.get("path") or not Path(tool["path"]).exists():
        return False
    if tool.get("direct_failures", 0) >= FAILURES_BEFORE_ENGINE_ONLY:
        return False
    good = sum(1 for use in book.get("uses") or [] if use.get("ok") and use.get("tool"))
    return good >= DIRECT_AFTER


def _book(content):
    if not (playbooks.ENABLED and llm_client.available()):
        return None
    book = playbooks.find(content)
    return book if book is not None and _ready(book) else None


def handles(content):
    """Whether this request is one a proven, used tool of this desktop's can do with no engine. No model is asked."""

    try:
        return _book(content) is not None
    except Exception as exc:
        log(f"Tool: could not look for a tool: {exc}")
        return False


def read_args(book, content):
    """
    This request as the tool's arguments: (list, "") or (None, why not). One
    model call; what it answers is held to the request before it is used.

    The worker's own model reads it, not a smaller one: whether a request is
    what the script does is a judgement, and a wrong yes runs a script that
    writes a file or drafts an email before any check sees it. A few hundred
    tokens on the better model is the cheap side of that trade.
    """

    tool = book["tool"]
    try:
        example = shlex.split(tool.get("example") or "")
    except ValueError:
        return None, "the tool's example could not be read"

    prompt = (
        f"The script: {tool['run']} {tool.get('usage') or ''}\n"
        f"What it does: {tool.get('does') or ''}\n\n"
        f"The request it was written from: {book.get('request') or ''}\n"
        f"Its arguments for that request: {json.dumps(example)}\n\n"
        f"The new request:\n{content}"
    )
    try:
        message, _ = llm_client.chat([{"role": "system", "content": ARGS_SYSTEM}, {"role": "user", "content": prompt}],
                                     max_tokens=400, response_format={"type": "json_object"}, purpose="py.tool.args")
        found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
        answer = json.loads(found.group(0)) if found else {}
    except (llm_client.LLMError, ValueError) as exc:
        return None, f"the request could not be read ({exc})"

    if not isinstance(answer, dict) or not answer.get("fits"):
        return None, str((answer or {}).get("why") or "the request is not what the tool does")[:300]
    try:
        confidence = float(answer.get("confidence", 0))
    except (TypeError, ValueError):
        confidence = 0.0
    if confidence < MIN_CONFIDENCE:
        return None, "the reading of the request was not sure enough"

    args = answer.get("args")
    if not isinstance(args, list) or not all(isinstance(a, str) for a in args) or len(args) != len(example):
        return None, "the reading gave another number of arguments than the tool takes"

    words = attachments.text(content) + "\n" + "\n".join(attachments.files(content))
    for arg in args:
        if not playbooks.in_request(arg, words):
            return None, f"an argument ({arg[:80]!r}) is not in the request"
    return args, ""


class NoSandbox(Exception):
    """The tool cannot run unattended here (script_sandbox): nothing was run."""


def _run(tool, args, drafts=None):
    """
    Run the tool, shut off (script_sandbox): in a scratch copy of the work folder with no network, no display and an
    environment of its own; what it wrote there is copied back into the work folder, and what it drafted is drafted
    for real by the agent. (exit code, what it printed). Raises NoSandbox when this desktop cannot shut it off - it
    is then not run at all, and the engine does the request. `drafts`: a list this fills with the emails the script
    drafted (attachments resolved to the files copied back); they are NOT drafted for real here - the caller checks
    them as a dry draft and makes them once, after the check passes (_draft_once).
    """

    interpreter = "bash" if tool["path"].endswith(".sh") else sys.executable
    # Read again as it is on disk now, before it runs with no engine and no approval: a script that names the screen,
    # the network or the gate's state is refused at once - an early refusal; the sandbox below is the boundary.
    try:
        script = Path(tool["path"]).read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        return 126, f"it could not be read ({exc})"
    wrong = playbooks._sound(script, "bash" if tool["path"].endswith(".sh") else "python")
    if wrong:
        return 126, f"it is not run without the engine: {wrong}"
    if not script_sandbox.available():
        raise NoSandbox(script_sandbox.why_not())

    work = Path(config.WORK_DIR)
    scratch = script_sandbox.new_scratch()
    try:
        if not script_sandbox.copy_work(work, scratch):
            return 126, "the work folder is too large to run it in a copy of"
        copy = scratch / "work"
        # The script runs as read now, from the scratch copy: it cannot change under the run.
        own = scratch / ("tool" + Path(tool["path"]).suffix)
        own.write_text(script, encoding="utf-8", newline="\n")
        args = [a.replace(str(work), str(copy)).replace("~/work", str(copy)) for a in args]
        before = script_sandbox.snapshot(copy)
        try:
            done = script_sandbox.run([interpreter, str(own)] + args, scratch, RUN_SECONDS)
        except subprocess.TimeoutExpired:
            return 124, f"it did not finish in {RUN_SECONDS} seconds"
        except RuntimeError as exc:
            raise NoSandbox(str(exc)) from None
        except OSError as exc:
            return 126, f"it could not be run ({exc})"
        printed = ((done.stdout or "") + (("\n" + done.stderr) if done.returncode and done.stderr else "")).strip()
        printed = printed.replace(str(copy), str(work))
        if done.returncode != 0:
            # (What a failed run wrote stays in the scratch copy: the engine starts from the work folder as it was.)
            return done.returncode, printed
        copied, refused = script_sandbox.copy_back(copy, work, script_sandbox.written(copy, before))
        if refused:
            return 126, (printed + f"\n\nNot copied into the work folder (a link, or a place outside it): "
                         + ", ".join(refused[:10])).strip()
        if drafts is not None:
            wrong = _dry_drafts(scratch / "drafts.jsonl", work, drafts)
            if wrong:
                return 126, (printed + "\n\n" + wrong).strip()
        return 0, printed
    finally:
        shutil.rmtree(scratch, ignore_errors=True)


def _dry_drafts(log_path, work, out):
    """
    Read what the script drafted in its scratch copy (office email there is kept, not sent to the control plane, which
    it cannot reach) into `out`, each with its attachments as the files of that name in the work folder (copied back).
    "" when all were read, else what went wrong. Nothing is put in the Outbox here: a check runs on this dry draft
    first (review 2026-11-03: drafting before the check made a redo draft every email twice).
    """

    from decurix_agent.tools import outbox

    try:
        records = outbox.read_log(log_path)
    except Exception as exc:  # noqa: BLE001 - what the script drafted is unknown: nothing is drafted (N-1)
        return f"What it drafted could not be read ({type(exc).__name__}): nothing was drafted."
    for record in records:
        attached = []
        for name in record.get("attachments") or []:
            found = [p for p in work.rglob("*") if p.name == name and p.is_file() and not p.is_symlink()]
            if len(found) != 1:
                return f"An email it drafted attaches {name}, which is not one file in the work folder: nothing was drafted."
            attached.append(str(found[0]))
        out.append(dict(record, paths=attached))
    return ""


def _same_draft(made, record):
    """Whether a draft already made (outbox.drafted) is this one: the same people, subject, body and files."""

    def low(items):
        return sorted(a.lower() for a in items or [])

    def words(text):
        return " ".join(str(text or "").split())

    return (low(made.get("to")) == low(record.get("to")) and low(made.get("cc")) == low(record.get("cc"))
            and words(made.get("subject")) == words(record.get("subject"))
            and words(made.get("body")) == words(record.get("body"))
            and sorted(made.get("attachments") or []) == sorted(record.get("attachments") or []))


def _draft_once(records):
    """
    Draft into the Outbox the emails a script drafted dry, once each: one this task already drafted with the same
    people, subject, body and files is not drafted again (a redo or a second pass makes no duplicate). "" when every
    draft is made, else what went wrong.
    """

    from decurix_agent.tools import outbox

    try:
        already = produced.drafts()
    except Exception as exc:  # noqa: BLE001 - which are made is unknown: nothing is drafted twice on a guess (N-1)
        return f"the task's drafts could not be read ({type(exc).__name__}), so nothing was drafted"
    for record in records:
        if any(_same_draft(made, record) for made in already):
            continue
        try:
            outbox.draft(record.get("to") or [], record.get("subject") or "", record.get("body") or "",
                         cc=record.get("cc") or None, attachments=record.get("paths") or [])
        except Exception as exc:  # noqa: BLE001 - a draft that could not be made is said, never dropped
            return f"An email it drafted could not be put in the Outbox ({exc})."
    return ""


def _note(book, ok, failed=False, wrong=""):
    """Count this run on the playbook: a use, a failure of the tool when it was one, and its end when its result was `wrong`."""

    fresh = playbooks.load(book["name"])
    if fresh is None:
        return
    use = playbooks.use(ok, [])
    use.update(tool=True, direct=True)
    fresh["uses"] = ((fresh.get("uses") or []) + [use])[-playbooks.USE_HISTORY:]
    tool = fresh.get("tool")
    if tool and wrong:
        playbooks.tool_failed(fresh, wrong)
    elif tool:
        tool["direct_failures"] = tool.get("direct_failures", 0) + 1 if failed else 0
    try:
        playbooks.save(fresh)
    except OSError as exc:
        log(f"Tool: could not count this run: {exc}")


def _to_engine(content, task_file, said, note=""):
    """The engine does the request after all - told, when the tool did run, what it left and what was wrong with it."""

    progress.emit(said)
    playbooks.decline()
    if note:
        guidance.begin(skill=None, task=content, run=None, mode="engine", text=note)

    from decurix_agent.harness import perform_task
    perform_task(content, task_file)


def perform(content, task_file=None):
    """Do the request with the worker's own tool; the engine only when that cannot be done or did not work."""

    state.begin(content, task_file)

    try:
        book = _book(content)
        if book is None:
            return _to_engine(content, task_file, "Working this one out.")
        tool = book["tool"]
        if not script_sandbox.available():
            # Before any model call: a tool that cannot be shut off here is not run unattended at all.
            log(f"Tool {book['name']}: not run with no engine: {script_sandbox.why_not()}")
            return _to_engine(content, task_file, "I have a script of my own for this kind of request, but it does "
                                                  f"not run unattended here: {script_sandbox.why_not()}. So I am "
                                                  "working it out myself.")

        args, why = read_args(book, content)
        if args is None:
            log(f"Tool {book['name']}: not used for {content!r}: {why}")
            return _to_engine(content, task_file, "This is not quite what my tool does, so I am working it out.")

        line = " ".join(shlex.quote(a) for a in args)
        progress.emit(f"Running the tool I wrote for this kind of request: {Path(tool['path']).name} {line}")
        try:
            dry = []
            code, printed = _run(tool, args, dry)
        except NoSandbox as exc:
            log(f"Tool {book['name']}: not run with no engine: {exc}")
            return _to_engine(content, task_file, f"My script for this does not run unattended here ({exc}), so I am "
                                                  "working it out myself.")
        log(f"Tool {book['name']} {line}: exit {code}\n{printed[:2000]}")

        if code != 0:
            _note(book, False, failed=True)
            return _to_engine(content, task_file, "My tool did not work this time, so I am doing it by hand.",
                              note=f"A script of this desktop's own was tried first for this request and failed "
                                   f"(exit {code}): {printed[-800:]}\nDo the request yourself. It may have left a "
                                   "partial result: check what is there before you write.")

        reply = (f"Done with the tool I wrote for this kind of request (`{Path(tool['path']).name} {line}`).\n\n"
                 f"{printed[:MAX_OUTPUT_CHARS]}")
        made = produced.files(reply, request=content)
        # The same code-decided facts about the files as the final gate's (produced.file_facts), with no model.
        # The task's drafts, read once: a log that cannot be read is a check not made (N-1).
        drafts_unread = ""
        try:
            task_drafts = dry or produced.drafts()
        except Exception as exc:  # noqa: BLE001
            task_drafts, drafts_unread = [], f"this task's email drafts could not be read ({type(exc).__name__})"
        facts = produced.file_facts(content, made, drafts_made=task_drafts)
        if drafts_unread:
            facts.could_not.append(drafts_unread)
        from decurix_agent import source_guard, task_context
        # The person's given files that changed: facts for the judge and lines for the person, never put back while
        # checking (E-12); a check that cannot be made leaves the result not checked (E-01).
        changed_for_judge, changed_for_person = [], []
        try:
            changes = source_guard.changed_sources(task_context.current(), content)
            changed_for_judge, changed_for_person = source_guard.judge_lines(changes), source_guard.report_lines(changes)
        except Exception as exc:  # noqa: BLE001 - not made, never passed
            log(f"tool_task: could not check the shared folder's files: {exc}")
            facts.could_not.append(f"whether your own files were changed could not be checked ({type(exc).__name__})")
        source_guard.tidy_own_backups(content)
        for line in changed_for_person:
            progress.emit(line)
        # Only the invariants decide here (harness audit, 2026-10-21; FileFacts.blocking - E-27); what reads the request
        # is evidence for the judge.
        wrong = facts.blocking()
        progress.emit("Reading back what it wrote to check it against the request.")
        if wrong:
            verdict = {"verdict": verify.NOT_DONE, "confidence": 1.0, "seen": "; ".join(wrong)[:600],
                       "why": "what it wrote is not finished: its workbook's formulas have no results stored, a file "
                              "asked for was not made, a column filled down a table stops short of it, a placeholder is "
                              "left, or a value is on no record"}
        else:
            verdict = verify.judge(content, claim=reply, screen=False,
                                   produced="\n".join(x for x in (produced.text(made), produced.drafts_text(task_drafts),
                                                                  produced.notes_text(facts.notes() + changed_for_judge
                                                                                      + changed_for_person)) if x),
                                   look_up=lambda terms: produced.lines_with(made, terms))

        # The one decision (verify.outcome): a done the judge is not sure of, or with a check that could not be made,
        # is not done here either - it goes to the engine below like any unsettled check.
        settled = verify.outcome(verdict, not_made=facts.not_made())
        if settled["status"] == verify.DONE:
            # The check passed on the dry draft: the real draft is made now, once.
            failed = _draft_once(dry)
            if failed:
                _note(book, False, failed=True)
                return _to_engine(content, task_file, "My tool's result is right but its email could not be drafted, so "
                                  "I am doing that myself.", note=f"A script of this desktop's own made the files and "
                                  f"{failed} Draft the email yourself (office email), and do not make a second copy of "
                                  "anything already made.")
            _note(book, True)
            seen = f"\n\nChecked against the files it wrote: {verdict['seen']}" if verdict.get("seen") else ""
            told = ("\n\nPlease check:\n" + "\n".join(f"- {line}" for line in changed_for_person)
                    if changed_for_person else "")
            return write_final_reply(reply + seen + told, kind="final", engine=ENGINE, checked=True)

        if settled["status"] == verify.NOT_DONE:
            wrong = " ".join(x for x in (verdict.get("seen"), verdict.get("why")) if x)
            _note(book, False, failed=True, wrong=wrong or "its result was not what the request asked")
            return _to_engine(content, task_file, f"My tool's result is not right ({wrong[:200]}), so I am correcting it.",
                              note=f"A script of this desktop's own was run first for this request (`{tool['run']} "
                                   f"{line}`) and a check of what it wrote found: {wrong}\nCorrect what is wrong "
                                   "where it is, and report only what the files show.")

        # The check could not settle it. An engine's own file work goes out marked not checked; a
        # script's does not get that benefit of the doubt - nobody was reading along when it ran. The
        # engine looks at what it left and finishes or corrects it.
        _note(book, False)
        doubt = " ".join(x for x in (verdict.get("seen"), verdict.get("why")) if x)
        return _to_engine(content, task_file, "I could not confirm my tool's result, so I am checking it by hand.",
                          note=f"A script of this desktop's own was run first for this request (`{tool['run']} "
                               f"{line}`) and printed:\n{printed[:1500]}\nA check could not confirm that this is "
                               f"what the request asks{': ' + doubt if doubt else ''}. Read what it wrote, compare "
                               "it with the request, and correct or finish it where it is - do not make a second "
                               "copy of anything it already made. Report only what the files show.")
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=ENGINE)
