import json
import os
import re
import time
import traceback
from pathlib import Path

from decurix_agent import org_knowledge
from decurix_agent import values as field_values
from decurix_agent import verdict as verdicts
from decurix_agent import ledger_checks
from decurix_agent import (attachments, binding, config, guidance, llm_client, messaging, playbooks, produced, progress,
                           request_reading, source_guard, state, task_context, usage, verify)
from decurix_agent.computer import (approval, constraints, core, escalation, evidence, experience, launchers, learning,
                                    mimic, replay, skills)
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log
from decurix_agent.messaging import ask_for_query, write_final_reply
from decurix_agent.replay_task import OPTION_RE

# Split out of this module (follow-up O-10), and re-exported here by name for whoever reads them through it: the
# commands and reply words (skill_commands), teaching (teaching), an engine's question in the middle of a task
# (engine_questions) and the final gate with its restore offer (final_gate). None of them imports this module: what
# they call back here is handed over below (use_run), each looked up here when called, so a test that patches
# skill_task._engine_can_drive or skill_task.write_final_reply patches what they read. A name only moved code reads
# (final_gate._retry_engine, teaching._remember) is patched where it lives, and is not re-exported.
from decurix_agent import engine_questions, final_gate, teaching
from decurix_agent.skill_commands import (  # noqa: F401 - re-exported
    ANYWAY_RE, CARRY_ON_RE, LEARN_ENGINE, learn_request, NUMBER_RE, parse_learn, parse_restore, parse_skill,
    _Refused, request_text, skill_context, SKILL_ENGINE, USE_RE,
)
from decurix_agent.teaching import (  # noqa: F401 - re-exported
    _ask, _learn, _learn_reply,
)
from decurix_agent.engine_questions import (  # noqa: F401 - re-exported
    _carried_on, _carries_on, guided_skill_title, learn_answer, _question_gate, _self_answered,
)
from decurix_agent.final_gate import (  # noqa: F401 - re-exported
    _actions_note, _attempt_record, _calls_off, _check_not_made, check_unchecked, _confirm_reply, _confirm_round,
    _confirm_rounds, _confirms, _did_screen_work, _gate, _given_up_text, ledger_held, NOT_YOURS_TO_FORCE, _rechecks,
    _record_unread, REPORT_CHARS, _restore_reply, _restore_request, UNCHECKED_ANSWER, _with_said,
)

teaching.use_run(
    write_final_reply=lambda *args, **kwargs: write_final_reply(*args, **kwargs),
    ask_for_query=lambda *args, **kwargs: ask_for_query(*args, **kwargs),
)
engine_questions.use_run(
    ask_for_query=lambda *args, **kwargs: ask_for_query(*args, **kwargs),
    _criteria=lambda *args, **kwargs: _criteria(*args, **kwargs),
    _for_this_run=lambda *args, **kwargs: _for_this_run(*args, **kwargs),
)
final_gate.use_run(
    write_final_reply=lambda *args, **kwargs: write_final_reply(*args, **kwargs),
    ask_for_query=lambda *args, **kwargs: ask_for_query(*args, **kwargs),
    _after_phase=lambda *args, **kwargs: _after_phase(*args, **kwargs),
    _basis=lambda *args, **kwargs: _basis(*args, **kwargs),
    _by_model=lambda *args, **kwargs: _by_model(*args, **kwargs),
    _correct=lambda *args, **kwargs: _correct(*args, **kwargs),
    _doubt_text=lambda *args, **kwargs: _doubt_text(*args, **kwargs),
    _engine=lambda *args, **kwargs: _engine(*args, **kwargs),
    _engine_can_drive=lambda *args, **kwargs: _engine_can_drive(*args, **kwargs),
    _evidence_text=lambda *args, **kwargs: _evidence_text(*args, **kwargs),
    _judge=lambda *args, **kwargs: _judge(*args, **kwargs),
    _ledger_faults=lambda *args, **kwargs: _ledger_faults(*args, **kwargs),
    _ledger_why=lambda *args, **kwargs: _ledger_why(*args, **kwargs),
    _never_entered=lambda *args, **kwargs: _never_entered(*args, **kwargs),
    _reads_replies=lambda *args, **kwargs: _reads_replies(*args, **kwargs),
    _record_verdict=lambda *args, **kwargs: _record_verdict(*args, **kwargs),
    _says_it_could_not=lambda *args, **kwargs: _says_it_could_not(*args, **kwargs),
    _stopped=lambda *args, **kwargs: _stopped(*args, **kwargs),
    _succeed=lambda *args, **kwargs: _succeed(*args, **kwargs),
    _success_verdict=lambda *args, **kwargs: _success_verdict(*args, **kwargs),
)


# ============================================================
# Teaching a desktop, and asking it to do what it was taught
#
# "/learn <recording> [<recording> ...]" turns demonstrations into a skill by
# asking the person who gave them whatever the recordings cannot say
# (computer/learning.py). "/skill <name> [param=value ...]" runs a skill it
# was taught: computer/replay.py does the work with no model, and this module
# is what happens around it when the desktop is not sure.
#
# Like "/replay" and "/record", neither goes to an engine: the command watcher
# dispatches them here, they queue behind a running task, carry their task ID
# and report through the same messaging calls, and every question is an
# ordinary query the next request in the chat answers. They report as engines
# "learn" and "skill", with no model and no tokens.
#
# The rule for running is the one replay.py has for finishing: never a
# success that was not seen, and never a guess. When something is not as it
# was - a value is missing, the screen is not where the task began, a
# control is gone - the desktop stops and asks. What a person answers is kept
# when it was a correction that then worked (a control's new name), never
# when it was a one-off (skip this step today).
#
# This is not a harness: harness.py still decides which engine answers
# everything else, and nothing here asks which one that is.
# ============================================================


MAX_STUCK_ASKS = 3


def _ask_model(system, prompt, purpose="py.skill.ask"):
    """
    One question to this desktop's own engine, for the escalation ladder. The
    import is here, not at the top, because harness.py reads the configured
    engine at import and this module must load without one (and be testable).
    `purpose`: what the question is for; the ladder and mimic name their own.
    """

    from decurix_agent import harness

    return harness.ask_model(system, prompt, purpose=purpose)


def _ladder(skill):
    """
    The escalation ladder for one run: healing always, the engine as second
    opinion only when it is up (a run must not wait on a backend that is still
    starting - the person is a better rung than a stalled request).
    """

    ready = getattr(state, "backend_ready", False) and escalation.MODEL_CALLS > 0
    return escalation.Ladder(ask_model=_ask_model if ready else None, goal=skill.get("title") or skill.get("name") or "",
                             explanation=skill.get("explanation") or "")


def handles(content):
    """Whether a request is one of this module's ("/learn", "/skill", "/restore"), which need no reasoning backend."""

    return parse_learn(content) is not None or parse_skill(content) is not None or parse_restore(content) is not None


def mimics(content):
    """
    Whether a plain request is a recorded task done again with other values
    (computer/mimic.py): what the command watcher asks before handing a request
    to the engine. Reads recordings only - no model.
    """

    # Its words only: the files a request brought are this run's documents
    # (binding.py reads its values from them), not part of what was asked.
    try:
        return mimic.plan(attachments.text(content)) is not None
    except Exception as exc:
        log(f"Could not compare the request with the recordings: {exc}")
        return False


def _engine(content, task_file):
    """Hand a request to the engine after all (imported here: harness.py reads the engine at import)."""

    from decurix_agent.harness import perform_task
    perform_task(content, task_file)


def may_be_taught(content):
    """
    Whether a request that mimic.py does not recognise is still worth putting
    to the hosted model beside what was taught here (request_reading.py): the
    same task worded another way. Cheap; the model is asked by perform_mimic.
    """

    try:
        return request_reading.worth_reading(attachments.text(content))
    except Exception as exc:
        log(f"Could not compare the request with the taught tasks: {exc}")
        return False


def perform_mimic(content, task_file=None):
    """
    Do a request the way a person recorded a similar task: the recording's own
    steps, with only what the request changes put in.

    With a hosted model, the model reads the request - which taught task it
    is, and what to type for each input (request_reading.py); what it says is
    checked against the request before it is used. Without one, the request is
    compared word by word with the recorded ticket (computer/mimic.py), the
    model is asked only for a value typed in another form, and a person only
    when it cannot say.
    """

    _begin(content, task_file)

    # What the person asked, without the files they attached: those are named
    # in `content`, which the run keeps as its context - a value the words do
    # not give is read from them (_prepare, binding.py).
    said = attachments.text(content)

    reading = None
    if request_reading.available():
        progress.emit("Reading the request against what you taught me.")
        try:
            reading = request_reading.read(said)
        except Exception as exc:
            log(f"Could not read the request with the model ({exc}); comparing it with the recordings instead.")
            log(traceback.format_exc())

    if reading is not None:
        _perform_read(content, task_file, reading)
        return

    try:
        found = mimic.plan(said)
        skill, one_off = mimic.load_skill(found) if found else (None, False)
    except (FileNotFoundError, ValueError, OSError) as exc:
        log(f"Could not load what to mimic for {content!r}: {exc}")
        found, skill = None, None

    if skill is None:
        # The recording went away, or has nothing a replay can run, between the
        # watcher's look and now: the engine takes it after all.
        _engine(content, task_file)
        return

    try:
        values, pending, changed = mimic.values_for(skill, found)
        unplaced = [span for span in found["spans"] if not span["placed"]]

        if unplaced and not pending:
            # A value changed that nothing typed holds: not something to adapt.
            log(f"{content!r} differs from {found['text']!r} in a way the recording cannot take - the engine does it")
            _engine(content, task_file)
            return

        if pending:
            progress.emit("The request changes something the recording typed in another form - "
                          "working out what to type now.")
            ask = _ask_model if state.backend_ready else None
            values, adapted = mimic.adapt_values(skill, found, said, values, pending, ask)
            if values is None:
                log(f"The model would type {found['text']!r}'s values unchanged for {content!r} - the engine does it")
                _engine(content, task_file)
                return
            changed += adapted

        _start_mimic(content, found, skill, one_off, values, changed)
    except _Refused as refusal:
        write_final_reply(str(refusal), kind="error", engine=SKILL_ENGINE)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=SKILL_ENGINE)


def _perform_read(content, task_file, reading):
    """Carry out a request the hosted model read (request_reading.read's answer)."""

    if reading["plan"] is None:
        # Not something taught here, as the model reads it: the engine's.
        _engine(content, task_file)
        return

    try:
        found, skill, values = dict(reading["plan"]), reading["skill"], dict(reading["values"])

        # What the request changes on a control or a page (never in what is
        # typed - those are the model's values): "open invoice 1043" clicks
        # "Invoice 1043". Only the word-by-word comparison knows where the
        # recording holds such a word, and only when it sees the same task.
        try:
            literal = mimic.plan(attachments.text(content))
        except Exception:
            literal = None
        if literal and (literal["kind"], literal["id"]) == (found["kind"], found["id"]):
            found["spans"] = [span for span in literal["spans"]
                              if span["placed"] and "typed" not in (span.get("where") or [])]

        changed = [(p.get("label") or p["name"], str(p.get("example")), values[p["name"]])
                   for p in skill.get("params") or []
                   if p["name"] in values and p.get("example") is not None and values[p["name"]] != str(p["example"])]

        if reading.get("dropped"):
            labels = ", ".join((p.get("label") or p["name"]) for p in skill.get("params") or []
                               if p["name"] in reading["dropped"])
            progress.emit(f"I could not find {labels} in your request, so I will ask rather than guess.")

        _start_mimic(content, found, skill, reading["one_off"], values, changed)
    except _Refused as refusal:
        write_final_reply(str(refusal), kind="error", engine=SKILL_ENGINE)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=SKILL_ENGINE)


def _start_mimic(content, found, skill, one_off, values, changed):
    said = f"Doing this the way you recorded \"{found['text']}\""
    if changed:
        said += " - typing " + "; ".join(f"\"{new}\" where you typed \"{old}\"" for _, old, new in changed)
    elif any(span["placed"] for span in found["spans"]):
        said += " - with " + "; ".join(f"\"{span['new']}\" for \"{span['old']}\"" for span in found["spans"])
    progress.emit(said + ".")
    log(f"Mimicking {found['kind']} {found['id']} for {content!r}: {changed or found['spans']}")

    run = {"skill": skill, "values": values, "start": 1, "checked": False, "patches": [], "asked": 0, "stuck": 0,
           "ladder": _ladder(skill), "asked_ids": [], "mimic": found, "one_off": one_off, "context": content,
           "began": time.time()}
    _prepare(run)


def _begin(content, task_file):
    state.begin(content, task_file)
    state.paused_skill = None

    # Known, not unknown: nothing here reads a prompt or costs anything.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)


def perform(content, task_file=None, source_files=None):
    """Run a "/learn" or "/skill" request as the task the watcher dispatched it as."""

    _begin(content, task_file)

    try:
        if parse_learn(content) is not None:
            _learn(*parse_learn(content))
        elif parse_restore(content) is not None:
            _restore_request(*parse_restore(content))
        else:
            _run_request(*parse_skill(content), context=skill_context(content), source_files=source_files)
    except _Refused as refusal:
        write_final_reply(str(refusal), kind="error", engine=LEARN_ENGINE if parse_learn(content) is not None else SKILL_ENGINE)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=SKILL_ENGINE)


def resume(reply, approval_outcome=None):
    """
    The human's reply to a question this module asked. The watcher has already
    settled an on-screen approval with it (approval_outcome is "granted",
    "declined" or None).
    """

    paused = state.paused_skill
    state.paused_skill = None
    state.waiting_for_query = False

    if paused is None:
        write_final_reply("There was nothing waiting for that answer.", kind="error", engine=SKILL_ENGINE)
        return

    engine = LEARN_ENGINE if paused["mode"] == "learn" else SKILL_ENGINE

    try:
        if paused["mode"] == "learn":
            _learn_reply(paused, reply)
        elif paused["mode"] == "confirm":
            _confirm_reply(paused, reply)
        elif paused["mode"] == "restore":
            _restore_reply(paused, reply)
        else:
            _run_reply(paused, reply, approval_outcome)
    except _Refused as refusal:
        write_final_reply(str(refusal), kind="error", engine=engine)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=engine)


# ============================================================
# Running
# ============================================================

