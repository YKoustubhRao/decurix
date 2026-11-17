"""
repairs.py - a stopped replay of a taught task is repaired one step at a time and goes on as a replay: a branch the
worker learned from an earlier repair is tried first (code), then the model is given that one stop to fix with a small
action budget, the harness checks the step's effect by code and resumes the person's steps from there. Only when the
budget is spent does the model get the rest of the run, as before.
"""

import copy
import json
import os
from pathlib import Path

from decurix_agent import progress, task_context
from decurix_agent.computer import branches, escalation, replay
from decurix_agent.logging_utils import log


# ============================================================
# Bounded repair: the model fixes the stop, the harness drives the loop
#
# Measured (2026-11-11, 26 curve runs): a replay that stopped handed the model the WHOLE rest of the run, and the model
# worked it click by click - 145 turns (6.3M tokens) for one new customer, 168 for one extra bill, 13-51 for a slow
# screen. The research (SkillDroid, AutoRPA) bounds the model to the step that failed and gives control back:
#
#   1. a branch first (branches.py): what an earlier repair of the same stop did, run by code through core.act, its
#      effect checked by code - the stopped step then goes through, or its missing result is on the screen;
#   2. else a REPAIR: the model is given that one stop (skill_task's phase "repair": a short guidance naming the step,
#      why it stopped, this item's values, the person's rules), with at most REPAIR_ACTIONS actions (counted from the
#      task's recording by the gui command, budget_refusal); when it reports, the harness looks:
#        before  (the step did not act) - the replay does the step itself from there, with all its checks;
#        after   (it acted, its result never came) - its recorded result must be on the screen now, read by code;
#        manual  (a click kept as a spot, left to a person) - the replay goes on from the next step, which must work;
#      and what the model pressed is compiled into a candidate branch, kept only when that check passed;
#   3. at most MAX_CONSECUTIVE repairs in a row that did not get past their step and MAX_TOTAL in a run; then the model
#      is given the rest of the run (today's takeover, which marks the run helped).
#
# A bounded repair marks only its step as needing help (run["repaired_ids"], the step's history in experience), never
# the whole run: the rest of the program still earns its evidence. A run with a repair or an unproven branch in it is
# never "verified" by the replay alone, so the final check always looks (finished_text, unverified).
# ============================================================

REPAIR_PHASE = "repair"
# A review before a commit (skill_task's parts driver) and its budget of actions: it looks, and enters only what this
# case needs that the taught steps did not.
REVIEW_PHASE = "review"
try:
    REVIEW_ACTIONS = max(10, int(os.environ.get("DECURIX_REVIEW_ACTIONS", "40")))
except ValueError:
    REVIEW_ACTIONS = 40
MAX_CONSECUTIVE = 2
MAX_TOTAL = 5
# A repair's actions: one stopped step made possible takes a few (look, pick, check). Measured in the learning curves
# (2026-11-17): at 30 the model filled the whole form around the step - 29 actions, too many to keep as a fix, so every
# later item stopped and was repaired again; a missing record is made by the run's own taught steps (skill_task.
# _make_missing), not a repair.
try:
    REPAIR_ACTIONS = max(3, int(os.environ.get("DECURIX_REPAIR_ACTIONS", "12")))
except ValueError:
    REPAIR_ACTIONS = 12
# The person's steps shown around the stopped one in a repair's guidance (the whole procedure is not this phase's).
CONTEXT_STEPS = 3
REPORT_CHARS = 600


def _norm(text):
    return " ".join(str(text or "").split())


def state(run):
    """The run's repair book: {"total", "consecutive", "pending", "job", "kept"}."""

    return run.setdefault("repair", {"total": 0, "consecutive": 0, "pending": None, "job": None, "kept": []})


def repairable(step, result):
    """Whether a replay's stop can be repaired one step at a time: a halt that can resume (not one whose commit may or
    may not have happened, an approval, or a private step a person types), or a click left to a person."""

    outcome = result.get("outcome")
    if outcome in (replay.HALTED, replay.HALTED_BEFORE_EFFECT):
        return result.get("resume_from") is not None and step is not None
    if outcome == replay.NEEDS_HUMAN and step is not None and step.get("kind") == "manual":
        what = _norm(step.get("by_hand")).lower()
        return bool(what) and what.startswith(("click", "double", "right", "press", "scroll"))
    return False


