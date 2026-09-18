import re
import traceback

from flowera_agent import state, usage
from flowera_agent.computer import approval, core, learning, recording, replay, skills
from flowera_agent.computer import elements as el
from flowera_agent.computer.errors import ComputerError
from flowera_agent.logging_utils import log
from flowera_agent.messaging import ask_for_query, write_final_reply
from flowera_agent.replay_task import OPTION_RE, STOP_RE


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

LEARN_ENGINE = "learn"
SKILL_ENGINE = "skill"

LEARN_RE = re.compile(r"^/learn(?:\s+(.*))?$", re.DOTALL)
SKILL_RE = re.compile(r"^/skills?(?:\s+(.*))?$", re.DOTALL)

ABANDON_RE = re.compile(r"^\W*(cancel|abort|stop|quit|never ?mind)\b", re.IGNORECASE)
RETRY_RE = re.compile(r"^\W*(retry|again|try again|look again)\b", re.IGNORECASE)
CARRY_ON_RE = re.compile(
    r"^\W*(skip|leave (?:it )?out|continue|carry on|go on|move on|done|it worked|i did (?:it|that)|looks (?:right|fine|good))\b",
    re.IGNORECASE,
)
USE_RE = re.compile(r"^\W*(?:use|try|click|press|select|choose)\b\s*(.+?)\s*$", re.IGNORECASE | re.DOTALL)

MAX_STUCK_ASKS = 3


class _Refused(Exception):
    """A request that cannot be carried out, in words a person can act on."""


# ============================================================
# Parsing
# ============================================================

def parse_learn(content):
    """([recording ids], name) of a "/learn" request, or None for any other request. No IDs means the latest demonstration."""

    match = LEARN_RE.match((content or "").strip())
    if not match:
        return None

    ids, name = [], None

    for word in (match.group(1) or "").split():
        if word.startswith("name="):
            name = word[len("name="):].strip("\"'")
        else:
            ids.append(word)

    return ids, name


def parse_skill(content):
    """(what was asked for, {param: value}) of a "/skill" request, or None. Nothing asked for lists the skills."""

    match = SKILL_RE.match((content or "").strip())
    if not match:
        return None

    rest = match.group(1) or ""
    params = {}

    for option in OPTION_RE.finditer(rest):
        params[option.group(1)] = next(v for v in option.groups()[1:] if v is not None)

    query = OPTION_RE.sub("", rest).strip()
    return ("" if query.lower() == "list" else query), params


def request_text(name, params=None):
    """
    The "/skill" request that runs `name` with `params` - what the control plane
    sends and the chat shows. Values are read back by parse_skill, so one that
    cannot be written into a request (it holds both kinds of quote) is refused
    rather than bent. A new line is the same as ";" in a list, and is written
    as one.
    """

    parts = [f"/skill {name}"]

    for key in sorted(params or {}):
        if not re.fullmatch(r"[A-Za-z_]\w*", key):
            raise ValueError(f"not a parameter name: {key!r}")

        value = ";".join(line.strip() for line in str(params[key]).splitlines() if line.strip())
        if not value:
            continue

        if '"' not in value:
            parts.append(f'{key}="{value}"')
        elif "'" not in value:
            parts.append(f"{key}='{value}'")
        else:
            raise ValueError(f"the value of {key} has both kinds of quote in it")

    return " ".join(parts)


def handles(content):
    """Whether a request is one of this module's ("/learn", "/skill"), which need no reasoning backend."""

    return parse_learn(content) is not None or parse_skill(content) is not None


def _begin(content, task_file):
    state.current_task = content
    state.current_task_file = task_file
    state.waiting_for_query = False
    state.paused_skill = None

    # Known, not unknown: nothing here reads a prompt or costs anything.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)


def perform(content, task_file=None):
    """Run a "/learn" or "/skill" request as the task the watcher dispatched it as."""

    _begin(content, task_file)

    try:
        if parse_learn(content) is not None:
            _learn(*parse_learn(content))
        else:
            _run_request(*parse_skill(content))
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
        else:
            _run_reply(paused, reply, approval_outcome)
    except _Refused as refusal:
        write_final_reply(str(refusal), kind="error", engine=engine)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=engine)


# ============================================================
# Learning
# ============================================================

def _sources(ids):
    """[(meta, events)] for the recordings named, or the latest finished demonstration when none is."""

    if not ids:
        latest = next((m for m in recording.sessions()
                       if m.get("kind") == "demonstration" and m.get("status") == "complete" and m.get("event_count")), None)
        if latest is None:
            raise _Refused("There is no finished demonstration on this desktop to learn from. Record one, then send "
                           "`/learn <recording>`.")
        ids = [latest["id"]]

    sources = []
    for session_id in ids:
        try:
            meta, events = recording.load(session_id)
        except (FileNotFoundError, ValueError):
            raise _Refused(f"There is no recording {session_id} on this desktop.")

        if meta.get("status") == "recording":
            raise _Refused(f"{session_id} is still being recorded - end it first, then teach it.")

        sources.append((meta, events))

    return sources


