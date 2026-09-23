import copy
import difflib
import re
import time

from decurix_agent.computer import elements as el
from decurix_agent.computer import constraints, replay, skills


# ============================================================
# Learning a skill from demonstrations
#
# Orby's public description of its Large Action Model is "observe, learn,
# automate": watch a person do the work (their screen, their page, their
# actions), generalise it into a workflow, show the workflow to that person to
# approve, then run it. This is that loop on Decurix's own parts - the
# recordings computer/demonstration.py already makes, the program
# computer/replay.py already compiles and runs - and it has no model in it, in
# the spirit of soul's "deterministic before clever": what can be worked out
# from the recordings is, and what cannot is asked.
#
# Generalising (build):
#   - One demonstration is a single instance of the task. Several are
#     aligned step by step (difflib over each step's kind and target), and
#     what differs between them is the task's shape: text typed differently
#     is a variable, a step only some demonstrations have is optional, and
#     the checks a step's result must satisfy shrink to those every
#     demonstration agrees on.
#   - A secret typed into a password field is never in a recording, so it is
#     never a parameter: that step becomes one a person does by hand.
#   - A block of steps repeated in a row with different text typed each time
#     ("add row, type a name" three times) is a loop candidate: asked about,
#     never assumed, because it decides whether a run takes one value or a
#     list of them.
#
# Asking (questions / answer):
#   - Everything a recording cannot say - is this text the same every run, is
#     this step always there, where does the task start, when is it done - is
#     a question with a stable ID, asked one at a time. Questions are derived
#     from the skill's own state, so a session interrupted half way asks only
#     what is left.
#   - An answer that is not understood is asked again once, then answered with
#     the cautious choice (asks for the value each run, stops rather than
#     skips) and the person is told. It is never guessed the other way.
#   - Nothing is saved as ready until a person says yes to the whole skill.
#
# Approval is not a question here: a step that was gated when recorded
# ("consequential") always stops for approval when the skill runs, whatever
# anyone answers, because the gate is computer/approval.py's and not the
# skill's to switch off.
# ============================================================

MAX_UNCLEAR = 2

# Kinds a step can be recorded as that a person's answer can change.
CLICKS = {"click", "double_click", "right_click", "drag", "select"}

MAX_OVERVIEW_CHARS = 1600

# The longest block of steps looked at for repetition.
MAX_LOOP_BLOCK = 12

SAME = re.compile(r"\b(same|constant|fixed|static|unchanged|never changes?|(does not|doesn'?t|do not|don'?t) change)\b", re.I)
CHANGES = re.compile(r"(?<!not )(?<!n't )\b(chang(?:e|es|ing)|differ(?:s|ent)?|vari(?:es|able|ed))\b", re.I)
SOMETIMES = re.compile(r"\b(sometimes?|occasionally|only if|only when|if (?:it|there)|depends|conditional|optional|maybe|not always)\b", re.I)
ALWAYS = re.compile(r"(?<!not )\b(always|every time|each time|required|mandatory|part of)\b", re.I)
KEEP = re.compile(r"\b(keep|try|as recorded|ok(?:ay)?|fine|go ahead)\b", re.I)
SKIP = re.compile(r"\b(skip|leave (?:it )?out|drop|remove|ignore)\b", re.I)
MANUAL = re.compile(r"\b(manual(?:ly)?|by hand|i'?ll do|let me)\b", re.I)
YES = re.compile(r"^\W*(yes|y|yeah|yep|ok|okay|sure|save|confirm|check|please|looks good|go ahead)\b", re.I)
NO = re.compile(r"^\W*(no|n|nope|nah|discard|cancel|throw|delete|don'?t|do not)\b", re.I)
EACH = re.compile(r"\b(each|list|per item|every item|items?|rows?|repeat|loop|yes|y)\b", re.I)
FIXED = re.compile(r"\b(fixed|once|as recorded|just those|only those|exactly|no|n)\b", re.I)
NOTHING = re.compile(r"^\W*(skip|none|n/?a|nothing|no|-)\W*$", re.I)
# The last question takes a name, so a yes or a no there has to be the whole
# reply: "Save invoice" and "Delete old files" are names, not answers.
KEEP_NAME = re.compile(
    r"^\W*(?:(?:yes|y|yeah|yep|ok|okay|sure|confirm|looks good|go ahead)"
    r"(?:[\s,.!-]+(?:please|thanks?|save(?: it)?|keep(?: it)?))*|(?:save|keep)(?: it| that)?)\W*$", re.I)