def _task():
    return task_context.current() or ""


def _events_now():
    """How many events the task's recording holds now (where a repair's own actions begin)."""

    from decurix_agent.computer import recording
    try:
        return int((recording.meta(recording.session_for_task(_task())) or {}).get("event_count") or 0)
    except Exception:  # noqa: BLE001 - no recording: nothing to compile later
        return 0


def _events():
    from decurix_agent.computer import recording
    try:
        return recording.load(recording.session_for_task(_task()))[1]
    except Exception:  # noqa: BLE001
        return []


def _inputs(run):
    """The text of the run's request and documents: what a branch's typed literal must not be copied from."""

    texts = [str(run.get("context") or "")]
    try:
        from decurix_agent import binding
        for d in list(run.get("documents") or []) + [str(p) for p in task_context.current_files() or []]:
            texts.append(str(binding._document_text(d) or ""))
    except Exception as exc:  # noqa: BLE001 - what cannot be read is compared with nothing (the request still is)
        log(f"Repairs: the run's documents were not read for the branch check: {exc}")
    return "\n".join(texts)


def _update_skill(run, change):
    """Apply `change(skill)` to the run's copy of its skill and to the saved one (the run's copy is saved whole by a
    success, the saved one by a failure: both must hold the branches). Never raises."""

    from decurix_agent.computer import skills
    out = change(run["skill"])
    if run.get("one_off"):
        return out
    try:
        fresh = skills.load(run["skill"]["name"])
        change(fresh)
        skills.save(fresh, actor="branch")
    except Exception as exc:  # noqa: BLE001 - kept on the run's copy, saved by its success
        log(f"Repairs: the saved skill was not updated: {exc}")
    return out


# ------------------------------------------------------------
# What happened to the last repair or branch: settled by the next replay result
# ------------------------------------------------------------

def settle(run, result):
    """
    Settle the pending repair or branch trial with the replay's next result: it passed when the replay got past the
    step it had to make possible (that step's own recorded result confirmed by code, when it has one), failed when it
    stopped there again. A repair that passed keeps its compiled candidate; a branch trial is counted (promoted or
    demoted, branches.note_trial). Returns "passed", "failed" or "" (nothing pending, or not decided yet).
    """

    book = state(run)
    pending = book.get("pending")
    if not pending:
        return ""
    must = pending["must_pass"]
    at = result.get("step")
    outcome = result.get("outcome")
    if outcome == replay.NEEDS_APPROVAL and at == must:
        return ""  # (the step waits for the person's yes: not yet decided)
    ladder = run.get("ladder")
    confirmed = not pending.get("has_expect") or (ladder is not None and pending.get("step_id") in ladder.confirmed)
    passed = (outcome in replay.SUCCESS or (at is not None and at > must)) and confirmed
    book["pending"] = None
    _conclude(run, pending, passed)
    return "passed" if passed else "failed"


def _conclude(run, pending, passed):
    book = state(run)
    task = _task()
    if pending["what"] == "repair":
        if passed:
            book["consecutive"] = 0
        candidate = pending.get("candidate")
        if passed and candidate:
            version = _update_skill(run, lambda s: branches.add_candidate(s, pending["shape"], candidate, task))
            if version:
                book["kept"].append(f"step {pending['n']}: kept what fixed it as a tool to try first next time "
                                    f"(v{version['v']}: {version['summary'][:160]})")
                progress.emit(f"Kept the fix for step {pending['n']} as a tool of my own, to try before the model next "
                              f"time the same stop comes: {version['summary'][:200]}")
        elif not passed:
            log(f"Repairs: the repair of step {pending['n']} did not get it through; nothing kept")
        return
    change = _update_skill(run, lambda s: branches.note_trial(s, pending["key"], pending["v"], passed, task))
    if passed:
        book["consecutive"] = 0
        progress.emit(f"Step {pending['n']}: the tool I learned for this stop worked (checked by code)"
                      + (" - it is now proven and used without the model" if change == branches.PROMOTED else "") + ".")
    else:
        progress.emit(f"Step {pending['n']}: the tool I learned for this stop did not get it through"
                      + (" - set aside; the model repairs it again" if change == branches.DEMOTED else "") + ".")


# ------------------------------------------------------------
# A stop
# ------------------------------------------------------------