def _listing():
    found = skills.all_skills()

    if not found:
        return ("I have not been taught any skills yet. Record yourself doing a task, then send `/learn <recording>` "
                "and I will ask what I need to know to do it myself.")

    lines = []
    for skill in found:
        if skill.get("status") == skills.READY:
            needs = ", ".join(p["name"] for p in skills.required(skill))
            lines.append(f"`{skill['name']}` - {skill['title']}, {len(skill['steps'])} steps" + (f" (needs {needs})" if needs else ""))
        else:
            lines.append(f"`{skill['name']}` - {skill['title']}: unfinished, send `/learn {skill['sources'][0]}` to finish it")

    return "Skills on this desktop:\n" + "\n".join(lines) + "\nRun one with `/skill <name>`."


def _run_request(query, given, context="", source_files=None):
    if not query:
        write_final_reply(_listing(), kind="final", engine=SKILL_ENGINE)
        return

    skill, candidates = skills.find(query)

    if skill is None:
        if candidates:
            names = ", ".join(f"`{s['name']}`" for s in candidates)
            # (A skill runs only on its exact name: shared words only suggest - D-16.)
            raise _Refused(f"No skill is named \"{query}\"; these come close: {names}. Say which, by its name.")
        raise _Refused(f"I do not know a skill like \"{query}\". " + _listing())

    if source_files is not None:
        try:
            skill = skills.bind_sources(skill, source_files)
        except ValueError as exc:
            raise _Refused(f"Cannot use those source files: {exc}") from exc

    run = {"skill": skill, "values": skills.initial_values(skill, given), "start": 1, "checked": False, "patches": [],
           "asked": 0, "stuck": 0, "ladder": _ladder(skill), "asked_ids": [], "context": context, "began": time.time()}
    _prepare(run)


def _start_pause(run):
    """
    What the person chose when they taught it (the `approval` question): some tasks are theirs to start (they spend
    money, commit them to something), and the words of a request cannot tell which. `ask` pauses a run from step 1
    for a yes before anything starts - whichever way it would start: the replay, the model doing it while it learns,
    or the model gathering its documents. `go` starts at once. A skill taught before this was asked has no answer and
    runs as it always did. True when the run paused. Either way it is about starting only: every consequential step
    still stops at the approval gate, every run.
    """

    skill = run["skill"]
    if run["start"] != 1 or skill.get("approval") != "ask" or run.get("approved") or run.get("one_off"):
        return False
    shown = "; ".join(f"{k} = {el._quote(str(v), 60)}" for k, v in run["values"].items())
    _pause(run, "go", f"You taught me to check with you before I start \"{skill.get('title') or skill['name']}\"."
           + (f" This run would use: {shown}." if shown else "")
           + " Reply `yes` to start, or `no` to stop.", ask_kind="approval")
    return True


def _pause(run, kind, text, ask_kind="query", secret=False, **extra):
    """Pause the run on a question. `secret`: the question asks for a value the skill itself marks secret (a
    parameter's own "secret"), so the person's whole reply is withheld wherever it is kept (X-6) - from structure,
    never from the question's words."""

    state.paused_skill = dict({"mode": "run", "kind": kind, "run": run}, **extra)
    ask_for_query(text, kind=ask_kind, engine=SKILL_ENGINE, secret=bool(secret))


def _prepare(run):
    """What must be settled before the first step: every value a run needs, and where the screen is."""

    skill = run["skill"]
    # Which taught skill drives this task, for its outcome (R7, C5): not a one-off mimic of a recording, which is no
    # taught skill.
    task_context.set_skill("" if run.get("one_off") else skill.get("name") or "", skill.get("version", 1))

    missing = [p for p in _needed(run) if not run["values"].get(p["name"])]

    # A row the demonstration had and this run's document does not is not a value to find or to ask for.
    if run.get("rows"):
        used = {name for row in run["rows"]["names"] for name in row}
        absent = {name for row in (skills.repeated_rows(skill) or {"rows": []})["rows"] for name in row} - used
        missing = [p for p in missing if p["name"] not in absent]

    # Lists that do not line up are asked for again, whole - which item a
    # value belongs to is not something to guess. (A gathered run's lists are the per-item block's; a navigation
    # loop before the block has none and is not run.)
    problem = None if missing else skills.list_problem({"params": _needed(run)} if run.get("gathered") == "done" else skill,
                                                       run["values"])
    if problem:
        missing = skills.list_params(skill)
        for param in missing:
            run["values"].pop(param["name"], None)

    # A value the field cannot take is asked for again, saying why, rather
    # than typed into it.
    wrong = [] if missing else constraints.violations(skills.required(skill), run["values"], split=skills.split_items)
    if wrong:
        why = " ".join(f"{param['label']}: {reason}." for param, reason in wrong)
        missing = [param for param, _ in wrong]
        for param in missing:
            run["values"].pop(param["name"], None)
        problem = why

    # Values the request did not give are read from its documents first, once
    # per run, when a hosted model is there to read them (binding.py). What it
    # cannot find is asked for exactly as before.
    if missing and not run.get("filled") and binding.available() and run.get("context"):
        run["filled"] = True
        progress.emit("Reading the values this task needs from its documents.")
        # A run the model does reads its own sources: only what the request and its documents hold is handed to it as
        # this run's values - a guess handed on reads as the person's (binding.fill).
        found, evidence, rows = binding.fill(skill, missing, run["context"], strict=_learning_by_model(run))
        if rows:
            # How many entries this run's document has of the part the demonstration repeated.
            run["rows"] = rows
            for row in (skills.repeated_rows(skill) or {"rows": []})["rows"][rows["count"]:]:
                for name in row:
                    run["values"].pop(name, None)
        if found:
            run["values"].update(found)
            run.setdefault("evidence", {}).update(evidence)
            progress.emit("Read from the documents:\n" + binding.report(evidence, found))
            _prepare(run)
            return

    # A task done once per document whose documents are not in hand - they come by mail or chat, and the person's
    # first steps save them: the model gathers them, then the lists are read from them and the per-item block replays
    # ("The per-document part" below).
    if missing and not problem and run.get("filled") and run["start"] == 1 and not run.get("gathered") \
            and any(p.get("list") for p in missing) and _loop_start(skill) > 1 and _engine_can_drive() \
            and binding.available():
        if not _start_pause(run):
            _gather(run)
        return

    # A task the model does while it is still learning it takes its values as guidance, and reads the run's own
    # documents itself: a value the request and its documents do not have is no reason to stop. One demonstration
    # fixes the task's shape to what it showed - a bill of two lines - and the next bill had one: found live, the run
    # asked for the "second item" before anything else, and "there is no second line" could not answer that question.
    if missing and not problem and run.get("filled") and run["start"] == 1 and _learning_by_model(run):
        if _start_pause(run):
            return
        progress.emit("The documents do not give " + ", ".join(p["name"] for p in missing)
                      + " - working from the documents themselves.")
        _by_model(run)
        return

    if missing:
        _pause(run, "values", (problem + " " if problem else "") + _need_text(skill, missing),
               secret=any(p.get("secret") for p in missing), missing=[p["name"] for p in missing])
        return

    if _start_pause(run):
        return

    if run["start"] == 1 and _learning_by_model(run):
        _by_model(run)
        return

    if run["start"] == 1 and not run["checked"]:
        checks = learning.start_checks(skill)

        if checks:
            try:
                observation = core.observe()
            except ComputerError as exc:
                raise _Refused(f"I could not look at the screen to check where to begin: {exc}")

            if replay.unmet(checks, observation):
                seen = replay._screen_brief(observation)
                _pause(run, "start", f"\"{skill['title']}\" begins from {', '.join(replay.describe_check(c) for c in checks)}, "
                       f"but {seen}. Carry on from here anyway? Reply `yes` to try, or `no` to stop.")
                return

    _execute(run)


# ============================================================
# The per-document part: the harness drives the loop, the model the exceptions
#
# A task taught on several documents folds into the steps before the first one (open the mail, make the month's
# folder, open the application), one block done once per document (learning._find_loops: each typed value a list with
# one item per document) and the steps after the last. Run as the engine's with the skill as its guide, every item
# cost the model its whole way through, and the model never reached for the replay (measured 2026-11-01: 3-8M tokens a
# month, the fourth month no cheaper than the first). So a run whose documents are not in hand goes:
#   gather   the model does the steps before the block for this request - saves every document where the person's
#            steps put theirs - and enters nothing (_by_model with phase "gather"; its report comes back through
#            _gate to _after_phase);
#   fill     the lists are read from the documents that appeared, in one model call (binding.fill_lists); a document
#            the person's rules leave out is named with its rule;
#   replay   the block runs once per document (_execute from its first step; skills.expand writes it out per item),
#            and a halt hands the rest to the model from that step, as any replay's halt does;
#   finish   _finish judges the result as it judges every replay.
# Trust (experience.stage) decides whether the plan is put to the person before the first replay - never what is
# checked: every step's expectation, the approval gate, the ledger's pre-save comparison and read-back apply as always.
# ============================================================

DOCUMENT_SUFFIXES = {".pdf", ".docx", ".doc", ".xlsx", ".xls", ".csv", ".jpg", ".jpeg", ".png", ".txt", ".odt", ".ods"}
MAX_GATHERED_FILES = 40
GATHER_PHASE = "gather"
# The documents the fill set aside as not fitting the taught shape: the model enters them after the replay (_finish).
REST_PHASE = "rest"


def _needed(run):
    """
    The values this run must be given: every one learned to change - or, for a run that begins at the per-item block
    after gathering its documents, those of the steps from there on that a document can give. What the gather typed is
    done, and an address (a page, a folder) is nowhere in a document: a step that types one is the model's when the
    replay reaches it.
    """

    skill = run["skill"]
    if run.get("gathered") == "done" and run["start"] > 1:
        return [p for p in skills.required(skill)
                if (p.get("step") or 0) >= run["start"] and not learning._is_address(p.get("example"))]
    return skills.required(skill)


def _item_loop(skill):
    """The per-item loop: the one that is on and repeats the most steps (a navigation repeat before it is small), or None."""

    loops = [loop for loop in skill.get("loops") or [] if loop.get("enabled") and loop.get("blocks") and loop["blocks"][0]]
    return max(loops, key=lambda loop: (loop.get("length") or 0) * (loop.get("reps") or 0), default=None)


def _loop_start(skill):
    """The number of the first step of the per-item block, or 0 when the skill has none."""

    loop = _item_loop(skill)
    by_id = {s["id"]: s["n"] for s in skill.get("steps") or []}
    return by_id.get(loop["blocks"][0][0], 0) if loop else 0


def _document_files():
    """Every document file under the home folder, with its modification time (hidden folders aside)."""

    found = {}
    for root, dirs, files in os.walk(Path.home()):
        dirs[:] = [d for d in dirs if not d.startswith(".") and d not in ("snap", "node_modules")]
        for name in files:
            path = Path(root) / name
            if path.suffix.lower() in DOCUMENT_SUFFIXES:
                try:
                    found[str(path)] = path.stat().st_mtime
                except OSError:
                    continue
    return found


def _gather(run):
    run["gathered"] = "started"
    run["files_before"] = _document_files()
    progress.emit("This task is done once per document: collecting this request's documents first, the way you showed me.")
    _by_model(run, phase=GATHER_PHASE)


def _new_documents(run):
    """The documents that appeared or changed since the gather began, oldest first: what it saved for this request."""

    before = run.get("files_before") or {}
    now = _document_files()
    since = (run.get("began") or 0) - 5
    new = [p for p, m in now.items() if (p not in before or m > before[p]) and m >= since]
    return sorted(new, key=lambda p: now[p])[:MAX_GATHERED_FILES]


def _after_phase(run, phase, reply, kind):
    """The engine's report on a phase of a per-document run (from _gate): decides what the run does next. True: handled."""

    skill = run["skill"]
    report = " ".join((reply or "").split())[:1500]

    if phase == REST_PHASE:
        # The model entered the documents the fill set aside; the replay's result and the model's report are judged as
        # one (the model's part is its own word until the check looks).
        result = dict(run.get("replay_result") or {"text": "", "outcome": replay.COMPLETED_UNVERIFIED})
        result["outcome"] = replay.COMPLETED_UNVERIFIED
        done = ", ".join(Path(x["document"]).name for x in run.get("by_hand") or [])
        result["text"] = (result.get("text") or "") + f"\n\nEntered by hand, outside the replay ({done}): " + (
            report if kind == "final" else f"NOT finished - the model stopped with {kind}: {report}")
        run["by_hand_done"], run["by_hand"] = run.get("by_hand") or [], []
        _finish(run, result)
        return True
    if phase != GATHER_PHASE:
        return False

    if kind != "final":
        progress.emit(f"Collecting the documents ended with {kind}, so the model does the whole task itself.")
        log(f"Skill {skill['name']}: collecting the documents ended with {kind}; the model does the whole task")
        run["gathered"] = "failed"
        _by_model(run, note=f"Collecting the documents first did not finish ({report[:300]}). Do the whole task yourself.")
        return True

    documents = _new_documents(run)
    if not documents:
        progress.emit(f"No document file appeared under the home folder since the collection began ({len(_document_files())} "
                      "document files seen in all), so the model does the rest of the task itself.")
        log(f"Skill {skill['name']}: collecting the documents saved none the harness can find; the model does the rest")
        run["gathered"] = "failed"
        _by_model(run, note=f"You collected the documents and reported: {report[:600]}. Now do the rest of the task "
                            "yourself: the person's steps after the folder is ready, once per document.")
        return True

    run["gathered"], run["documents"], run["gather_report"] = "done", documents, report
    run["start"] = _loop_start(skill)
    wanted = [p for p in _needed(run) if not run["values"].get(p["name"])]
    _same_addresses(run)
    progress.emit(f"{len(documents)} document(s) collected. Reading each one's values for the steps done per document.")
    values, evidence, rows, left_out = binding.fill_lists(
        skill, wanted, (run.get("context") or "") + "\n" + report, documents, rules=_taught(skill))
    if rows:
        run["rows"] = rows
    if values:
        run["values"].update(values)
        run.setdefault("evidence", {}).update(evidence)
        progress.emit("Read from the documents:\n" + binding.report(evidence, values))
    run["left_out"] = [x for x in left_out if x.get("kind") != "by_hand"]
    run["by_hand"] = [x for x in left_out if x.get("kind") == "by_hand"]
    if run["left_out"]:
        progress.emit("Left out under your rules:\n" + "\n".join(f"- {x['document']}: {x['why']}" for x in run["left_out"]))
    if run["by_hand"]:
        progress.emit("Not of the taught shape - to be entered by hand after the replay:\n"
                      + "\n".join(f"- {x['document']}: {x['why']}" for x in run["by_hand"]))

    still = [p for p in _needed(run) if not run["values"].get(p["name"])]
    if still:
        progress.emit("The documents do not give " + ", ".join(p["name"] for p in still[:12])
                      + (" ..." if len(still) > 12 else "") + ", so the model does the rest of the task itself.")
        log(f"Skill {skill['name']}: the documents do not give {', '.join(p['name'] for p in still)}; the model does the rest")
        run["gathered"], run["start"] = "failed", 1
        _by_model(run, note=f"The documents are collected ({len(documents)}: "
                            + ", ".join(Path(d).name for d in documents[:12]) + f"); you reported: {report[:500]}. "
                            "Carry on from the person's steps after the folder is ready: enter each document.")
        return True

    _same_addresses(run)
    if _stage(run) == experience.APPRENTICE and not run.get("plan_ok"):
        _pause(run, "plan", _plan_text(run))
        return True
    _prepare(run)
    return True