THROW_AWAY = re.compile(
    r"^\W*(?:(?:no|n|nope|nah)(?:[\s,.!-]+(?:thanks?|throw (?:it )?away|discard(?: it)?|delete(?: it)?|"
    r"don'?t save(?: it)?))*|discard(?: it)?|cancel|throw (?:it )?away|delete(?: it)?|"
    r"(?:don'?t|do not) save(?: it)?)\W*$", re.I)
# The longest skill name a person can give, as build() keeps it.
MAX_NAME = 80

CHOICES = {
    "variable": [("same", SAME), ("changes", CHANGES)],
    "optional": [("sometimes", SOMETIMES), ("always", ALWAYS)],
    "coordinates": [("keep", KEEP), ("skip", SKIP), ("manual", MANUAL)],
    "unsupported": [("skip", SKIP), ("manual", MANUAL)],
    "start": [("yes", YES), ("no", NO)],
    "loop": [("each", EACH), ("fixed", FIXED)],
    "confirm": [("yes", KEEP_NAME), ("no", THROW_AWAY)],
}

# What is assumed when a question was not understood twice: the answer that
# costs a person a moment rather than costing them data.
CAUTIOUS = {
    "variable": "changes",
    "optional": "always",
    "coordinates": "manual",
    "unsupported": "manual",
    "start": "yes",
    # As recorded changes nothing: the typed fields are then asked about one
    # by one, like any others.
    "loop": "fixed",
    "confirm": "hold",
}


# ============================================================
# Building
# ============================================================

def build(sources, name=None):
    """
    A draft skill from demonstrations: `sources` is [(meta, events)], the
    first one the base the others are aligned to. Raises ValueError when none
    of them has a step a replay can run.
    """

    programs = []
    for meta, events in sources:
        program = replay.compile_program(meta, events)
        if program["steps"]:
            programs.append((meta, events, program))

    if not programs:
        raise ValueError("there is nothing in that recording to learn from - no action on the screen was kept")

    base_meta, base_events, base_program = programs[0]
    steps = _merge([_annotate(program) for _, _, program in programs])

    # Numbers follow the steps' order and change when one is removed; an ID
    # is what a question (and its saved answer) names a step by, and does not.
    for n, step in enumerate(steps, 1):
        step["id"] = f"s{n}"
        step["n"] = n

    params = _generalise(steps, len(programs))
    loops = _find_loops(steps, len(programs))
    title = (base_meta.get("title") or base_meta.get("description") or "").strip() or "A recorded task"
    # What the person asked for when they recorded it, even after a rename.
    goal = (base_meta.get("task") or "").strip() or title

    # A name the person gave is what the skill is called and shown as; the
    # recording's title stays its goal (what a request is matched against,
    # mimic.plan). Either way the file name is free: naming a new
    # skill after one that exists must not overwrite it.
    given = " ".join((name or "").split())[:80]
    name = skills.unique_name(given or title)

    skill = {
        "schema": skills.SCHEMA,
        "name": name,
        "title": given or title,
        "named": bool(given),
        "goal": goal,
        "status": skills.DRAFT,
        "sources": [program["session"] for _, _, program in programs],
        "demos": len(programs),
        "version": 1,
        "runs": 0,
        "created_at": time.time(),
        "starts": _starts(steps, programs),
        "done_when": None,
        "notes": _notes(steps, programs),
        "params": params,
        "steps": steps,
        "loops": loops,
        "learned": [],
        "answers": {},
        "unclear": {},
    }

    return skills.renumber(skill)


