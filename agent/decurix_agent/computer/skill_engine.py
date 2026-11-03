import json
import time

from decurix_agent import config, filelock, llm_client, task_context
from decurix_agent.computer import escalation, core, learning, replay, skills
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log

# Which skills each task had replayed to their end (so a replay is never run twice for one request).
REPLAYED_DIR = config.DECURIX_DIR / "computer" / "replayed"
KEEP_REPLAYED = 60


# ============================================================
# Skills, for an engine
#
# What an engine (Ollama, TheBot, Claude) is given to find and run a skill the
# desktop was taught (computer/learning.py): `gui skills` / `gui skill` / `gui
# do`, and the same as MCP tools. A person teaches and corrects a skill in the
# chat (skill_task.py); an engine has no such channel of its own, so where
# skill_task asks a person, this refuses in words the engine can act on - it
# says what is missing, and the engine raises it with the human the way it
# raises anything else, then runs again with the answer.
#
# A run goes through the same replay.run, and so the same approval gate and
# audit trail, as everything else. It never rewrites a skill: a correction is
# something a person makes, in the chat, and a run outside the chat has no one
# to make it.
# ============================================================


def summary_line(skill):
    """One line for a skill in a list."""

    needs = ", ".join(p["name"] for p in skills.required(skill))
    said = skills.explanation(skill)
    summary = (skill.get("understanding") or {}).get("summary")
    where = ", ".join(skills.places(skill))
    return (f"{skill['name']} {el._quote(skill.get('title') or '', 70)} - {len(skill['steps'])} steps, run {skill.get('runs', 0)} times"
            + (f"; works in {where}" if where else "")
            + (f"; needs {needs}" if needs else "")
            + (f"; what it is: {el._quote(summary, 200)}" if summary else "")
            + (f"; the person who taught it said: {el._quote(said, 160)}" if said else ""))


def render_text(skill, budget=3500):
    """A skill as an engine reads it: what it needs, what it does, and how to run it."""

    lines = [f"Skill {skill['name']} {el._quote(skill.get('title') or '', 80)}: {len(skill['steps'])} steps, "
             f"learned from {skill.get('demos', 1)} demonstration(s), run {skill.get('runs', 0)} times."]

    where = skills.places(skill)
    if where:
        lines.append(f"Works in: {', '.join(where)} - it does its task there and nowhere else; a request "
                     "for the same kind of work in another app or site is not this skill.")

    said = skills.explanation(skill)
    if said:
        lines.append("The person who taught it said: " + el._quote(said, 600)
                     + " - that is how they want this task done; follow it.")

    understood = learning.understood_text(skill)
    if understood:
        lines.append(understood)
    for correction in skill.get("corrections") or []:
        lines.append(f"A correction after an earlier run: {correction['text']}")

    needed = skills.required(skill)
    if needed:
        lines.append("Needs: " + ", ".join(
            f"{p['name']} ({p['label']}" + ("; a list, items separated by ';'" if p.get("list") else "") + ")" for p in needed))

    gated = sum(1 for step in skill["steps"] if step.get("consequential"))
    if gated:
        lines.append(f"{gated} step(s) stop for human approval every run.")

    args = "".join(f' --param {p["name"]}="' + ("ITEM 1; ITEM 2; ..." if p.get("list") else "...") + '"'
                   for p in needed)
    footer = f"Run it with no model: gui do {skill['name']}{args} [--from N]"
    if any(p.get("list") for p in needed):
        # Found live: an engine read a two-line PO and ran the skill with the
        # first line only, twice. A list is every item, in one run.
        footer += ("\nA list value is every item of the document, in its order, all lists the same length - "
                   "one run enters them all; never give only the first item.")

    lines.append(learning.overview(skill, budget=max(200, budget - sum(len(x) + 1 for x in lines) - len(footer) - 20)))
    lines.append(footer)

    return "\n".join(lines)


def _refusal(skill, reason, missing=()):
    """A run that did not start, shaped like replay.run's answer so callers print it the same way."""

    return {
        "outcome": replay.HALTED_BEFORE_EFFECT,
        "text": f"Did not run {el._quote(skill.get('title') or skill['name'], 80)}: {reason}",
        "reason": reason,
        "step": None,
        "resume_from": None,
        "missing": list(missing),
        "done": 0, "verified": 0, "unverified": 0, "skipped": 0,
        "total": len(skill["steps"]),
        "session": f"skill-{skill['name']}",
    }


FITS_SYSTEM = (
    "You decide whether a taught procedure can do a request. The procedure was demonstrated in one app or "
    "site (where it works) and replays clicks there. The same kind of work asked for in a DIFFERENT app, "
    "site or system (another desktop app, another web app, one the request names or says is open) is not this "
    "procedure's to do. Reply with JSON only: {\"same_system\": true|false, \"requested_system\": \"<the "
    "app or site the request is for, or empty>\"}. When the request names no system, it is the same."
)