def _documents_note(run):
    """
    What code can say about a gathered run's documents, for the final check and the report: each file saved for this
    request is on disk now (its size), or is not; which the person's rules left out and which the model entered by
    hand. Found live (2026-11-02): in three months of four the check answered "cannot confirm - the observations do
    not verify that all six files are in the folder" and a person had to look, though the files were there - the
    gather's phase had saved them and nothing of it reached the check but the worker's word. A fact a program can
    decide is decided in code.
    """

    docs = run.get("documents") or []
    if not docs:
        return ""
    present, missing = [], []
    for d in docs:
        try:
            present.append(f"{Path(d).name} ({Path(d).stat().st_size:,} bytes)")
        except OSError:
            missing.append(Path(d).name)
    folders = sorted({str(Path(d).parent) for d in docs})
    lines = [f"CHECKED IN CODE, NOT CLAIMED - this request's document files on this desktop now: {len(present)} of "
             f"{len(docs)} present under {' and '.join(folders)}: " + (", ".join(present) or "none") + "."]
    if missing:
        lines.append("NOT present (saved during the task and gone since): " + ", ".join(missing) + ".")
    kinds = [("Left out under the person's rules (saved, not entered)", run.get("left_out")),
             ("Entered by the model by hand, not by the taught steps", run.get("by_hand_done"))]
    for head, items in kinds:
        if items:
            lines.append(f"{head}: " + "; ".join(f"{Path(x['document']).name} ({x['why']})" for x in items) + ".")
    return "\n".join(lines)


def _assumed_note(run):
    """
    This run's values the fill worked out rather than read (binding.WORKED, binding.NUMBERED), for the final check:
    assumptions, never the person's word. Found live (2026-11-02): a folder name the fill numbered after the taught
    example was held over the one the work rightly took from the screen, as the person had said to, and a person was
    asked twice.
    """

    marks = (binding.WORKED, binding.NUMBERED)
    values, said = run.get("values") or {}, run.get("evidence") or {}
    lines = [f"{name} = {values[name]} ({why})" for name, why in said.items()
             if name in values and any(m in str(why) for m in marks)]
    if not lines:
        return ""
    return ("These of this run's values were WORKED OUT by the harness, not read as written in what the run was given - "
            "assumptions, not the person's word. Judge the work by the documents, the person's rules and the screen; "
            "where those name such a thing another way, the work is right to follow them:\n- " + "\n- ".join(lines))


def _same_addresses(run):
    """
    An address the person typed the same way before every item (the fresh form's page) is their own fixed value, however
    the lesson was answered about it: a gathered run types it as they did, once per item. One typed differently each
    time stays unfilled (the model's, when the replay reaches it).
    """

    skill = run["skill"]
    lists = [v for p in skills.list_params(skill) if (v := run["values"].get(p["name"]))]
    items = len(skills.split_items(lists[0])) if lists else 1
    for p in skills.required(skill):
        if (p.get("step") or 0) < run["start"] or run["values"].get(p["name"]) or not learning._is_address(p.get("example")):
            continue
        examples = p.get("examples") or [p.get("example")]
        if len({str(e) for e in examples}) == 1:
            run["values"][p["name"]] = "; ".join([str(p["example"])] * items) if p.get("list") else str(p["example"])


def _plan_text(run):
    """The plan put to the person before the first replay of a run still being learned: the documents, each item's values."""

    from decurix_agent.thebot_harness import PLAN_QUESTION

    skill, docs = run["skill"], run.get("documents") or []
    lines = [PLAN_QUESTION,
             f"- {len(docs)} document(s) collected: " + ", ".join(Path(d).name for d in docs[:12]) + (" ..." if len(docs) > 12 else "")]
    for x in run.get("left_out") or []:
        lines.append(f"- Left out ({x['why']}): {Path(x['document']).name}")
    for x in run.get("by_hand") or []:
        lines.append(f"- Entered by hand after the others, not by the taught steps ({x['why']}): {Path(x['document']).name}")
    lists = [p for p in skills.list_params(skill) if run["values"].get(p["name"])]
    items = {p["name"]: skills.split_items(run["values"][p["name"]]) for p in lists}
    for n in range(max((len(v) for v in items.values()), default=0)):
        shown = "; ".join(f"{name}={el._quote(v[n], 40)}" for name, v in items.items() if n < len(v))
        lines.append(f"- Item {n + 1}: {shown}")
    lines.append(f"- Then your steps from step {run['start']}, once per item, each value checked against its document "
                 "before the save.")
    lines.append("Reply `yes` to go ahead, or say what to change.")
    return "\n".join(lines)


def _need_text(skill, missing):
    """Ask for what a run still needs, saying how each is to be given."""

    def wanted(param):
        recorded = el._quote(param.get("example") or "", 40)
        if param.get("list"):
            return f"{param['name']} ({param['label']}: one per item, separated by `;`, recorded as {recorded}...)"
        return f"{param['name']} ({param['label']}, recorded as {recorded})"

    example = " ".join(f'{p["name"]}="{"a;b;c" if p.get("list") else "..."}"' for p in missing)
    return (f"To do \"{skill['title']}\" I need: {', '.join(wanted(p) for p in missing)}. Reply with {example}"
            + (", or just the value." if len(missing) == 1 else "."))


def _execute(run):
    skill = run["skill"]
    program, values = skills.expand(skill, run["values"])
    run["expanded"] = program

    # A point click a person said to make anyway goes without its picture,
    # on this run's expanded copy only: the saved skill keeps the check.
    # (A new dict, not a pop: expand hands back the skill's own step objects.)
    program["steps"] = [
        {k: v for k, v in step.items() if k != "look"}
        if step.get("id") in run.get("unanchored", ()) and step.get("point") else step
        for step in program["steps"]
    ]

    # A mimicked request: what it changes goes into this run's copy of the
    # steps - what they target, open and expect - never into a saved skill.
    if run.get("mimic"):
        spans = mimic.step_spans(run["mimic"], skill, run["values"])
        program["steps"] = [mimic.substitute_step(step, spans) for step in program["steps"]]

    # Carrying on past the last step - the person skipped it, or did it by
    # hand - leaves nothing to run. That is the end of the task, and said as
    # what it is: replay.run would call it a halt at a step that does not exist.
    # A gathered run begins at the per-item block: the step of the expanded program that carries the block's first
    # step id (a loop before it, written out zero times for want of items, moves the numbers).
    if run.get("gathered") == "done" and (loop := _item_loop(skill)):
        first = next((s["n"] for s in program["steps"] if s.get("id") == loop["blocks"][0][0]), None)
        if first:
            run["start"] = first

    if run["start"] > len(program["steps"]):
        _finish(run, {"text": f"Finished \"{skill['title']}\": nothing was left to do after step {run['start'] - 1}. "
                              "A step that was skipped or done by hand was not checked."})
        return

    ladder = run["ladder"]
    ladder.total = len(program["steps"])

    result = replay.run(program, params=values, start=run["start"], ladder=ladder)
    outcome = result["outcome"]

    log(f"Skill {skill['name']} ended {outcome}: {result['text']}")

    if outcome in replay.SUCCESS:
        _finish(run, result)
    elif outcome == replay.NEEDS_HUMAN and run.get("gathered") == "done" and _engine_can_drive() \
            and _click_left_to_a_person(program["steps"], result.get("step")):
        # A click the recording kept only as a spot on the screen, made "a person does this" when it was taught, is
        # the model's in a run that gathered its documents: it can see the screen and find the control, and the person
        # is not at this desktop. Found live (2026-11-02): the replay stopped at it inside item 1 of 4 and the run ended.
        run["taken_over"] = True
        _own_run(run, False)
        _settle(run, outcome)
        step = program["steps"][result["step"] - 1]
        _by_model(run, from_step=result["step"],
                  note=f"Step {result['step']} is a click the recording kept only as a spot on the screen "
                       f"({step.get('by_hand') or step.get('summary')}); nobody is at the screen to do it. Look (gui observe), "
                       "find the control that is there and click it yourself, then carry on.")
    elif outcome in (replay.NEEDS_APPROVAL, replay.NEEDS_HUMAN):
        kind = "approval" if outcome == replay.NEEDS_APPROVAL else "human"
        text = _approval_text(run, result, values) if kind == "approval" else result["text"]
        # A by-hand step the recording kept private (a password the person types) asks for a secret: its reply is
        # withheld whole wherever it is kept - from the recording's structure, never the question's words (X-6).
        from decurix_agent.replay_task import _secret_step
        _pause(run, kind, text, "approval" if kind == "approval" else "query",
               secret=kind == "human" and _secret_step(program, result.get("step")),
               step=result["step"], resume_from=result["resume_from"])
    elif outcome == replay.RECONCILIATION_REQUIRED or result["resume_from"] is None or run["stuck"] >= MAX_STUCK_ASKS:
        _settle(run, outcome)
        _record_verdict(run, False, why=result["text"])
        write_final_reply(result["text"], kind="error", engine=SKILL_ENGINE)
    elif _engine_can_drive() and not run.get("taken_over"):
        # A replay that got stuck hands the rest to the model before it asks
        # a person: it can see the screen, read what was meant, and adapt.
        # The run then counts as one that needed the model, so the skill goes
        # back to being learned with it (experience.stage).
        run["taken_over"] = True
        _own_run(run, False)
        _settle(run, outcome)
        ran = result["resume_from"] == result["step"] + 1
        _by_model(run, from_step=result["step"], note=result["text"], ran=ran)
    else:
        run["stuck"] += 1
        _ask_stuck(run, result)


def _click_left_to_a_person(steps, n):
    """Whether step n is a manual step that was a click or a key press (not a secret typed), from learning's "manual"."""

    if not n or n > len(steps):
        return False
    step = steps[n - 1]
    what = " ".join(str(step.get("by_hand") or "").lower().split())
    return step.get("kind") == "manual" and bool(what) and what.startswith(("click", "double", "right", "press", "scroll"))


def _approval_text(run, result, values):
    """
    What a person is shown before saying yes: the gate's own words, then what
    this step does, what it would type, what it led to when it was recorded, and
    - if this desktop found the control itself - how, so nothing is approved
    blind. A yes is still one action's; the gate is not this module's to open.
    """

    step = run["expanded"]["steps"][result["step"] - 1]
    lines = [result["text"], f"This is step {step['n']} of {result['total']}: {step['summary']}."]

    if step["kind"] == "type" and values.get(step.get("param")):
        lines.append(f"It would type {el._quote(values[step['param']], 80)}.")
    if step["kind"] == "select" and values.get(step.get("param")):
        lines.append(f"It would choose {el._quote(values[step['param']], 80)}.")

    if step.get("expect"):
        lines.append("When this was recorded it led to " + " and ".join(replay.describe_check(c) for c in step["expect"]) + ".")

    healed = next((h for h in run["ladder"].healed if h["n"] == step["n"]), None)
    if healed:
        lines.append(f"I found this control myself: it was \"{healed['was']}\" and is now \"{healed['now']}\".")

    lines.append("Reply `yes` to allow this one action, or `no` to stop here.")
    return " ".join(lines)


def _settle(run, outcome):
    """
    Record how a run that did not succeed went, on a fresh copy of the skill: a
    run's own copy holds corrections that have not been shown to work, and
    only a success keeps those (_finish).
    """

    if run.get("one_off"):
        return

    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return

    experience.commit(fresh, run["ladder"], outcome, False, asked=run["asked_ids"])
    skills.save(fresh)


def _ask_stuck(run, result, preface="", unclear=0):
    ran = result["resume_from"] == result["step"] + 1
    found = result.get("options") or []
    step = run["expanded"]["steps"][result["step"] - 1]

    reopen = None if ran else result.get("reopen")

    if ran:
        choices = "Reply `continue` if the screen looks right and I should carry on, or `stop`."
    elif reopen:
        # Found live: the app the recording opened fresh was already open, on
        # another page, and every picture of its start page missed.
        how = ("was already open" if reopen.get("already")
               else f"opened at step {reopen['step']} where it was last left (it keeps running between uses)")
        choices = (f"{el._quote(reopen['window'], 60)} {how}, showing something else than when you recorded this, "
                   "where it had just been opened fresh. Reply `restart` to close it and open it fresh (whatever is "
                   "open in it now is closed), `retry` to look again, `skip` to leave this step out, or `stop`.")
    elif _unanchorable(step, result) and result.get("after_change"):
        # Found live: a skill recorded clicking a search's first result, run
        # for another search whose page opened with a large overview - the
        # first result was below the fold. Scrolling and taking the first
        # thing that looks alike was tried on the real page: it matched a link
        # inside the overview first. Which result is "the first" is a
        # person's call; doing that one click and carrying on is one word.
        choices = ("This run typed something else than the recording, and the page it led to is laid out "
                   "differently - nothing like what you clicked then is where it was. If it is further down or "
                   "elsewhere, click it yourself on the screen and reply `done` to carry on; or `click anyway` to "
                   "click the recorded spot, `retry`, `skip` or `stop`.")
    elif _unanchorable(step, result):
        choices = ("Look at the screen: if the spot I would click is the right one, reply `click anyway`. Otherwise "
                   "`retry` to look again, `skip` to leave this step out, or `stop`.")
    elif found:
        choices = ("Reply the number to use one of those, `retry` to look again, `skip` to leave this step out and carry on, "
                   "`stop`, or name the control - for example `use \"Save as\"`.")
    else:
        choices = ("Reply `retry` to look again, `skip` to leave this step out and carry on, `stop`, or tell me which "
                   "control to use instead - for example `use \"Save as\"`.")

    detail = ""
    if step.get("intent"):
        detail += f" You said about this step: {el._quote(step['intent'], 160)}."
    if result.get("trail"):
        detail += " I tried: " + "; ".join(result["trail"][:4]) + "."
    if found and not ran:
        detail += " Closest on the screen: " + "; ".join(f"{o['n']}) {o['role']} {el._quote(o['name'] or '', 50)}" for o in found) + "."

    phases = max((s.get("phase", 1) for s in run["expanded"]["steps"]), default=1)
    if phases > 1 and step.get("phase_label"):
        detail += f" (Part {step['phase']} of {phases}: {step['phase_label']}.)"

    if step.get("id") is not None and step["id"] not in run["asked_ids"]:
        run["asked_ids"].append(step["id"])

    _pause(run, "stuck", f"{preface} {result['text']}{detail} {choices}".strip(), step=result["step"],
           resume_from=result["resume_from"], ran=ran, why=result["text"], unclear=unclear, options=found,
           trail=result.get("trail") or [], reopen=reopen)