def _learn(ids, name):
    sources = _sources(ids)
    draft = skills.draft_for([meta["id"] for meta, _ in sources])

    if draft is not None:
        log(f"Continuing the unfinished skill {draft['name']}")
        return _ask(draft, "Picking up where we left off.")

    try:
        skill = learning.build(sources, name=name)
    except ValueError as exc:
        raise _Refused(f"I cannot learn from that: {exc}.")

    _ask(skill)


def _ask(skill, preface="", question=None):
    """Save the draft and put its next question to the person."""

    question = question or learning.next_question(skill)
    state.paused_skill = {"mode": "learn", "skill": skill, "question": question}

    skills.save(skill)
    ask_for_query(f"{preface} {question['text']}".strip(), kind="query", engine=LEARN_ENGINE)


def _learn_reply(paused, reply):
    skill, question = paused["skill"], paused["question"]

    if ABANDON_RE.match(reply or ""):
        skills.save(skill)
        write_final_reply(
            f"Stopped. What you have told me about \"{skill['title']}\" is kept - send `/learn {skill['sources'][0]}` "
            "to carry on where we left off.", kind="final", engine=LEARN_ENGINE)
        return

    result = learning.answer(skill, question, reply)

    if not result["understood"]:
        _ask(skill, result["note"], question=question)
        return

    decision = result["decision"]

    if decision == "save":
        skills.save(skill)
        write_final_reply(_learned_text(skill), kind="final", engine=LEARN_ENGINE)
    elif decision == "discard":
        skills.delete(skill["name"])
        write_final_reply(f"Threw away what I had learned about \"{skill['title']}\". The recording is untouched.",
                          kind="final", engine=LEARN_ENGINE)
    elif decision == "hold":
        skills.save(skill)
        write_final_reply(f"I could not tell whether to save \"{skill['title']}\", so I left it unfinished. "
                          f"Send `/learn {skill['sources'][0]}` to answer that last question.",
                          kind="final", engine=LEARN_ENGINE)
    else:
        _ask(skill, result["note"])


def _learned_text(skill):
    params = "".join(f' {p["name"]}="{"a;b;c" if p.get("list") else "..."}"' for p in skills.required(skill))
    gated = sum(1 for s in skill["steps"] if s.get("consequential"))

    text = f"Learned \"{skill['title']}\" as `{skill['name']}`: {len(skill['steps'])} steps. Run it with `/skill {skill['name']}{params}`."

    if gated:
        text += f" It will stop for your approval before {gated} of them, every time."

    return text + " If it gets stuck or is unsure it will ask you, and remember a correction that works."


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


def _run_request(query, given):
    if not query:
        write_final_reply(_listing(), kind="final", engine=SKILL_ENGINE)
        return

    skill, candidates = skills.find(query)

    if skill is None:
        if candidates:
            names = ", ".join(f"`{s['name']}`" for s in candidates)
            raise _Refused(f"More than one skill fits \"{query}\": {names}. Say which, by its name.")
        raise _Refused(f"I do not know a skill like \"{query}\". " + _listing())

    run = {"skill": skill, "values": skills.initial_values(skill, given), "start": 1, "checked": False, "patches": [], "asked": 0, "stuck": 0}
    _prepare(run)


def _pause(run, kind, text, ask_kind="query", **extra):
    state.paused_skill = dict({"mode": "run", "kind": kind, "run": run}, **extra)
    ask_for_query(text, kind=ask_kind, engine=SKILL_ENGINE)


def _prepare(run):
    """What must be settled before the first step: every value a run needs, and where the screen is."""

    skill = run["skill"]

    missing = [p for p in skills.required(skill) if not run["values"].get(p["name"])]

    # Lists that do not line up are asked for again, whole - which item a
    # value belongs to is not something to guess.
    problem = None if missing else skills.list_problem(skill, run["values"])
    if problem:
        missing = skills.list_params(skill)
        for param in missing:
            run["values"].pop(param["name"], None)

    if missing:
        _pause(run, "values", (problem + " " if problem else "") + _need_text(skill, missing),
               missing=[p["name"] for p in missing])
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

    # Carrying on past the last step - the person skipped it, or did it by
    # hand - leaves nothing to run. That is the end of the task, and said as
    # what it is: replay.run would call it a halt at a step that does not exist.
    if run["start"] > len(program["steps"]):
        _finish(run, {"text": f"Finished \"{skill['title']}\": nothing was left to do after step {run['start'] - 1}. "
                              "A step that was skipped or done by hand was not checked."})
        return

    result = replay.run(program, params=values, start=run["start"])
    outcome = result["outcome"]

    log(f"Skill {skill['name']} ended {outcome}: {result['text']}")

    if outcome in replay.SUCCESS:
        _finish(run, result)
    elif outcome in (replay.NEEDS_APPROVAL, replay.NEEDS_HUMAN):
        kind = "approval" if outcome == replay.NEEDS_APPROVAL else "human"
        _pause(run, kind, result["text"], "approval" if kind == "approval" else "query",
               step=result["step"], resume_from=result["resume_from"])
    elif outcome == replay.RECONCILIATION_REQUIRED or result["resume_from"] is None or run["stuck"] >= MAX_STUCK_ASKS:
        write_final_reply(result["text"], kind="error", engine=SKILL_ENGINE)
    else:
        run["stuck"] += 1
        _ask_stuck(run, result)