def other_system(skill, request):
    """
    The app or site the request is for, when that is not where the skill works;
    "" when it is, when it cannot be told, or with no hosted model to ask.
    A procedure demonstrated in one application must not open it over the
    application requested by the user and save the result in the wrong place.
    """

    request = (request or "").strip()
    where = skills.places(skill)
    if not request or not where or not llm_client.available():
        return ""
    prompt = (f"Procedure: {skill.get('title') or skill['name']}\nWorks in: {', '.join(where)}\n"
              f"What it does: {(skills.explanation(skill) or skill.get('goal') or '')[:400]}\n\nRequest:\n{request[:1500]}")
    try:
        message, _ = llm_client.chat([{"role": "system", "content": FITS_SYSTEM}, {"role": "user", "content": prompt}],
                                     max_tokens=200, response_format={"type": "json_object"})
        answer = json.loads(message.get("content") or "{}")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"skill_engine: could not tell which system {skill['name']} is for: {exc}")
        return ""
    if answer.get("same_system") is False:
        return str(answer.get("requested_system") or "another app").strip()[:80] or "another app"
    return ""


def run(skill, params=None, start=1, source_files=None):
    """
    Run a ready skill with no model and say how it ended, in replay.run's shape.
    A skill that is not ready, is missing a value the recording could not
    supply, or is not where it began (when a person asked for that to be
    checked) does nothing and says why. One run of a skill at a time per task
    (or outside any task): a second, started while the first runs, is refused -
    its check of what was already done would pass before the first noted it.
    Found in review (2026-11-03, D-30).
    """

    REPLAYED_DIR.mkdir(parents=True, exist_ok=True)
    lock_path = REPLAYED_DIR / f".lock-{_scope()}-{skill.get('name') or 'skill'}"
    try:
        handle = open(lock_path, "a+")
    except OSError as exc:
        return _refusal(skill, f"its run lock could not be opened ({type(exc).__name__}), so it is not run")
    try:
        if not filelock.try_exclusive(handle):
            return _refusal(skill, "another run of it is going on now - wait for that one to end, then look at "
                                   "what it did before running it again")
        return _run(skill, params, start, source_files)
    finally:
        handle.close()


def _run(skill, params=None, start=1, source_files=None):
    """run(), under its lock."""

    if skill.get("status") != skills.READY:
        return _refusal(skill, "it was never finished being taught - a person confirms a skill in the chat with /learn")
    if source_files is not None:
        try:
            skill = skills.bind_sources(skill, source_files)
        except ValueError as exc:
            return _refusal(skill, str(exc))

    # A task that was handed over as this very skill is not asked whether it
    # belongs to it. Found live on every run: the skill's places were window
    # titles ("Desktop", "New record - new-record-x"), the request named the
    # application, and the model said they were different systems.
    from decurix_agent import guidance
    elsewhere = "" if guidance.handed(skill["name"]) else other_system(skill, task_context.current_request())
    if elsewhere:
        return _refusal(skill, f"this request is for {elsewhere}, and the skill works in "
                               f"{', '.join(skills.places(skill)) or 'another app'} - do the task in {elsewhere} with gui "
                               "instead (the skill's steps show what the work involves)")

    values = skills.initial_values(skill, params)

    # This run's own program: its number of rows, and its own choices from the application's lists
    # (skills.fit). It used to be refused whenever it chose another entry than the demonstration did.
    count, _ = skills.run_rows(skill, values)
    absent = set()
    if count:
        absent = {name for row in (skills.repeated_rows(skill) or {"rows": []})["rows"][count:] for name in row}

    missing = [p for p in skills.needed_from(skill, start) if not values.get(p["name"]) and p["name"] not in absent]
    if missing:
        # Given, and empty: this run has nothing to put there (a document that leaves a field out). That is not a
        # value to ask a person for - found live: "ask the human" for a number the document said did not exist.
        blank = [p for p in missing if values.get(p["name"]) == "" and p["name"] in values]
        if blank and len(blank) == len(missing):
            names = ", ".join(f"{p['name']} ({p['label']})" for p in blank)
            return _refusal(skill, f"{names} was given empty, and a replay cannot leave a field out. If this run really "
                                   "has nothing for it, do the steps yourself with gui, following the skill's steps, and "
                                   "leave that field as the application has it; if you do not know, ask the human")
        wanted = ", ".join(f"{p['name']} ({p['label']})" for p in missing)
        return _refusal(skill, f"it needs {wanted} - ask the human for the value, then pass it with --param", [p["name"] for p in missing])

    problem = skills.list_problem(skill, values)
    if problem:
        return _refusal(skill, problem, [p["name"] for p in skills.list_params(skill)])

    checks = learning.start_checks(skill) if start == 1 else []
    if checks:
        try:
            observation = core.observe()
        except ComputerError as exc:
            return _refusal(skill, f"the screen could not be read to check where to begin: {exc}")

        if replay.unmet(checks, observation):
            return _refusal(skill, f"it begins from {', '.join(replay.describe_check(c) for c in checks)}, but "
                                   f"{replay._screen_brief(observation)}. Put the screen where it begins, or ask the human")

    # A replay that ran to its end did the task - it saves, submits or sends at its end. Run again for the same
    # request from before its end, it does it a second time. Found live (2026-10-14): sent back over one value of a
    # record the replay had saved, the engine ran `gui do` from step 1 and a second employee record was made.
    done_to = _replayed_to(skill["name"])
    if done_to and start <= done_to:
        return _refusal(skill, f"this request already had it replayed to its end (step {done_to}) - the task is done, "
                               "and replaying it again from step " + str(start) + " would do it a second time. Open "
                               "what it made and correct that by hand with gui observe / click / type / select")

    # The engine running this is itself the model, and there is no person to
    # ask - so only healing's own rung: a control that has plainly been
    # renamed is found, and anything less halts as it always did. Nothing is
    # kept from it: a skill is rewritten by a person's correction or by a run
    # skill_task did, never by an engine's.
    # A run of it this request that stopped part way did the steps before its stop: it carries on from there, never
    # from an earlier step (review 2026-11-02: only a run that reached its end was noted, so a run halted after its
    # save could be started again from step 1 and save a second record).
    stopped = _stopped_at(skill["name"])
    if stopped:
        at, how = stopped["at"], stopped.get("outcome")
        if how == replay.RECONCILIATION_REQUIRED and start <= at:
            return _refusal(skill, f"an earlier run of it for this request did step {at} and its result never "
                                   "appeared - it may or may not have taken effect, so no step up to it is done again. "
                                   "Look at the screen (gui observe) and finish or correct the work by hand, or ask "
                                   "the human")
        if start < at:
            return _refusal(skill, f"an earlier run of it for this request did steps up to {at - 1} and stopped at "
                                   f"step {at}; running it from step {start} would do steps {start}-{at - 1} a second "
                                   f"time. Put right what stopped it, then carry on with gui do {skill['name']} "
                                   f"--from {at} (with the same --param values)")

    program, expanded = skills.expand(skill, values)
    result = replay.run(program, params=expanded, start=start,
                        ladder=escalation.Ladder(ask_model=None, goal=skill.get("title") or "", total=len(program["steps"])))

    if result["outcome"] in replay.SUCCESS:
        # Ran to its end (seen or not): it is never run again for this request. Only a run whose every result was seen
        # counts as a good run of the skill.
        if result["outcome"] == replay.VERIFIED:
            skills.note_run(skill["name"])
        _note_replayed(skill["name"], len(program["steps"]))
    elif result["outcome"] != replay.HALTED_BEFORE_EFFECT or start > 1:
        at = result.get("resume_from") or result.get("step") or start
        _note_stopped(skill["name"], int(at), result["outcome"])

    return result