def _annotate(program):
    """A program's steps as merge material: each remembers which session it is from and what it typed."""

    params = {p["name"]: p for p in program["params"]}
    steps = []

    for original in program["steps"]:
        step = copy.deepcopy(original)
        step["seen"] = 1
        step["session"] = program["session"]

        if step["kind"] == "type":
            param = params[step["param"]]
            step["label"] = param["label"]
            step["secret"] = bool(param["secret"])
            step["secret_reason"] = param.get("reason")
            step["samples"] = [None if param["secret"] else param["default"]]

        steps.append(step)

    return steps


def _signature(step):
    """What makes two demonstrations' steps the same step: what was done, and to what - not what was typed."""

    target = step.get("target") or {}

    if target.get("role"):
        who = (target["role"], replay._norm(target.get("name")))
    elif step.get("point"):
        who = ("point",)
    else:
        who = ()

    return (step["kind"], *who, *(step.get("keys") or []))


def _merge(runs):
    """
    The steps of several demonstrations as one list: aligned steps become one
    (counting how many demonstrations had it, and keeping everything typed
    into it), steps only some had stay where they fell.
    """

    merged = runs[0]

    for other in runs[1:]:
        matcher = difflib.SequenceMatcher(None, [_signature(s) for s in merged], [_signature(s) for s in other], autojunk=False)
        combined = []

        for tag, a0, a1, b0, b1 in matcher.get_opcodes():
            if tag != "equal":
                combined.extend(merged[a0:a1])
                combined.extend(other[b0:b1])
                continue

            for step, seen in zip(merged[a0:a1], other[b0:b1]):
                step["seen"] += 1
                step["expect"] = _common_checks(step.get("expect") or [], seen.get("expect") or [])
                step["consequential"] = step.get("consequential") or seen.get("consequential", False)

                if step["kind"] == "type":
                    step["samples"].extend(seen["samples"])
                    step["secret"] = step["secret"] or seen["secret"]
                    step["secret_reason"] = step.get("secret_reason") or seen.get("secret_reason")

                combined.append(step)

        merged = combined

    return merged


def _check_key(check):
    return (check["type"], check.get("value"), check.get("role"), replay._norm(check.get("name")))


def _common_checks(first, second):
    """The checks both demonstrations' result satisfied: what a run can rely on seeing."""

    theirs = {_check_key(c) for c in second}
    return [c for c in first if _check_key(c) in theirs]


# Which field of each kind of result check holds text a changing value can
# appear in (see _follow_param; replay.unmet fills it back in).
CHECK_TEXT = {"element": "name", "window": "value", "active_window": "value"}


def _follow_param(skill, param):
    """
    Make a result check follow a value that changes every run. What a
    demonstration typed often comes back in what the screen then says ("Saved
    Initech"); left as it was recorded, every run with another value would be
    told its result never appeared. The example becomes `{name}`, filled in from
    the run's own value (replay.fill_name).
    """

    example = " ".join((param.get("example") or "").split())
    if len(example) < 2:
        return

    pattern = re.compile(re.escape(example), re.IGNORECASE)

    for step in skill["steps"]:
        for check in step.get("expect") or []:
            # The text a check compares: an element's name, or a window's
            # title - a search typed into a browser comes back as the title
            # "<what was typed> - Google Search - Chromium", and a run with
            # another value halted there after the search had worked.
            key = CHECK_TEXT.get(check["type"])
            if key and pattern.search(check.get(key) or ""):
                check[key] = pattern.sub("{" + param["name"] + "}", check[key])