def _finish(run, result):
    """
    A replay that ran to its end. It is a success only once it is shown to be
    one: every step's result seen on the screen, or the model looking at the
    screen, or the person saying so. Anything else is asked about - never
    reported as done (verify.py).
    """

    outcome = result.get("outcome", replay.COMPLETED_UNVERIFIED)
    screen_checked = outcome == replay.VERIFIED and not result.get("skipped")

    # The documents the fill set aside as not of the taught shape are the model's to enter now, by hand, before the
    # whole is judged ("The per-document part").
    if run.get("by_hand") and _engine_can_drive():
        run["replay_result"] = dict(result)
        _by_model(run, phase=REST_PHASE)
        return

    spot = verify.available() and _spot(run)
    if screen_checked and not (verify.available() and _stage(run) == experience.APPRENTICE) and not spot:
        return _succeed(run, result, "screen")

    progress.emit("Checking the screen to see that the task was really done.")
    # A gathered run's files are a fact code decides (_documents_note, added in _judge): weighed as found, not claimed.
    verdict = _judge(run, result["text"], purpose="py.judge.spot" if spot else "py.judge")

    if verdict["verdict"] == verify.DONE:
        return _succeed(run, result, "model", seen=verdict["seen"])
    if spot and verdict["verdict"] == verify.NOT_DONE:
        # A trusted skill the spot check finds not done is learning again from here (W0.5), whatever comes next. Kept
        # on the saved skill now, and on the run, so its outcome still says so if a person later confirms it.
        said = " ".join(x for x in (verdict.get("seen"), verdict.get("why")) if x)
        run["spot_not_done"] = said[:300]
        _record_verdict(run, False, why=("spot check: " + said)[:300], basis=["model"])
    if screen_checked and verdict["verdict"] == verify.UNSURE:
        # Every step's own result was on the screen; the model could not tell
        # more than that. The screen is the proof it always was.
        return _succeed(run, result, "screen")

    # A replay that ran to its end and still cannot be confirmed is the model's to finish before a person
    # is asked - and the sign that this task needs more than its recorded steps (_supervise). Not for a run that
    # gathered its documents: the model has done its parts (the gather, the documents set aside), the replay the rest,
    # and a third pass over work already entered risks entering it twice - the person is asked instead.
    if not run.get("one_off") and not run.get("taken_over") and not run.get("gathered") and _engine_can_drive():
        run["taken_over"] = True
        note = " ".join(x for x in (verdict.get("seen"), verdict.get("why")) if x) or "the result could not be confirmed"
        _supervise(run, note)
        run["attempt_note"], run["attempt_report"], run["attempts"] = note, result["text"].strip()[:1500], 1
        run["attempt_blank"] = False
        run.pop("attempt_actions", None)  # (the replay's own steps ran to their end: the report above says what)
        progress.emit(f"The steps ran, but I cannot confirm the result ({note[:160]}). Looking into it myself.")
        return _by_model(run)

    _pause(run, "confirm", _doubt_text(result["text"], verdict), result=result, verdict=verdict)


# What a person says when a task has something to look at before it starts.
CHECK_FIRST = re.compile(
    r"\bbefore (?:you |i |we )?(?:start|begin|enter|creat|sav|add|do)\w*|\bfirst(?:,)? (?:check|search|look|make sure|verify)\b|"
    r"\b(?:check|search|look|make sure|verify)\b[^.\n]{0,60}\bfirst\b|\balready (?:exists?|entered|there|present|in)\b|"
    r"\bduplicates?\b|\bonly if\b|\bunless\b", re.IGNORECASE)


def _asks_for_a_check_first(skill):
    """
    Whether the person's own words for a task call for something no recorded
    step does: a look before starting, or a condition on doing it at all
    ("before you start, search for ...", "if one already exists, do not ...",
    "only if ..."). Such a task is the model's from its first run on (see
    _supervise) - telling it by the words spares the one replay that finds
    it out the hard way, and errs towards more thought, never less.
    """

    said = [skills.explanation(skill) or ""]
    said += [str(x.get("answer") or "") for x in skill.get("dialogue") or [] if isinstance(x, dict)]
    said += [str(x.get("text") or "") for x in skill.get("corrections") or [] if isinstance(x, dict)]
    return bool(CHECK_FIRST.search("\n".join(said)))


def _supervise(run, why):
    """
    Mark a skill as one the model has to do, with the replay as its tool,
    however often it has gone well. Found by design and then live: a person
    taught "before you start, search for a bill with the same number" - which
    no recorded step does and only something that reads the rule can - so a
    bare replay of the trusted skill entered the bill and the final check,
    rightly, would not confirm a run in which nothing showed that search. A
    task whose rules ask for more than its steps is not made cheaper by
    dropping the part that keeps the rules: it stays the model's, which runs
    the steps with one command (the numbered procedure in _procedure) and
    does the rest itself. Correctness first, then cost.
    """

    run["skill"]["supervised"] = why[:300]
    try:
        fresh = skills.load(run["skill"]["name"])
        fresh["supervised"] = why[:300]
        skills.save(fresh)
        log(f"Skill {fresh['name']} is now always done under the model's eye: {why[:200]}")
    except (FileNotFoundError, OSError) as exc:
        log(f"Could not mark the skill as supervised: {exc}")


def _evidence_text(run):
    """
    The report's list of values read from the run's documents (binding.py),
    or "". Every way a /skill run can succeed - replay, or the engine while
    the skill is still being learned (_gate) - says where its values came from.
    """
    if not run or not run.get("evidence"):
        return ""
    filled = {k: run["values"][k] for k in run["evidence"] if k in run["values"]}
    return "\n\nValues read from the documents:\n" + binding.report(run["evidence"], filled)


def _own_run(run, worked):
    """Count a replay of the worker's own earlier run for or against keeping it as the way (playbooks.own_replayed)."""

    if run.get("one_off") and run["skill"].get("own") and run.get("mimic"):
        try:
            playbooks.own_replayed(run["mimic"]["id"], worked)
        except Exception as exc:
            log(f"Could not count a replay of this desktop's own run: {exc}")


def _read_back(run, text):
    """
    (problems, not_made): what a taught run's own files say once read back by the code-decided checks every route
    shares (produced.file_facts) - the files the run wrote, never the person's own sources. Nothing written, nothing
    to read: ([], []). A check that could not be made is in not_made, never passed (review 2026-11-03: a taught run
    reported checked=True with its files unread).
    """

    request = state.current_task or ""
    run["_read_back_files"] = 0
    run["_read_back_compared"] = False
    try:
        made = [m for m in produced.files(text, request=request)
                if not source_guard.is_source(m, task_context.current(), request)]
        run["_read_back_files"] = len(made)
        if not made and not produced.drafts():
            return [], []
        facts = produced.file_facts(request, made, drafts_made=produced.drafts())
        # (A fact of the request's sources the worker did not write, found in the files, and nothing they contradict:
        # the `code` basis rests on this, never on the files opening nor on the request's own figures - review round
        # 2, B5; round 3, R2.)
        run["_read_back_compared"] = facts.compared_clean()
    except Exception as exc:  # noqa: BLE001 - a read-back that breaks is a check not made
        log(f"Skill {run['skill']['name']}: could not read the run's files back: {exc}")
        return [], [f"the files it wrote could not be read back ({' '.join(str(exc).split())[:160]})"]
    # (One definition for every route: FileFacts.blocking / not_made - E-27.)
    return facts.blocking(), facts.not_made()


def _spot_every():
    """1 in how many trusted replays is judged all the same (DECURIX_SPOT_JUDGE_EVERY; 0 never, 1 always)."""

    try:
        return max(0, int(os.environ.get("DECURIX_SPOT_JUDGE_EVERY", "5")))
    except ValueError:
        return 5


def _spot(run):
    """
    Whether this run of a trusted skill is one the judge looks at anyway (W0.5): a deterministic share of task ids,
    1 in _spot_every(), so a rerun of the same task is sampled the same way and no run can choose to be skipped. A
    trusted replay is otherwise taken on the screen's word that each step's result appeared - which is not a check
    that counts towards trust (experience.INDEPENDENT), and so needs sampling to catch a skill gone wrong.
    """

    every = _spot_every()
    if every == 0 or run.get("one_off") or _stage(run) != experience.TRUSTED:
        return False
    if every == 1:
        return True
    task_id = task_context.current()
    if not task_id:
        return False
    import hashlib
    return int(hashlib.sha256(task_id.encode("utf-8")).hexdigest()[:8], 16) % every == 0


def _basis(checked_by, code=False):
    """
    The kinds of check this run passed (experience.INDEPENDENT): `checked_by` itself (screen, model or person), "code"
    when the caller read the run's own files back clean, and "ledger" when every record the task declared was read back
    from the application with its values shown (records.all_read_back).
    """

    from decurix_agent.computer import records
    basis = {checked_by} if checked_by else set()
    if code:
        basis.add("code")
    # Every declared record read back - and no save outside them: the records this task's saves made equal the
    # declared records it saved (review round 9, B: a second save, outside the ledger, passed as "every declared
    # record read back").
    if records.all_read_back(task_context.current()) and not _ledger_faults():
        basis.add("ledger")
    return sorted(basis)


def _ledger_faults(reply=""):
    """What code holds against the task's saved records, and each item of the report's JUDGEMENTS list the worker
    itself marked "| not backed": ledger_checks.findings, the one check every route that can say done makes (review
    round 9, R9-B3). [] when none; a check that could not be made is a finding of its own kind. Never raises."""

    return ledger_checks.findings(task_context.current(), reply)


not_backed_items = ledger_checks.not_backed_items


def _ledger_why(found):
    """What a task the ledger check holds back is told to do, for the kinds of what it found."""

    kinds = {getattr(f, "kind", "") for f in found}
    out = []
    if kinds & {ledger_checks.UNDECLARED, ledger_checks.UNBACKED_PICK, ledger_checks.NOT_BACKED}:
        out.append("A record saved in the application holds a value nothing given backs. Ask the person (HELP) for "
                   "each value no source gives, then correct the saved record in the application (never enter it "
                   "again); a field you can show a source for is declared on the saved record (gui record KEY --from "
                   "SOURCE).")
    if ledger_checks.UNCOVERED in kinds:
        out.append("A record was made that no declaration covers: say in your report which record it is and why it "
                   "was made, or ask the person (HELP) whether it should be there.")
    if ledger_checks.SAVED_TWICE in kinds:
        out.append("The same record was saved twice: the second one is the person's to remove - write the WRONG IN "
                   "THE APPLICATION line for it.")
    return " ".join(out) or "A check in code holds a saved record back: ask the person (HELP)."


def _keep_noted_since(skill):
    """Merge into `skill` (a run's copy) the verdicts the saved skill holds that the copy does not: noted while the run
    went on, by _record_verdict on a fresh copy. Never raises."""

    try:
        fresh = skills.load(skill["name"])
    except Exception:  # noqa: BLE001 - nothing saved to merge
        return
    mine = (skill.setdefault("experience", {"runs": [], "steps": {}})).setdefault("verdicts", [])
    seen = {json.dumps(v, sort_keys=True, default=str) for v in mine}
    later = [v for v in (fresh.get("experience") or {}).get("verdicts") or []
             if json.dumps(v, sort_keys=True, default=str) not in seen]
    if later:
        merged = sorted(mine + later, key=lambda v: v.get("at") or 0)
        mine[:] = merged[-experience.RUN_HISTORY:]


def _success_verdict(checked_by, basis, seen=""):
    """
    The verdict of a taught run that succeeded (R6): what checked it - the screen (every step's result seen), the
    model looking at it, or a person - and, beside it, the run's files read back by code and its records read back
    from the application, when those passed (`basis`, _basis).
    """

    checks = [verdicts.check({"screen": "screen.every_step", "model": "judge", "person": "person"}.get(checked_by,
                                                                                                    "check"),
                             verdicts.PASS, checked_by if checked_by in verdicts.BASES else "model", seen)]
    if "code" in basis:
        checks.append(verdicts.check("files.read_back", verdicts.PASS, "code", "the files the run made read back"))
    if "ledger" in basis:
        checks.append(verdicts.check("ledger.read_back", verdicts.PASS, "ledger",
                                     "every declared record read back from the application"))
    evidence = [f"Checked on the screen: {seen}"] if seen and checked_by == "model" else []
    return verdicts.make(verdicts.DONE, "replay" if checked_by == "screen" else "skill_gate", checks=checks,
                         evidence=evidence)