def on_halt(run, program, values, result, *, execute, by_model, observe=None, make=None):
    """
    Handle a replay stop one step at a time. True: handled (a branch ran and the replay goes on, or the model was given
    the stop as a repair); False: not repairable or the budget is spent - the caller hands the rest to the model.

    `make(run, n, text)`: True when the stop is an entry missing from the application's list that the run's own taught
    steps make (skill_task._make_missing) - it is made by replay, and the replay goes on; no repair is spent.
    """

    steps = program.get("steps") or []
    n = result.get("step")
    step = steps[n - 1] if n and n <= len(steps) else None
    if make is not None and n and make(run, n, result.get("text")):
        return True
    if not repairable(step, result):
        return False
    book = state(run)
    found = branches.shape(step, result)
    k = branches.key(found)
    held_note = ""

    version = None if run.get("one_off") else branches.best(run["skill"], k)
    if version is not None and not book.get("pending"):
        ran = _try_branch(run, step, values, found, k, version, execute, observe, make=make)
        if ran is True:
            return True
        held_note = ran or ""

    if book["total"] >= MAX_TOTAL or book["consecutive"] >= MAX_CONSECUTIVE:
        log(f"Repairs: budget spent ({book['total']} repairs, {book['consecutive']} in a row without getting past "
            f"their step) - the model takes the rest")
        progress.emit(f"Step {n} stopped again after {book['total']} repairs in this run; the model takes the rest.")
        return False
    _start_repair(run, step, values, result, found, by_model, extra=held_note, observe=observe)
    return True


def _try_branch(run, step, values, found, k, version, execute, observe, make=None):
    """Run a kept branch for this stop. True when it ran and the replay went on; else a note for the model (why the
    learned fix did not settle it), "" for none."""

    n = step["n"]
    program, params = branches.program_for(version, values, step, run["skill"].get("name") or "")
    if program is None:
        return ""
    progress.emit(f"Step {n} stopped the way it did before; trying the fix I learned then (no model): "
                  f"{version.get('summary', '')[:200]}")
    ladder = escalation.Ladder(ask_model=None, goal=run["skill"].get("title") or "", total=len(program["steps"]))
    outcome = replay.run(program, params=params, start=1, ladder=ladder)
    if outcome["outcome"] == replay.NEEDS_APPROVAL:
        _update_skill(run, lambda s: branches.note_held(s, k, version["v"]))
        return ("The fix learned from an earlier repair stopped at a press the approval gate puts to the person: make "
                "that press yourself, and the gate will ask them. ")
    if outcome["outcome"] not in replay.SUCCESS and make is not None and make(run, n, outcome.get("text")):
        # (the fix stopped because the entry it picks is not there yet - not the fix's fault: it is made, and the
        # item is entered again)
        return True
    if outcome["outcome"] not in replay.SUCCESS:
        _conclude(run, {"what": "branch", "key": k, "v": version["v"], "n": n}, False)
        return (f"A fix learned from an earlier repair was tried and stopped ({_norm(outcome.get('text'))[:200]}); look "
                "at the screen as it is now. ")
    if version.get("status") != branches.PROMOTED:
        run.setdefault("repaired_ids", []).append(step.get("id") or n)  # (an unproven tool: its step is not yet proven)
        run["unproven_tool"] = True
    if found["when"] == "after":
        ok = not replay.unmet(step.get("expect") or [], (observe or _observe)(), values)
        _conclude(run, {"what": "branch", "key": k, "v": version["v"], "n": n}, ok)
        if not ok:
            return "A fix learned from an earlier repair ran, and the step's result is still not on the screen. "
        run["start"], run["resuming"] = n + 1, True
        execute(run)
        return True
    if found["when"] == "manual":
        # (a fix for a click kept as a spot did the entry that click began - what was typed into what it focused: the
        # replay goes on after it, as after a repair of it, and never types that value again)
        span = _focus_span(run, n) if step.get("point") else []
        must = (span or [n])[-1] + 1
        run.setdefault("unchecked_steps", []).extend([n] + span)
    else:
        must = n if found["when"] == "before" else n + 1
    nxt = (run.get("expanded") or {}).get("steps") or []
    must_step = nxt[must - 1] if must <= len(nxt) else step
    state(run)["pending"] = {"what": "branch", "key": k, "v": version["v"], "n": n, "must_pass": must,
                             "step_id": must_step.get("id") or must, "has_expect": bool(must_step.get("expect"))}
    run["start"], run["resuming"] = must, True
    execute(run)
    return True


