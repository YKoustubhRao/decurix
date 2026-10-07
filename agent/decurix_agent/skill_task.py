import json
import re
import threading
import time
import traceback

from decurix_agent import (attachments, binding, config, guidance, lessons, llm_client, messaging, progress, request_reading, state,
                           study, task_context, usage, verify)
from decurix_agent.computer import (approval, constraints, core, escalation, evidence, experience, launchers, learning,
                                    mimic, recording, replay, skills)
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log
from decurix_agent.messaging import ask_for_query, remember, write_final_reply
from decurix_agent.replay_task import OPTION_RE, STOP_RE


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
NUMBER_RE = re.compile(r"^\W*(?:option\s*|number\s*|#)?(\d{1,2})\W*$", re.IGNORECASE)
# A person who can see the screen saying a point click is right where it is:
# "click anyway", "click it", "click there". Only offered, and only read, for a
# click recorded as a point whose picture did not match (see _unanchor).
ANYWAY_RE = re.compile(r"^\W*(click\s+(it\s+)?anyway|click\s+(it|there)|anyway)\W*$", re.IGNORECASE)
# Close the app a step found already open, and open it fresh (see replay's
# "reopen"). Only offered, and only read, when that is what stopped the run.
RESTART_RE = re.compile(r"^\W*(restart|re-?open|re-?start|open it (?:again|fresh)|start it (?:again|fresh)|close it and (?:re-?)?open it)\b",
                        re.IGNORECASE)
USE_RE = re.compile(r"^\W*(?:use|try|click|press|select|choose)\b\s*(.+?)\s*$", re.IGNORECASE | re.DOTALL)

MAX_STUCK_ASKS = 3


class _Refused(Exception):
    """A request that cannot be carried out, in words a person can act on."""


def _ask_model(system, prompt):
    """
    One question to this desktop's own engine, for the escalation ladder. The
    import is here, not at the top, because harness.py reads the configured
    engine at import and this module must load without one (and be testable).
    """

    from decurix_agent import harness

    return harness.ask_model(system, prompt)


def _ladder(skill):
    """
    The escalation ladder for one run: healing always, the engine as second
    opinion only when it is up (a run must not wait on a backend that is still
    starting - the person is a better rung than a stalled request).
    """

    ready = getattr(state, "backend_ready", False) and escalation.MODEL_CALLS > 0
    return escalation.Ladder(ask_model=_ask_model if ready else None, goal=skill.get("title") or skill.get("name") or "",
                             explanation=skill.get("explanation") or "")


# ============================================================
# Parsing
# ============================================================

def parse_learn(content):
    """([recording ids], name) of a "/learn" request, or None for any other request. No IDs means the latest demonstration."""

    match = LEARN_RE.match((content or "").strip())
    if not match:
        return None

    rest, name = match.group(1) or "", None

    # name="Google a phrase" may have spaces in it, so options are read the
    # way "/skill" reads its values, before the rest is split into IDs.
    for option in OPTION_RE.finditer(rest):
        if option.group(1) == "name":
            name = next(v for v in option.groups()[1:] if v is not None).strip() or None
    ids = OPTION_RE.sub(" ", rest).split()

    return ids, name


def parse_skill(content):
    """
    (what was asked for, {param: value}) of a "/skill" request, or None. Nothing
    asked for lists the skills. Only the first line is the command: lines after
    it are the run's context (an email, the files it brought - skill_context),
    which values the command does not give are read from.
    """

    match = SKILL_RE.match((content or "").strip().split("\n", 1)[0].strip())
    if not match:
        return None

    rest = match.group(1) or ""
    params = {}

    for option in OPTION_RE.finditer(rest):
        params[option.group(1)] = next(v for v in option.groups()[1:] if v is not None)

    query = OPTION_RE.sub("", rest).strip()
    return ("" if query.lower() == "list" else query), params


def skill_context(content):
    """What follows a "/skill" request's command line: the documents and email of this run."""

    parts = (content or "").strip().split("\n", 1)
    return parts[1].strip() if len(parts) > 1 else ""


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


def learn_request(sessions, name=None):
    """
    The "/learn" request that teaches from `sessions`, naming the skill `name` -
    what the control plane sends and the chat shows (httpapi.learnContent
    builds the same string). A name holding both kinds of quote is refused.
    """

    content = "/learn " + " ".join(sessions)
    name = " ".join((name or "").split())
    if not name:
        return content

    if '"' not in name:
        return f'{content} name="{name}"'
    if "'" not in name:
        return f"{content} name='{name}'"
    raise ValueError("the name has both kinds of quote in it")


def handles(content):
    """Whether a request is one of this module's ("/learn", "/skill"), which need no reasoning backend."""

    return parse_learn(content) is not None or parse_skill(content) is not None


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
           "ladder": _ladder(skill), "asked_ids": [], "mimic": found, "one_off": one_off, "context": content}
    _prepare(run)


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
            _run_request(*parse_skill(content), context=skill_context(content))
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

    if study.available():
        # The model studies the demonstration first, and asks what it could
        # not work out (study.py) - a lesson for a task whose next run will be
        # different, not only a replay of this one.
        progress.emit("Studying your recording to work out what the task is and what I need to ask.")
        understanding = study.study(skill)
        if understanding is not None:
            skill["understanding"] = understanding

    _ask(skill)


