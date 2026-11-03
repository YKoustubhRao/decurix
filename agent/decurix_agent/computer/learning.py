import copy
import difflib
import math
import re
import time

from decurix_agent.computer import elements as el
from decurix_agent.computer import constraints, launchers, replay, skills


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
MAX_LOOP_BLOCK = 60  # a document entered as one record is 30-50 steps
MAX_LOOP_GAP = 60    # what a person may do between two items: read the next document, make a record it needs

YES = re.compile(r"^\W*(yes|y|yeah|yep|ok|okay|sure|save|confirm|check|please|looks good|go ahead)\b", re.I)
NO = re.compile(r"^\W*(no|n|nope|nah|discard|cancel|throw|delete|don'?t|do not)\b", re.I)
NOTHING = re.compile(r"^\W*(skip|none|n/?a|nothing|no|-)\W*$", re.I)

KEEP_NAME = re.compile(
    r"^\W*(?:(?:yes|y|yeah|yep|ok|okay|sure|confirm|looks good|go ahead)"
    r"(?:[\s,.!-]+(?:please|thanks?|save(?: it)?|keep(?: it)?))*|(?:save|keep)(?: it| that)?(?: (?:for good|permanently))?|perma(?:nent(?:ly)?)?|save (?:it )?perma(?:nent(?:ly)?)?)\W*$", re.I)
THROW_AWAY = re.compile(
    r"^\W*(?:(?:no|n|nope|nah)(?:[\s,.!-]+(?:thanks?|throw (?:it )?away|discard(?: it)?|delete(?: it)?|"
    r"don'?t save(?: it)?))*|discard(?: it)?|cancel|throw (?:it )?away|delete(?: it)?|"
    r"(?:don'?t|do not) save(?: it)?)\W*$", re.I)
# The longest skill name a person can give, as build() keeps it.
MAX_NAME = 80
# The longest explanation of a task kept, and of what one step is for.
MAX_EXPLANATION = 1500
MAX_INTENT = 300

# The last question takes a name, so a yes or a no there has to be the whole reply: "Save invoice" and "Delete old
# files" are names, not answers. (Every other question's plain answers are PLAIN.)
CHOICES = {
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
    # Asking costs a person one reply; a task that should have asked costs more.
    "approval": "ask",
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
    tidied = []
    for meta, events in sources:
        program = replay.compile_program(meta, events)
        for note in _tidy(program, events):
            if note not in tidied:
                tidied.append(note)
        if program["steps"]:
            programs.append((meta, events, program))

    if not programs:
        raise ValueError("there is nothing in that recording to learn from - no action on the screen was kept")

    base_meta = programs[0][0]
    steps = _merge([_annotate(program) for _, _, program in programs])

    # Numbers follow the steps' order and change when one is removed; an ID
    # is what a question (and its saved answer) names a step by, and does not.
    for n, step in enumerate(steps, 1):
        step["id"] = f"s{n}"
        step["n"] = n

    for note in _name_points(steps):
        if note not in tidied:
            tidied.append(note)

    params = _generalise(steps)
    loops = _find_loops(steps, len(programs))
    title = (base_meta.get("title") or base_meta.get("description") or "").strip() or "A recorded task"
    # What the person asked for when they recorded it, even after a rename.
    goal = (base_meta.get("task") or "").strip() or title

    # A name the person gave is what the skill is called and shown as; the
    # recording's title stays its goal (what a request is matched against,
    # mimic.plan). Either way the file name is free: naming a new
    # skill after one that exists must not overwrite it.
    given = " ".join((name or "").split())[:MAX_NAME]
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
        "documents": _documents(programs),
        "params": params,
        "steps": steps,
        "loops": loops,
        "learned": [],
        "tidied": tidied,
        "answers": {},
        "unclear": {},
        # None for a skill taught before this was asked; "ask" or "go" once a person has said.
        "approval": None,
    }

    return skills.renumber(skill)


# How close together presses on one spot must be to count as one attempt to
# do one thing. Measured on a real demonstration: opening an app from a desktop
# icon on a 1-vCPU desktop was recorded as click, an 11 px "drag", double
# click, click, click - 10 s of a person waiting for a slow launch - and the
# app opened three times.
REPEAT_PIXELS = 16
REPEAT_SECONDS = 4.0
# How far apart two runs of typing into the same field can be and still be one
# piece of text the recorder happened to split ("fl" + "amingo breeding season").
SPLIT_TYPING_SECONDS = 10.0
PRESSES = {"click", "double_click", "drag"}


def _when(step, events):
    i = step.get("event")
    if isinstance(i, int) and 0 <= i < len(events):
        return events[i].get("t")
    return None


def _target_key(step):
    target = step.get("target") or {}
    return (target.get("role"), replay._norm(target.get("name")), tuple(target.get("path") or ()))


def _press_point(step):
    """Where a press landed, or None when it was not one: a drag counts only when it barely moved."""

    point = step.get("point")
    if step["kind"] == "drag":
        end = step.get("end")
        if not point or not end or math.hypot(end[0] - point[0], end[1] - point[1]) > REPEAT_PIXELS:
            return None
    return point


def _same_spot(a, b):
    if _target_key(a) != _target_key(b) or (a.get("window") or "") != (b.get("window") or ""):
        return False
    pa, pb = _press_point(a), _press_point(b)
    if pa is not None and pb is not None:
        return math.hypot(pa[0] - pb[0], pa[1] - pb[1]) <= REPEAT_PIXELS
    # Presses on one named control recorded without a position (a drag that
    # really moved is never one).
    return bool(a.get("target")) and "drag" not in (a["kind"], b["kind"])


def _repeated_presses(steps, events, start):
    """How many steps from `start` are one burst of presses on one spot that opens something (see _tidy)."""

    end = start + 1
    while end < len(steps):
        prev, step = steps[end - 1], steps[end]
        t0, t1 = _when(prev, events), _when(step, events)
        # A press whose own result showed is its own step - except a drag that
        # barely moved, whose only result is a nudged icon (XFCE moves a
        # desktop icon on an 11 px drag).
        had_result = bool(prev.get("expect")) and not (prev["kind"] == "drag" and _press_point(prev))
        if (step["kind"] not in PRESSES or not _same_spot(steps[start], step) or had_result
                or t0 is None or t1 is None or t1 - t0 > REPEAT_SECONDS):
            break
        end += 1

    burst = steps[start:end]
    if (len(burst) < 2 or burst[0]["kind"] not in PRESSES or not any(s["kind"] == "double_click" for s in burst)
            or any(s.get("consequential") for s in burst)):
        return 1
    return len(burst)