def _observe():
    from decurix_agent.computer import core
    return core.observe()


# What follows a click kept only as a spot and acts on what that click focused: typing into "the focused field", and an
# Enter or Tab that confirms it. They are one entry with the click (choose a party: click the list, type, Enter).
_FOCUS_KEYS = {"return", "enter", "kp_enter", "tab"}
MAX_FOCUS_SPAN = 3


def _focus_span(run, n):
    """The numbers of the steps right after step n that act on what step n's click focused (none for other steps)."""

    steps = (run.get("expanded") or {}).get("steps") or []
    span = []
    for k in range(n + 1, min(n + MAX_FOCUS_SPAN, len(steps)) + 1):
        s = steps[k - 1]
        keys = {"".join(str(x).lower().split()) for x in s.get("keys") or []}
        if s.get("kind") == "type" and not _norm((s.get("target") or {}).get("name")):
            span.append(k)
        elif s.get("kind") == "key" and keys and keys <= _FOCUS_KEYS and span:
            span.append(k)
        else:
            break
    return span


def _start_repair(run, step, values, result, found, by_model, extra="", observe=None):
    book = state(run)
    n = step["n"]
    # A click kept only as a spot, and what was typed into what it focused, are repaired as one entry: the replay never
    # types that value again into whatever has the keyboard after the repair (found live, 2026-11-16: the party went
    # into the date field). Only a spot click's: a named control's step is found by its name.
    span = _focus_span(run, n) if found["when"] == "manual" and step.get("point") else []
    steps = (run.get("expanded") or {}).get("steps") or []
    typed = {steps[k - 1]["param"]: values.get(steps[k - 1]["param"]) for k in span
             if steps[k - 1].get("param") and values.get(steps[k - 1]["param"])}
    # Whether the step's own recorded result was missing when it stopped: if the model then does the step itself, its
    # result appears, and the replay goes on after it rather than pressing it twice (after_repair).
    missing_at_halt = False
    if found["when"] == "before" and step.get("expect") and step.get("kind") not in ("type", "select"):
        try:
            missing_at_halt = bool(replay.unmet(step["expect"], (observe or _observe)(), values))
        except Exception:  # noqa: BLE001 - not looked at: the replay does the step after the repair
            missing_at_halt = False
    book["total"] += 1
    book["consecutive"] += 1
    run.setdefault("repaired_ids", []).append(step.get("id") or n)
    book["job"] = {"n": n, "step": copy.deepcopy(step), "shape": found, "values": dict(values),
                   "result": {k: result.get(k) for k in ("outcome", "step", "resume_from", "reason", "evidence", "text",
                                                          "options")},
                   "events_from": _events_now(), "extra": extra, "missing_at_halt": missing_at_halt,
                   "span": span, "span_values": typed}
    reason = _norm(result.get("reason") or result.get("text"))[:400]
    progress.emit(f"The replay stopped at step {n} ({_norm(step.get('summary'))[:120]}): {reason}. The model is given "
                  f"this one step to fix (at most {REPAIR_ACTIONS} actions); the taught steps carry on after it.")
    by_model(run, from_step=n, note=reason, phase=REPAIR_PHASE)


def phase_extra(run):
    """What the repair phase's note carries for the gui command (budget_refusal): where its actions start, its budget."""

    job = state(run).get("job") or {}
    return {"events_from": int(job.get("events_from") or 0), "budget": REPAIR_ACTIONS, "step": job.get("n")}


def budget_refusal(task_id=None):
    """
    Why the gui command refuses an action now: the task is in a repair (phase.py, "repair") and the actions recorded
    since it began have reached its budget. None otherwise. Decided from the recording's own count, never from what
    the action is.
    """

    from decurix_agent.computer import phase, recording
    noted = phase.read()
    task_id = task_id if task_id is not None else _task()
    if noted.get("phase") not in (REPAIR_PHASE, REVIEW_PHASE) or noted.get("task") != task_id:
        return None
    try:
        count = int((recording.meta(recording.session_for_task(task_id)) or {}).get("event_count") or 0)
    except Exception:  # noqa: BLE001 - a count that cannot be read refuses nothing
        return None
    used = count - int(noted.get("events_from") or 0)
    budget = int(noted.get("budget") or REPAIR_ACTIONS)
    if used < budget:
        return None
    if noted.get("phase") == REVIEW_PHASE:
        return (f"this check before the commit has used its {budget} actions - report now what is entered and what is "
                "still missing (HELP for anything not done); nothing more is yours to do.")
    return (f"this repair of step {noted.get('step')} has used its {budget} actions - report now: DONE if the step can "
            "go on, HELP with what stops it. The taught steps carry on after you; nothing more is yours to do.")