def _generalise(steps, demos):
    """
    Turn typed text into parameters, in step order, and return them. A secret
    field's step becomes a manual one instead: no recording has its value,
    and a skill must not need it in the chat either.
    """

    used, params = set(), []

    for step in steps:
        if step["kind"] == "upload" and step.get("files"):
            label = step.get("label") or "file to upload"
            name = replay._param_name(label, used)
            step["param"] = name
            sample = step["files"][0] if step["files"] else "document.pdf"
            params.append({
                "name": name,
                "label": label,
                "default": sample,
                "example": sample,
                "varies": True,
                "step": step["n"],
                "constraint": None,
            })
            step["summary"] = replay._summary(step)
            continue

        if step["kind"] != "type":
            continue

        if step["secret"]:
            step["kind"] = "manual"
            step["reason"] = f"type your {step['label'] or 'secret'} yourself - it is never recorded"
            for key in ("param", "samples"):
                step.pop(key, None)
            step["expect"] = []
            step["summary"] = replay._summary(step)
            continue

        name = replay._param_name(step["label"], used)
        old = step["param"]
        step["param"] = name

        for check in step.get("expect") or []:
            if check.get("param") == old:
                check["param"] = name

        samples = step["samples"]
        varies = True if len(set(samples)) > 1 else None

        params.append({
            "name": name,
            "label": step["label"],
            "default": None if varies else samples[0],
            "example": samples[0],
            # True: learned to change every run (demonstrations differed, or a
            # person said so). False: a person said it is always this text.
            # None: not asked yet.
            "varies": varies,
            "step": step["n"],
            "constraint": constraints.infer(step["label"], (step.get("target") or {}).get("role"), samples[0]),
        })
        step["summary"] = replay._summary(step)

    return params


def _find_loops(steps, demos):
    """
    Blocks of steps repeated back to back, with different text typed each
    time: [{"id", "length", "reps", "blocks": [[step ids] per repetition],
    "enabled": None}], earliest first. The longest run of repetition wins,
    and the shorter block when two cover the same steps.
    """

    signatures = [_signature(step) for step in steps]
    loops, i = [], 0

    while i < len(steps):
        best = None

        for length in range(2, MAX_LOOP_BLOCK + 1):
            if i + 2 * length > len(steps):
                break

            reps = 1
            while signatures[i + reps * length:i + (reps + 1) * length] == signatures[i:i + length]:
                reps += 1

            if reps >= 2 and _loops_over_data(steps, i, length, reps, demos):
                if best is None or reps * length > best[0] * best[1]:
                    best = (reps, length)

        if best is None:
            i += 1
            continue

        reps, length = best
        blocks = [[s["id"] for s in steps[i + r * length:i + (r + 1) * length]] for r in range(reps)]
        loops.append({"id": f"l{len(loops) + 1}", "length": length, "reps": reps, "blocks": blocks, "enabled": None})
        i += reps * length

    return loops


def _loops_over_data(steps, start, length, reps, demos):
    """Whether repeating this block is about data: it types something different each time."""

    blocks = [steps[start + r * length:start + (r + 1) * length] for r in range(reps)]

    if any(s["kind"] in ("manual", "open") or s.get("seen", demos) < demos for block in blocks for s in block):
        return False

    for position in range(length):
        if blocks[0][position]["kind"] == "type" and len({block[position]["samples"][0] for block in blocks}) > 1:
            return True
        if blocks[0][position]["kind"] == "upload" and len({tuple(block[position].get("files", [])) for block in blocks}) > 1:
            return True

    return False


def _starts(steps, programs):
    """Where the recording began, as far as it is stable: a window title and/or a page's shape."""

    first = steps[0]

    if first["kind"] in ("open", "manual"):
        return None

    events = next((events for _, events, program in programs if program["session"] == first["session"]), [])
    recorded = next((e for e in events if e.get("i") == first.get("event")), {})

    window = replay._stable_title(first.get("window"))
    url = replay.url_shape(recorded.get("url"))

    if not (window or url):
        return None

    return {"window": window, "url": url, "check": None}


def _notes(steps, programs):
    """A person's own written explanations, each with the step it came before."""

    notes = []

    for index, (meta, events, program) in enumerate(programs):
        own = [s for s in steps if s.get("session") == program["session"]]

        for event in events:
            text = event.get("text")
            if event.get("kind") != "note" or not text:
                continue

            following = next((s for s in own if (s.get("event") or 0) > event["i"]), None) if index == 0 else None
            notes.append({"text": text, "before": following["id"] if following else None})

    return notes


def start_checks(skill):
    """What a run confirms about the screen before it begins - none unless a person said to check."""

    starts = skill.get("starts") or {}

    if not starts.get("check"):
        return []

    checks = []
    if starts.get("url"):
        checks.append({"type": "url", "value": starts["url"]})
    if starts.get("window"):
        checks.append({"type": "window", "value": starts["window"]})

    return checks