def _tidy(program, events):
    """
    Take the noise a person makes out of a program, in place, before anything
    is asked about it; returns what was changed, in words. The recording
    itself is untouched (a /replay still does exactly what was recorded).

    - A burst of presses on one spot around a double-click - clicks, a press
      that barely moved, more clicks while a slow app starts - is one attempt
      to open that thing: it becomes that double-click, expecting what the
      last press produced. Replaying every press opened the app three times.
      Only a burst with a double-click in it: two single clicks on one spot
      can be meant (a counter, a toggle), a double-click already opens.
    - A small drag on the bare desktop only nudged an icon and is left out.
    - Two runs of typing into the same field, one straight after the other,
      are one piece of text the recorder split: "fl" and "amingo breeding
      season" would otherwise be asked about as two values. Never for a
      secret.
    """

    params = {p["name"]: p for p in program["params"]}
    notes = []
    tidy = []
    i = 0

    steps = program["steps"]

    while i < len(steps):
        length = _repeated_presses(steps, events, i)
        if length > 1:
            burst = steps[i:i + length]
            # The double-click is what opened it: its position, and the picture
            # of the screen just before it (what a replay looks for), stand for
            # the burst.
            one = copy.deepcopy(next(s for s in burst if s["kind"] == "double_click"))
            one.pop("end", None)
            one["expect"] = copy.deepcopy(burst[-1].get("expect") or [])
            one["summary"] = replay._summary(one)
            tidy.append(one)
            notes.append(f"{length} quick presses on the same spot ({', '.join(s['kind'].replace('_', ' ') for s in burst)}) "
                         "became one double click")
            i += length
            continue
        tidy.append(steps[i])
        i += 1

    # A small drag on the bare desktop that no burst took in only nudged an
    # icon (XFCE keeps them on a grid): not something to do again. Found live:
    # an icon pressed, held 17 s and moved 16 px before its double click -
    # just past REPEAT_PIXELS, and far past REPEAT_SECONDS from where the
    # press began.
    kept = [step for step in tidy if not replay._desktop_nudge(step)]
    if len(kept) != len(tidy):
        notes.append(f"{len(tidy) - len(kept)} small drag(s) on the desktop that only nudged an icon were left out")
    tidy = kept

    joined = []
    for step in tidy:
        prev = joined[-1] if joined else None
        if (prev and prev["kind"] == "type" and step["kind"] == "type" and not prev.get("expect")
                and _target_key(prev) == _target_key(step) and (prev.get("window") or "") == (step.get("window") or "")):
            first, second = params.get(prev.get("param")), params.get(step.get("param"))
            t0, t1 = _when(prev, events), _when(step, events)
            if (first and second and not first.get("secret") and not second.get("secret")
                    and first.get("default") is not None and second.get("default") is not None
                    and t0 is not None and t1 is not None and t1 - t0 <= SPLIT_TYPING_SECONDS):
                notes.append(f"typing split in two ({el._quote(first['default'], 30)} + {el._quote(second['default'], 30)}) "
                             "became one")
                first["default"] = first["default"] + second["default"]
                prev["expect"] = copy.deepcopy(step.get("expect") or [])
                params.pop(second["name"], None)
                continue
        joined.append(step)

    if notes:
        for n, step in enumerate(joined, 1):
            step["n"] = n
            if step.get("param") in params:
                params[step["param"]]["step"] = n
        program["steps"] = joined
        program["params"] = [p for p in program["params"] if p["name"] in params]

    return notes


NAMED_BY_NEXT = ("type", "key", "select", "upload")


def _inside(point, box, slack=4):
    return (box and len(box) == 4 and box[0] - slack <= point[0] <= box[0] + box[2] + slack
            and box[1] - slack <= point[1] <= box[1] + box[3] + slack)


def _name_points(steps):
    """
    Name presses the recorder could only keep as a place on the screen, from
    what the demonstration itself shows (in place); returns what was named,
    in words. Nothing is asked about a press this settles.

    - A press straight before typing (or a key, a choice, an upload) into a
      control whose recorded box holds the press point was on that control:
      found live (2026-10-16), the click that put the cursor in a chat's
      message box was recorded while the chat was still opening, as "click
      664,709", and the very next step typed into the message box drawn right
      there.
    - A press on the bare desktop that opened a window one launcher makes is
      that launcher's icon (replay opens it by its launcher, whatever the
      desktop looks like).
    """

    notes = []
    for index, step in enumerate(steps):
        if step["kind"] not in ("click", "double_click") or step.get("target") or not step.get("point"):
            continue
        following = steps[index + 1] if index + 1 < len(steps) else None
        locate = (following or {}).get("locate") or {}
        if (following and following.get("target") and following["kind"] in NAMED_BY_NEXT
                and (following.get("window") or "") == (step.get("window") or "")
                and (not step.get("screen") or not locate.get("screen") or list(step["screen"]) == list(locate["screen"]))
                and _inside(step["point"], locate.get("box"))):
            step["target"] = copy.deepcopy(following["target"])
            step["locate"] = copy.deepcopy(locate)
            step.pop("look", None)
            step["summary"] = replay._summary(step)
            notes.append(f"the press at step {step['n']} was on {replay._summary(following).split(' into ')[-1]}, "
                         f"which step {following['n']} used")
            continue
        if replay._on_desktop(step):
            window = replay._opened_window(step) or (replay._stable_title(following.get("window")) if following else None)
            try:
                entry = launchers.for_window(window) if window else None
            except Exception:
                entry = None
            if entry:
                step["launcher"] = entry["name"]
                step["summary"] = replay._summary(step)
                notes.append(f"the press at step {step['n']} was the desktop icon {el._quote(entry['name'], 60)}")
    return notes


def _annotate(program):
    """A program's steps as merge material: each remembers which session it is from and what it typed."""

    params = {p["name"]: p for p in program["params"]}
    steps = []

    for original in program["steps"]:
        step = copy.deepcopy(original)
        step["seen"] = 1
        step["session"] = program["session"]

        if step["kind"] == "type" or (step["kind"] == "select" and step.get("param")):
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

                if step["kind"] == "upload":
                    step["file_samples"] = (step.get("file_samples") or [step.get("files") or []]) + \
                        (seen.get("file_samples") or [seen.get("files") or []])

                if step["kind"] == "type" or (step["kind"] == "select" and "samples" in step and "samples" in seen):
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


def _generalise(steps):
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
            # Asked about like a typed value (the grouped question): the file
            # a run uploads usually comes with that run's documents (the next
            # new joiner's offer letter), but may be the same every time.
            params.append({
                "name": name,
                "label": label,
                "default": None,
                "example": sample,
                # Demonstrations that uploaded different files: it changes, as
                # differing typed text does. One demonstration: asked.
                "varies": True if len({tuple(f) for f in step.get("file_samples") or []}) > 1 else None,
                "step": step["n"],
                "constraint": None,
                "file": True,
            })
            step["summary"] = replay._summary(step)
            continue

        if step["kind"] != "type" and not (step["kind"] == "select" and step.get("samples")):
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
            "constraint": None if step["kind"] == "select"
            else constraints.infer(step["label"], (step.get("target") or {}).get("role"), samples[0]),
        })
        if step["kind"] == "select":
            # Chosen from a drop-down's list, not typed: asked about and
            # reported in those words.
            params[-1]["choice"] = True
        step["summary"] = replay._summary(step)

    return params