# ------------------------------------------------------------
# The model's report on a repair
# ------------------------------------------------------------

def after_repair(run, reply, kind, *, execute, by_model, takeover, observe=None):
    """
    The model reported on a repair (skill_task._after_phase): the harness checks the step's effect by code and resumes
    the replay, or repairs again within the budget, or hands the rest to the model (`takeover(run, n, note)`).
    True: handled.
    """

    book = state(run)
    job = book.get("job")
    if not job:
        return False
    book["job"] = None
    step, n, found, values = job["step"], job["n"], job["shape"], job["values"]
    report = _norm(reply)[:REPORT_CHARS]
    run.setdefault("repair_reports", []).append(
        f"Step {n} ({_norm(step.get('summary'))[:100]}) stopped - {_norm(job['result'].get('reason'))[:200]} - and the "
        f"model fixed it ({'reported: ' + report if kind == 'final' else 'it ended with ' + kind + ': ' + report})")

    if kind != "final":
        takeover(run, n, f"A repair of step {n} ended with {kind} ({report[:300]}). {job['result'].get('reason') or ''}")
        return True

    try:
        candidate, why = branches.compile_candidate(run["skill"], step, values, _events(), inputs=_inputs(run),
                                                    since=job["events_from"])
    except Exception as exc:  # noqa: BLE001 - no tool from this repair; the run goes on
        candidate, why = None, f"it could not be compiled: {exc}"
    if candidate is None:
        log(f"Repairs: nothing kept from the repair of step {n}: {why}")

    look = observe or _observe
    if found["when"] == "after":
        try:
            unmet = replay.unmet(step.get("expect") or [], look(), values)
        except Exception as exc:  # noqa: BLE001 - a look that failed checked nothing
            unmet = [{"type": "look", "why": str(exc)}]
        pending = {"what": "repair", "n": n, "must_pass": n, "candidate": candidate, "shape": found}
        if not unmet:
            _conclude(run, pending, True)
            run["start"], run["resuming"] = n + 1, True
            execute(run)
            return True
        if book["total"] >= MAX_TOTAL or book["consecutive"] >= MAX_CONSECUTIVE:
            takeover(run, n, f"Step {n} ran and its result - {', '.join(replay.describe_check(c) for c in unmet if 'role' in c or 'value' in c)[:300]} - "
                             "is still not on the screen after the repairs this run allows.")
            return True
        _start_repair(run, step, values, job["result"], found, by_model,
                      extra=f"Your last repair reported \"{report[:200]}\", and the step's result is still not on the "
                            "screen. ")
        return True

    if found["when"] == "manual":
        # (the click, and what was typed into what it focused, were the repair's: the replay goes on after them)
        must = (job.get("span") or [n])[-1] + 1
        run["start"] = must
        run.setdefault("unchecked_steps", []).extend([n] + list(job.get("span") or []))
    else:
        must = n
        run["start"] = n
        if job.get("missing_at_halt"):
            # The model did the step itself after all (its recorded result, missing when it stopped, is on the screen
            # now): the replay goes on after it, never presses it a second time.
            try:
                if not replay.unmet(step["expect"], look(), values):
                    run["start"] = must = n + 1
            except Exception:  # noqa: BLE001 - not looked at: the replay does the step
                pass
    nxt = (run.get("expanded") or {}).get("steps") or []
    must_step = nxt[must - 1] if must <= len(nxt) else step
    book["pending"] = {"what": "repair", "n": n, "must_pass": must, "candidate": candidate, "shape": found,
                       "step_id": must_step.get("id") or must, "has_expect": bool(must_step.get("expect"))}
    run["resuming"] = True
    execute(run)
    return True


# ------------------------------------------------------------
# What the model is told for a repair, and what the run says after
# ------------------------------------------------------------

def job(run):
    """The repair in hand ({"n", "step", "shape", ...}) or None."""

    return state(run).get("job")


