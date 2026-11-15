"""
The review pass after a task the final check judged done (docs/learning-loop.md, "The review pass").

After the report of a run checked done, the model that did the work is asked once more, by soul and from soul's own
copy of the run (its last transcript: the same prefix the run's last turn sent, so the call is mostly cache reads),
whether the run showed something a later task should know. It may propose a lesson about the person or the work, or a
note on how an application is operated, each resting on exact quotes. It only proposes: every proposal is checked in
code here, staged on the control plane (POST /api/desktops/proposals) and given to no worker until a person approves
it on the Workforce page. Proposing nothing is the expected outcome; nothing pushes the model to find something.

When it runs (wanted, something_to_learn): a report of kind "final" whose verdict is a checked done; a hosted brain (a
local 8192-token brain cannot be sent a run's transcript); a run TheBot's model did (no engine named: not a trusted
replay, a learned tool, a taught skill's replay, a recording or a web answer); not a task any message of which was
marked or withheld as secret (as kept facts); and something to learn from - the person answered a question on the task,
or the run ran at least DECURIX_REVIEW_MIN_COMMANDS commands that worked. DECURIX_REVIEW_PASS=0 turns it off.

It never blocks, delays or fails the report: messaging.write_final_reply calls after_report once the report is sent;
what it needs of the task is read there, at once, and the rest runs on a daemon thread of its own whose every failure
is logged and dropped (like progress.emit). The review's own tokens are booked to the task by the proxy under its own
purpose (py.learn.review); they are not in the totals the report already carried.

The checks before anything is staged (check), each mechanical and domain-free:
- every quote stands word for word (spacing aside, case kept) in what the run read (soul says which: `in_run`) or in
  the person's own words on the task (its request, their confirmed answers); one that stands in neither drops the
  proposal;
- nothing holding a secret: the task's own secret values, a remembered one, or anything shaped like one
  (kept_facts._holds_secret), and nothing privacy.scrub would change;
- no duplicate of what the worker is already given (the organisation's lines) or keeps in memory, or of another
  proposal of the run;
- a `replaces` must be the exact current text of a line this worker is given (read before write);
- at most MAX_PROPOSALS.
The control plane checks again what it can: the person's quotes against its own record, secrets, duplicates, the
replaced line, and the cap.
"""

import json
import os
import re
import threading
import urllib.error
import urllib.request

from decurix_agent import config
from decurix_agent.logging_utils import log

SWITCH = "DECURIX_REVIEW_PASS"
MIN_COMMANDS_SETTING = "DECURIX_REVIEW_MIN_COMMANDS"
# Commands that worked, on a run where the person said nothing: below this, a run is too short to teach anything.
DEFAULT_MIN_COMMANDS = 8
MAX_PROPOSALS = 3
MAX_QUOTES = 3
KINDS = ("lesson", "app_note")
# The review's answer is a few proposals of a sentence or two each.
MAX_TOKENS = 1200
# The call's own deadline. Short enough that the proxy's row of it lands within the minute after the run's end that a
# trajectory joins (docs/trajectory.md).
TIMEOUT_S = 50
# The one retry of an answer cut off at the limit gets room to think: it runs after the report, on its own thread.
RETRY_TIMEOUT_S = 240
POST_TIMEOUT_S = 10
PROPOSALS_PATH = "/api/desktops/proposals"
MAX_NOTES_CHARS = 8000
# A knowledge line as the worker is told it: "Lesson (worker): <text>".
_LINE = re.compile(r"^[A-Z][a-z_]* \([^)]*\): ")


def enabled():
    """Whether the review pass is on (DECURIX_REVIEW_PASS, on unless 0/false/off/no)."""

    return os.environ.get(SWITCH, "1").strip().lower() not in ("0", "false", "off", "no")


def min_commands():
    """DECURIX_REVIEW_MIN_COMMANDS, at least 1; DEFAULT_MIN_COMMANDS when unset or not a number."""

    try:
        return max(1, int(os.environ.get(MIN_COMMANDS_SETTING, "") or DEFAULT_MIN_COMMANDS))
    except ValueError:
        return DEFAULT_MIN_COMMANDS


def wanted(kind, checked, verdict, engine, quiet=False, interrupted=False, stopped=False):
    """(True, "") when a report's run is one to review; else (False, why). Reads nothing but its arguments and the
    desktop's settings: what was learned is asked later (something_to_learn)."""

    if not enabled():
        return False, "the review pass is off"
    if kind != "final" or quiet or interrupted or stopped:
        return False, "not a finished task's report"
    if not checked or not isinstance(verdict, dict) or verdict.get("verdict") != "done":
        return False, "the final check did not judge it done"
    if not config.HOSTED_BRAIN:
        return False, "a local brain cannot be sent the run again"
    if engine not in (None, ""):
        return False, "the run was not the model's own"
    return True, ""