# ============================================================
# Saying what was learned
# ============================================================

def _line(step):
    line = f"{step['n']}. {step['summary']}"

    if step.get("optional"):
        line += " [only if it is there]"
    if step.get("repeat"):
        line += " [for each item]"
    if step.get("consequential"):
        line += " [stops for approval]"

    return line


def overview(skill, budget=MAX_OVERVIEW_CHARS):
    """The skill's steps, one line each, within `budget` characters."""

    steps = skill["steps"]
    lines, used = [], 0

    for step in steps:
        line = _line(step)
        if used + len(line) + 1 > budget:
            lines.append(f"(+{len(steps) - len(lines)} more steps)")
            break
        lines.append(line)
        used += len(line) + 1

    return "\n".join(lines)


def _notes_text(skill):
    notes = [n["text"] for n in skill.get("notes") or []]
    if not notes:
        return ""
    return "Your own notes while recording: " + " / ".join(el._quote(n, 160) for n in notes[:4]) + "."


# ============================================================
# Questions
# ============================================================

def _step_by_id(skill, step_id):
    return next((s for s in skill["steps"] if s["id"] == step_id), None)


def questions(skill):
    """Every question this skill has, in the order they are asked, with their answers filled in where given."""

    found = [_goal(skill)]

    starts = skill.get("starts")
    if starts:
        found.append(_start(starts))

    params = {p["name"]: p for p in skill["params"]}

    # A loop nobody has answered about decides what its steps' typed fields
    # are (one value, or a list), so they are not asked about until it is.
    undecided = [loop for loop in skill.get("loops") or [] if loop["enabled"] is None]
    in_loop = {step_id for loop in undecided for block in loop["blocks"] for step_id in block}
    loop_at = {loop["blocks"][0][0]: loop for loop in undecided}

    for step in skill["steps"]:
        if step["id"] in loop_at:
            found.append(_loop(skill, loop_at[step["id"]]))
        if step["id"] in in_loop:
            continue

        param = params.get(step.get("param"))
        if param and param["varies"] is None:
            found.append(_variable(skill, step, param))

        if step.get("seen", skill["demos"]) < skill["demos"]:
            found.append(_optional(skill, step))

        if step["kind"] not in replay.REPLAYABLE:
            found.append(_unsupported(step))
        elif step["kind"] in CLICKS and not step.get("target") and step.get("point"):
            found.append(_coordinates(step))

    found.append(_confirm(skill))
    return found


def next_question(skill):
    """The first question not answered yet, or None."""

    return next((q for q in questions(skill) if q["id"] not in skill["answers"]), None)


def _question(qid, kind, text, step=None):
    return {"id": qid, "kind": kind, "step": step, "text": text, "default": CAUTIOUS.get(kind)}


def _goal(skill):
    sources = f" from {skill['demos']} demonstrations" if skill["demos"] > 1 else ""
    parts = [
        f"I watched \"{skill['title']}\" and learned {len(skill['steps'])} steps{sources}:",
        overview(skill),
    ]

    notes = _notes_text(skill)
    if notes:
        parts.append(notes)

    learned = [p for p in skill["params"] if p["varies"]]
    if learned:
        parts.append("What differed between your demonstrations, so it changes each run: "
                     + ", ".join(f"{p['name']} ({p['label']})" for p in learned) + ".")

    parts.append("A few questions before I try to do it myself. First: when is this task finished - "
                 "what should be on the screen, or true, at the end? A sentence is fine, or reply `skip`.")

    return _question("goal", "goal", "\n".join(parts))


def _start(starts):
    where = " at ".join(x for x in (el._quote(starts["window"], 60) if starts.get("window") else "", starts.get("url") or "") if x)
    text = (f"You began with {where} on the screen. Should I check that before I start, and ask you if it "
            "is not there? Reply `yes` or `no`.")
    return _question("start", "start", text)