def _succeed(run, result, checked_by, seen="", helped=False):
    skill = run["skill"]
    text = result["text"]
    read_clean = False
    if checked_by != "person":
        # checked=True only when the files the run made were read back (review 2026-11-03).
        problems, not_made = _read_back(run, text)
        # What code holds against the saved records (review round 9, R9-B3: a run's read-back passed over a value no
        # source gives, and done stood).
        found_in_ledger = _ledger_faults(text)
        file_problems = list(problems)
        problems = list(problems) + ledger_checks.faults(found_in_ledger)
        not_made = list(not_made) + ledger_checks.not_made(found_in_ledger)
        # Code found the request's sources in at least one file the run made (FileFacts.compared_clean), and it
        # found nothing wrong and nothing it could not check.
        read_clean = (bool(run.get("_read_back_files")) and bool(run.get("_read_back_compared")) and not problems
                      and not not_made)
        decided = verify.outcome({"verdict": verify.DONE, "seen": seen} if not problems else
                                 {"verdict": verify.NOT_DONE, "seen": "; ".join(problems)}, problems, not_made)
        if decided["status"] != verify.DONE:
            # A run the check could not make is not checked, which is neither a pass nor a failure: it counts for
            # nothing (round-3 recheck: counted as a failure, a skill whose files cannot all be read back never
            # matured). Only a run whose files do not hold up is one.
            if decided["status"] == verify.NOT_DONE:
                _own_run(run, False)
                _record_verdict(run, False, why=decided["why"])
            # Said by what found it: the files the run wrote, or the records it saved in an application (a ledger
            # finding is no file of the run's - review round 11), with what to do about each.
            ledger_found = ledger_checks.faults(found_in_ledger)
            if decided["status"] != verify.NOT_DONE:
                head = "Not checked - "
            elif ledger_found and not file_problems:
                head = "Not done - a record it saved in the application does not hold up: "
            elif ledger_found:
                head = "Not done - the files it wrote and a record it saved in the application do not hold up: "
            else:
                head = "Not done - the files it wrote do not hold up: "
            tail = (" " + _ledger_why(ledger_found)) if ledger_found and decided["status"] == verify.NOT_DONE else ""
            write_final_reply(f"{head}{decided['why']}{tail}" + chr(10) * 2 + text, kind=decided["kind"], engine=SKILL_ENGINE,
                              checked=decided["checked"], gated=True,
                              verdict=verdicts.from_outcome(decided, "skill_gate"))
            return
    _own_run(run, True)
    basis = _basis(checked_by, code=read_clean)
    verdict = _success_verdict(checked_by, basis, seen)
    if run.get("spot_not_done"):
        # The judge's dissent stays on the outcome: the person's confirmation decides, and the record says both.
        verdict["evidence"].append(verdicts._text(f"The spot check had found it not done: {run['spot_not_done']}"))

    if skill.get("own"):
        # Its steps were held to their controls only, so "0 were confirmed, look at the result" is not
        # what there is to say: the result was looked at, by the check, and that is what is reported.
        text = f"Done the way I did it before, with this request's values ({result.get('total') or len(skill['steps'])} steps)."

    if checked_by == "model" and seen:
        text += f"\n\nChecked on the screen: {seen}"
    elif checked_by == "person":
        text += "\n\nYou confirmed it was done."

    text += _evidence_text(run)
    if _documents_note(run):
        text += "\n\n" + _documents_note(run).replace("CHECKED IN CODE, NOT CLAIMED - this", "Checked on this desktop: this")

    if run.get("one_off"):
        # Built from a recording for this request only: nothing to save, and a
        # person has not said which parts change - Learn is how they do.
        write_final_reply(text + f" (Done from your recording of \"{run['mimic']['text']}\". Teach it with Learn "
                                 "and I will also know which parts change.)", kind="final", engine=SKILL_ENGINE,
                          checked=True, verdict=verdict)
        return

    if run["patches"]:
        for patch in run["patches"]:
            skill.setdefault("learned", []).append(f"step {patch['n']}: {patch['from']!r} is now {patch['to']!r}")
        skill["version"] = skill.get("version", 1) + 1
        text += " I have remembered " + "; ".join(f"the control you named ({p['to']})" for p in run["patches"]) + " for next time."

    learned = experience.commit(skill, run["ladder"], result.get("outcome", replay.COMPLETED_UNVERIFIED), True,
                                asked=run["asked_ids"])
    if learned:
        text += " I have learned from this run: " + "; ".join(learned) + "."

    skill["runs"] = skill.get("runs", 0) + 1
    # What was noted on the saved skill while this run went on (a spot check's "not done", review round 2, B6) is kept:
    # the run's own copy is older, and saving it whole would erase that.
    _keep_noted_since(skill)
    experience.note_verdict(skill, "replay", True, checked_by=checked_by,
                            helped=helped or bool(run["asked_ids"]) or bool(run.get("taken_over")),
                            basis=basis)
    skills.save(skill)
    text += experience.advice(skill)

    write_final_reply(text, kind="final", engine=SKILL_ENGINE, checked=True, verdict=verdict)


def _stopped(text, engine=None):
    """
    The report of a run the person stopped: never kind "final", which the task record shows as done (E-36, review
    2026-11-03). Like every route's stop (watchers, replay_task, thebot_harness), kind "error", not checked, and the
    words start "Stopped, as you said".
    """

    return messaging.write_stopped(f"Stopped, as you said, {text}", engine=engine or SKILL_ENGINE,
                                   send=write_final_reply)


def _stop(step, run=None):
    if run is not None:
        _settle(run, "stopped")
        _record_verdict(run, False, why="stopped by the person")

    _stopped(f"before step {step}. Nothing from that step on was done.")


def _run_reply(paused, reply, approval_outcome):
    run, kind = paused["run"], paused["kind"]

    if kind == "values":
        return _values_reply(paused, reply)

    if kind == "confirm":
        return _confirm_reply(dict(paused, claim=paused["result"]["text"]), reply)

    if kind == "plan":
        # The plan before the first replay of a per-document run (_after_phase). A yes as it stands starts it; a
        # change, a condition or a correction gives the rest to the model with the person's words in hand; only a plain
        # no stops. Found live (2026-11-02): "the values look right ... one stray field, don't type it" was read as a
        # refusal and nothing was entered.
        said = approval.read_choice(reply, paused.get("plan_text") or _plan_text(run), {
            "as_planned": "it approves the plan exactly as written and adds, changes, asks or answers nothing else",
            "changed": "it approves going ahead but adds, corrects or changes something (a value, a field, a rule, a "
                       "document to leave out or add), or answers a point",
            "stop": "it refuses: nothing is to be entered",
        }, plain={"as_planned": ["yes", "ok", "okay", "fine", "go ahead", "proceed", "looks good", "yes, go ahead"],
                  "stop": ["no", "stop", "don't", "do not", "cancel"]})
        if said["choice"] == "as_planned" and not said.get("instruction"):
            run["plan_ok"] = True
            return _prepare(run)
        if said["choice"] == "stop":
            return _stopped("before entering anything. The documents are collected and nothing was entered.")
        run["asked_person"], run["start"] = True, 1
        return _by_model(run, note=f"You put this plan to the person:\n{_plan_text(run)[:1200]}\nThe person answered: "
                                   f"{reply}\nThe documents are already collected. Do the rest with their words.")

    if kind in ("go", "start"):
        # The reply's one reading (approval.reading). A word list OR'd with it read "check the amount first, it should
        # be 12,750" as a yes (audit, 2026-10-24): a yes with a condition, or anything unclear, is asked again once.
        action = ("start this run of the taught task now, with the values shown" if kind == "go"
                  else "carry on with the taught task from the screen as it is now")
        said = approval.reading(reply, action)
        if said["decision"] == "yes":
            run["approved" if kind == "go" else "checked"] = True
            return _prepare(run)
        if said["decision"] == "if" and kind == "start" and _engine_can_drive():
            # "yes, close the popup first": the model takes the run, with their words.
            run["checked"] = True
            return _by_model(run, note=f"The person said, about starting from the screen as it was: {reply}")
        if said["decision"] == "no" or paused.get("asked_again"):
            return _stopped("before " + ("starting" if kind == "go" else "the first step") + ". Nothing was done.")
        heard = said.get("condition") or said.get("wants") or ""
        return _pause(run, kind, (f"You said: \"{el._quote(heard, 200)}\". " if heard else "")
                      + "I can only start with the values shown, or not at all - to use other values, reply `no` and "
                      "send the request again with them. Reply `yes` to start, or `no` to stop.",
                      ask_kind="approval" if kind == "go" else "query", asked_again=True)

    if kind == "approval":
        stop = approval_outcome == "declined" or (approval_outcome is None and not approval.says_yes(reply))
    elif kind == "human":
        said = approval.read_choice(reply, "A step of the task was left for the person to do by hand on the screen; "
                                    "they were asked to reply when it is done.", HAND_CHOICES, plain=HAND_PLAIN)
        if said["choice"] == "done" and said["instruction"] and _engine_can_drive():
            # Done, and more to it ("done - and the code field is filled, don't type it again"): the model carries on.
            return _by_model(run, from_step=paused["resume_from"],
                             note=f"The person did step {paused['step']} by hand and said: {reply}")
        if said["choice"] in ("not_done", "unclear") and not paused.get("asked_again"):
            # "not yet, the code hasn't come": not a reason to go on as if it were done (audit, 2026-10-24).
            return _pause(run, "human", "Reply `done` once that step is done on the screen, or `stop`.",
                          step=paused["step"], resume_from=paused["resume_from"], asked_again=True)
        stop = said["choice"] != "done"
    else:
        return _stuck_reply(paused, reply)

    if stop:
        return _stop(paused["step"], run)

    run["start"] = paused["resume_from"]
    _execute(run)


# What a reply to a step left for the person to do by hand can decide, and the replies that decide it whole.
HAND_CHOICES = {
    "done": "they did the step: carry on from the next one",
    "not_done": "the step is not done yet, or could not be done (the reply says why, or what is missing)",
    "stop": "stop the run here",
}
HAND_PLAIN = {
    "done": ("done", "did it", "i did it", "i did that", "finished", "ok done", "yes done", "carry on", "continue",
             "go on", "ok", "okay", "yes"),
    "stop": ("stop", "cancel", "abort", "quit"),
}

# What a reply to "step N could not be done - retry, skip, stop, or tell me which to use" can decide.
STUCK_PLAIN = {
    "stop": ("stop", "cancel", "abort", "quit", "never mind", "nevermind"),
    "carry_on": ("skip", "continue", "carry on", "go on", "move on", "done", "it worked", "i did it", "i did that",
                 "looks right", "looks fine", "looks good", "leave it out"),
    "retry": ("retry", "again", "try again", "look again"),
    "restart": ("restart", "reopen", "re-open", "re-start", "open it again", "open it fresh", "start it again",
                "start it fresh", "close it and open it", "close it and reopen it"),
    "anyway": ("click anyway", "click it anyway", "click it", "click there", "anyway"),
}


def _values_reply(paused, reply):
    run, missing = paused["run"], paused["missing"]

    if _calls_off(reply, "values the run needs"):
        return _stopped("before the first step. Nothing was done.")

    options = {o.group(1): next(v for v in o.groups()[1:] if v is not None) for o in OPTION_RE.finditer(reply or "")}

    if not options and len(missing) == 1:
        options[missing[0]] = (reply or "").strip().strip("\"'`")

    run["values"].update({name: value for name, value in options.items() if name in missing and value})
    run["asked"] += 1

    left = [name for name in missing if not run["values"].get(name)]
    if run["asked"] >= 2 and (left or skills.list_problem(run["skill"], run["values"])
                              or constraints.violations(skills.required(run["skill"]), run["values"], split=skills.split_items)):
        raise _Refused(f"I still do not have usable values for {', '.join(missing)}, so I did not start.")

    _prepare(run)


def _stuck_reply(paused, reply):
    run = paused["run"]
    step_n, ran = paused["step"], paused["ran"]
    reply = reply or ""

    # A number alone picks one of the candidates offered (below); anything else is the reply's one reading. Word
    # lists anchored at its first word read "no, use option 2" as stop, "retry but use the second file" as a plain
    # retry and "skip 3, do 4 with the new rate" as a plain skip (audit, 2026-10-24).
    choices = {"stop": "stop the run here; nothing after this step is done",
               "carry_on": "this step is done (they did it) or is to be left out: go on with the next step"}
    if not ran:
        choices["retry"] = "look again and do this same step"
        if paused.get("reopen"):
            choices["restart"] = "close the app that is open and open it fresh, then go on"
        choices["anyway"] = "click the recorded spot anyway"
        choices["use"] = ("do this step on another control the reply names - the instruction is that control's name, "
                          "as they wrote it")
    number = None if ran else NUMBER_RE.match(reply)
    said = {"choice": "number", "instruction": ""} if number else approval.read_choice(
        reply, paused.get("why") or f"Step {step_n} could not be done.", choices,
        plain={name: phrases for name, phrases in STUCK_PLAIN.items() if name in choices})
    choice, told = said["choice"], said["instruction"]

    if choice == "stop":
        return _stop(step_n, run)

    if told and choice in ("carry_on", "retry", "restart") and _engine_can_drive():
        # Their choice comes with words a replay cannot follow: the model takes the run from here, with them.
        return _by_model(run, from_step=step_n + 1 if choice == "carry_on" else step_n,
                         note=f"Step {step_n} could not be done; the person said: {reply}")

    if choice == "carry_on":
        run["start"] = step_n + 1
        return _execute(run)

    if not ran:
        if choice == "retry":
            run["start"] = step_n
            return _execute(run)

        reopen = paused.get("reopen")
        if reopen and choice == "restart":
            progress.emit(f"Closing {el._quote(reopen['window'], 60)} to open it fresh")
            if not launchers.close_recorded(reopen["window"]):
                return _ask_stuck(run, {"step": step_n, "resume_from": paused["resume_from"], "text": paused["why"],
                                        "options": paused.get("options"), "trail": paused.get("trail")},
                                  f"{el._quote(reopen['window'], 60)} did not close (it may be asking something - "
                                  "look at the screen).")
            run["start"] = reopen["step"]
            return _execute(run)

        # A number picks one of the candidates the desktop offered.
        if number:
            option = next((o for o in paused.get("options") or [] if o["n"] == int(number.group(1))), None)
            if option and _retarget(run, step_n, option["name"], role=option["role"]):
                run["start"] = step_n
                return _execute(run)

        if choice == "anyway" and _unanchor(run, step_n):
            run["start"] = step_n
            return _execute(run)

        # The control to use instead, as the reading names it; with no model to read the reply, "use X" as written.
        hint = told if choice == "use" else ""
        if not hint and choice in ("use", "unclear") and not _reads_replies():
            written = USE_RE.match(reply)
            hint = written.group(1) if written else ""
        if hint and _retarget(run, step_n, hint):
            run["start"] = step_n
            return _execute(run)

    # Neither understood nor guessed at: one more try to be told, then stop.
    if paused["unclear"] < 1:
        result = {"step": step_n, "resume_from": paused["resume_from"], "text": paused["why"],
                  "options": paused.get("options"), "trail": paused.get("trail")}
        return _ask_stuck(run, result, "I did not catch that.", unclear=1)

    _settle(run, "halted")
    write_final_reply(f"I could not tell what to do at step {step_n} and would rather not guess, so I stopped there. "
                      "Nothing after it was done.", kind="error", engine=SKILL_ENGINE)