def _ask_stuck(run, result, preface="", unclear=0):
    ran = result["resume_from"] == result["step"] + 1

    if ran:
        options = "Reply `continue` if the screen looks right and I should carry on, or `stop`."
    else:
        options = ("Reply `retry` to look again, `skip` to leave this step out and carry on, `stop`, or tell me which "
                   "control to use instead - for example `use \"Save as\"`.")

    _pause(run, "stuck", f"{preface} {result['text']} {options}".strip(), step=result["step"],
           resume_from=result["resume_from"], ran=ran, why=result["text"], unclear=unclear)


def _finish(run, result):
    skill = run["skill"]
    text = result["text"]

    if run["patches"]:
        for patch in run["patches"]:
            skill.setdefault("learned", []).append(f"step {patch['n']}: {patch['from']!r} is now {patch['to']!r}")
        skill["version"] = skill.get("version", 1) + 1
        text += " I have remembered " + "; ".join(f"the control you named ({p['to']})" for p in run["patches"]) + " for next time."

    skill["runs"] = skill.get("runs", 0) + 1
    skills.save(skill)

    if skill.get("done_when"):
        text += f" You said it is finished when: {el._quote(skill['done_when'], 200)} - worth a look."

    write_final_reply(text, kind="final", engine=SKILL_ENGINE)


def _stop(step):
    write_final_reply(f"Stopped before step {step}, as you asked. Nothing from that step on was done.",
                      kind="final", engine=SKILL_ENGINE)


def _run_reply(paused, reply, approval_outcome):
    run, kind = paused["run"], paused["kind"]

    if kind == "values":
        return _values_reply(paused, reply)

    if kind == "start":
        if learning.YES.match(reply or ""):
            run["checked"] = True
            return _prepare(run)
        return write_final_reply("Stopped before the first step, as you asked. Nothing was done.", kind="final", engine=SKILL_ENGINE)

    if kind == "approval":
        stop = approval_outcome == "declined" or (approval_outcome is None and not approval.is_approval(reply))
    elif kind == "human":
        stop = bool(STOP_RE.match(reply or ""))
    else:
        return _stuck_reply(paused, reply)

    if stop:
        return _stop(paused["step"])

    run["start"] = paused["resume_from"]
    _execute(run)


def _values_reply(paused, reply):
    run, missing = paused["run"], paused["missing"]

    if STOP_RE.match(reply or ""):
        return write_final_reply("Stopped before the first step, as you asked. Nothing was done.", kind="final", engine=SKILL_ENGINE)

    options = {o.group(1): next(v for v in o.groups()[1:] if v is not None) for o in OPTION_RE.finditer(reply or "")}

    if not options and len(missing) == 1:
        options[missing[0]] = (reply or "").strip().strip("\"'`")

    run["values"].update({name: value for name, value in options.items() if name in missing and value})
    run["asked"] += 1

    left = [name for name in missing if not run["values"].get(name)]
    if run["asked"] >= 2 and (left or skills.list_problem(run["skill"], run["values"])):
        raise _Refused(f"I still do not have usable values for {', '.join(missing)}, so I did not start.")

    _prepare(run)


def _stuck_reply(paused, reply):
    run = paused["run"]
    step_n, ran = paused["step"], paused["ran"]
    reply = reply or ""

    if STOP_RE.match(reply) or ABANDON_RE.match(reply):
        return _stop(step_n)

    if CARRY_ON_RE.match(reply):
        run["start"] = step_n + 1
        return _execute(run)

    if not ran:
        if RETRY_RE.match(reply):
            run["start"] = step_n
            return _execute(run)

        hint = USE_RE.match(reply)
        if hint and _retarget(run, step_n, hint.group(1)):
            run["start"] = step_n
            return _execute(run)

    # Neither understood nor guessed at: one more try to be told, then stop.
    if paused["unclear"] < 1:
        result = {"step": step_n, "resume_from": paused["resume_from"], "text": paused["why"]}
        return _ask_stuck(run, result, "I did not catch that.", unclear=1)

    write_final_reply(f"I could not tell what to do at step {step_n} and would rather not guess, so I stopped there. "
                      "Nothing after it was done.", kind="error", engine=SKILL_ENGINE)


def _retarget(run, step_n, wanted):
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
    step["target"] = {key: value for key, value in dict(target, name=name).items() if key != "path"}
    step["summary"] = replay._summary(step)

    return True