def _variable(skill, step, param):
    samples = [s for s in step["samples"] if s]
    same = " (both times you typed the same)" if len(samples) > 1 else ""
    target = step.get("target") or {}
    where = f"{target['role']} {el._quote(target.get('name') or '', 50)}" if target.get("role") else "the focused field"
    text = (f"Step {step['n']}: you typed {el._quote(param['example'] or '', 60)} into {where}{same}. Is that the "
            "same every time, or does it change from run to run (a different customer, amount, ...)? "
            "Reply `same` or `changes`.")
    return _question(f"variable:{step['id']}", "variable", text, step["id"])


def _loop(skill, loop):
    by_id = {s["id"]: s for s in skill["steps"]}
    first = [by_id[step_id] for step_id in loop["blocks"][0] if step_id in by_id]

    typed = []
    for position, step in enumerate(first):
        if step["kind"] == "type":
            values = [by_id[block[position]]["samples"][0] for block in loop["blocks"]]
            typed.append(f"{step['param']}: " + ", ".join(el._quote(v or "", 30) for v in values))

    text = (f"Steps {first[0]['n']}-{first[-1]['n']} ({'; '.join(s['summary'] for s in first)}) were done "
            f"{loop['reps']} times in a row with different text each time - {' / '.join(typed)}. Should I do them once "
            "per item in a list you give me each run? Reply `each`, or `fixed` if it was just those "
            f"{loop['reps']} times.")

    return _question(f"loop:{loop['id']}", "loop", text)


def _optional(skill, step):
    text = (f"Step {step['n']} ({step['summary']}) happened in {step['seen']} of {skill['demos']} demonstrations. "
            "Does it only sometimes happen - I would do it only when I can find it - or is it always part of the "
            "task? Reply `sometimes` or `always`.")
    return _question(f"optional:{step['id']}", "optional", text, step["id"])


def _coordinates(step):
    text = (f"Step {step['n']} ({step['summary']}) was recorded as a click at a spot on the screen with no named "
            "control, which only works if the window is exactly where it was. Reply `keep` to try it as recorded, "
            "`skip` to leave it out, or `manual` to have me stop and let you do it.")
    return _question(f"coordinates:{step['id']}", "coordinates", text, step["id"])


def _unsupported(step):
    text = (f"Step {step['n']} ({step['summary']}) is something I cannot repeat on my own. Reply `skip` to leave it "
            "out or `manual` to have me stop there and let you do it.")
    return _question(f"unsupported:{step['id']}", "unsupported", text, step["id"])


def _confirm(skill):
    parts = ["That is everything I wanted to ask."]

    gated = [step for step in skill["steps"] if step.get("consequential")]
    if gated:
        parts.append("I will always stop and ask you before: " + "; ".join(f"{s['n']}. {s['summary']}" for s in gated) + ".")

    by_hand = [step for step in skill["steps"] if step["kind"] == "manual"]
    if by_hand:
        parts.append("I will stop and let you do: " + "; ".join(f"{s['n']}. {s['summary']}" for s in by_hand) + ".")

    needed = [p for p in skill["params"] if p["varies"]]
    if needed:
        parts.append("Each run I need: " + ", ".join(f"{p['name']} ({p['label']})" for p in needed) + ".")

    if skill.get("done_when"):
        parts.append(f"It is finished when: {el._quote(skill['done_when'], 200)}.")

    if skill.get("named"):
        parts.append(f"I will save it as \"{skill['title']}\". Reply `yes` to save it, a different name to call it "
                     "that instead, or `no` to throw it away.")
    else:
        parts.append(f"What should I call it? Reply with a name to save it (`yes` keeps \"{skill['title']}\"), "
                     "or `no` to throw it away.")

    return _question("confirm", "confirm", " ".join(parts))


# ============================================================
# Answers
# ============================================================

def _choose(kind, text):
    """The one option a reply names, or None when it names none or more than one."""

    hits = {option for option, pattern in CHOICES[kind] if pattern.search(text)}
    return hits.pop() if len(hits) == 1 else None