def _reads_replies():
    """Whether a model is here to read a person's reply (approval.read_choice); without one, only plain replies count."""

    try:
        from decurix_agent import llm_client
        return llm_client.available()
    except Exception:  # noqa: BLE001
        return False


def _unanchorable(step, result):
    """Whether a halted step is a point click that stopped only because its picture did not match."""

    return bool(step.get("point") and step.get("look") and replay.LOOK_MISSING in (result.get("text") or ""))


def _unanchor(run, step_n):
    """
    Let a point click go without its picture check, for the rest of this run:
    True when step `step_n` is one. The person looked at the screen and said the
    spot is right - a focus ring, an open drop-down or a hover state is enough
    to make the picture differ. Never written back into the skill.
    """

    steps = (run.get("expanded") or {}).get("steps") or []
    if not 1 <= step_n <= len(steps):
        return False

    step = steps[step_n - 1]
    if not step.get("point"):
        return False

    run.setdefault("unanchored", [])
    if step.get("id") not in run["unanchored"]:
        run["unanchored"].append(step.get("id"))
    return True


def _retarget(run, step_n, wanted, role=None):
    """
    Point a step at the control a person named instead: True when it had a named
    control to change. Held on this run's copy of the skill and saved only if
    the run then succeeds - a correction that did not work is not learned.
    """

    # The step number is the run's - a loop is written out once per item - but
    # a correction belongs to the skill's own step, so it holds in every item.
    original = run["expanded"]["steps"][step_n - 1]["id"]
    step = next(s for s in run["skill"]["steps"] if s["id"] == original)
    target = step.get("target") or {}

    if not target.get("role"):
        return False

    name = wanted.strip().strip("\"'`“”").strip()
    name = re.sub(rf"\s+(instead|{re.escape(target['role'])})$", "", name, flags=re.IGNORECASE).strip("\"'`“”").strip()

    if not name:
        return False

    run["patches"].append({"n": step["n"], "from": target.get("name"), "to": name})
    step["target"] = {key: value for key, value in dict(target, name=name, role=role or target["role"]).items() if key != "path"}
    # The name it had stays known, in case the screen goes back.
    experience._promote(step, target.get("role"), target.get("name"))
    step["summary"] = replay._summary(step)

    return True


# ============================================================
# Learning a skill with the model, and checking every finish
#
# A worker still learning its range of tasks leans on the brain: until a skill
# has run TRUST_AFTER times in a row cleanly (experience.stage), the engine
# does it, with the skill - the person's steps, their words, what the lesson
# worked out, corrections from earlier runs, this run's values - as its guide,
# and it can look at the screen each step was done on (`gui skill NAME
# --look N`) or replay the stretches that fit (`gui do`). Once the skill runs
# smoothly it is a replay, and the model is called only when the replay gets
# stuck (_execute). A failure puts it back to learning.
#
# Either way, no success is reported until it is shown: _finish judges a
# replay's end, final_gate._gate an engine's "done", and anything short of a confident
# "done" asks the person (_doubt_text) - who may say it is done, have it tried
# again, or say what is wrong, which is kept as a correction the model reads
# on every later run.
# ============================================================

MAX_CORRECTIONS = 8


def _engine_can_drive():
    """Whether this desktop's engine can do screen work with a hosted model (TheBot on one, or the Office engine)."""

    from decurix_agent import harness  # (the one file that branches on AGENT_HARNESS; imported here, not at the top)
    return harness.drives_screen_with_hosted_model() and llm_client.available() and getattr(state, "backend_ready", False)


def _stage(run):
    return experience.APPRENTICE if run.get("one_off") else experience.stage(run["skill"])


def _learning_by_model(run):
    # A recording nobody has taught is mimicked: there is no lesson for a
    # model to follow, only what the person did - and that, with this run's
    # values put where theirs were, is exactly what a replay does. The model
    # is for what a replay cannot do: it takes over where the replay gets
    # stuck (_execute), and judges the result before anything is reported
    # (_finish). Found live with the model first: it replayed the recording
    # as it was, saving an order for the customer of the demonstration, and
    # only then did the one it was asked for.
    if run.get("one_off"):
        return False

    if not _engine_can_drive() or run.get("replay_only"):
        return False
    # A run that gathered its documents and read the per-item lists from them replays the per-item block, apprentice
    # or not: the model did the part that varies (the gather), and takes over where the replay stops. Its trust decides
    # whether the plan is put to the person first (_after_phase), never what is checked. See "The per-document part".
    if run.get("gathered") == "done":
        return False
    if _stage(run) == experience.APPRENTICE:
        return True
    # A task whose rules ask for more than its recorded steps (_supervise) - found out when a replay of it
    # could not be confirmed, or plain from the person's own words.
    if run["skill"].get("supervised") or _asks_for_a_check_first(run["skill"]):
        return True

    # Trusted: replayed. A task with a value picked from a list, or with a row
    # per entry of a document, used to stay the model's however often it had
    # gone well - a replay clicked the entry the demonstration chose and
    # entered the demonstration's number of rows. The program is now fitted to
    # the run (skills.fit): it searches each list for this run's entry and
    # enters this run's rows, and stops where it cannot. Its result is judged
    # before it is reported (_finish), and a replay that stops is the model's
    # to carry on (_execute).
    return False


# What the engine and the check are told about a value the person typed only
# to search with. Found live: the words read from a document for such a value
# (a long product name, then a shorter part of it) were typed
# whole, matched nothing, and the correct record picked in the end ("A4
# Printing Paper") was refused by the check as "another value".
CHOSEN_NOTE = ("a search, not the text to end up in the field: the person typed a few words, a list of the "
               "application's existing entries appeared, and they chose one. Do it with `gui select FIELD \"these "
               "words\"`: it searches for them and for parts of them and takes the one existing entry whose own "
               "name is these words or lies within them, and says which it took. When it takes none it lists what "
               "the searches offered - then choose one of those by its ref only if it is plainly the same thing, "
               "and ask (HELP) if none is. Never type these words and click in the list by position")


def _first_step_inside(run):
    """
    The first step of the task that is done inside the application: the one
    after those that open it from the desktop. An engine that has the
    application open already (it has just made a check in it) starts there.
    """

    steps = run["skill"].get("steps") or []
    for step in steps:
        opens = step.get("kind") == "open" or (step.get("kind") in ("click", "double_click")
                                               and step.get("point") and not step.get("target"))
        if not opens:
            return step.get("n") or 1
    return 1


def _start_page(run):
    """The address of the page the application showed when the person began working in it, or ""."""

    inside = _first_step_inside(run)
    for step in (run["skill"].get("steps") or [])[:max(inside - 1, 1)]:
        for check in step.get("expect") or []:
            if check.get("type") == "url" and check.get("value") and "*" not in check["value"] and "{" not in check["value"]:
                return check["value"]
    return ""


def _back_to_start(run):
    """
    The command that puts an application that is already open back on the
    page the person's steps begin on, to run before the steps. Found live: a
    worker made its before-you-start check, which left the application on the
    record of the last task; the steps begin on its opening page, where the
    controls they use are, and the replay rightly refused to start from
    anywhere else - so the worker asked a person what to do.
    """

    page = _start_page(run)
    return f"gui open {page} && " if page else ""


def _do_line(run, start=None):
    """`gui do` for this run, with its values written out: what the engine runs to have the steps done for it."""

    import shlex

    parts = [f"gui do {run['skill']['name']}"]
    parts += [f"--param {shlex.quote(name + '=' + str(value))}" for name, value in (run.get("values") or {}).items()
              if value not in (None, "")]
    if start and start > 1:
        parts.append(f"--from {start}")
    return " ".join(parts)


def _chosen_for(run):
    """The names of this run's values that are picked from a list: the skill's, and those of the rows added for this run."""

    chosen = set(skills.chosen_from_list(run["skill"]))
    # A value set in a drop-down is one of its entries too, by the entry's own name. Found live (2026-10-14): a form
    # saying "IT department" was entered as the drop-down's "IT", and the check refused the correct record.
    chosen |= {s["param"].split("__")[0] for s in run["skill"].get("steps") or [] if s.get("kind") == "select" and s.get("param")}
    for name, base in ((run.get("rows") or {}).get("base") or {}).items():
        if base in chosen:
            chosen.add(name)
    return chosen


def _rows_note(run):
    """What this run's document says about the part the demonstration repeated, for the engine and the check."""

    rows = run.get("rows")
    if not rows:
        return ""
    return (f"The fields {', '.join(rows['fields'])} are one row, entered once per entry of the document. This run's "
            f"document has {rows['count']} such entr{'y' if rows['count'] == 1 else 'ies'} (the demonstration's had "
            f"{rows['demo']}), so exactly {rows['count']} row{'' if rows['count'] == 1 else 's'} belong"
            f"{'s' if rows['count'] == 1 else ''} in the record: one per entry, none more and none fewer. A row for "
            "every entry of the document is right, however many the demonstration showed.")


def _same_value(wanted, typed):
    """Whether what was typed is decidedly the value wanted (values.same_value is True): the same text, the same number
    to its last digit however it is written, the same day under each side's date order. (It went through float(), which
    loses every digit past the 15th: two 17-digit account numbers were one, W0.3.)"""

    a, b = " ".join(str(wanted).split()).casefold(), " ".join(str(typed).split()).casefold()
    if a == b:
        return True
    # The value wanted is the documents' (the source's date order), what was typed the application's.
    return field_values.same_value(wanted, typed, a_order=task_context.source_date_order(),
                                   b_order=task_context.app_date_order()) is True


def _never_entered(run):
    """
    This run's values that no tool typed or selected, as {name: value}. Found
    live: a row's rate was never typed, the application's own default stood in
    its place, and the run reported the row as entered. A value that goes into
    a field by typing leaves a typing behind; one that does not was not
    entered, whatever the report says. Values picked from a list are left to
    the check (the entry taken goes by its own name), and so are files.
    """

    # Only what this run read from its documents: a value the skill carries
    # from the demonstration (a fixed search, a default) may be reached
    # another way - the app opened by its name, a page by its address.
    skill = run["skill"]
    values = {name: value for name, value in (run.get("values") or {}).items() if name in (run.get("evidence") or {})}
    if not values:
        return {}
    chosen = _chosen_for(run)
    files = {p["name"] for p in skill.get("params") or [] if p.get("file")}
    done = evidence.entered()
    if not done:
        # Nothing at all was recorded as typed: the task was done another way, or not on the screen.
        return {}

    # A value the tools read off the screen is there too, typed or not. Found
    # live: a field the application had filled with the very figure the
    # document gives was read, left alone - and the run was refused for a
    # value "never typed".
    seen = evidence.read()
    # (Every digit counts, as in _same_value: Decimal, never float.)
    tokens = re.findall(r"(?<![\w.])\d[\d,]*(?:\.\d+)?(?![\w])", seen)
    seen_numbers = {n for n in (field_values.parse_number(t) for t in tokens) if n is not None}

    def was_read(item):
        text = " ".join(str(item).split())
        if text.casefold() in " ".join(seen.split()).casefold():
            return True
        number = field_values.parse_number(text)
        return number is not None and number in seen_numbers

    # A reference a run reached by pressing the control named for it was used, not left out. Found live
    # (2026-10-15): a delivery received against PO-4423, opened by clicking its link in the list, was refused as
    # "PO-4423 was never typed" - and the open doubt then swallowed the next delivery's request. Only a value
    # with a letter in it: a quantity is not reached by a link that happens to read "12".
    names = [" ".join(n.split()).casefold() for n in evidence.pressed()]

    def was_pressed(item):
        text = " ".join(str(item).split()).casefold()
        return bool(re.search(r"[a-z]", text)) and any(
            re.search(r"(?<![\w-])" + re.escape(text) + r"(?![\w-])", n) for n in names)

    missing = {}
    for name, value in values.items():
        if name in chosen or name.split("__")[0] in chosen or name in files or value in (None, ""):
            continue
        items = skills.split_items(value) if any(p.get("name") == name and p.get("list")
                                                 for p in skill.get("params") or []) else [value]
        for item in items:
            if not any(_same_value(item, typed) for typed in done) and not was_read(item) and not was_pressed(item):
                missing[name] = item
    return missing


def _for_this_run(text, skill, values):
    """
    `text` about the task with each value the demonstration typed replaced by
    this run's. What was understood from a demonstration is worded in its own
    values ("a LinkedIn profile is open for Koustubh Rao"): read as it stands
    it sends the model after the demonstration's result, and - found live -
    the check called a run for another name done because the demonstration's
    page was still on the screen.
    """

    # What the demonstration typed to search a list is not a value the lesson speaks of: it is a few letters of
    # the entry that was then picked. Found live: the demonstration typed the first word of a two-word entry; a
    # run whose document named another kind had that word replaced all through the person's own words, so the rule
    # that named the entry read as a mix of the two - an entry the list never had - and the check failed a
    # correct record for not showing it.
    searched = skills.chosen_from_list(skill)

    for param in skill.get("params") or []:
        example, value = param.get("example"), (values or {}).get(param.get("name"))
        if value not in (None, ""):
            # A lesson writes an input as its name in braces (study.py).
            text = text.replace("{" + str(param.get("name")) + "}", str(value))
        if not example or value in (None, "") or param.get("list") or len(str(example)) < 3:
            continue
        if str(param.get("name") or "").split("__")[0] in searched:
            continue
        # As a whole word or phrase only - never the letters of it inside another word.
        text = re.sub(r"(?<!\w)" + re.escape(str(example)) + r"(?!\w)", lambda _m, v=str(value): v, text,
                      flags=re.IGNORECASE)
    return text


# What a result's description still holds of the demonstration once its typed
# values are replaced.
EXAMPLE_NOTE = (
    "READ THIS FIRST. What follows was written while watching ONE demonstration, done with other values than this "
    "run's. This run's values have been put in where the demonstration's were. Anything else in it that describes "
    "that demonstration's own result - a place, a date, an amount, a company, a job title, a count - is only an "
    "example of the KIND of result, and this run's result will show its own instead: do not require it, do not "
    "look for it, and do not treat its absence as a failure. What must hold is the kind of outcome (the right page "
    "or record for THIS run's values, saved or opened or answered as the task asks). The demonstration's own result "
    "still being on the screen is not this run's result."
)