def something_to_learn(answers, commands):
    """Whether a run has something to learn from: the person answered a question on it, or it ran at least
    min_commands() commands that worked."""

    return bool(answers) or int(commands or 0) >= min_commands()


def after_report(task_id, kind, checked, verdict, engine, quiet=False, interrupted=False, stopped=False):
    """
    Start the review of the run whose report was just sent, on a thread of its own, and return the thread (None when
    the run is not one to review). Called by messaging.write_final_reply after the report is sent and before the task
    lets go of its values: what the review needs of the task (its request, how many commands worked, its secret values)
    is read here, at once. Never raises and never waits.
    """

    try:
        ok, why = wanted(kind, checked, verdict, engine, quiet=quiet, interrupted=interrupted, stopped=stopped)
        if not ok:
            return None
        from decurix_agent import playbooks, state, task_context
        from decurix_agent.computer import privacy
        task = task_id or task_context.current()
        if not task_context.valid(task) or task != task_context.current():
            return None
        if task_context.held_values(task) or getattr(state, "reply_secret", False):
            log("Review: not made - this task's messages held a secret")
            return None
        secrets = tuple(privacy.plain_values(task_context.secret_values(task)))
        request = task_context.current_request(full=True) or str(state.current_task or "")
        commands = len(playbooks.ran_commands())
        thread = threading.Thread(target=_review_safely, args=(task, request, commands, secrets), daemon=True,
                                  name="review-pass")
        thread.start()
        return thread
    except Exception as exc:  # noqa: BLE001 - the review never costs the report
        log(f"Review: could not start ({type(exc).__name__}: {exc})")
        return None


def _review_safely(task, request, commands, secrets):
    try:
        review(task, request, commands, secrets)
    except Exception as exc:  # noqa: BLE001 - best-effort, like progress.emit
        log(f"Review: not made ({type(exc).__name__}: {str(exc)[:200]})")


def review(task, request, commands, secrets):
    """Review one finished run: ask soul, check what it proposes, stage what passes. Returns what was posted."""

    from decurix_agent import person_record, soul_call
    answers = person_record.answers(task)
    if not something_to_learn(answers, commands):
        log(f"Review: not made - the person answered nothing and {commands} command(s) worked")
        return []
    try:
        answer = soul_call.review(task, secrets, notes=_notes(answers), max_tokens=MAX_TOKENS, timeout=TIMEOUT_S)
        if answer.get("truncated"):
            # A reasoning model can spend the whole budget thinking and answer nothing (llm_client's rule: an answer
            # cut off at the limit is no answer - once more, with room). Found live (2026-11-15): the review got no
            # JSON at 1200 tokens and proposed nothing.
            from decurix_agent.llm_client import EMPTY_RETRY_TOKENS
            log(f"Review: the answer was cut off at {MAX_TOKENS} tokens - asking again with {EMPTY_RETRY_TOKENS}")
            answer = soul_call.review(task, secrets, notes=_notes(answers), max_tokens=EMPTY_RETRY_TOKENS,
                                      timeout=RETRY_TIMEOUT_S)
    except soul_call.Unusable as exc:
        log(f"Review: not made - soul could not ({exc})")
        return []
    decision = answer.get("decision") or {}
    if decision.get("error"):
        log(f"Review: the answer could not be read ({str(decision['error'])[:120]}); nothing proposed")
        return []
    knowledge, memory = _known()
    dropped = {}
    proposals = check(decision.get("proposals"), request=request, answers=answers, secrets=secrets,
                      knowledge=knowledge, memory=memory, dropped=dropped)
    raw = decision.get("proposals") if isinstance(decision.get("proposals"), list) else []
    left_out = "; ".join(f"{why}: {n}" for why, n in sorted(dropped.items()))
    log(f"Review ({decision.get('source') or 'unknown'}): {len(raw)} proposed, {len(proposals)} passed the checks"
        + (f" (left out - {left_out})" if left_out else ""))
    if proposals:
        post(task, proposals, model=str(answer.get("model") or ""))
    return proposals


def _notes(answers):
    """The person's confirmed answers on the task, for a review soul makes from its job log."""

    lines = []
    for a in answers or ():
        asked, said = " ".join(str(a.get("asked") or "").split()), " ".join(str(a.get("text") or "").split())
        if said:
            lines.append((f"Asked: {asked}\n" if asked else "") + f"Said: {said}")
    return "\n\n".join(lines)[:MAX_NOTES_CHARS]