def _ask(skill, preface="", question=None):
    """Save the draft and put its next question to the person."""

    question = question or learning.next_question(skill)
    state.paused_skill = {"mode": "learn", "skill": skill, "question": question}

    skills.save(skill)
    ask_for_query(f"{preface} {question['text']}".strip(), kind="query", engine=LEARN_ENGINE)


def _learn_reply(paused, reply):
    skill, question = paused["skill"], paused["question"]

    # Only a bare "stop" / "cancel" ends a lesson: an answer may well start
    # with the word ("stop and tell me" to "what if it already exists?").
    if ABANDON_RE.match(reply or "") and len((reply or "").split()) <= 2:
        skills.save(skill)
        write_final_reply(
            f"Stopped. What you have told me about \"{skill['title']}\" is kept - send `/learn {skill['sources'][0]}` "
            "to carry on where we left off.", kind="final", engine=LEARN_ENGINE)
        return

    # A correction to what the model understood, given at the last question,
    # is not a name: names are short, sentences are not.
    if question["kind"] == "confirm" and skill.get("understanding") and len((reply or "").split()) > MAX_NAME_WORDS:
        progress.emit("Taking in your correction.")
        study.absorb(skill, "Is this how the task should be done? (the summary and rules above)", reply)
        _ask(skill, "Got it - here it is again.")
        return

    # The last question takes a name, which moves the skill to another file.
    drafted_as = skill["name"]
    result = learning.answer(skill, question, reply)

    if not result["understood"]:
        _ask(skill, result["note"], question=question)
        return

    # What the person told the model that studied the demonstration is folded
    # into what it understood; an answer that opens something it must know
    # gets one follow-up, asked next.
    if skill.get("understanding") is not None and question["kind"] in ("study", "goal") \
            and not learning.NOTHING.match(reply or "") \
            and not (question["kind"] == "goal" and skill["answers"].get("goal") == (reply or "").strip()[:300]
                     and learning.YES.match(reply or "") and len((reply or "").split()) <= 3):
        progress.emit("Taking in your answer.")
        follow_up = study.absorb(skill, question["text"], reply)
        if follow_up:
            _add_follow_up(skill, question, follow_up)

    decision = result["decision"]

    if decision == "save":
        skills.save(skill)
        if skill["name"] != drafted_as:
            skills.delete(drafted_as)
        # The skill is already durable and ready to run. Sending its optional
        # summary to the control plane must never leave the teaching UI in
        # "Processing" behind a slow or unreachable memory endpoint.
        _remember_async(skill)
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


MAX_NAME_WORDS = 6


def _add_follow_up(skill, question, text):
    """Ask `text` right after `question` (study.absorb's follow-up)."""

    asked = skill["understanding"].setdefault("questions", [])
    new = {"id": f"f{skill['understanding'].get('follow_ups', len(asked))}", "text": text, "why": "follow-up"}
    at = next((i + 1 for i, q in enumerate(asked) if f"study:{q['id']}" == question["id"]), 0)
    asked.insert(at, new)


def _remember_async(skill):
    """Record optional learned memory without delaying the completed lesson."""

    threading.Thread(target=_remember, args=(skill,), daemon=True).start()


def _remember(skill):
    """Put what was just learned into the desktop's worker memory (its card's "learned memory")."""

    steps = skill["steps"]
    params = skills.required(skill)
    gated = sum(1 for s in steps if s.get("consequential"))
    by_hand = sum(1 for s in steps if s["kind"] == "manual")

    lines = [f"Taught by demonstration ({len(skill.get('sources') or [])} recording(s), {len(steps)} steps)."]

    said = skills.explanation(skill)
    if said:
        lines.append(f"The person who taught it said: {said.rstrip('. ')}.")

    understood = skill.get("understanding") or {}
    if understood.get("summary"):
        lines.append(f"What it does: {understood['summary']}")
    # What was asked and answered while it was taught, word for word: the
    # lesson is the person's, and memory is where every engine reads it.
    for said in (understood.get("dialogue") or [])[:12]:
        lines.append(f"Asked when it was taught: {said['q']} - the person answered: {said['a']}.")

    if params:
        wanted = ", ".join(f'{p["name"]} ({p["label"]})' for p in params)
        lines.append(f"Needs a value for: {wanted}.")

    if gated:
        lines.append(f"Stops for approval before {gated} step(s), every run.")

    if by_hand:
        lines.append(f"{by_hand} step(s) are left to a person, such as a sign-in.")

    example = "".join(f' {p["name"]}="..."' for p in params)
    lines.append(f"Run it with `/skill {skill['name']}{example}`, or `gui do {skill['name']}` from a shell.")

    remember(f"Skill: {skill.get('title') or skill['name']}", " ".join(lines)[:3800], category="skill")


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