def _criteria(skill, values=None, since=None):
    """
    What the person said about the task and what done looks like, for the
    model and the check - in this run's values when they are given. since: when this run began; what the person
    answered after it is this run's word, put last and above the run's values (_said_this_run).
    """

    taught = _taught(skill, since)
    if values:
        taught = EXAMPLE_NOTE + "\n" + _for_this_run(taught, skill, values)
    now = _said_this_run(skill, since)
    return taught + ("\n" + now if now else "")


def _said_this_run(skill, since):
    """
    The person's answers during this run, as the word that decides. Found live (2026-10-24): the person said an invoice
    series the run started with was invented and where the real numbers are, the work was corrected - and the check,
    shown that answer as one "asked during an earlier run" beside "This run's values: first number = <the invented
    one>", sent the corrected work back to the invented series.
    """

    said = [s for s in skill.get("run_answers") or [] if since and (s.get("at") or 0) >= since]
    if not said:
        return ""
    return ("What the person said during THIS run - it decides over this run's values and everything above: where "
            "they disagree, what the person said now is what must hold:\n"
            + "\n".join(f"- asked: {s['q'][:400]} - the person answered: {s['a']}" for s in said))


def _taught(skill, since=None):
    """What the person taught and answered before this run (since: when it began; None, every answer)."""

    parts = []
    # What the organisation holds for this worker (its role's and the org's rules and facts, org_knowledge.py) comes
    # first, the task's own items last among them; the skill's own taught lines after it read as refinements. Cached
    # per task, so a run's prompt stays the same.
    parts.extend(org_knowledge.cached(task_id=task_context.current() or None))
    said = skills.explanation(skill)
    if said:
        parts.append(said)
    understood = skill.get("understanding") or {}
    if understood.get("done_when") and understood["done_when"] not in said:
        parts.append(f"Done when: {understood['done_when']}")
    for rule in understood.get("rules") or []:
        parts.append(f"Rule: {rule}")
    for note in understood.get("web_notes") or []:
        # Looked up while the task was taught (study._researched): how the app works, never a value.
        parts.append(f"About the app (from its documentation, looked up when it was taught): {note}")
    for said in understood.get("dialogue") or []:
        # The person's own words, which the rules above were written from.
        parts.append(f"Asked when it was taught: {said['q']} - the person answered: {said['a']}")
    for correction in skill.get("corrections") or []:
        parts.append(f"A correction the person made after an earlier run: {correction['text']}")
    for said in skill.get("run_answers") or []:
        if since and (said.get("at") or 0) >= since:
            continue  # this run's (_said_this_run)
        parts.append(f"Asked during an earlier run: {said['q']} - the person answered: {said['a']}")
    return "\n".join(parts)


def _procedure(run, from_step=None, note="", ran=False, phase=None):
    """What the engine is told when it does a taught task (guidance.py puts it into its model's context)."""

    skill = run["skill"]
    name, title = skill["name"], skill.get("title") or skill["name"]
    one_off = run.get("one_off")
    streak = 0 if one_off else experience.clean_streak(skill)

    lines = []
    if one_off:
        lines.append(f"This request is a task a person recorded on this desktop (\"{run['mimic']['text']}\"), not yet taught. "
                     "Do it the way they did, adapting to what the screen shows now.")
    else:
        lines.append(f"This request is a task a person taught this desktop by doing it: skill `{name}` (\"{title}\"). "
                     f"You are still learning it ({streak} of {experience.TRUST_AFTER} clean runs before it runs on its "
                     "own), so do it yourself, with care: follow what the person did and said, and adapt to what the "
                     "screen shows now - the screen is the truth, the recording is the guide.")

    if run.get("attempt_note") and run.get("attempt_blank"):
        # The first attempt never touched the screen (its recording has no action - said only then, never from a reading
        # of what it pressed: independent review, 2026-10-22), so there is nothing of it to find and correct. Found live:
        # told all the same that its "first attempt already saved its work", the engine took the record an
        # EARLIER task had left open for that work, saw it was another record, and three times over did nothing.
        lines.append("YOUR FIRST ATTEMPT AT THIS REQUEST KEPT NOTHING IN THE APPLICATION (nothing was saved, added or submitted)"
                     + (f" (it reported: {' '.join(run['attempt_report'].split())[:500]})" if run.get("attempt_report") else "")
                     + f". A check of the screen then found: {run['attempt_note']}"
                     + ". Nothing of this request has been kept: no record of it exists yet, and whatever is open on "
                       "the screen now was not saved - leave anything of an earlier task as it is. Do the task now, from its first step, "
                       "yourself: one gui command at a time, following the steps below. Where gui do refused to "
                       "replay it, that is not a reason to stop - it means the steps are yours to do.")
    elif run.get("attempt_note"):
        # Found live, three documents running: sent back after the check, the
        # engine did the whole task again from an empty form and left a second
        # record beside the first. Told only at the end of its instructions to
        # "fix that", it still did - so it is told first, with what it reported.
        again = ["THIS IS YOUR SECOND ATTEMPT AT THIS REQUEST. Your first attempt finished and reported:"]
        if run.get("attempt_report"):
            again.append(run["attempt_report"])
        again.append(f"A check of the screen then found: {run['attempt_note']}")
        if _actions_note(run):
            again.append(_actions_note(run))
        # Whether that kept a record is not known here (Found live: a failed login kept none; a keyboard save keeps
        # one with no "Save" pressed): the engine looks in the application first (independent review, 2026-10-22).
        again.append("Whatever the first attempt saved, created or sent STILL EXISTS - but whether it kept a record is "
                     "not known. FIRST look in the application for this request's record (find it by the name or "
                     "number in the report above, or by searching for this request's own reference). If it is there, "
                     "open it, look at what it holds, and correct only what the check found wrong or could not see: do "
                     "not start again from an empty form and do not create anything a second time - that enters the "
                     "request twice. Only if the application shows no such record, enter it, once. If you cannot "
                     "tell, ask (HELP) before making another.")
        again.append(NOT_YOURS_TO_FORCE)
        lines.append("\n".join(again))

    understood = skill.get("understanding") or {}
    if understood.get("summary"):
        lines.append(f"What the task is: {_for_this_run(understood['summary'], skill, run['values'])}")
    criteria = _criteria(skill, run["values"], since=run.get("began"))
    if criteria:
        lines.append("What the person said, and what was agreed when they taught it (follow it):\n" + criteria)

    if run["values"]:
        params = {p["name"]: p for p in skill.get("params") or []}
        chosen = _chosen_for(run)
        bases = (run.get("rows") or {}).get("base") or {}
        shown = []
        for key, value in run["values"].items():
            param = params.get(key) or params.get(bases.get(key)) or {}
            meaning = ((understood.get("inputs") or {}).get(key) or (understood.get("inputs") or {}).get(bases.get(key))
                       or param.get("label") or key)
            shown.append(f"- {key} = {json.dumps(value, ensure_ascii=False)} ({meaning}; the person typed "
                         f"{json.dumps(param.get('example') or '', ensure_ascii=False)}"
                         + (f"; {CHOSEN_NOTE}" if key in chosen else "") + ")")
        lines.append("This run's values - type these where the person typed theirs:\n" + "\n".join(shown))
        if _rows_note(run):
            lines.append(_rows_note(run) + " Add a row for each entry after the first the way the person added "
                         "theirs, and never add one after the last.")

    if skill.get("documents"):
        lines.append("When they showed this, the person worked from these documents: "
                     + attachments.names(skill["documents"]) + ". They are examples of the kind of document the task "
                     "starts from - this run's own documents are the files its request names; read those (`office "
                     "read PATH`) and take this run's values from them, never from the examples.")

    steps = []
    for step in skill["steps"]:
        if phase == GATHER_PHASE and step["n"] >= _loop_start(skill):
            break  # (the per-item block and what follows are not this phase's)
        if phase == REST_PHASE and step["n"] < _loop_start(skill):
            continue  # (the steps before the block were done by the gather)
        line = f"{step['n']}. {step.get('summary') or step['kind']}"
        if step.get("intent"):
            line += f" - the person said: {step['intent']}"
        purpose = (understood.get("steps") or {}).get(str(step["n"]))
        if purpose:
            line += f" - purpose: {purpose}"
        steps.append(line)
    lines.append("The person's steps:\n" + "\n".join(steps))
    for loop in skill.get("loops") or []:
        # What the person did between two items when they taught it (learning._apply_loop): not part of every item,
        # so no step repeats it - done when its case comes up (a record the next item needed, a document to read).
        for gap in loop.get("between_summaries") or []:
            lines.append("Between two items, the person also did this once (not part of every item - do it only when "
                         "the same case comes up): " + "; ".join(gap)[:600])

    if from_step:
        lines.append(f"A replay of these steps already ran up to step {from_step} and stopped there: {note} "
                     + ("That step WAS done but its result did not show: look at the screen before doing anything - "
                        "never repeat it blindly. " if ran else "")
                     + f"Carry on from step {from_step}.")
    elif note:
        lines.append(note)

    if one_off:
        how = [
            "How to work:",
            f"- `gui recording {run['mimic']['id']}` lists what the person did; work each step with gui observe / "
            "click / type / key, adapting to the screen.",
            f"- `gui replay {run['mimic']['id']} --from N` replays their steps from step N with THIS run's values "
            "(they are filled in for you); use it only when the screen matches the recording, and check where it "
            "stopped. Never redo a step that already ran: it would do the task twice.",
        ]
    else:
        how = ["How to work:"]
    look = (f"- `gui skill {name} --look N` shows the screen as it was when the person did step N, with the spot they "
            "pressed ringed in red. Use it for any step recorded as a point on the screen, and whenever you are unsure "
            "what a step meant.")
    if run.get("attempt_note") and not run.get("attempt_blank"):
        # Found live (2026-10-14): sent back over one value of a record it had saved, the engine ran the replay from
        # the start - which ends by saving - and a second employee record was made. A second attempt corrects; it
        # never replays the task (gui do refuses it too, for a skill already replayed to its end in this task).
        how += [look, f"- Do NOT run `gui do {name}` again for this request: the first attempt worked in the "
                      "application, and the replay starts a new record and saves it. Find what the first attempt made "
                      "and correct it by hand with gui observe / click / type / select / key."]
    corrects = bool(run.get("attempt_note") and not run.get("attempt_blank"))
    how += [] if one_off or corrects else [
        look,
        "- Do it in this order. It is the order the person works in, and each part has its command:\n"
        "  1. Read this request's document(s) (office read), if you have not.\n"
        "  2. Do what the person's rules say to check BEFORE starting - a rule about what ALREADY EXISTS (below) "
        "is one - with the application's own search or list. A check made after the record exists is too late.\n"
        "  3. Do the person's steps with THIS run's values - the taught steps are your guide. The quickest way is to "
        "have them done for you by ONE of these commands; it searches each list for this run's entry, takes it only "
        "when it is plainly the same thing, and enters one row per entry of this run's document:\n"
        "       when the application is not open:      " + _do_line(run) + "\n"
        "       when it is already open (after step 2): " + _back_to_start(run)
        + _do_line(run, start=_first_step_inside(run)) + "\n"
        # (Not a mandate: while a kind of work is new the engine may work its steps by hand, and the check of the work
        # is told the same. Found live (2026-10-25): told "run exactly one gui do, do not enter by hand", the engine
        # entered by hand anyway, and three review rounds in a row were spent on that one contradiction.)
        "     Working the steps by hand instead is fine - then type every value from this run's documents exactly, "
        "and check each entry on the screen as you go.\n"
        "  4. The command stops at the first step it cannot do, and says which step and why. Look (gui observe), do THAT "
        "ONE step yourself with gui, and carry on with the same command and `--from N` for the next step number - "
        "never from the start again: what it entered is there, and starting over enters the request twice. If it "
        "stopped because a list has no entry for something, that is a question for the person (HELP), not "
        "something to work around.\n"
        "  5. When it has run to the end it has checked nothing. Read the result off the screen (gui observe) "
        "against this run's values and the person's rules, and report what you read. Do not press its last "
        "step again to make sure: it ran. Found live - a save shortcut pressed a second time on a record just "
        "saved is, in some applications, the next action on it (submit, post), and it opened \"Permanently "
        "Submit?\". Change only a value you read as wrong.",
        "- Work a step by hand with gui observe / click / type / select / key wherever the command cannot.",
    ]
    how += [
        "- What the person answered - when they taught this and during earlier runs - is settled. Act on it and "
        "never ask them the same thing again; it also covers the same kind of choice met somewhere else (told to "
        "take the first of several matching conversations, take the first of several matching people in any list).",
        "- If the request asks for several items (a list), do the task once per item, one after another, unless "
        "the person said otherwise.",
        # Found live: taught "if a bill with the same number is already entered, do not enter it again", the
        # worker entered the bill and then reported that it had not checked. No step of a demonstration shows
        # a check that found nothing, so the rule has to say what keeping it takes.
        "- A rule of the person's about what ALREADY EXISTS (\"if one with the same number is already entered, do "
        "not enter it again\") is kept by looking BEFORE you create anything: search the application for it - its "
        "search box, or the list of such records filtered by that number or name - and only then start. Say in "
        "your report what you searched for and what you found. If it exists, stop and tell the person; do not "
        "open a new form.",
        "- Before you say DONE, look at the screen (gui verify) and make sure it shows the task done for THIS run's "
        "values. If it does not, keep working, or ask (HELP) - never report DONE for what the screen does not show. "
        "Your report is checked against the screen, and a false DONE is the worst outcome.",
        "- When the task ends by closing or leaving the page that shows the result, read the result off that page "
        "FIRST (gui observe, or gui verify --text with each value that matters: the reference, the date, the "
        "total). The check sees only the screen as you leave it and what your tools read before - a value nobody "
        "read cannot be confirmed afterwards, and the task comes back to you.",
        "- A value a field refuses (it does not keep what you typed) is not entered: type it again in the form "
        "the field shows it takes, and read the field back. Never pick a date from a calendar by guessing a day.",
    ]
    if phase == GATHER_PHASE:
        how = _gather_how(run)
    elif phase == REST_PHASE:
        how = _rest_how(run)
    elif run.get("by_hand"):
        # A takeover part way through the replay: the documents set aside are still to be entered, by the model.
        how.append("- These documents of this request are NOT in the lists above and are yours to enter by hand, each "
                   "once, the way the person's steps do, reading each from its file: "
                   + "; ".join(f"{x['document']} ({x['why']})" for x in run["by_hand"]) + ".")
    lines.append("\n".join(how))

    return "\n\n".join(lines)