def answer(skill, question, reply):
    """
    Apply a person's reply to a question. Returns {"understood", "note",
    "decision"}: `understood` False means ask it again; `note` is something to
    say before the next question; `decision` is set by the last one - "save",
    "discard" or "hold".
    """

    qid, kind = question["id"], question["kind"]
    text = (reply or "").strip()

    if kind == "goal":
        skill["done_when"] = None if NOTHING.match(text) else text[:300]
        skill["answers"][qid] = text[:300]
        return {"understood": True, "note": "", "decision": None}

    # Anything but a yes or a no is the name to save it as; a reply with
    # nothing to name it by is unclear, like any other.
    if kind == "confirm" and not KEEP_NAME.match(text) and not THROW_AWAY.match(text) and rename(skill, text):
        text = "yes"

    choice = _choose(kind, text)
    note = ""

    if choice is None:
        tries = skill["unclear"][qid] = skill["unclear"].get(qid, 0) + 1

        if tries < MAX_UNCLEAR:
            return {"understood": False, "note": "I did not catch that.", "decision": None}

        choice = question["default"]
        note = f"I could not tell what you meant, so I took the cautious answer ({choice})."
        text = f"(assumed {choice})"

    decision = _apply(skill, question, choice)
    skill["answers"][qid] = text[:300]

    # Not saved, not thrown away: the question stays open for the next time
    # this skill is taught.
    if decision == "hold":
        skill["answers"].pop(qid, None)
        skill["unclear"].pop(qid, None)

    return {"understood": True, "note": note, "decision": decision}


def rename(skill, given):
    """
    Call a skill what a person named it: its title becomes the name as typed,
    its file name a free slug of it. The goal (what a request is matched
    against) stays what was recorded. False when the name has nothing to slug.
    """

    given = " ".join((given or "").split()).strip("`\"' ")[:MAX_NAME]
    if not re.search(r"[A-Za-z0-9]", given):
        return False

    taken = {s["name"] for s in skills.all_skills()} - {skill["name"]}
    skill["name"] = skills.unique_name(given, taken)
    skill["title"] = given
    skill["named"] = True
    return True


def _apply_loop(skill, loop, choice):
    """
    `each`: keep one repetition, and make what it types a list a run is given.
    `fixed`: leave every step as recorded, to be asked about like any other.
    """

    if choice != "each":
        loop["enabled"] = False
        return

    by_id = {s["id"]: s for s in skill["steps"]}
    first = loop["blocks"][0]
    dropped = {step_id for block in loop["blocks"][1:] for step_id in block}

    for position, step_id in enumerate(first):
        step = by_id[step_id]
        step["repeat"] = loop["id"]

        if step.get("param"):
            examples = [by_id[block[position]]["samples"][0] for block in loop["blocks"]]
            param = next(p for p in skill["params"] if p["name"] == step["param"])
            param.update(varies=True, default=None, list=True, examples=examples, example=examples[0])

    skill["steps"] = [s for s in skill["steps"] if s["id"] not in dropped]
    skills.renumber(skill)

    loop["enabled"] = True
    loop["blocks"] = [first]


def _apply(skill, question, choice):
    kind = question["kind"]
    step = _step_by_id(skill, question["step"]) if question["step"] else None

    if kind == "loop":
        _apply_loop(skill, next(l for l in skill["loops"] if f"loop:{l['id']}" == question["id"]), choice)

    elif kind == "start":
        skill["starts"]["check"] = choice == "yes"

    elif kind == "variable":
        param = next(p for p in skill["params"] if p["name"] == step["param"])
        param["varies"] = choice == "changes"
        param["default"] = param["example"] if choice == "same" else None

        if param["varies"]:
            _follow_param(skill, param)

    elif kind == "optional":
        step["optional"] = choice == "sometimes"
        step["summary"] = replay._summary(step)

    elif kind in ("coordinates", "unsupported"):
        if choice == "skip":
            skill["steps"].remove(step)
            skills.renumber(skill)
        elif choice == "manual":
            what = step["summary"]
            step["kind"] = "manual"
            step["reason"] = f"a person does this one ({what})"
            step["summary"] = replay._summary(step)
            step["expect"] = []
            step["consequential"] = False

    elif kind == "confirm":
        if choice == "yes":
            skill["status"] = skills.READY
            skill["learned_at"] = time.time()
        return {"yes": "save", "no": "discard"}.get(choice, "hold")

    return None