def _find_loops(steps, demos):
    """
    Blocks of steps repeated, with different text typed each time: [{"id", "length", "reps", "blocks": [[step ids] per
    repetition], "between": [[step ids] between one repetition and the next], "enabled": None}], earliest first. The
    longest run of repetition wins, and the shorter block when two cover the same steps.

    Repetitions need not be back to back. What the person did between two of them - opened the next document to read
    it, made a record the next one needed - is kept apart as `between`: not part of every item, so never replayed per
    item, and kept for the model to read when such a case comes up. What they did right before EVERY repetition after
    the first (brought up a fresh form) is the way to the next item, and belongs to the block: the first repetition is
    then shorter by that much, the form having been reached another way. Measured (2026-11-01): a demonstration of five
    bills was 292 steps - five copies of a 41-step block, with 6 to 37 steps between them - and a search for blocks of
    at most 12 steps back to back found none of it, so the task stayed the model's step by step every month.
    """

    signatures = [_signature(step) for step in steps]
    n = len(steps)

    def candidate(reps, length):
        """(steps covered, reps, length, gaps, prefix) for repetitions of a block, or None when it is not about data."""
        # An item does not begin with the person going somewhere different each time: an address typed differently
        # before each repetition (the next document opened to read it) is what came between, not the item's first
        # step, however exactly the window matches. The block begins after it and the key press that sent it.
        while length >= 3 and (shift := _reading_shift(steps, reps)):
            reps, length = [r + shift for r in reps], length - shift
        # Nor does an item end with it: the next document opened to read it (or, after the last item, the report
        # opened to check them) is typed the same way, so every repetition matched with it at its end.
        while length >= 3 and (cut := _trailing_reading(steps, reps, length)):
            length -= cut
        if len(reps) < 2 or length < 2 or not _loops_over_data([steps[r:r + length] for r in reps], demos):
            return None
        # The steps every gap ends with are the way to the next item: they count as the block's too.
        gaps = [list(range(reps[r] + length, reps[r + 1])) for r in range(len(reps) - 1)]
        prefix = _common_tail(steps, signatures, gaps)
        return (len(reps) * length + (len(reps) - 1) * prefix, reps, length, gaps, prefix)

    candidates = []
    for length in range(2, min(MAX_LOOP_BLOCK, n // 2) + 1):
        windows = {}
        for k in range(n - length + 1):
            windows.setdefault(tuple(signatures[k:k + length]), []).append(k)
        for positions in windows.values():
            if len(positions) < 2:
                continue
            first = positions[0]
            reps, end = [first], first + length
            for j in positions[1:]:
                if j < end:
                    continue
                if j - end > MAX_LOOP_GAP:
                    break
                reps.append(j)
                end = j + length
            if (c := candidate(reps, length)) is not None:
                candidates.append(c)

    # The loop that covers the most steps is the task's shape; the others fit around it. Found live (2026-11-01): a
    # 4-step "type an address, Enter" repeated 7 times over a recording of five bills began earlier than the first
    # bill's block, was taken for the loop, and the five 41-step bill blocks became "steps between" and were dropped.
    # A loop that overlaps one already taken is cut to the repetitions before the overlap, and tried again as that.
    loops, taken = [], set()
    candidates.sort(key=lambda c: (-c[0], c[1][0]))
    while candidates:
        covered, reps, length, gaps, prefix = candidates.pop(0)
        span = [set(range(start - (prefix if r else 0), start + length)) for r, start in enumerate(reps)]
        clear = 0
        while clear < len(reps) and not (span[clear] & taken) and (clear == 0 or not (set(gaps[clear - 1]) & taken)):
            clear += 1
        if clear < len(reps):
            if clear >= 2 and (c := candidate(reps[:clear], length)) is not None:
                candidates.append(c)
                candidates.sort(key=lambda c: (-c[0], c[1][0]))
            continue
        for s in span:
            taken |= s
        for gap in gaps:
            taken |= set(gap)
        blocks = [[steps[k]["id"] for k in sorted(s)] for s in span]
        between = [[steps[k]["id"] for k in gap[:len(gap) - prefix]] for gap in gaps]
        loops.append({"length": length + prefix, "reps": len(reps), "blocks": blocks, "between": between, "enabled": None})

    loops.sort(key=lambda loop: steps.index(next(s for s in steps if s["id"] == loop["blocks"][0][0])))
    for i, loop in enumerate(loops, 1):
        loop["id"] = f"l{i}"
    return loops


def _is_address(text):
    """Whether typed text is a place to go rather than a value: a web address or an absolute path."""

    t = str(text or "").strip().lower()
    return t.startswith(("http://", "https://", "file://", "/", "~/")) or bool(re.match(r"^[a-z]:[\\/]", t))


def _differing_addresses(steps, at):
    """Whether the steps at these positions each type an address, a different one each time."""

    if any(k < 0 or k >= len(steps) for k in at):
        return False
    typed = [steps[k] for k in at]
    if any(s["kind"] != "type" or not _is_address((s.get("samples") or [""])[0]) for s in typed):
        return False
    return len({(s.get("samples") or [""])[0] for s in typed}) > 1


def _reading_shift(steps, reps):
    """
    How many leading steps of a block are the person going somewhere different before each repetition (an address
    typed differently each time, and the key press that sent it): 0 when the block begins with the item itself.
    """

    if _differing_addresses(steps, reps):
        return 2 if all(steps[r + 1]["kind"] == "key" for r in reps if r + 1 < len(steps)) else 1
    if all(steps[r]["kind"] == "key" for r in reps) and _differing_addresses(steps, [r - 1 for r in reps]):
        return 1
    return 0


def _trailing_reading(steps, reps, length):
    """How many steps at a block's end type an address differently each time (with the key press that sent it): 0, 1 or 2."""

    last = [r + length - 1 for r in reps]
    if _differing_addresses(steps, last):
        return 1
    if all(steps[k]["kind"] == "key" for k in last) and _differing_addresses(steps, [k - 1 for k in last]):
        return 2
    return 0


def _common_tail(steps, signatures, gaps):
    """
    How many steps every gap ends with the same way (0 when a gap is empty): the steps that bring the next item's form
    up. The same way means typed the same too - the way to a fresh form is the same each time, and an address typed
    differently before each item is the person opening that item's document to read it, which no run repeats.
    """

    if not gaps or any(not gap for gap in gaps):
        return 0

    def loose(k):
        # A key press goes to whatever has focus, which the recording names differently each time (the last field of
        # the form just saved): the press itself is what is the same.
        return ("key", *(steps[k].get("keys") or [])) if steps[k]["kind"] == "key" else signatures[k]

    tail = 0
    while all(len(gap) > tail for gap in gaps):
        at = [gap[-1 - tail] for gap in gaps]
        if len({loose(k) for k in at}) != 1:
            break
        if steps[at[0]]["kind"] == "type" and len({(steps[k].get("samples") or [None])[0] for k in at}) != 1:
            break
        tail += 1
    # A key press completes the typing before it: the way to the form begins with something else.
    while tail > 0 and steps[gaps[0][-tail]]["kind"] == "key":
        tail -= 1
    return tail


def _aligned(blocks, position, length):
    """The steps of each block at `position` of a block of `length` - a shorter (first) block aligned from its end."""

    found = []
    for block in blocks:
        at = position - (length - len(block))
        if at >= 0:
            found.append(block[at])
    return found


def _loops_over_data(blocks, demos):
    """Whether repeating this block is about data: it types something different each time."""

    if any(s["kind"] in ("manual", "open") or s.get("seen", demos) < demos for block in blocks for s in block):
        return False

    length = max(len(block) for block in blocks)
    for position in range(length):
        at = _aligned(blocks, position, length)
        if len(at) < 2:
            continue
        if at[0]["kind"] == "type" and len({s["samples"][0] for s in at}) > 1:
            return True
        if at[0]["kind"] == "upload" and len({tuple(s.get("files", [])) for s in at}) > 1:
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


def _documents(programs):
    """
    The documents the person worked from, as paths on this desktop: the files
    each demonstration's ticket was given (record_task.py keeps them in the
    recording's meta), in order, each once. A lesson's model reads them beside
    what was typed (study.py), and a later run is told they were examples.
    """

    found = []
    for meta, _, _ in programs:
        for path in meta.get("files") or []:
            if isinstance(path, str) and path and path not in found:
                found.append(path)
    return found


def _notes(steps, programs):
    """A person's own written explanations, each with the step it came before."""

    notes = []

    for index, (_, events, program) in enumerate(programs):
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
    if step.get("intent"):
        line += f" - you said: {el._quote(step['intent'], 80)}"

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

    # What the model that studied the demonstration could not work out
    # (study.py), one at a time, before the rule-based questions: its answers
    # can settle those too (which values change, what a step is for).
    for asked in (skill.get("understanding") or {}).get("questions") or []:
        found.append(_question(f"study:{asked['id']}", "study", asked["text"]))

    starts = skill.get("starts")
    if starts:
        found.append(_start(starts))

    params = {p["name"]: p for p in skill["params"]}

    # A loop nobody has answered about decides what its steps' typed fields
    # are (one value, or a list), so they are not asked about until it is.
    undecided = [loop for loop in skill.get("loops") or [] if loop["enabled"] is None]
    in_loop = {step_id for loop in undecided for block in loop["blocks"] + (loop.get("between") or []) for step_id in block}
    loop_at = {loop["blocks"][0][0]: loop for loop in undecided}

    # Every click recorded only as a position is asked about in one question,
    # where the first of them would have been: one at a time, a recording of
    # an app shown as a picture (a host app, a canvas) asked the same thing
    # six times in a row. An answer is still kept per step, so a lesson taught
    # before this, half answered, asks only about the rest.
    positional = [step for step in skill["steps"]
                  if step["id"] not in in_loop and step["kind"] in replay.REPLAYABLE and step["kind"] in CLICKS
                  and not step.get("target") and step.get("point") and not step.get("launcher")
                  and f"coordinates:{step['id']}" not in skill["answers"]]
    first_positional = positional[0]["id"] if positional else None

    # Every typed value nobody has said changes or stays is asked about in
    # one question too: a demonstration of keying a document into a system
    # (a PO into an application, a new joiner into an HRMS) types ten fields, and ten
    # questions in a row, each answered "changes", is what made teaching one
    # tedious. Answers are still kept per step (`variable:<id>`).
    undecided_values = [step for step in skill["steps"]
                        if step["id"] not in in_loop and params.get(step.get("param"))
                        and params[step["param"]]["varies"] is None]
    first_value = undecided_values[0]["id"] if len(undecided_values) > 1 else None

    for step in skill["steps"]:
        if step["id"] in loop_at:
            found.append(_loop(skill, loop_at[step["id"]]))
        if step["id"] in in_loop:
            continue

        param = params.get(step.get("param"))
        if step["id"] == first_value:
            found.append(_variables(skill, undecided_values, params))
        elif param and param["varies"] is None and first_value is None:
            found.append(_variable(skill, step, param))

        if step.get("seen", skill["demos"]) < skill["demos"]:
            found.append(_optional(skill, step))

        if step["kind"] not in replay.REPLAYABLE:
            found.append(_unsupported(step))
        elif step["id"] == first_positional:
            found.append(_coordinates(positional, skill))

    found.append(_approval())
    found.append(_confirm(skill))
    return found


def next_question(skill):
    """The first question not answered yet, or None."""

    return next((q for q in questions(skill) if q["id"] not in skill["answers"]), None)


def _question(qid, kind, text, step=None):
    return {"id": qid, "kind": kind, "step": step, "text": text, "default": CAUTIOUS.get(kind)}


def understood_text(skill):
    """What the model that studied a demonstration understood (study.py), for the person to check; '' when none."""

    understanding = skill.get("understanding") or {}
    if not understanding.get("summary"):
        return ""
    lines = [f"What I understood: {understanding['summary']}"]
    if understanding.get("done_when"):
        lines.append(f"It is done when: {understanding['done_when']}")
    brief = {b["topic"]: b for b in understanding.get("brief") or []}
    if brief:
        # The rules of the work, by topic: each says where it came from, so a guess is seen as one and corrected.
        lines.append("The rules of this work as I understood them (correct any by its number):")
        n = 0
        for rule in understanding.get("rules") or []:
            n += 1
            topic = rule[1:rule.index("]")] if rule.startswith("[") and "]" in rule else ""
            item = brief.get(topic) or {}
            where = (f" - seen: {item['evidence']}" if item.get("basis") == "seen" and item.get("evidence") else
                     " - my guess, please confirm" if item.get("basis") == "assumed" else
                     " - as you told me" if item.get("basis") in ("answered", "confirmed") else "")
            lines.append(f"{n}. {rule.replace(' (my assumption)', '')}{where}")
        unknown = [t for t, b in brief.items() if b.get("basis") == "unknown"]
        waiting = {str(q.get("topic") or "") for q in understanding.get("questions") or []
                   if f"study:{q['id']}" not in (skill.get("answers") or {})}
        if [t for t in unknown if t in waiting]:
            lines.append("Not clear from the recording, asked next: " + ", ".join(t for t in unknown if t in waiting) + ".")
        if [t for t in unknown if t not in waiting]:
            lines.append("Still not settled - tell me if you can: " + ", ".join(t for t in unknown if t not in waiting) + ".")
        return "\n".join(lines)
    for rule in understanding.get("rules") or []:
        lines.append(f"- {rule}")
    return "\n".join(lines)


def _goal(skill):
    sources = f" from {skill['demos']} demonstrations" if skill["demos"] > 1 else ""
    parts = [
        f"I watched \"{skill['title']}\" and learned {len(skill['steps'])} steps{sources}:",
        overview(skill),
    ]

    notes = _notes_text(skill)
    if notes:
        parts.append(notes)

    tidied = skill.get("tidied") or []
    if tidied:
        parts.append("I tidied what you did: " + "; ".join(tidied) + ". The recording itself is unchanged.")

    learned = [p for p in skill["params"] if p["varies"]]
    if learned:
        parts.append("What differed between your demonstrations, so it changes each run: "
                     + ", ".join(f"{p['name']} ({p['label']})" for p in learned) + ".")

    understood = understood_text(skill)
    if understood:
        parts.append(understood)
        parts.append("A few questions before I try to do it myself. First: is that right? Correct anything I got "
                     "wrong, and add whatever I should know - what it is for, what can be different next time, "
                     "what to watch out for. Reply `yes` if it is right as it stands.")
        return _question("goal", "goal", "\n".join(parts))

    parts.append("A few questions before I try to do it myself. First: when is this task finished - "
                 "what should be on the screen, or true, at the end? And if you like, explain the task or any step "
                 "in your own words: a replay only has what you did, and what you meant is what helps when the next "
                 "run's screen looks different - for example `step 6 clicks the first search result, whatever it "
                 "is`. Write as much as you like, or reply `skip`.")

    return _question("goal", "goal", "\n".join(parts))


def _start(starts):
    where = " at ".join(x for x in (el._quote(starts["window"], 60) if starts.get("window") else "", starts.get("url") or "") if x)
    text = (f"You began with {where} on the screen. Should I check that before I start, and ask you if it "
            "is not there? Reply `yes` or `no`.")
    return _question("start", "start", text)


def _variable(skill, step, param):
    samples = [s for s in step["samples"] if s]
    did, into = ("chose", "in") if param.get("choice") else ("typed", "into")
    same = f" (both times you {did} the same)" if len(samples) > 1 else ""
    target = step.get("target") or {}
    where = f"{target['role']} {el._quote(target.get('name') or '', 50)}" if target.get("role") else "the focused field"
    meaning = ((skill.get("understanding") or {}).get("inputs") or {}).get(param["name"])
    if meaning:
        same += f" ({el._quote(meaning, 120)})"
    text = (f"Step {step['n']}: you {did} {el._quote(param['example'] or '', 60)} {into} {where}{same}. Is that the "
            "same every time, or does it change from run to run (a different customer, amount, ...)? "
            "Reply `same` or `changes`.")
    return _question(f"variable:{step['id']}", "variable", text, step["id"])


def _variables(skill, steps, params):
    """One question for every typed value whose changing or staying is not known yet."""

    def where(step):
        target = step.get("target") or {}
        if step.get("group"):
            return f"the radio buttons {el._quote(step['group'], 40)}"
        if target.get("role"):
            return f"{target['role']} {el._quote(target.get('name') or '', 40)}"
        return "the focused field"

    understanding = skill.get("understanding") or {}

    def given(step):
        param = params[step["param"]]
        if param.get("file"):
            said = f"the file {el._quote(param['example'] or '', 60)} uploaded to {where(step)}"
        elif param.get("choice"):
            said = f"{el._quote(param['example'] or '', 50)} chosen in {where(step)}"
        else:
            said = f"{el._quote(param['example'] or '', 50)} into {where(step)}"
        meaning = (understanding.get("inputs") or {}).get(step["param"])
        return f"{said} ({el._quote(meaning, 120)})" if meaning else said

    listed = "; ".join(f"{i}) {given(s)}" for i, s in enumerate(steps, 1))
    guess = understanding.get("varies") or {}
    if guess and all(s["param"] in guess for s in steps):
        changing = [str(i) for i, s in enumerate(steps, 1) if guess[s["param"]]]
        listed += (". I think " + (" and ".join(changing) + (" change" if len(changing) > 1 else " changes")
                                   if changing else "none of them change"))
    text = (f"You gave {len(steps)} values: {listed}. Which of them change from run to run (a different "
            "customer, amount, date, document number...)? Reply `all change` or `all same`, or name them - for "
            f"example `all change except {len(steps)}`, or `1 and 2 change` (the others then stay as you typed them).")

    question = _question("variables:" + "+".join(s["id"] for s in steps), "variables", text)
    question["default"] = CAUTIOUS["variable"]
    question["values"] = [{"id": s["id"], "param": s["param"]} for s in steps]
    return question


OPPOSITE = {"changes": "same", "same": "changes"}
EXCEPT = re.compile(r"\b(?:except|but not|other than|apart from|besides)\b", re.I)


def _variable_choices(question, text):
    """
    What a reply to the grouped values question decides for each step:
    {step id: changes|same}, or None when it cannot be read. "all change
    except 3" makes 3 the opposite; "1 and 4 change" makes the rest the
    opposite; a bare verb decides every one.
    """

    count = len(question["values"])
    parts = EXCEPT.split(text, maxsplit=1)
    head, tail = parts[0], (parts[1] if len(parts) > 1 else "")

    head_choice = _choose("variable", head)
    head_numbers = _step_numbers(head)
    tail_numbers = _step_numbers(tail) if tail else []

    if head_choice is None or head_numbers is None or tail_numbers is None or (tail and not tail_numbers):
        return None

    named = {n: head_choice for n in head_numbers}
    tail_choice = (_choose("variable", tail) if tail else None) or OPPOSITE[head_choice]
    for n in tail_numbers:
        if named.get(n, tail_choice) != tail_choice:
            return None
        named[n] = tail_choice

    if any(n < 1 or n > count for n in named):
        return None

    rest = OPPOSITE[head_choice] if head_numbers else head_choice
    return {item["id"]: named.get(i, rest) for i, item in enumerate(question["values"], 1)}


def _answer_values(skill, question, text):
    """The reply to the grouped values question, applied to each value it covers."""

    qid = question["id"]
    choices = _variable_choices(question, text)
    note = ""

    if choices is None:
        tries = skill["unclear"][qid] = skill["unclear"].get(qid, 0) + 1
        if tries < MAX_UNCLEAR:
            return {"understood": False, "note": "I did not catch that - reply `all change`, `all same`, or for "
                    f"example `all change except {len(question['values'])}`.", "decision": None}
        choices = {item["id"]: question["default"] for item in question["values"]}
        note = "I could not tell what you meant, so I will ask for every one of those values on each run."
        text = f"(assumed {question['default']})"

    for step_id, choice in choices.items():
        if _step_by_id(skill, step_id) is None:
            continue
        _apply(skill, {"kind": "variable", "step": step_id, "id": f"variable:{step_id}"}, choice)
        skill["answers"][f"variable:{step_id}"] = choice

    skill["answers"][qid] = text[:300]

    if not note:
        stays = [i for i, item in enumerate(question["values"], 1) if choices.get(item["id"]) == "same"]
        changes = len(question["values"]) - len(stays)
        note = (f"Got it: {changes} change each run" + (f", {', '.join(map(str, stays))} stay as typed" if stays else "")
                + ".")

    return {"understood": True, "note": note, "decision": None}


def _template(skill, loop):
    """The steps of a loop's first whole repetition (the first may be shorter: it reached the form another way)."""

    by_id = {s["id"]: s for s in skill["steps"]}
    blocks = [[by_id[step_id] for step_id in block if step_id in by_id] for block in loop["blocks"]]
    return blocks, next((b for b in blocks if len(b) == loop["length"]), blocks[0])


def _loop(skill, loop):
    blocks, template = _template(skill, loop)
    first = blocks[0]

    typed = []
    for position, step in enumerate(template):
        if step["kind"] == "type":
            values = [s["samples"][0] for s in _aligned(blocks, position, loop["length"])]
            if len(set(values)) > 1:
                typed.append(f"{step['param']}: " + ", ".join(el._quote(v or "", 30) for v in values))

    shown = "; ".join(s["summary"] for s in template)
    if len(shown) > 600:
        shown = shown[:600].rstrip() + " ..."
    text = (f"Steps {first[0]['n']}-{first[-1]['n']} ({shown}) were done {loop['reps']} times"
            + (" in a row" if not any(loop.get("between") or []) else "")
            + f" with different text each time - {' / '.join(typed)}.")
    between = [g for g in loop.get("between") or [] if g]
    if between:
        by_id = {s["id"]: s for s in skill["steps"]}
        gaps = [", ".join(by_id[i]["summary"] for i in g if i in by_id) for g in between]
        text += (f" Between two of them you did {sum(len(g) for g in between)} other step(s) ("
                 + "; then ".join(el._quote(g, 160) for g in gaps[:3]) + ("; ..." if len(gaps) > 3 else "")
                 + ") - not part of every item, so I would not repeat those; I would do them only when the case comes up.")
    text += (" Should I do them once per item in a list you give me each run? Reply `each`, or `fixed` if it was "
             f"just those {loop['reps']} times.")

    return _question(f"loop:{loop['id']}", "loop", text)


def _optional(skill, step):
    text = (f"Step {step['n']} ({step['summary']}) happened in {step['seen']} of {skill['demos']} demonstrations. "
            "Does it only sometimes happen - I would do it only when I can find it - or is it always part of the "
            "task? Reply `sometimes` or `always`.")
    return _question(f"optional:{step['id']}", "optional", text, step["id"])


def _coordinates(steps, skill=None):
    """
    One question for every press still known only by its place on the screen
    (_name_points names the rest). Each is described by what the model that
    studied the demonstration saw under it, when it did - a person answers
    about "the WhatsApp icon", not about 73,300.
    """

    seen = ((skill or {}).get("understanding") or {}).get("steps") or {}

    def what(step):
        purpose = seen.get(str(step["n"]))
        return f"{step['n']}. {purpose}" if purpose else f"{step['n']}. {step['summary']}"

    if len(steps) == 1:
        step = steps[0]
        purpose = seen.get(str(step["n"]))
        text = (f"Step {step['n']} ({step['summary']}{' - ' + purpose if purpose else ''}) pressed a spot on the screen "
                "and I could not tell which control was there. "
                "Tell me what it is (for example `the Next button`) and I will find it by name, or reply `keep` to press "
                "the same spot, `skip` to leave it out, or `manual` to let you do it.")
        return _question(f"coordinates:{step['id']}", "coordinates", text, step["id"])

    listed = "\n".join(what(s) for s in steps)
    text = (f"{len(steps)} steps were recorded as clicks at spots on the screen, and I could not tell which control "
            f"was under them:\n{listed}\nReply `keep` to press the same spots (I check they still look the same first), "
            f"`manual` to let you do them, or name exceptions - for example `skip {steps[-1]['n']}`.")
    question = _question("coordinates:" + "+".join(s["id"] for s in steps), "coordinates", text)
    question["steps"] = [{"id": s["id"], "n": s["n"]} for s in steps]
    return question


def _step_numbers(text):
    """The step numbers a clause names: `2`, `2 4`, `2-4`, `2 to 4`."""

    numbers = []
    for low, high in re.findall(r"(\d+)(?:\s*(?:-|–|to)\s*(\d+))?", text):
        low = int(low)
        high = int(high) if high else low
        if high < low or high - low > 200:
            return None
        numbers.extend(range(low, high + 1))
    return numbers


def _coordinate_choices(question, text):
    """
    What a reply to the grouped positional question decides for each step:
    {step id: keep|skip|manual}, or None when it cannot be read. A verb with
    no numbers is the choice for every step not named otherwise; a verb with
    numbers is the choice for those; bare numbers carry the verb before them
    ("skip 2 and 4"). Anything not named is kept.
    """

    by_number = {s["n"]: s["id"] for s in question["steps"]}
    everything, named, last = None, {}, None

    for clause in re.split(r"[;,\n]|\band\b|\bbut\b|\bexcept\b", text):
        if not clause.strip():
            continue
        choice = _choose("coordinates", clause)
        numbers = _step_numbers(clause)
        if numbers is None:
            return None

        if choice is None:
            if not numbers or last is None:
                continue
            choice = last

        if numbers:
            for n in numbers:
                if n not in by_number or named.get(by_number[n], choice) != choice:
                    return None
                named[by_number[n]] = choice
            last = choice
        elif everything not in (None, choice):
            return None
        else:
            everything = choice

    if everything is None and not named:
        return None

    return {step_id: named.get(step_id, everything or "keep") for step_id in by_number.values()}


def _unsupported(step):
    text = (f"Step {step['n']} ({step['summary']}) is something I cannot repeat on my own. Reply `skip` to leave it "
            "out or `manual` to have me stop there and let you do it.")
    return _question(f"unsupported:{step['id']}", "unsupported", text, step["id"])


# How the question about starting a task opens (the tests' stand-in for a person finds it by this).
APPROVAL_OPENING = "Once you ask me to do this task"


def _approval():
    """Whether a later request for this task waits for the person's yes. Decided here, once, by the person who knows
    what the task does - not by words in a request (a "Purchase Invoice" is a document; "purchase" is an act)."""

    text = (APPROVAL_OPENING + ", should I check with you before I start, or go ahead on my own? "
            "Reply `ask first` or `go ahead`. Either way I always stop and ask before a step that sends, pays or "
            "publishes - this is about starting the task at all, so say `ask first` if it spends money or commits "
            "you to something.")
    return _question("approval", "approval", text)


def _confirm(skill):
    parts = ["That is everything I wanted to ask."]

    gated = [step for step in skill["steps"] if step.get("consequential")]
    if gated:
        parts.append("I will always stop and ask you before: " + "; ".join(f"{s['n']}. {s['summary']}" for s in gated) + ".")

    by_hand = [step for step in skill["steps"] if step["kind"] == "manual"]
    if by_hand:
        parts.append("I will stop and let you do: " + "; ".join(f"{s['n']}. {s['summary']}" for s in by_hand) + ".")

    if skill.get("approval") == "go":
        parts.append("When asked, I will start this task without checking with you first.")
    else:
        parts.append("When asked, I will check with you for a yes before I start.")

    needed = [p for p in skill["params"] if p["varies"]]
    if needed:
        meant = (skill.get("understanding") or {}).get("inputs") or {}
        parts.append("Each run I need: " + "; ".join(
            f"{p['name']} ({el._quote(meant[p['name']], 80) if meant.get(p['name']) else p['label']})" for p in needed) + ".")

    understood = understood_text(skill)
    if understood:
        parts.append("\n" + understood + "\n")
    elif skill.get("done_when"):
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

# The whole replies that are plainly one choice, for each question with set answers. Code decides only a reply (or one
# clause of a reply about numbered steps) that is, whole, one of these - numbers, "step", "and", "all" and politeness
# aside. Found in review (2026-11-03, D-22): the patterns above were searched anywhere in a reply of up to three words,
# so "don't go ahead" set a skill to run without asking and "not the same" made a value fixed. Anything else is the
# model's to read (_read_choice), and with no model it is asked again - never guessed.
PLAIN = {
    "variable": {"same": {"same", "constant", "fixed", "static", "unchanged", "never changes", "does not change",
                          "doesn't change", "stays same", "same every time", "same each time"},
                 "changes": {"changes", "change", "changing", "differs", "different", "varies", "vary", "variable",
                             "changes every time", "changes each time", "different every time",
                             "different each time"}},
    "optional": {"sometimes": {"sometimes", "occasionally", "optional", "not always", "depends", "only sometimes"},
                 "always": {"always", "every time", "each time", "required", "mandatory"}},
    "coordinates": {"keep": {"keep", "try", "as recorded", "ok", "okay", "fine", "go ahead"},
                    "skip": {"skip", "leave out", "drop", "remove", "ignore"},
                    "manual": {"manual", "manually", "by hand", "i'll do", "i will do", "i'll do by hand",
                               "i will do by hand", "let me", "let me do"}},
    "unsupported": {"skip": {"skip", "leave out", "drop", "remove", "ignore"},
                    "manual": {"manual", "manually", "by hand", "i'll do", "i will do", "i'll do by hand",
                               "i will do by hand", "let me", "let me do"}},
    "start": {"yes": {"yes", "y", "yeah", "yep", "ok", "okay", "sure", "confirm", "looks good", "go ahead"},
              "no": {"no", "n", "nope", "nah", "don't", "do not"}},
    "loop": {"each": {"each", "each one", "for each", "for each item", "every item", "per item", "items", "rows",
                      "repeat", "loop", "yes", "y"},
             "fixed": {"fixed", "once", "as recorded", "just those", "only those", "exactly", "no", "n"}},
    "approval": {"ask": {"ask", "ask me", "ask first", "ask me first", "check with me", "confirm", "wait for me",
                         "every time", "each time"},
                 "go": {"go", "go ahead", "don't ask", "do not ask", "never ask", "no need", "no need to ask",
                        "just do", "proceed", "automatically", "on your own", "by yourself", "without asking"}},
    "confirm": {"yes": set(), "no": set()},  # whole-reply patterns of their own (KEEP_NAME, THROW_AWAY)
}
_STEP_NUMBERS = re.compile(r"#?\b\d+(?:\s*(?:-|to)\s*\d+)?(?:st|nd|rd|th)?\b", re.I)
_FILLER = set("and all the a an one rest others other ones step steps please thanks thank you it is it's its they "
              "are them".split())


def _plain_words(text):
    """A reply (or a clause of one) as the words that may name a choice: lower case, step numbers, punctuation and
    _FILLER dropped."""

    text = _STEP_NUMBERS.sub(" ", str(text or "").lower().replace("’", "'"))
    words = re.findall(r"[a-z']+", text)
    return " ".join(w for w in words if w not in _FILLER)


def _choose(kind, text):
    """The one option a reply (or a clause of one) plainly is, whole (PLAIN), or None."""

    if kind == "confirm":
        text = str(text or "")
        hits = {option for option, pattern in CHOICES[kind] if pattern.match(text)}
        return hits.pop() if len(hits) == 1 else None
    words = _plain_words(text)
    hits = {option for option, phrases in PLAIN.get(kind, {}).items() if words and words in phrases}
    return hits.pop() if len(hits) == 1 else None


# The teaching questions whose answer is one choice and often comes with more ("same, except for one customer"), and
# what each choice means - for the one reading of a reply that is more than a choice (_read_choice).
READ_KINDS = {
    "variable": {"same": "the value is the same every time the task is done",
                 "changes": "the value changes from one run of the task to the next"},
    "optional": {"sometimes": "the step happens only sometimes, or only when something holds",
                 "always": "the step happens every time"},
    "start": {"yes": "yes - start from here as it is", "no": "no - do not start from here"},
    "approval": {"ask": "ask the person before each run of the task",
                 "go": "go ahead with each run on its own, without asking"},
    "coordinates": {"keep": "keep the step as it was recorded", "skip": "leave the step out",
                    "manual": "the person does this step by hand"},
    "unsupported": {"skip": "leave the step out", "manual": "the person does this step by hand"},
    "loop": {"each": "the steps are done once for each item of a list", "fixed": "the steps are done as recorded"},
}


def _read_choice(kind, question, text):
    """
    (choice or None, what the reply adds) for a teaching question whose answer is one choice. A reply that is, whole,
    one choice's plain phrase (PLAIN) is that choice. Every other reply is read once (approval.read_choice): "same,
    except for one customer it's 5%" and "yes, and check the client name too" made a choice and said more, and the
    word lists kept only the choice (audit, 2026-10-24). With no model such a reply is not guessed at: it is asked
    again (D-22).
    """

    hit = _choose(kind, text)
    if hit is not None:
        return hit, ""
    from decurix_agent import llm_client
    from decurix_agent.computer import approval
    try:
        model = llm_client.available()
    except Exception:  # noqa: BLE001
        model = False
    if not model:
        return None, ""
    said = approval.read_choice(text, question.get("text") or "", READ_KINDS[kind])
    return (said["choice"], said["instruction"]) if said["choice"] in READ_KINDS[kind] else (None, "")


# What a reply to "save it as ...?" can be, beyond a plain yes or no (KEEP_NAME, THROW_AWAY).
CONFIRM_CHOICES = {"save": "save the skill under the name shown",
                   "discard": "throw away what was learned",
                   "rename": "the reply is the name to save the skill under",
                   "unclear": "not yet, a correction, or anything else"}


def _renamed_draft(skill, given):
    """rename(), and the draft kept under its new name only (saved there, its old file removed), still a draft."""

    old = skill["name"]
    if not rename(skill, given):
        return False
    if skill["name"] != old:
        skills.save(skill)
        skills.delete(old)
    return True


def _answer_confirm(skill, question, text):
    """
    The last question's reply. A plain yes saves and a plain no throws away. Anything else is never a save by itself:
    a name is put back to the person ("I will save it as ..."), who says yes to it. Found in review (2026-11-03, D-21):
    every other reply became the skill's name and the skill was saved as ready - "not yet" and "wait, step 3 is wrong"
    saved it. With a model the reply is read first; one that is not a name is asked again.
    """

    choice = _choose("confirm", text)
    if choice is None:
        from decurix_agent import llm_client
        from decurix_agent.computer import approval
        try:
            model = llm_client.available()
        except Exception:  # noqa: BLE001
            model = False
        said = approval.read_choice(text, question.get("text") or "", CONFIRM_CHOICES)["choice"] if model else "rename"
        if said == "save":
            choice = "yes"
        elif said == "discard":
            choice = "no"
        elif said == "rename" and _renamed_draft(skill, text):
            # Asked again, with the name: only the person's yes to it saves.
            return {"understood": True, "decision": None,
                    "note": f"I will save it as \"{skill['title']}\" - reply `yes` to save it, a different name, "
                            "or `no` to throw it away."}
        else:
            tries = skill["unclear"][question["id"]] = skill["unclear"].get(question["id"], 0) + 1
            if tries < MAX_UNCLEAR:
                return {"understood": False, "note": "I did not catch that.", "decision": None}
            choice = question["default"]
    decision = _apply(skill, question, choice)
    if decision == "hold":
        skill["answers"].pop(question["id"], None)
        skill["unclear"].pop(question["id"], None)
    else:
        skill["answers"][question["id"]] = text[:300]
    return {"understood": True, "note": "", "decision": decision}


def _keep_added(skill, question, added):
    """
    What a teaching reply added to its choice ("same, except for one customer it's 5%"), which the choice itself cannot
    hold: kept as one of the task's rules, in the person's words - every run's model and its check read the rules
    (skill_task._taught).
    """

    added = " ".join(str(added or "").split())[:300]
    if not added:
        return
    understanding = skill.get("understanding") or {}
    rules = understanding.setdefault("rules", [])
    about = " ".join(str(question.get("text") or "").split())[:160]
    rule = f"{added} (said when asked: {about})" if about else added
    if rule not in rules:
        rules.append(rule)
    skill["understanding"] = understanding


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
        if skill.get("understanding", {}).get("summary") and (YES.match(text) and len(text.split()) <= 3):
            # "yes" to what the model understood: nothing of the person's own
            # to keep as their explanation.
            skill["answers"]["goal"] = text[:300]
            return {"understood": True, "note": "", "decision": None}
        return _answer_goal(skill, text)

    if kind == "study":
        # Whatever the person says is the answer, "skip" included; skill_task
        # hands it to the model to fold in (study.absorb).
        skill["answers"][qid] = text[:1000] or "skip"
        return {"understood": True, "note": "", "decision": None}

    if question.get("steps"):
        return _answer_positional(skill, question, text)

    if question.get("values"):
        return _answer_values(skill, question, text)

    if kind == "coordinates" and question.get("step") and _choose(kind, text) is None:
        named = _named_control(text)
        step = _step_by_id(skill, question["step"]) if named else None
        if step is not None:
            _target_named(step, *named)
            skill["answers"][qid] = text[:300]
            return {"understood": True, "note": f"Step {step['n']} is now: {step['summary']}.", "decision": None}

    if kind == "confirm":
        return _answer_confirm(skill, question, text)

    added = ""
    if kind in READ_KINDS:
        choice, added = _read_choice(kind, question, text)
    else:
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
    if added:
        _keep_added(skill, question, added)
        note = (note + " " if note else "") + f"I will also keep this: {added[:200]}"

    # Not saved, not thrown away: the question stays open for the next time
    # this skill is taught.
    if decision == "hold":
        skill["answers"].pop(qid, None)
        skill["unclear"].pop(qid, None)

    return {"understood": True, "note": note, "decision": decision}


# A step a person's explanation is about: "step 6", "6th step", "the last step".
STEP_MENTION = re.compile(r"\bsteps?\s*#?\s*(\d+)\b|\b(\d+)(?:st|nd|rd|th)\s+step\b|\b(last|final)\s+step\b", re.I)
PIECES = re.compile(r"(?:\n+|;|(?<=[.!?])\s+)")


def _answer_goal(skill, text):
    """
    The first question's reply: when the task is finished, and whatever the
    person explained in their own words. It is kept whole as the skill's
    `explanation`; each part that names a step ("step 6 clicks the first
    result") goes on that step as its `intent` - which a run hands to whoever
    has to find that step's target again, the engine or the person - and the
    rest is what "finished" means (`done_when`).
    """

    skill["answers"]["goal"] = text[:300]
    for step in skill["steps"]:
        step.pop("intent", None)

    if not text or NOTHING.match(text):
        skill["done_when"] = skill["explanation"] = None
        return {"understood": True, "note": "", "decision": None}

    text = text[:MAX_EXPLANATION]
    skill["explanation"] = text

    by_n = {step["n"]: step for step in skill["steps"]}
    attached, rest = [], []
    for piece in (p.strip() for p in PIECES.split(text)):
        if not piece:
            continue
        hits = []
        for match in STEP_MENTION.finditer(piece):
            n = skill["steps"][-1]["n"] if match.group(3) and skill["steps"] else int(match.group(1) or match.group(2))
            if n in by_n and n not in hits:
                hits.append(n)
        for n in hits:
            step = by_n[n]
            step["intent"] = ((step["intent"] + " ") if step.get("intent") else "") + piece[:MAX_INTENT]
            attached.append(n)
        if not hits:
            rest.append(piece)

    skill["done_when"] = " ".join(rest)[:300] or None
    note = ""
    if attached:
        numbers = ", ".join(str(n) for n in dict.fromkeys(attached))
        note = f"Noted what you said about step{'s' if len(set(attached)) > 1 else ''} {numbers} - I will go by it when the screen looks different."
    return {"understood": True, "note": note, "decision": None}


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


PARAM_NAME = re.compile(r"^[a-z][a-z0-9_]{1,30}$")


def _replace_braces(value, old, new):
    if isinstance(value, str):
        return value.replace("{" + old + "}", "{" + new + "}")
    if isinstance(value, list):
        return [_replace_braces(v, old, new) for v in value]
    if isinstance(value, dict):
        return {k: _replace_braces(v, old, new) for k, v in value.items()}
    return value


def rename_params(skill, names):
    """
    Call each value by what it is ({contact}, {message}) instead of the label
    of the field it was typed into ({type_a_message_to_koustubh_og}, named
    after the demonstration's own value). `names` is {old: new}; a new name
    that is not a short snake_case word, or that is taken, is left alone.
    """

    taken = {p["name"] for p in skill.get("params") or []}
    for param in skill.get("params") or []:
        old, new = param["name"], str(names.get(param["name"]) or "").strip().lower()
        if not PARAM_NAME.match(new) or new == old or new in taken:
            continue
        taken.discard(old)
        taken.add(new)
        param["name"] = new
        for step in skill.get("steps") or []:
            if step.get("param") == old:
                step["param"] = new
            for check in step.get("expect") or []:
                if check.get("param") == old:
                    check["param"] = new
        skill["steps"] = _replace_braces(skill["steps"], old, new)
        understanding = skill.get("understanding")
        if understanding:
            for key in ("inputs", "varies"):
                if old in (understanding.get(key) or {}):
                    understanding[key][new] = understanding[key].pop(old)
            for key in ("summary", "done_when", "steps", "rules", "questions"):
                if key in understanding:
                    understanding[key] = _replace_braces(understanding[key], old, new)
    for step in skill.get("steps") or []:
        step["summary"] = replay._summary(step)


def _apply_loop(skill, loop, choice):
    """
    `each`: keep one repetition, and make what it types a list a run is given.
    `fixed`: leave every step as recorded, to be asked about like any other.
    """

    if choice != "each":
        loop["enabled"] = False
        return

    by_id = {s["id"]: s for s in skill["steps"]}
    blocks, template = _template(skill, loop)
    kept = {s["id"] for s in template}
    dropped = {step_id for block in loop["blocks"] for step_id in block if step_id not in kept}
    # What came between two items is not part of every item: it goes out of the steps, and stays on the loop in words
    # for the model, which reads it when such a case comes up (skill_task._procedure).
    between = [[by_id[i]["summary"] for i in gap if i in by_id] for gap in loop.get("between") or []]
    dropped |= {step_id for gap in loop.get("between") or [] for step_id in gap}

    for position, step in enumerate(template):
        step["repeat"] = loop["id"]

        if step.get("param"):
            examples = [s["samples"][0] for s in _aligned(blocks, position, loop["length"])]
            param = next(p for p in skill["params"] if p["name"] == step["param"])
            if len(set(examples)) == 1:
                # Typed the same into every repetition (the income account on
                # every line of an order): part of the procedure, not of the
                # data - kept as typed, the way a `same` answer keeps it, so a
                # run is not asked for one copy of it per item.
                param.update(varies=False, default=examples[0], example=examples[0])
            else:
                param.update(varies=True, default=None, list=True, examples=examples, example=examples[0])

    skill["steps"] = [s for s in skill["steps"] if s["id"] not in dropped]
    skills.renumber(skill)

    loop["enabled"] = True
    loop["blocks"] = [[s["id"] for s in template]]
    loop["between"] = []
    loop["between_summaries"] = [g for g in between if g]


def _apply_step_choice(skill, step, choice):
    """keep, skip or manual for one step; the caller renumbers."""

    if choice == "skip":
        skill["steps"].remove(step)
    elif choice == "manual":
        what = step["summary"]
        step["kind"] = "manual"
        step["reason"] = f"a person does this one ({what})"
        step["by_hand"] = what
        step["summary"] = replay._summary(step)
        step["expect"] = []
        step["consequential"] = False


# A person naming the control a position-only click was on: "it's the Next button", "the "Save draft" button",
# "button Submit", "the Customers tab". Found live (2026-10-14): asked about a click recorded as a bare point, a
# person said what it was - and the only answers taken were keep / skip / manual.
CONTROL_ROLES = {"button": "button", "link": "link", "tab": "tab", "checkbox": "checkbox", "check box": "checkbox",
                 "radio button": "radio", "radio": "radio", "menu item": "menuitem", "option": "option"}
_ROLE_WORDS = "|".join(sorted((re.escape(r) for r in CONTROL_ROLES), key=len, reverse=True))
_QUOTES = "\"'`“”‘’"
_LEAD = re.compile(r"^(?:step\s*\d+\s+(?:is|was)\s+|(?:it|that|this)(?:'s|\s+is|\s+was)\s+|(?:click|press)(?:\s+on)?\s+|the\s+|a\s+)", re.I)
_WHERE = re.compile(r"\s+(?:at|on|in|near|under|below|above)\s+the\b.*$", re.I)
_NAME_ROLE = re.compile(rf"^(?P<name>.+?)\s+(?P<role>{_ROLE_WORDS})$", re.I)
_ROLE_NAME = re.compile(rf"^(?P<role>{_ROLE_WORDS})\s+(?:called\s+|named\s+|labell?ed\s+)?(?P<name>.+)$", re.I)


def _named_control(text):
    """(role, name) of the one control a short reply names, or None."""

    t = " ".join((text or "").split()).rstrip(".!")
    if re.search(r"[,;]|\b(?:not|or|and|but)\b", t, re.I):
        return None  # more than naming one control
    t = _WHERE.sub("", t)
    while True:
        shorter = _LEAD.sub("", t, count=1)
        if shorter == t:
            break
        t = shorter
    m = _NAME_ROLE.match(t) or _ROLE_NAME.match(t)
    if not m:
        return None
    name = m.group("name").strip().strip(_QUOTES).strip()
    if not name or len(name.split()) > 5 or re.fullmatch(r"keep|skip|manual|it|that|this|same|one", name, re.I):
        return None
    return CONTROL_ROLES[m.group("role").lower()], name


def _target_named(step, role, name):
    """A position-only step pointed at the control a person named; it is found by that from now on, never by the spot."""

    step["target"] = {"role": role, "name": name}
    for key in ("point", "look", "element_missing"):
        step.pop(key, None)
    step["summary"] = replay._summary(step)


def _answer_positional(skill, question, text):
    """The reply to the grouped positional question (see _coordinate_choices), applied to each step it covers."""

    qid = question["id"]
    # A person often says what a click was while choosing for it: "keep (11 is the Product drop-down of line 1, 16 is
    # the Add line button)". What they say a step is goes on the step as its intent - what finds it again when the
    # screen has moved - and is not read as choices: its "line 1" made the whole reply unreadable (found live).
    said = {}
    for piece in re.findall(r"\(([^)]*)\)", text) + re.findall(r"(?:^|[;,])\s*((?:step\s*)?\d+\s+(?:is|=)\s+[^,;()]+)", text, re.I):
        for part in re.split(r"[;,]|\band\b(?=\s*(?:step\s*)?\d+\s+(?:is|=))", piece):
            mt = re.match(r"\s*(?:step\s*)?(\d+)\s*(?:is|=|:|-)\s*(.+?)\s*$", part, re.I)
            if mt and any(s["n"] == int(mt.group(1)) for s in question["steps"]):
                said[int(mt.group(1))] = mt.group(2)
    bare = re.sub(r"\([^)]*\)", " ", text)
    for n, what in said.items():
        bare = re.sub(rf"(?:^|(?<=[;,]))\s*(?:step\s*)?{n}\s+(?:is|=)\s+{re.escape(what)}", " ", bare, flags=re.I)
    choices = _coordinate_choices(question, bare)
    note = ""
    named_steps = set()
    for step in question["steps"]:
        if step["n"] in said:
            found = _step_by_id(skill, step["id"])
            if found is None:
                continue
            named = _named_control(said[step["n"]])
            if named:
                _target_named(found, *named)
                named_steps.add(step["id"])
            else:
                found["intent"] = ((found["intent"] + " ") if found.get("intent") else "") + f"It is {said[step['n']]}."[:MAX_INTENT]
    if choices is None and named_steps and not bare.strip(" ,;.\n"):
        choices = {s["id"]: "keep" for s in question["steps"]}

    if choices is None:
        tries = skill["unclear"][qid] = skill["unclear"].get(qid, 0) + 1
        if tries < MAX_UNCLEAR:
            return {"understood": False, "note": "I did not catch that - reply `keep`, `manual`, or for example "
                    f"`skip {question['steps'][-1]['n']}`.", "decision": None}
        choices = {s["id"]: question["default"] for s in question["steps"]}
        note = "I could not tell what you meant, so I will stop at each of those and let you do it."
        text = f"(assumed {question['default']})"

    numbers = {s["id"]: s["n"] for s in question["steps"]}
    by_choice = {}
    for step_id, choice in choices.items():
        step = _step_by_id(skill, step_id)
        if step is None:
            continue
        _apply_step_choice(skill, step, choice)
        skill["answers"][f"coordinates:{step_id}"] = choice
        by_choice.setdefault(choice, []).append(str(numbers[step_id]))

    skills.renumber(skill)
    skill["answers"][qid] = text[:300]

    if not note and len(by_choice) > 1:
        words = {"keep": "keeping", "skip": "leaving out", "manual": "stopping for you at"}
        note = "Got it: " + "; ".join(f"{words[c]} {', '.join(ns)}" for c, ns in by_choice.items()) + "."

    return {"understood": True, "note": note, "decision": None}


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
        _apply_step_choice(skill, step, choice)
        skills.renumber(skill)

    elif kind == "approval":
        skill["approval"] = choice

    elif kind == "confirm":
        if choice == "yes":
            skill["status"] = skills.READY
            skill["learned_at"] = time.time()
        return {"yes": "save", "no": "discard"}.get(choice, "hold")

    return None