def _gather_how(run):
    """How the engine works the gather phase of a per-document run: the steps before the per-item block, and no entry."""

    start = _loop_start(run["skill"])
    return [
        "How to work - THIS PHASE ONLY:",
        f"- Do the person's steps BEFORE step {start} (the ones listed above) for THIS request: open where its documents "
        "are, save every one of them where the person's steps put theirs (the client's folder for the period; make the "
        "folder if it is missing, named like the folders beside it), and open the application to where step "
        f"{start} begins.",
        "- Do NOT enter, create, save or submit any record in the application: the entering is done after this phase, "
        "once per document, from the files you save. Nothing of the per-item steps is yours now.",
        "- Save EVERY document of this request, whatever their number - the demonstration had its own number. Count "
        "them against what the request, the mail or the chat shows, and say if one looks missing or doubled.",
        "- What the person answered - when they taught this and during earlier runs - is settled. Act on it and never "
        "ask them the same thing again. Ask (HELP) only for what nothing in front of you can tell.",
        "- When every document is saved, finish with DONE and a report that lists the folder, then one line per file "
        "saved in the form `FULL PATH - saved, not entered in this phase`, and anything about a document that looked "
        "wrong (missing, a duplicate, not of this kind, cancelled). Entering is the next phase's: a report of this "
        "phase is complete when every document is saved.",
    ]


def _rest_how(run):
    """How the engine works the rest phase: the documents the fill set aside, entered by hand, and nothing else."""

    docs = run.get("documents") or []
    aside = {x["document"] for x in (run.get("by_hand") or []) + (run.get("left_out") or [])}
    done = [Path(d).name for d in docs if d not in aside]
    return [
        "How to work - THIS PHASE ONLY:",
        # Found live (2026-11-02): the model read the first phase's "enter nothing" as a rule over the whole task, called
        # the vouchers the replay had saved since a breach of "the settled collection-only restriction", and asked the
        # person whether to undo them. The phases are told in order, and what each did.
        "- This task ran in phases, in this order: (1) YOU collected and read this request's documents and entered "
        f"nothing - that phase is OVER, and its 'enter nothing' was that phase's rule alone, never a restriction on "
        "this task; (2) the person's own taught steps were then run once per document" + (
            ", on a plan the person approved" if run.get("plan_ok") else "") + "; (3) now this phase. The records "
        "made in phase 2 are this request's work, right to have been made, and stay: never undo, question or report "
        "them as a breach. The sources were all collected and read BEFORE any record was made, so the order 'review "
        "the sources, then enter' has been kept - do not redo that review and do not report it as missed.",
        "- The taught steps have ALREADY been run once per document for these documents, which are entered: "
        + (", ".join(done) or "none") + ". Do NOT enter them again, and do not check them now.",
        "- Enter BY HAND, each once, the way the person's steps do (gui observe / click / type / select / key, one at a "
        "time, every value read from the document's own file with office read), ONLY these documents, which were set "
        "aside because they do not fit the taught shape: "
        + "; ".join(f"{x['document']} ({x['why']})" for x in run.get("by_hand") or []) + ".",
        "- Documents the person's rules leave out stay out: " + ("; ".join(
            f"{Path(x['document']).name} ({x['why']})" for x in run.get("left_out") or []) or "none") + ".",
        "- What the person answered - when they taught this and during earlier runs - is settled. Act on it and never "
        "ask them the same thing again. Ask (HELP) only for what nothing in front of you can tell.",
        "- Before you say DONE, read each record you made back off the screen (gui observe) against its document, and "
        "report per document: what was entered, its reference, and what you read back. Never report DONE for what the "
        "screen does not show.",
    ]


def _engine_request(run, phase=None):
    """The request the engine is given for a taught task (`phase`: one part of it)."""

    skill = run["skill"]

    # A request matched to a recording goes in the same words as a `/skill`
    # run, with what the person typed after them - never as their words alone.
    # Found live: `search "sundar pichai"`, matched to a taught search, went to
    # the engine as typed; its triage called that a question, its chat skill -
    # no tools - answered "DONE", and nothing had touched the screen. The
    # engine sends "Do the task you were taught" to its operator, always.
    if run.get("one_off"):
        request = f"Do the task you were taught by a recording of \"{run['mimic']['text']}\""
    elif phase == GATHER_PHASE:
        request = (f"Do the FIRST PART of the task you were taught, \"{skill.get('title') or skill['name']}\" (skill "
                   f"{skill['name']}): collect this request's documents the way the person's first steps do, and enter "
                   "nothing")
    elif phase == REST_PHASE:
        entered = len(run.get("documents") or []) - len((run.get("by_hand") or []) + (run.get("left_out") or []))
        request = (f"Do the REST of the task you were taught, \"{skill.get('title') or skill['name']}\" (skill "
                   f"{skill['name']}): the first part (collecting this request's documents, entering nothing) is "
                   f"finished, and the taught steps have since entered {max(entered, 0)} of them, rightly; enter by hand "
                   "only the documents your instructions name as left for you, each once, and nothing else")
    else:
        request = f"Do the task you were taught, \"{skill.get('title') or skill['name']}\" (skill {skill['name']})"
    if run["values"]:
        stated, inferred = _stated_values(run)
        if stated:
            request += " with " + ", ".join(f"{k}={json.dumps(v, ensure_ascii=False)}" for k, v in stated.items())
        if inferred:
            # A value the request does not say is a reading, not the person's word. Found live (2026-10-29): the
            # demonstration's "6. SEPTEMBER" was read for December as "12. DECEMBER"; sent as "with
            # month_folder_name=...", the review took it for the person's instruction and sent correct work back nine
            # times (the client's folders number months from April: "9. DECEMBER").
            request += ("; values read for this run, not said by the person - check each against the documents and "
                        "the application (a folder's name against the folders beside it) before you use it: "
                        + ", ".join(f"{k}={json.dumps(v, ensure_ascii=False)}" for k, v in inferred.items()))
    request += "."
    if run.get("attempt_note") and run.get("attempt_blank"):
        request += ("\n\nYOUR FIRST ATTEMPT KEPT NOTHING IN THE APPLICATION (nothing was saved, added or submitted), so "
                    "nothing of this request is entered yet. Whatever is open now is from an earlier task: leave it alone. Do this request now from its first step, "
                    "with gui commands of your own.")
    elif run.get("attempt_note"):
        # In the request itself, not only in the guidance: found live, with it
        # in the guidance alone the engine still entered the document again.
        request += ("\n\nTHIS IS A SECOND ATTEMPT - DO NOT ENTER IT TWICE. Your first attempt worked in the application"
                    + (f" (it reported: {' '.join(run['attempt_report'].split())[:400]})" if run.get("attempt_report") else "")
                    + f". A check then found: {run['attempt_note']} "
                    + (_actions_note(run) + " " if _actions_note(run) else "")
                    + "Whether it saved a record is not known: FIRST look in the application for what it saved. If it "
                      "is there, read what it holds and correct only what is really wrong there; if it is all "
                      "correct, change nothing: read its values off the screen and report them. Creating a second "
                      "record is the one thing you must not do - only if the application shows none, enter it once. "
                      "In your report, call that record what it is - the one this request produced, with its number "
                      "- never something that \"was already entered\". " + NOT_YOURS_TO_FORCE)
    # The person's answers outlive the attempt they were given in. Found live (2026-10-31): without them the second
    # attempt took the check's note for the fault, found "no such instruction" from the person, and undid a correction
    # the person had given.
    said = guidance.said_this_task() if run.get("attempt_note") else ""
    if said:
        request += ("\n\n" + said + "\nThese answers stand over the check's note above: where the note contradicts one "
                    "of them, the person's answer is right.")
    if run.get("mimic"):
        request += "\n\nThe person asked: " + (run.get("context") or run["mimic"]["text"])
    elif run.get("context"):
        request += "\n\n" + run["context"]
    return request


def _stated_values(run):
    """(stated, inferred): the run's values the person's own words hold (case and spacing aside), and the rest - read
    for this run from the request and the documents."""

    said = " ".join(str(run.get("context") or (run.get("mimic") or {}).get("text") or "").lower().split())
    stated, inferred = {}, {}
    for name, value in run["values"].items():
        text = " ".join(str(value).lower().split())
        (stated if text and text in said else inferred)[name] = value
    return stated, inferred


def _by_model(run, from_step=None, note="", ran=False, phase=None):
    """Hand the task to the engine, guided by the skill (see the section above); `phase`: one part of it only."""

    skill = run["skill"]
    request = _engine_request(run, phase)
    run.setdefault("attempts", 0)
    run["attempts"] += 1

    guidance.begin(skill=skill["name"], task=request, text=_procedure(run, from_step, note, ran, phase=phase),
                   values=run["values"], mode="phase" if phase else ("takeover" if from_step else "model"), run=run,
                   phase=phase)

    if phase == GATHER_PHASE:
        progress.emit("Collecting this request's documents with the model, the way you showed me; nothing is entered yet.")
    elif phase == REST_PHASE:
        progress.emit("The taught steps have run once per document; the model now enters by hand the documents that did "
                      "not fit them: " + ", ".join(Path(x["document"]).name for x in run.get("by_hand") or []))
    elif from_step:
        progress.emit(f"The replay stopped at step {from_step}; the model is taking it from there.")
    else:
        progress.emit(f"Doing \"{skill.get('title') or skill['name']}\" with the model, guided by what you taught "
                      "(it is still learning this task).")
    log(f"Skill {skill['name']}: handed to the engine ({'takeover at ' + str(from_step) if from_step else 'learning'})")

    # Whether the run may start without asking was settled before this (_start_pause: the teacher's `approval`
    # answer, or the person's yes just now). The engine is told nothing of it: a yes is one action's, so every
    # consequential action of the run still stops at the approval gate (soul takes no approval from a request).
    _engine(request, state.current_task_file)


def _judge(run, claim, note="", purpose="py.judge"):
    """The final check's judge on a skill run (`note`: evidence code found, for the judge to weigh; `purpose`: what the
    judge's model call is for - a trusted replay's spot check is "py.judge.spot")."""
    skill = run["skill"]
    steps = "; ".join(f"{s['n']}. {s.get('summary')}" for s in skill["steps"])[:1500]
    # With what the person said during the task. Found live (2026-10-31): asked about a document's identifier that failed
    # its own check, the person gave the right one; the check, shown only the request, failed the work for not using
    # the printed one.
    # A gathered run's files are a fact code decides (_documents_note), on every way the run ends - the replay's and
    # the model's alike (found live 2026-11-02: a month the model did itself after the gather was judged without them).
    found = "\n".join(x for x in (note, _documents_note(run), _assumed_note(run)) if x)
    gone = _record_unread()
    if gone:
        return {"verdict": verify.UNSURE, "confidence": 0.0, "seen": "",
                "why": verify.outcome(None, not_made=[gone])["why"]}
    return verify.judge(_with_said(run.get("context") or _engine_request(run)), claim=claim, found=found,
                        criteria=(_rows_note(run) + "\n" if _rows_note(run) else "")
                        + _criteria(skill, run["values"], since=run.get("began")),
                        values=run["values"], steps=steps, live_png=evidence.live(),
                        reference_png=evidence.final_frame(skill), observed=evidence.observed(), screen_text=evidence.live_text(),
                        chosen=_chosen_for(run), purpose=purpose)


def _says_it_could_not(run, reply):
    """Whether the check reads the engine's report as "I could not do this, because ..." (one model call)."""

    try:
        verdict = _judge(run, reply)
    except Exception as exc:
        log(f"Skill {run['skill']['name']}: could not ask whether the report claims success: {exc}")
        return False
    if verdict["verdict"] != verify.CANNOT:
        return False
    run["_verdict"] = verdict
    return True


def _doubt_text(claim, verdict, on_screen=True):
    lead = {verify.NOT_DONE: "I do not think this is done.",
            verify.UNSURE: "I cannot confirm this is done."}.get(verdict["verdict"], "I cannot confirm this is done.")
    parts = [lead]
    if verdict.get("seen"):
        parts.append(f"{'The screen shows' if on_screen else 'What I wrote shows'}: {verdict['seen']}")
    if verdict.get("why"):
        parts.append(f"({verdict['why']})")
    head = " ".join(parts)
    # The report is shown whole (up to REPORT_CHARS): found live, a 600-character
    # cut ended at "ID_scan.png does not..." - the one finding the person being
    # asked most needed to read.
    report = (claim or "").strip()
    if len(report) > REPORT_CHARS:
        report = report[:REPORT_CHARS].rstrip() + " ..."
    return (f"{head}\n\nWhat was reported:\n{report}\n\nLook at {'the screen' if on_screen else 'the files'}. "
            "Reply `done` if it is done, `retry` "
            "to have it tried again, or tell me what is wrong - I will remember it for next time.")


def _record_verdict(run, ok, checked_by=None, helped=False, why="", mode=None, basis=()):
    """Note how a run ended on the saved skill (a fresh copy: the run's own may hold unproven changes). `basis`: the
    kinds of check that passed (_basis), of which only an independent one counts towards trust."""

    if run.get("one_off"):
        return
    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return
    spent = usage.snapshot()
    tokens = (spent.get("input_tokens", 0) + spent.get("output_tokens", 0)
              if "input_tokens" in spent or "output_tokens" in spent else None)
    experience.note_verdict(fresh, mode or ("model" if run.get("attempts") else "replay"), ok, checked_by=checked_by,
                            helped=helped, why=why, tokens=tokens, basis=basis)
    if ok:
        fresh["runs"] = fresh.get("runs", 0) + 1
    skills.save(fresh)


def _correct(run, text):
    """Keep what a person said was wrong, for every later run's model (and the check)."""

    # A secret reply is never kept as a correction (review round 2, B3); what is kept is withheld whole, then cut.
    if getattr(state, "reply_secret", False):
        return
    from decurix_agent.computer import privacy
    text = privacy.withhold_known(" ".join((text or "").split()))[:500]
    if run.get("one_off") or not text:
        return
    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return
    fresh["corrections"] = ((fresh.get("corrections") or []) + [{"at": int(time.time()), "text": text}])[-MAX_CORRECTIONS:]
    skills.save(fresh)
    run["skill"]["corrections"] = fresh["corrections"]
    # Copied to the control plane under the person who said it (R8): a recreated desktop still has it.
    messaging.post_learned("correction", fresh["name"], fresh.get("version", 1), "", text,
                           derived_from=state.reply_judgement)


# messaging's two gates (engine_questions._question_gate, final_gate._gate) are put in place by importing this module,
# as before the split.
messaging.set_query_gate(_question_gate)
messaging.set_final_gate(_gate)