# Outside any task, what was replayed counts for this long (a run from the shell, a test, an engine with no task):
# before, nothing was kept there and a second run did the work again (D-30).
NO_TASK_SECONDS = 3600


def _scope():
    """The task this run is for, or "no-task"."""

    return task_context.current() or "no-task"


def _replayed_path():
    return REPLAYED_DIR / f"{_scope()}.json"


def _read_replayed(path):
    """What a run's note file holds; outside any task, an old note no longer counts."""

    done = json.loads(path.read_text(encoding="utf-8"))
    if path.stem == "no-task" and time.time() - float(done.get("#at") or 0) > NO_TASK_SECONDS:
        return {}
    return done


def _replayed_to(name):
    """The last step of `name` this task already had replayed to the end, or 0."""

    path = _replayed_path()
    try:
        return int(_read_replayed(path).get(name) or 0) if path else 0
    except (OSError, ValueError, TypeError):
        return 0


def _stopped_at(name):
    """Where a run of `name` for this task stopped part way: {"at": the step it carries on from, "outcome"}, or None."""

    path = _replayed_path()
    try:
        kept = _read_replayed(path).get(f"{name}#stopped") if path else None
        return {"at": int(kept["at"]), "outcome": kept.get("outcome")} if kept else None
    except (OSError, ValueError, TypeError, KeyError):
        return None


def _note_stopped(name, at, outcome):
    """Keep where a run of `name` stopped part way, so the next run carries on from there (or refuses)."""

    _note_replayed(name, None, stopped={"at": at, "outcome": outcome})


def _note_replayed(name, last, stopped=None):
    path = _replayed_path()
    if path is None:
        return
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        done = _read_replayed(path) if path.exists() else {}
        done["#at"] = time.time()
        if stopped is not None:
            done[f"{name}#stopped"] = stopped
        else:
            done[name] = last
            done.pop(f"{name}#stopped", None)
        path.write_text(json.dumps(done), encoding="utf-8")
        for old in sorted(REPLAYED_DIR.glob("*.json"), key=lambda p: p.stat().st_mtime)[:-KEEP_REPLAYED]:
            old.unlink()
    except (OSError, ValueError):
        pass