def _known():
    """(the texts of the lines this worker is given, the worker's memory entries' texts), each [] when unread."""

    knowledge, memory = [], []
    try:
        from decurix_agent import org_knowledge
        knowledge = [_LINE.sub("", line, count=1) for line in org_knowledge.cached()]
    except Exception as exc:  # noqa: BLE001
        log(f"Review: the organisation's lines could not be read ({type(exc).__name__})")
    try:
        from decurix_agent import memory_context
        memory = [entry["text"] for entry in memory_context._entries()]
    except Exception as exc:  # noqa: BLE001
        log(f"Review: memory could not be read ({type(exc).__name__})")
    return knowledge, memory


def _collapse(text):
    return " ".join(str(text or "").split())


def _words(text):
    return " ".join(re.findall(r"[^\W_]+", str(text or "").lower()))


def _stands_in(quote, texts):
    """Whether quote stands word for word (spacing aside, case kept) in one of texts."""

    q = _collapse(quote)
    return bool(q) and any(q in _collapse(t) for t in texts if t)


def _duplicate(text, known):
    """Whether text says what one of known already says: the same words, or all of them within a longer entry."""

    words = _words(text)
    return bool(words) and any(words == _words(k) or f" {words} " in f" {_words(k)} " for k in known if k)


def _holds_secret(texts, secrets):
    from decurix_agent import kept_facts
    return kept_facts._holds_secret(*texts, held=secrets)


def check(raw, *, request="", answers=(), secrets=(), knowledge=(), memory=(), dropped=None):
    """
    The proposals that pass the code checks (see the module's docstring), each as the control plane takes it:
    {"kind", "text", "why", "evidence": [{"quote", "where": "run" | "person"}], "replaces"}. `dropped`, a dict when given,
    counts why each other proposal was left out (by check, never its text), for the log.
    """

    def drop(why):
        if dropped is not None:
            dropped[why] = dropped.get(why, 0) + 1

    person = [request] + [str(a.get("text") or "") for a in answers or () if isinstance(a, dict)]
    out = []
    for p in raw if isinstance(raw, list) else []:
        if len(out) >= MAX_PROPOSALS:
            drop("over the cap")
            break
        if not isinstance(p, dict) or p.get("kind") not in KINDS:
            drop("not a known kind")
            continue
        text, why, replaces = _collapse(p.get("text")), _collapse(p.get("why")), _collapse(p.get("replaces"))
        quotes, in_run = p.get("evidence"), p.get("in_run")
        if not text or not isinstance(quotes, list) or not quotes or len(quotes) > MAX_QUOTES:
            drop("no text or no quotes")
            continue
        if not isinstance(in_run, list) or len(in_run) != len(quotes):
            in_run = [False] * len(quotes)
        evidence = []
        for quote, ran in zip(quotes, in_run):
            if not isinstance(quote, str) or not quote.strip():
                break
            if _stands_in(quote, person):
                evidence.append({"quote": quote.strip(), "where": "person"})
            elif ran is True:
                evidence.append({"quote": quote.strip(), "where": "run"})
            else:
                break
        if len(evidence) != len(quotes):
            drop("a quote stands nowhere")
            continue  # a quote that stands nowhere: the whole proposal is suspect
        texts = [text, why, replaces] + [e["quote"] for e in evidence]
        if _holds_secret(texts, secrets):
            drop("holds a secret")
            continue
        item = {"kind": p["kind"], "text": text, "why": why, "evidence": evidence, "replaces": replaces}
        try:
            from decurix_agent.computer import privacy
            if privacy.scrub(item) != item:
                drop("withholding would change it")
                continue
        except Exception:  # noqa: BLE001 - what cannot be checked is not proposed
            drop("could not be checked")
            continue
        if _duplicate(text, list(knowledge) + list(memory) + [o["text"] for o in out]):
            drop("already known")
            continue
        if replaces and not any(_collapse(k) == replaces for k in knowledge):
            drop("replaces a line it is not given")
            continue
        out.append(item)
    return out


def post(task, proposals, model=""):
    """Stage proposals on the control plane, as this desktop. Returns its answer ({"kept", "refused"}) or None. Not
    retried: a proposal lost is only a lesson not proposed."""

    body = {"task_id": task, "model": model[:128], "proposals": proposals}
    request = urllib.request.Request(config.CONTROL_PLANE_URL.rstrip("/") + PROPOSALS_PATH,
                                     data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
                                     headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=POST_TIMEOUT_S) as response:
            answer = json.loads(response.read(64 * 1024).decode("utf-8"))
    except urllib.error.HTTPError as exc:
        log(f"Review: the control plane did not stage the proposals ({exc.code})")
        return None
    except Exception as exc:  # noqa: BLE001
        log(f"Review: the proposals could not be sent ({type(exc).__name__})")
        return None
    kept = len(answer.get("kept") or []) if isinstance(answer, dict) else 0
    refused = answer.get("refused") or [] if isinstance(answer, dict) else []
    log(f"Review: {kept} proposal(s) staged for a person" + (f", {len(refused)} refused there" if refused else ""))
    return answer