def steps_around(run, n):
    """The written-out program's steps around step n, as "N. summary" lines (the repair's context)."""

    steps = (run.get("expanded") or {}).get("steps") or []
    lo, hi = max(1, n - CONTEXT_STEPS), min(len(steps), n + CONTEXT_STEPS)
    return [f"{s['n']}. {_norm(s.get('summary') or s.get('kind'))}" + ("   <- STOPPED HERE" if s["n"] == n else "")
            for s in steps[lo - 1:hi]]


def how_lines(run):
    """How the model works a repair (skill_task._procedure for the repair phase)."""

    current = job(run) or {}
    n, step, found = current.get("n"), current.get("step") or {}, current.get("shape") or {}
    reason = _norm((current.get("result") or {}).get("reason"))[:400]
    value = (current.get("values") or {}).get(step.get("param")) if step.get("param") else None
    what = _norm(step.get("summary"))[:160]
    lines = ["How to work - THIS REPAIR ONLY:",
             f"- The replay of the person's steps is running and stopped at step {n} ({what}): {reason}. "
             + (current.get("extra") or "")]
    if found.get("when") == "after":
        lines.append(f"- Step {n} WAS done, and what it should leave on the screen never appeared. Look (gui observe). "
                     "If the screen shows the step did its job, change nothing and report DONE. Otherwise bring about "
                     "only what that step was for. Never press a save, submit or send a second time.")
    elif found.get("when") == "manual" and current.get("span"):
        span, typed = current["span"], current.get("span_values") or {}
        said = ", ".join(json.dumps(v, ensure_ascii=False) for v in typed.values()) or "this run's value"
        lines.append(f"- Step {n} is a click the recording kept only as a spot on the screen, and steps "
                     f"{', '.join(str(k) for k in span)} entered {said} into what that click focused (one entry: a list "
                     f"chosen, or a field filled). Look (gui observe), find the control that is meant and put {said} "
                     f"in it - choose it from its list or type it - that one entry, nothing else; then report DONE. The "
                     f"replay goes on after step {span[-1]}, so nothing of it is typed again.")
    elif found.get("when") == "manual":
        lines.append(f"- Step {n} is a click the recording kept only as a spot on the screen. Look (gui observe), find "
                     "the control that is meant and click it - that one action - then report DONE.")
    else:
        lines.append(f"- Make step {n} possible and leave the screen where it can be done"
                     + (f" with this run's value {value!r}" if value else "") + ": for example, make the entry an "
                     "application's list lacks the way the person's steps make one, close what is in the way, or bring "
                     f"the form back. Do NOT do step {n} itself and do not go on to later steps: when you report DONE "
                     f"the replay does step {n} with this run's value and carries on, with every check. If the control is "
                     "there now and nothing needs doing, report DONE at once.")
    lines += [f"- You have at most {REPAIR_ACTIONS} actions; the gui command refuses any more. Every press is judged by "
              "the approval gate as always.",
              "- If this cannot be fixed from the screen and the documents, or it needs a value nothing gives, report "
              "HELP with why - never guess a value and never enter a record the replay will enter.",
              "- Finish with DONE and one line per thing you did (what you pressed or made, and its name)."]
    return lines


def request_text(run, title, name):
    """The engine's request for a repair."""

    current = job(run) or {}
    return (f"Fix ONE stopped step of the task you were taught, \"{title}\" (skill {name}): the replay of the person's "
            f"steps stopped at step {current.get('n')}; make it possible to go on, then report - the taught steps do "
            "the rest")


def finished_text(run):
    """What a run that had repairs says beside its result, for the final check and the report ("" when none)."""

    lines = list(run.get("repair_reports") or [])
    lines += list(state(run).get("kept") or [])
    if run.get("unchecked_steps"):
        lines.append("Not checked by the replay (a click the model placed for a step kept as a spot): step "
                     + ", ".join(str(n) for n in run["unchecked_steps"]) + ".")
    if not lines:
        return ""
    return "\n\nRepairs during the replay (the model fixed one step each; the person's steps did the rest):\n- " \
        + "\n- ".join(lines)


def unverified(run):
    """Whether a run's replay result may not stand as verified by the screen alone: a step was repaired by the model, a
    learned tool not yet proven ran, or a step went unchecked. The final check then always looks."""

    return bool(run.get("repaired_ids") or run.get("unproven_tool") or run.get("unchecked_steps"))