def _run_request(query, given, context=""):
    if not query:
        write_final_reply(_listing(), kind="final", engine=SKILL_ENGINE)
        return

    skill, candidates = skills.find(query)

    if skill is None:
        if candidates:
            names = ", ".join(f"`{s['name']}`" for s in candidates)
            raise _Refused(f"More than one skill fits \"{query}\": {names}. Say which, by its name.")
        raise _Refused(f"I do not know a skill like \"{query}\". " + _listing())

    run = {"skill": skill, "values": skills.initial_values(skill, given), "start": 1, "checked": False, "patches": [],
           "asked": 0, "stuck": 0, "ladder": _ladder(skill), "asked_ids": [], "context": context}
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
        found, evidence = binding.fill(skill, missing, run["context"])
        if found:
            run["values"].update(found)
            run.setdefault("evidence", {}).update(evidence)
            progress.emit("Read from the documents:\n" + binding.report(evidence, found))
            _prepare(run)
            return

    # A task the model does while it is still learning it takes its values as guidance, and reads the run's own
    # documents itself: a value the request and its documents do not have is no reason to stop. One demonstration
    # fixes the task's shape to what it showed - a bill of two lines - and the next bill had one: found live, the run
    # asked for the "second item" before anything else, and "there is no second line" could not answer that question.
    if missing and not problem and run.get("filled") and run["start"] == 1 and _learning_by_model(run):
        progress.emit("The documents do not give " + ", ".join(p["name"] for p in missing)
                      + " - working from the documents themselves.")
        _by_model(run)
        return

    if missing:
        _pause(run, "values", (problem + " " if problem else "") + _need_text(skill, missing),
               missing=[p["name"] for p in missing])
        return

    # What the person chose when they taught it: some tasks are theirs to start (they spend money, commit them to
    # something), and the words of a request cannot tell which. A skill taught before this was asked has no answer
    # and runs as it always did.
    if run["start"] == 1 and skill.get("approval") == "ask" and not run.get("approved") and not run.get("one_off"):
        shown = "; ".join(f"{k} = {el._quote(str(v), 60)}" for k, v in run["values"].items())
        _pause(run, "go", f"You taught me to check with you before I start \"{skill.get('title') or skill['name']}\"."
               + (f" This run would use: {shown}." if shown else "")
               + " Reply `yes` to start, or `no` to stop.", ask_kind="approval")
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
    elif outcome in (replay.NEEDS_APPROVAL, replay.NEEDS_HUMAN):
        kind = "approval" if outcome == replay.NEEDS_APPROVAL else "human"
        text = _approval_text(run, result, values) if kind == "approval" else result["text"]
        _pause(run, kind, text, "approval" if kind == "approval" else "query",
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
        _settle(run, outcome)
        ran = result["resume_from"] == result["step"] + 1
        _by_model(run, from_step=result["step"], note=result["text"], ran=ran)
    else:
        run["stuck"] += 1
        _ask_stuck(run, result)


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

    if screen_checked and not (verify.available() and _stage(run) == experience.APPRENTICE):
        return _succeed(run, result, "screen")

    progress.emit("Checking the screen to see that the task was really done.")
    verdict = _judge(run, result["text"])

    if verdict["verdict"] == verify.DONE:
        return _succeed(run, result, "model", seen=verdict["seen"])
    if screen_checked and verdict["verdict"] == verify.UNSURE:
        # Every step's own result was on the screen; the model could not tell
        # more than that. The screen is the proof it always was.
        return _succeed(run, result, "screen")

    _pause(run, "confirm", _doubt_text(result["text"], verdict), result=result, verdict=verdict)


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


def _succeed(run, result, checked_by, seen="", helped=False):
    skill = run["skill"]
    text = result["text"]

    if checked_by == "model" and seen:
        text += f"\n\nChecked on the screen: {seen}"
    elif checked_by == "person":
        text += "\n\nYou confirmed it was done."

    text += _evidence_text(run)

    if run.get("one_off"):
        # Built from a recording for this request only: nothing to save, and a
        # person has not said which parts change - Learn is how they do.
        write_final_reply(text + f" (Done from your recording of \"{run['mimic']['text']}\". Teach it with Learn "
                                 "and I will also know which parts change.)", kind="final", engine=SKILL_ENGINE,
                          checked=True)
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
    experience.note_verdict(skill, "replay", True, checked_by=checked_by,
                            helped=helped or bool(run["asked_ids"]) or bool(run.get("taken_over")))
    skills.save(skill)
    text += experience.advice(skill)

    write_final_reply(text, kind="final", engine=SKILL_ENGINE, checked=True)


def _stop(step, run=None):
    if run is not None:
        _settle(run, "stopped")
        _record_verdict(run, False, why="stopped by the person")

    write_final_reply(f"Stopped before step {step}, as you asked. Nothing from that step on was done.",
                      kind="final", engine=SKILL_ENGINE)


def _run_reply(paused, reply, approval_outcome):
    run, kind = paused["run"], paused["kind"]

    if kind == "values":
        return _values_reply(paused, reply)

    if kind == "confirm":
        return _confirm_reply(dict(paused, claim=paused["result"]["text"]), reply)

    if kind == "go":
        if approval.is_approval(reply) or learning.YES.match(reply or ""):
            run["approved"] = True
            return _prepare(run)
        return write_final_reply("Stopped before starting, as you asked. Nothing was done.", kind="final", engine=SKILL_ENGINE)

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
        return _stop(paused["step"], run)

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
    if run["asked"] >= 2 and (left or skills.list_problem(run["skill"], run["values"])
                              or constraints.violations(skills.required(run["skill"]), run["values"], split=skills.split_items)):
        raise _Refused(f"I still do not have usable values for {', '.join(missing)}, so I did not start.")

    _prepare(run)


def _stuck_reply(paused, reply):
    run = paused["run"]
    step_n, ran = paused["step"], paused["ran"]
    reply = reply or ""

    if STOP_RE.match(reply) or ABANDON_RE.match(reply):
        return _stop(step_n, run)

    if CARRY_ON_RE.match(reply):
        run["start"] = step_n + 1
        return _execute(run)

    if not ran:
        if RETRY_RE.match(reply):
            run["start"] = step_n
            return _execute(run)

        reopen = paused.get("reopen")
        if reopen and RESTART_RE.match(reply):
            progress.emit(f"Closing {el._quote(reopen['window'], 60)} to open it fresh")
            if not launchers.close(reopen["window"], replay.title_matches):
                return _ask_stuck(run, {"step": step_n, "resume_from": paused["resume_from"], "text": paused["why"],
                                        "options": paused.get("options"), "trail": paused.get("trail")},
                                  f"{el._quote(reopen['window'], 60)} did not close (it may be asking something - "
                                  "look at the screen).")
            run["start"] = reopen["step"]
            return _execute(run)

        # A number picks one of the candidates the desktop offered.
        number = NUMBER_RE.match(reply)
        if number:
            option = next((o for o in paused.get("options") or [] if o["n"] == int(number.group(1))), None)
            if option and _retarget(run, step_n, option["name"], role=option["role"]):
                run["start"] = step_n
                return _execute(run)

        if ANYWAY_RE.match(reply) and _unanchor(run, step_n):
            run["start"] = step_n
            return _execute(run)

        hint = USE_RE.match(reply)
        if hint and _retarget(run, step_n, hint.group(1)):
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
# replay's end, _gate an engine's "done", and anything short of a confident
# "done" asks the person (_doubt_text) - who may say it is done, have it tried
# again, or say what is wrong, which is kept as a correction the model reads
# on every later run.
# ============================================================

MAX_MODEL_ATTEMPTS = 2
MAX_CORRECTIONS = 8
# What confirms an unchecked result: words about the result itself. A bare yes or ok does not: found live, a "yes"
# meant for an approval asked a moment before arrived after the worker had moved on to "I cannot confirm this is
# done", and recorded a task nobody had looked at as confirmed by a person.
CONFIRM_RE = re.compile(r"^\W*(done|it(?:'s| is| was)? done|it worked|correct|confirmed|looks (?:right|good|fine))\b",
                        re.IGNORECASE)
VAGUE_YES_RE = re.compile(r"^\W*(yes|y|yep|yeah|ok(?:ay)?|good|right|sure|fine|go ahead|proceed)\b", re.IGNORECASE)
REDO_RE = re.compile(r"^\W*(retry|try again|again|redo|do it again|finish it|keep going|continue)\b", re.IGNORECASE)


def _engine_can_drive():
    """Whether this desktop's engine can do screen work with a hosted model (TheBot on one, or the Office engine)."""

    engine_ok = (config.AGENT_HARNESS == "thebot" and config.HOSTED_BRAIN) or config.AGENT_HARNESS == "azure"
    return engine_ok and llm_client.available() and getattr(state, "backend_ready", False)


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

    return _engine_can_drive() and not run.get("replay_only") and _stage(run) == experience.APPRENTICE


def _for_this_run(text, skill, values):
    """
    `text` about the task with each value the demonstration typed replaced by
    this run's. What was understood from a demonstration is worded in its own
    values ("a LinkedIn profile is open for Koustubh Rao"): read as it stands
    it sends the model after the demonstration's result, and - found live -
    the check called a run for another name done because the demonstration's
    page was still on the screen.
    """

    for param in skill.get("params") or []:
        example, value = param.get("example"), (values or {}).get(param.get("name"))
        if value not in (None, ""):
            # A lesson writes an input as its name in braces (study.py).
            text = text.replace("{" + str(param.get("name")) + "}", str(value))
        if not example or value in (None, "") or param.get("list") or len(str(example)) < 3:
            continue
        text = re.sub(re.escape(str(example)), lambda _m, v=str(value): v, text, flags=re.IGNORECASE)
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


def _criteria(skill, values=None):
    """
    What the person said about the task and what done looks like, for the
    model and the check - in this run's values when they are given.
    """

    if values:
        return EXAMPLE_NOTE + "\n" + _for_this_run(_criteria(skill), skill, values)

    parts = []
    said = skills.explanation(skill)
    if said:
        parts.append(said)
    understood = skill.get("understanding") or {}
    if understood.get("done_when") and understood["done_when"] not in said:
        parts.append(f"Done when: {understood['done_when']}")
    for rule in understood.get("rules") or []:
        parts.append(f"Rule: {rule}")
    for said in understood.get("dialogue") or []:
        # The person's own words, which the rules above were written from.
        parts.append(f"Asked when it was taught: {said['q']} - the person answered: {said['a']}")
    for correction in skill.get("corrections") or []:
        parts.append(f"A correction the person made after an earlier run: {correction['text']}")
    for said in skill.get("run_answers") or []:
        parts.append(f"Asked during an earlier run: {said['q']} - the person answered: {said['a']}")
    return "\n".join(parts)


def _procedure(run, from_step=None, note="", ran=False):
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

    understood = skill.get("understanding") or {}
    if understood.get("summary"):
        lines.append(f"What the task is: {_for_this_run(understood['summary'], skill, run['values'])}")
    criteria = _criteria(skill, run["values"])
    if criteria:
        lines.append("What the person said, and what was agreed when they taught it (follow it):\n" + criteria)

    if run["values"]:
        params = {p["name"]: p for p in skill.get("params") or []}
        shown = []
        for key, value in run["values"].items():
            param = params.get(key) or {}
            meaning = (understood.get("inputs") or {}).get(key) or param.get("label") or key
            shown.append(f"- {key} = {json.dumps(value, ensure_ascii=False)} ({meaning}; the person typed "
                         f"{json.dumps(param.get('example') or '', ensure_ascii=False)})")
        lines.append("This run's values - type these where the person typed theirs:\n" + "\n".join(shown))

    if skill.get("documents"):
        lines.append("When they showed this, the person worked from these documents: "
                     + attachments.names(skill["documents"]) + ". They are examples of the kind of document the task "
                     "starts from - this run's own documents are the files its request names; read those (`office "
                     "read PATH`) and take this run's values from them, never from the examples.")

    steps = []
    for step in skill["steps"]:
        line = f"{step['n']}. {step.get('summary') or step['kind']}"
        if step.get("intent"):
            line += f" - the person said: {step['intent']}"
        purpose = (understood.get("steps") or {}).get(str(step["n"]))
        if purpose:
            line += f" - purpose: {purpose}"
        steps.append(line)
    lines.append("The person's steps:\n" + "\n".join(steps))

    if from_step:
        lines.append(f"A replay of these steps already ran up to step {from_step} and stopped there: {note} "
                     + ("That step WAS done but its result did not show: look at the screen before doing anything - "
                        "never repeat it blindly. " if ran else "")
                     + f"Carry on from step {from_step}.")

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
    how += [] if one_off else [
        f"- `gui skill {name} --look N` shows the screen as it was when the person did step N, with the spot they "
        "pressed ringed in red. Use it for any step recorded as a point on the screen, and whenever you are unsure "
        "what a step meant.",
        f"- `gui do {name} --from N` replays from step N exactly as recorded, stopping at the first step it cannot "
        "do; use it only when the screen matches the recording AND every clicked option still matches this run. "
        "Suggestion options in the recording name the example's own choices (a person, a product, a category): "
        "changing a typed parameter does not change those clicks. When this run's choices, amounts or number of "
        "rows differ from the example's, use gui observe / select / type to choose this run's matching options and "
        "fill each row yourself; do not replay the example or click its coordinates. Check where any replay "
        "stopped.",
        "- Otherwise work each step with gui observe / click / type / key as usual.",
    ]
    how += [
        "- What the person answered - when they taught this and during earlier runs - is settled. Act on it and "
        "never ask them the same thing again; it also covers the same kind of choice met somewhere else (told to "
        "take the first of several matching conversations, take the first of several matching people in any list).",
        "- If the request asks for several items (a list), do the task once per item, one after another, unless "
        "the person said otherwise.",
        "- Before you say DONE, look at the screen (gui verify) and make sure it shows the task done for THIS run's "
        "values. If it does not, keep working, or ask (HELP) - never report DONE for what the screen does not show. "
        "Your report is checked against the screen, and a false DONE is the worst outcome.",
    ]
    if run.get("attempt_note"):
        how.append(f"- A check after your previous attempt found: {run['attempt_note']} Fix that.")
    lines.append("\n".join(how))

    return "\n\n".join(lines)


def _engine_request(run):
    """The request the engine is given for a taught task."""

    skill = run["skill"]

    # A request matched to a recording goes in the same words as a `/skill`
    # run, with what the person typed after them - never as their words alone.
    # Found live: `search "sundar pichai"`, matched to a taught search, went to
    # the engine as typed; its triage called that a question, its chat skill -
    # no tools - answered "DONE", and nothing had touched the screen. The
    # engine sends "Do the task you were taught" to its operator, always.
    if run.get("one_off"):
        request = f"Do the task you were taught by a recording of \"{run['mimic']['text']}\""
    else:
        request = f"Do the task you were taught, \"{skill.get('title') or skill['name']}\" (skill {skill['name']})"
    if run["values"]:
        request += " with " + ", ".join(f"{k}={json.dumps(v, ensure_ascii=False)}" for k, v in run["values"].items())
    request += "."
    if run.get("mimic"):
        request += "\n\nThe person asked: " + (run.get("context") or run["mimic"]["text"])
    elif run.get("context"):
        request += "\n\n" + run["context"]
    return request


def _by_model(run, from_step=None, note="", ran=False):
    """Hand the task to the engine, guided by the skill (see the section above)."""

    skill = run["skill"]
    request = _engine_request(run)
    run.setdefault("attempts", 0)
    run["attempts"] += 1

    guidance.begin(skill=skill["name"], task=request, text=_procedure(run, from_step, note, ran),
                   values=run["values"], mode="takeover" if from_step else "model", run=run)

    if from_step:
        progress.emit(f"The replay stopped at step {from_step}; the model is taking it from there.")
    else:
        progress.emit(f"Doing \"{skill.get('title') or skill['name']}\" with the model, guided by what you taught "
                      "(it is still learning this task).")
    log(f"Skill {skill['name']}: handed to the engine ({'takeover at ' + str(from_step) if from_step else 'learning'})")

    # A person who taught this task and said to go ahead (or said yes just now) has approved it as a kind of task;
    # the engine's check on the request's own words is not asked of them again.
    state.request_preapproved = skill.get("approval") == "go" or bool(run.get("approved"))
    try:
        _engine(request, state.current_task_file)
    finally:
        state.request_preapproved = False


def _judge(run, claim):
    skill = run["skill"]
    steps = "; ".join(f"{s['n']}. {s.get('summary')}" for s in skill["steps"])[:1500]
    return verify.judge(run.get("context") or _engine_request(run), claim=claim, criteria=_criteria(skill, run["values"]),
                        values=run["values"], steps=steps, live_png=evidence.live(),
                        reference_png=evidence.final_frame(skill), observed=evidence.observed())


REPORT_CHARS = 4000


def _doubt_text(claim, verdict):
    lead = {verify.NOT_DONE: "I do not think this is done.",
            verify.UNSURE: "I cannot confirm this is done."}.get(verdict["verdict"], "I cannot confirm this is done.")
    parts = [lead]
    if verdict.get("seen"):
        parts.append(f"The screen shows: {verdict['seen']}")
    if verdict.get("why"):
        parts.append(f"({verdict['why']})")
    head = " ".join(parts)
    # The report is shown whole (up to REPORT_CHARS): found live, a 600-character
    # cut ended at "ID_scan.png does not..." - the one finding the person being
    # asked most needed to read.
    report = (claim or "").strip()
    if len(report) > REPORT_CHARS:
        report = report[:REPORT_CHARS].rstrip() + " ..."
    return (f"{head}\n\nWhat was reported:\n{report}\n\nLook at the screen. Reply `done` if it is done, `retry` "
            "to have it tried again, or tell me what is wrong - I will remember it for next time.")


def _record_verdict(run, ok, checked_by=None, helped=False, why="", mode=None):
    """Note how a run ended on the saved skill (a fresh copy: the run's own may hold unproven changes)."""

    if run.get("one_off"):
        return
    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return
    experience.note_verdict(fresh, mode or ("model" if run.get("attempts") else "replay"), ok, checked_by=checked_by,
                            helped=helped, why=why)
    if ok:
        fresh["runs"] = fresh.get("runs", 0) + 1
    skills.save(fresh)


def _correct(run, text):
    """Keep what a person said was wrong, for every later run's model (and the check)."""

    text = " ".join((text or "").split())[:500]
    if run.get("one_off") or not text:
        return
    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return
    fresh["corrections"] = ((fresh.get("corrections") or []) + [{"at": int(time.time()), "text": text}])[-MAX_CORRECTIONS:]
    skills.save(fresh)
    run["skill"]["corrections"] = fresh["corrections"]


# ------------------------------------------------------------
# A question an engine asks in the middle of a task
#
# Found live (2026-10-04): taught "if several conversations match, choose the
# first one", the engine met several *people* matching in the new-message box
# and asked which. The person answered "always choose first" - and that went
# to the engine for that run only; nothing kept it, so the next run would ask
# again. Three things now:
#   - what a person answers is kept: on the skill, with its question, when the
#     task is a taught one (learn_answer), and in the worker's memory as a
#     lesson for every task (lessons.py, from the watcher);
#   - before a person is paged, the question is put to the hosted model beside
#     everything they have taught and answered - the skill's lesson and the
#     worker's kept lessons - and one that is already settled is answered from
#     it. Never an approval, never a question this module asks itself, at most
#     MAX_SELF_ANSWERS a task, and only on a confident reading;
#   - the engine is told the same in its own context (_procedure, and worker
#     memory), so it should rarely get as far as asking.
# ------------------------------------------------------------

MAX_SELF_ANSWERS = 2
MAX_RUN_ANSWERS = 12
SELF_ANSWER_CONFIDENCE = 0.8

# Who may be answered: an engine working a task. A question from a replay, a
# lesson, a skill's own run, a ticket or a web lookup is that module's, and
# its reply goes back to it, not to an engine.
ENGINES_THAT_ASK = (None, "", "thebot", "azure", "claude", "ollama")

ANSWERED_SYSTEM = """A computer agent working for a person wants to stop its task and ask that person a question. You are given what the person has already told it - what they taught about this task (their own words, the rules agreed, the questions they answered when teaching and during earlier runs) and the lessons kept from their answers in earlier tasks - and the agent's question. Decide whether the person has already answered it.

They have when they answered the same question, or the same kind of choice arising somewhere else (told to take the first of several matching conversations, the first of several matching people in another list is the same choice).

They have NOT when: nothing they said touches it; the earlier answer was about one task's own value (a particular person, amount, file or date) and this one's differs; the agent is asking for approval, a password, a sign-in, a code, a check that it is a human, or a value it was not given; or something has gone wrong (an error, a model or page that does not answer, a control it cannot find). When in doubt they have not: the person is then asked, which costs little.

Reply with JSON only: {"answered": true, "answer": "<what the agent should do, in a sentence or two, in the person's terms>", "from": "<the person's words this rests on, quoted>", "confidence": 0.0-1.0} or {"answered": false}"""

_self_answered = {"task": None, "count": 0}


def guided_skill_title():
    """The taught task the engine is on right now, by its name for a person, or ""."""

    run = (guidance.get() or {}).get("run")
    if not run or run.get("one_off"):
        return ""
    return run["skill"].get("title") or run["skill"]["name"]


def _already_told(run):
    """Everything the person has said that could settle a question: this task's lesson, then the kept lessons."""

    parts = []
    if run is not None and not run.get("one_off"):
        taught = _for_this_run(_criteria(run["skill"]), run["skill"], run["values"])
        if taught.strip():
            parts.append(f"What the person taught about this task (\"{run['skill'].get('title') or run['skill']['name']}\"):\n{taught}")
    kept = lessons.known()
    if kept:
        parts.append(f"Lessons kept from the person's answers in earlier tasks:\n{kept}")
    return "\n\n".join(parts)


def _question_gate(problem, kind, engine):
    """messaging's query gate: True when what the person already said answered the engine's question and nobody was asked."""

    if (kind != "query" or engine not in ENGINES_THAT_ASK
            or state.paused_skill is not None or state.paused_replay is not None):
        return False

    guided = guidance.get()
    run = (guided or {}).get("run")

    # What the person's reply, if they are asked, will be the answer to.
    if run is not None:
        guided["asked"] = problem

    task = task_context.current() or state.current_task
    if _self_answered["task"] != task:
        _self_answered.update(task=task, count=0)
    if _self_answered["count"] >= MAX_SELF_ANSWERS or not llm_client.available():
        return False

    told = _already_told(run)
    if not told:
        return False

    values = f"This run's values: {json.dumps(run['values'], ensure_ascii=False)}\n\n" if run is not None else ""
    try:
        data = study._call(ANSWERED_SYSTEM, f"{told}\n\nThe task: {(state.current_task or '')[:600]}\n\n{values}"
                                            f"The agent's question:\n{problem[:2000]}", max_tokens=400)
    except (llm_client.LLMError, ValueError) as exc:
        log(f"Could not check the question against what the person already said: {exc}")
        return False

    answer = " ".join(str(data.get("answer") or "").split())[:500]
    try:
        confidence = float(data.get("confidence") or 0)
    except (TypeError, ValueError):
        confidence = 0.0
    if data.get("answered") is not True or not answer or confidence < SELF_ANSWER_CONFIDENCE:
        return False

    _self_answered["count"] += 1
    if run is not None:
        guided["asked"] = None
    source = " ".join(str(data.get("from") or "").split())[:200]
    progress.emit(f"Not asking you - you already told me: {answer}" + (f" (from: {source})" if source else ""))
    log(f"Answered the engine's question from what the person already said: {problem[:200]!r} -> {answer!r}")

    # On a thread of its own: the engine is still inside the call that asked.
    threading.Thread(target=_answer_engine, args=(answer,), daemon=True).start()
    return True


def _answer_engine(answer):
    from decurix_agent.harness import continue_after_query

    try:
        continue_after_query(f"The person already answered this earlier, so they were not asked again: {answer}")
    except Exception as exc:
        log(traceback.format_exc())
        ask_for_query(f"I could not carry on after answering my own question from what you told me earlier ({exc}). "
                      "Tell me what to do next.", engine=SKILL_ENGINE)


def learn_answer(reply):
    """
    Keep a person's answer to a question the engine asked during a taught
    task (the watcher calls this as it hands the reply to the engine), so no
    later run asks it again.
    """

    guided = guidance.get()
    run = (guided or {}).get("run")
    asked = (guided or {}).pop("asked", None) if guided else None
    reply = " ".join((reply or "").split())[:500]

    if run is None or run.get("one_off") or not asked or not reply:
        return
    if ABANDON_RE.match(reply):
        return

    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return

    said = {"at": int(time.time()), "q": " ".join(asked.split())[:500], "a": reply}
    kept = [x for x in fresh.get("run_answers") or [] if (x.get("q"), x.get("a")) != (said["q"], said["a"])]
    fresh["run_answers"] = (kept + [said])[-MAX_RUN_ANSWERS:]
    skills.save(fresh)
    run["skill"]["run_answers"] = fresh["run_answers"]
    log(f"Skill {fresh['name']}: kept the person's answer to {said['q'][:120]!r}")


messaging.set_query_gate(_question_gate)


def _acts_on_screen(skill):
    """Whether a skill's own steps are work on the screen (every recorded skill's are)."""

    return any(step.get("kind") in ("click", "double_click", "right_click", "type", "key", "select", "drag", "scroll",
                                    "open", "upload") for step in skill.get("steps") or [])


def _did_screen_work():
    """Whether the task being worked on acted on the screen (its recording has an action in it)."""

    task_id = task_context.current()
    if not task_id:
        return False
    try:
        meta = recording.meta(recording.session_for_task(task_id))
    except (OSError, ValueError):
        return False
    return bool(meta and meta.get("event_count"))


def _retry_engine(guided, note):
    """Send a task the engine did not finish back to it, told what the check saw (a task no skill guides)."""

    progress.emit(f"Not done yet: {note} Trying again.")
    guidance.begin(skill=None, task=guided["task"], run=None, mode="engine", attempt=guided.get("attempt", 1) + 1,
                   text=f"A check of the screen after the previous attempt at this task found: {note} Finish the "
                        "task; do not report DONE until the screen shows it done.")
    _engine(guided["task"], state.current_task_file)


def _gate(reply, kind, engine):
    """
    messaging's final gate: a task an engine finished is checked before its
    report goes out - a taught task always (guidance.py), any other task when
    it acted on the screen. True when this took the report over.
    """

    guided = guidance.get()
    if guided is None:
        if kind != "final" or not verify.available() or not _did_screen_work():
            return False
        guided = guidance.begin(skill=None, task=state.current_task or "", text="", mode="engine", run=None)

    run = guided.get("run")

    if kind != "final":
        guidance.end()
        if run is not None:
            _record_verdict(run, False, why=(reply or "")[:300])
        return False

    # The person said no to the action the task stopped for. Found live
    # (2026-10-03): told "no, do not send it" at Send, the engine stopped as
    # asked - and this check, seeing the message unsent, sent it back to do
    # the whole task again and ask for the same approval. A refusal is the
    # person's decision about the task, not a failure to repair.
    said_no = guidance.refused()
    if said_no:
        guidance.end()
        if run is not None:
            _record_verdict(run, False, why=f"the person said no at an approval: {said_no}"[:300])
        write_final_reply(f"Stopped there, as you said (\"{said_no}\"): the action I asked about was not carried out.\n\n"
                          f"{reply}", kind="final", engine=engine, checked=True)
        return True

    progress.emit("Checking the screen to see that the task was really done.")
    if run is not None and _acts_on_screen(run["skill"]) and not _did_screen_work():
        # Nothing was opened, clicked or typed during this task, and the task
        # is done on the screen: no picture of the screen can make that done -
        # what it shows is left from before (found live: the demonstration's
        # own page, still open, passed for a run that had done nothing).
        verdict = {"verdict": verify.NOT_DONE, "confidence": 1.0, "seen": "",
                   "why": "Nothing was done on the screen during this task: no app was opened, nothing clicked or "
                          "typed. The task has to be carried out with the gui command."}
    else:
        verdict = _judge(run, reply) if run is not None else verify.judge(guided["task"], claim=reply,
                                                                              live_png=evidence.live(),
                                                                              observed=evidence.observed())

    if verdict["verdict"] == verify.DONE:
        guidance.end()
        if run is not None:
            # A takeover, or a run the check had to send back, worked in the
            # end but did not run smoothly: it does not count towards trust.
            _record_verdict(run, True, checked_by="model",
                            helped=guided["mode"] == "takeover" or run.get("attempts", 1) > 1)
        text = f"{reply}\n\nChecked on the screen: {verdict['seen']}" if verdict.get("seen") else reply
        write_final_reply(text + _evidence_text(run), kind="final", engine=engine, checked=True)
        return True

    # The model said done and the screen says not: one more go, told what the
    # check saw, before a person is asked.
    attempts = run.get("attempts", 0) if run is not None else guided.get("attempt", 1)
    if verdict["verdict"] == verify.NOT_DONE and attempts < MAX_MODEL_ATTEMPTS and _engine_can_drive():
        note = " ".join(x for x in (verdict.get("seen"), verdict.get("why")) if x)
        if run is not None:
            run["attempt_note"] = note
            progress.emit(f"Not done yet: {note} Trying again.")
            _by_model(run)
        else:
            _retry_engine(guided, note)
        return True

    guidance.end()
    state.paused_skill = {"mode": "confirm", "run": run, "claim": reply, "verdict": verdict, "engine": engine,
                          "task": guided["task"]}
    ask_for_query(_doubt_text(reply, verdict), kind="query", engine=engine)
    return True


def check_unchecked(claim, engine):
    """
    A run that ended without seeing every result (a /replay's
    completed_unverified): judged on the screen, else asked - never reported
    done unseen.
    """

    progress.emit("Checking the screen to see that the task was really done.")
    verdict = verify.judge(state.current_task or "", claim=claim, live_png=evidence.live(), observed=evidence.observed())
    if verdict["verdict"] == verify.DONE:
        return write_final_reply(f"{claim}\n\nChecked on the screen: {verdict['seen']}", kind="final", engine=engine,
                                 checked=True)
    state.paused_skill = {"mode": "confirm", "run": None, "claim": claim, "verdict": verdict, "engine": engine,
                          "task": state.current_task or ""}
    ask_for_query(_doubt_text(claim, verdict), kind="query", engine=engine)


def _confirm_reply(paused, reply):
    """The person's answer to "I cannot confirm this is done" - after an engine's run or a replay's."""

    run, claim = paused["run"], paused.get("claim") or ""
    engine = paused.get("engine") or SKILL_ENGINE
    seen = paused["verdict"].get("seen") or ""
    reply = (reply or "").strip()

    if CONFIRM_RE.match(reply) and not re.search(r"\b(not|n't|isn'?t|wrong|but)\b", reply, re.IGNORECASE):
        if run is not None and paused.get("result") is not None:
            return _succeed(run, paused["result"], "person", helped=True)
        if run is not None:
            _record_verdict(run, True, checked_by="person", helped=True)
        return write_final_reply(f"{claim}\n\nYou confirmed it was done.", kind="final", engine=engine, checked=True)

    if VAGUE_YES_RE.match(reply):
        # Neither a confirmation nor something to correct: the question stays open.
        state.paused_skill = paused
        return ask_for_query("To mark it done, reply `done` - a plain yes is not enough here, it may have been meant "
                             "for another question. Reply `retry` to have it tried again, or say what is wrong.",
                             kind="query", engine=engine)

    if STOP_RE.match(reply) or ABANDON_RE.match(reply) or learning.NO.match(reply):
        if run is not None:
            _record_verdict(run, False, why="the person said it was not done")
        return write_final_reply("Not done - marked as a failure. " + seen, kind="error", engine=engine, checked=True)

    redo = bool(REDO_RE.match(reply))
    if not redo and run is not None:
        # What is wrong, in their words: kept for every later run.
        _correct(run, reply)
        _record_verdict(run, False, why=reply)

    # A command (/replay, /skill) is not something to hand the engine as a task.
    if _engine_can_drive() and (run is not None or not (paused.get("task") or "").startswith("/")):
        note = (seen or "it was not done") if redo else f"the person said: {reply}"
        progress.emit("Trying again." if redo else "Trying again with what you said.")
        if run is not None:
            run["attempt_note"], run["attempts"] = note, 0
            return _by_model(run)
        return _retry_engine({"task": paused.get("task") or claim, "attempt": 0}, note)

    write_final_reply("Not done - marked as a failure" + ("" if redo else f", and I will remember what you said: "
                                                          f"{el._quote(reply, 200)}"), kind="error", engine=engine,
                      checked=True)


messaging.set_final_gate(_gate)
