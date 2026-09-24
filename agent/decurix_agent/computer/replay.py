import os
import re
import time
import urllib.parse
from datetime import date

from decurix_agent import progress
from decurix_agent.computer import approval, audit, constraints, core, privacy, recording, screen, visual, volatility
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log


# ============================================================
# Replay: a recording run again, with no model
#
# openadapt-flow's idea on Decurix's action layer. A recording - a person's
# demonstration, or a task an agent finished - is compiled into a program: its
# steps, the text it typed as parameters, and for every step what it should
# leave on the screen. Running the program makes no model call, and every step
# goes through core.act, so the approval gate and the audit trail apply to it
# exactly as they apply to an engine.
#
# What makes it safe to leave running is how it ends, also from openadapt-flow:
#
# - a step's target is found again as an element - by its path, then by role
#   and name - and never guessed: no candidate, or two, halts the run
# - coordinates are used only for a step recorded without an element, on a
#   screen of the same size, in the same window - and, when the recording kept
#   a picture of what was around the click (a "look", visual.py), only where
#   that picture still is: found again nearby it is clicked there, and not
#   found, or found in two places, it halts
# - after each step, what it left on the screen when it was recorded (a page,
#   a window, a dialog) has to appear; text that names a moment or a count is
#   never checked (volatility.py)
# - a run never reports a success it did not see, and every outcome says what
#   is known: verified; completed_unverified (every step ran, some had nothing
#   to check); halted_before_effect (stopped before doing anything); halted
#   (stopped part way, nothing after the stop was done);
#   reconciliation_required (a consequential step ran and its result never
#   appeared - it may or may not have happened, so it is never retried); and
#   needs_approval / needs_human, which pause and resume from a step.
# ============================================================

FIND_SECONDS = float(os.environ.get("DECURIX_REPLAY_FIND_SECONDS", "12"))
EXPECT_SECONDS = float(os.environ.get("DECURIX_REPLAY_EXPECT_SECONDS", "10"))
POLL_SECONDS = 0.6

VERIFIED = "verified"
COMPLETED_UNVERIFIED = "completed_unverified"
HALTED_BEFORE_EFFECT = "halted_before_effect"
HALTED = "halted"
RECONCILIATION_REQUIRED = "reconciliation_required"
NEEDS_APPROVAL = "needs_approval"
NEEDS_HUMAN = "needs_human"

SUCCESS = (VERIFIED, COMPLETED_UNVERIFIED)

# What a replay can do. A recorded action outside this (a middle click) is
# still a step - one the run halts at, rather than skips and calls verified.
REPLAYABLE = {"click", "double_click", "right_click", "type", "key", "scroll", "select", "drag", "open", "manual", "upload"}

# What a replay waits for when a step made it appear, most telling first.
CHECKED_ROLES = {"dialog": 0, "alert": 0, "heading": 1, "status": 2,
                 "tab": 3, "button": 4, "link": 4, "textbox": 4, "menuitem": 4}
MAX_ELEMENT_CHECKS = 2

# The clicks a picture of the surroundings is kept for.
LOOK_KINDS = {"click", "double_click", "right_click"}

# Steps that act on a control, as opposed to typing into whatever has focus or pressing a key.
NEEDS_TARGET = {"click", "double_click", "right_click", "select", "drag", "upload"}

# What a control known only by its label may be: things a person acts on.
ACTIVATING_ROLES = {"button", "link", "menuitem", "tab", "checkbox", "radio", "switch", "combobox",
                    "textbox", "searchbox", "spinbutton", "option", "listitem", "treeitem", "cell"}
MAX_WINDOW_CHECKS = 2

TITLE_SPLIT = re.compile(r"\s[-–—|]\s")
WORD = re.compile(r"[a-z0-9]+")
STOP_WORDS = {"a", "an", "and", "at", "by", "do", "for", "from", "how", "i", "in", "into",
              "is", "it", "my", "of", "on", "please", "that", "the", "this", "to", "with"}


# ============================================================
# Compiling
# ============================================================

def load_program(session_id):
    """(meta, program) for a recording on this desktop. Raises FileNotFoundError."""

    meta, events = recording.load(session_id)
    return meta, compile_program(meta, events)


class _Frames:
    """A recording's saved frames, read as PNG bytes (each one once)."""

    def __init__(self, session_id):
        self.session_id = session_id
        self._seen = {}

    def png(self, ref):
        name = str(ref or "").rsplit("/", 1)[-1]

        if name not in self._seen:
            data = None
            try:
                path = recording.frame_file(self.session_id, name)
                data = path.read_bytes() if path else None
            except (OSError, ValueError):
                pass
            self._seen[name] = data

        return self._seen[name]


def compile_program(meta, events, frames=None):
    """
    A recording as a program: {"session", "title", "kind", "params", "steps"}.

    A step is an action that completed when it was recorded. Actions that
    failed, or were stopped for approval, are not steps; the approved retry
    that followed is, and is marked consequential. Each typed text becomes a
    parameter named after its field ("Customer name" -> customer_name) whose
    default is what was typed - or which has no default, when the recording
    withheld it, and must be given to run.
    """

    reference = date.fromtimestamp(meta.get("started_at") or time.time())
    frames = frames or _Frames(meta.get("id"))
    steps = []
    params = []
    used = set()

    for event in events:
        kind = event.get("kind")

        # A note is not an action - a person's own written explanation,
        # never replayed. Left out of the program entirely, not merely
        # excluded from REPLAYABLE, so it can never reach run()'s halt for
        # "a replay cannot <kind>".
        if not kind or kind == "note":
            continue
        if kind != "manual" and event.get("outcome") not in ("done", None):
            continue

        element = event.get("element") or {}
        step = {"n": len(steps) + 1, "event": event.get("i"), "kind": kind, "actor": event.get("actor") or "agent"}

        # A control a recording knew only by its label (one imported from a
        # tool that saw the page's accessible names, ingest.py) is still a target:
        # it is found by that name alone, and never guessed between.
        if element.get("role") or element.get("path") or element.get("name"):
            step["target"] = {key: element[key] for key in ("role", "name", "path", "window_title", "source", "nth")
                              if element.get(key) is not None}

        for key in ("point", "end", "keys", "option", "direction", "amount", "append", "files"):
            if event.get(key) not in (None, [], ""):
                step[key] = event[key]

        # A click on something the screen exposed no element for is a
        # coordinate, and a coordinate alone says nothing about what is there
        # now. A picture of what was around it, taken from the frame it was
        # made on, lets the replay check.
        if kind in LOOK_KINDS and step.get("point") and "target" not in step and event.get("before"):
            png = frames.png(event["before"])
            look = visual.make_look(png, step["point"], event.get("screen") or meta.get("screen")) if png else None
            if look:
                step["look"] = look

        # A step an importer could not tie to a control (it named one by a number, or a CSS
        # selector, and nothing said what that was). It is not "type into whatever has
        # focus": that is a different step, and this one meant a particular field.
        if (event.get("imported") or {}).get("why"):
            step["unresolved"] = event["imported"]["why"]

        if kind == "open":
            step["open"] = event.get("target") or ""
            step["new_tab"] = bool(event.get("new_tab"))

        if kind == "manual":
            step["reason"] = event.get("reason") or "a private step"

        if kind == "type":
            # A field with no name (a login form's two boxes) is called by the label
            # beside it, as a person would: "Username".
            label = element.get("name") or element.get("hint") or ""
            withheld = (event.get("withheld") or {}).get("text")
            secret = bool(withheld or element.get("secret"))
            step["param"] = _param_name(label, used)
            params.append({
                "name": step["param"],
                "label": label or "Typed text",
                "default": None if secret else (event.get("text") or ""),
                "secret": secret,
                "reason": withheld or element.get("secret"),
                "step": step["n"],
                # What the field takes, checked against the value a run is given.
                "constraint": None if secret else constraints.infer(label, element.get("role"), event.get("text")),
            })

        context = {"window": event.get("window") or "", "url": event.get("url") or ""}
        gate_element = element if element.get("role") else None
        if gate_element is None and kind in LOOK_KINDS and element.get("name"):
            # Known by its label only: judged as the button it is almost always.
            gate_element = {"role": "button", "name": element["name"]}

        gated = approval.assess(kind, gate_element, text=event.get("text"),
                                keys=event.get("keys"), context=context)
        step["consequential"] = bool(event.get("approved")) or gated is not None

        step["expect"] = mine_checks(event.get("expect") or {}, reference)

        # A field that came out exactly as typed when recorded is checked for
        # the text typed this time. One that reformatted it ("1000" ->
        # "1,000.00") is not: the replay cannot know the new format.
        if kind == "type" and not params[-1]["secret"] and not step.get("append"):
            recorded = event.get("text")
            for value in (event.get("expect") or {}).get("values") or []:
                if recorded and value.get("value") == recorded and value.get("name") == element.get("name"):
                    step["expect"].append({"type": "value", "role": value["role"], "name": value["name"],
                                           "param": step["param"]})
                    break
        step["screen"] = event.get("screen") or meta.get("screen")
        step["window"] = event.get("window") or ""
        step["summary"] = _summary(step)

        # Where the control was on the screen, kept beside the target rather
        # than in it so nothing that compares targets changes. It is what lets
        # healing.py tell a control that moved from a different one (the
        # containers and the name it also has from the target's path).
        if element.get("box"):
            step["locate"] = {"box": element["box"], "screen": step["screen"], "anchors": []}

        steps.append(step)

    _phases(steps)

    return {
        "schema": "decurix.replay-program/v1",
        "session": meta.get("id"),
        "title": meta.get("title") or "",
        "kind": meta.get("kind"),
        "params": params,
        "steps": steps,
    }


def _phases(steps):
    """
    Divide a program into phases: a run of steps in one place (one site or one
    window), numbered from 1 and named for it. Not used to run anything - it is
    what lets a question or a halt say "step 6, in Customers" rather than only
    "step 6", and what a person is shown when something needs deciding.
    """

    phase, place, seen = 0, None, {}

    for step in steps:
        here = _place(step)

        if here != place or not phase:
            phase, place = phase + 1, here

        seen.setdefault(phase, here)
        step["phase"] = phase
        step["phase_label"] = seen[phase]


def _place(step):
    """Where a step happened: the stable part of its window title, else its site."""

    title = _stable_title(step.get("window"))
    if title:
        return title

    target = step.get("target") or {}
    site = (target.get("path") or "").split(" > ")[0]
    return site or (step.get("window") or "").strip() or "the screen"


def _param_name(label, used):
    base = re.sub(r"[^a-z0-9]+", "_", (label or "").lower()).strip("_")[:32] or "text"
    if not base[0].isalpha():
        base = "field_" + base

    name, n = base, 2
    while name in used:
        name = f"{base}_{n}"
        n += 1

    used.add(name)
    return name


def _stable_title(title, reference=None):
    """
    The parts of a window title a replay can expect to see again: segments
    with no digits in them ("Untitled 2 - LibreOffice Calc" -> "LibreOffice
    Calc") that are not volatile. None when nothing is left.
    """

    if not title:
        return None

    kept = []
    for segment in TITLE_SPLIT.split(volatility.without_counters(title.lstrip("*"))):
        segment = segment.strip()
        if segment and not re.search(r"\d", segment) and volatility.classify(segment, reference) != "clock_time":
            if volatility.classify(segment, reference) in (None, "too_short") and len(segment) >= 3:
                kept.append(segment)

    return " - ".join(kept) or None


def mine_checks(expect, reference=None):
    """
    What a step's recorded result lets a replay check: only what will read
    the same next time.
    """

    checks = []

    url = url_shape(expect.get("url"))
    if url:
        checks.append({"type": "url", "value": url})

    for title in (expect.get("windows_opened") or [])[:MAX_WINDOW_CHECKS]:
        stable = _stable_title(title, reference)
        if stable:
            checks.append({"type": "window", "value": stable})

    if not any(check["type"] == "window" for check in checks):
        stable = _stable_title(expect.get("active_window"), reference)
        if stable:
            checks.append({"type": "active_window", "value": stable})

    appeared = sorted(
        (e for e in expect.get("appeared") or [] if e.get("role") in CHECKED_ROLES),
        key=lambda e: CHECKED_ROLES[e["role"]],
    )

    elements = 0
    for element in appeared:
        name = volatility.without_counters(element.get("name") or "")
        # A name with a number in it is an invoice number, a row count or a
        # date far more often than it is a label.
        if name and not re.search(r"\d", name) and volatility.classify(name, reference) is None:
            checks.append({"type": "element", "role": element["role"], "name": name})
            elements += 1
            if elements >= MAX_ELEMENT_CHECKS:
                break

    return checks


def url_shape(url):
    """A URL as a replay compares it: origin and path, id-like path segments as '*'."""

    if not url:
        return None

    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return None

    if parts.scheme not in ("http", "https", "file") or not (parts.netloc or parts.scheme == "file"):
        return None

    segments = ["*" if _volatile_segment(s) else s for s in parts.path.split("/")]
    return f"{parts.scheme}://{parts.netloc}" + "/".join(segments)


def _volatile_segment(segment):
    return len(re.findall(r"\d", segment)) >= 2 or bool(re.fullmatch(r"[A-Za-z0-9_-]{20,}", segment))


def url_matches(shape, url):
    try:
        want = urllib.parse.urlsplit(shape)
        have = urllib.parse.urlsplit(url or "")
    except ValueError:
        return False

    if (want.scheme, want.netloc) != (have.scheme, have.netloc):
        return False

    wanted = want.path.rstrip("/").split("/")
    actual = have.path.rstrip("/").split("/")

    return len(wanted) == len(actual) and all(w == "*" or w == a for w, a in zip(wanted, actual))


def _summary(step):
    kind = step["kind"]
    target = step.get("target")

    if target:
        what = f"{target.get('role', 'element')} {el._quote(target.get('name') or '', 50)}"
    elif step.get("point"):
        what = f"{step['point'][0]},{step['point'][1]}"
    else:
        what = ""

    if kind == "type":
        return f"type {{{step['param']}}} into {what or 'the focused field'}"
    if kind == "key":
        return f"press {' '.join(step.get('keys') or [])}" + (f" on {what}" if target else "")
    if kind == "open":
        return f"open {step.get('open')}"
    if kind == "manual":
        if step.get("by_hand"):
            return f"you do this by hand: {step['by_hand']}"
        return f"a private step done by hand ({step.get('reason')})"
    if kind == "scroll":
        return f"scroll {step.get('direction', 'down')}" + (f" on {what}" if what else "")
    if kind == "select":
        return f"select {el._quote(step.get('option') or '')} in {what}"
    if kind == "drag":
        end = step.get("end") or ["?", "?"]
        return f"drag {what} to {end[0]},{end[1]}"
    if kind == "upload":
        files_str = ", ".join(step.get("files") or [])
        return f"upload {files_str} to {what}"
    return f"{kind.replace('_', ' ')} {what}".strip()


# ============================================================
# Checking the screen
# ============================================================

def _norm(text):
    return " ".join(volatility.without_counters(text or "").lower().split())


def _segments(title):
    return [s for s in (_norm(part.lstrip("*")) for part in TITLE_SPLIT.split(title or "")) if s]


def title_matches(wanted, title):
    """Whether every segment of `wanted` is a segment of `title`, in order."""

    have = _segments(title)
    index = 0

    for segment in _segments(wanted):
        while index < len(have) and have[index] != segment:
            index += 1
        if index == len(have):
            return False
        index += 1

    return True


PLACEHOLDER = re.compile(r"\{(\w+)\}")


def fill_name(name, values):
    """
    A check's element name with its `{param}` placeholders filled in from the
    run's values (a result like "Saved Initech" that follows the customer typed).
    A placeholder with no value stays as written, so it fails to match rather
    than passing on something that was not seen.
    """

    return PLACEHOLDER.sub(lambda m: str((values or {}).get(m.group(1), m.group(0))), name or "")


def unmet(checks, observation, values=None):
    """The checks an observation does not satisfy. `values` are the run's parameters."""

    titles = [w.get("title") or "" for w in observation.get("windows") or []]
    active = observation.get("active_title") or ""
    missing = []

    for check in checks:
        kind = check["type"]

        if kind == "url":
            ok = url_matches(check["value"], observation.get("page_url") or "")
        elif kind == "window":
            want = fill_name(check["value"], values)
            ok = any(title_matches(want, t) for t in titles + [active])
        elif kind == "active_window":
            ok = title_matches(fill_name(check["value"], values), active)
        elif kind == "element":
            want = _norm(fill_name(check["name"], values))
            ok = any(e.get("role") == check["role"] and _norm(e.get("name")) == want
                     for e in observation.get("elements") or [])
        elif kind == "value":
            want = " ".join(((values or {}).get(check["param"]) or "").split())
            ok = any(e.get("role") == check["role"] and _norm(e.get("name")) == _norm(check["name"])
                     and " ".join((e.get("value") or "").split()) == want
                     for e in observation.get("elements") or [])
        else:
            ok = True

        if not ok:
            missing.append(check)

    return missing


def describe_check(check):
    kind = check["type"]
    if kind == "url":
        return f"the page {check['value']}"
    if kind == "window":
        return f"a window {el._quote(check['value'], 60)}"
    if kind == "active_window":
        return f"{el._quote(check['value'], 60)} in front"
    if kind == "value":
        return f"{check['role']} {el._quote(check['name'], 60)} holding {{{check['param']}}}"
    return f"{check['role']} {el._quote(check['name'], 60)}"


def _screen_brief(observation):
    if not observation:
        return ""
    brief = f"the screen shows {el._quote(observation.get('active_title') or 'no active window', 80)}"
    if observation.get("page_url"):
        brief += f" at {(privacy.reduce_url(observation['page_url']) or '')[:120]}"
    return brief


# ============================================================
# Running
# ============================================================

class _Halt(Exception):

    def __init__(self, reason, evidence="", missing=False, options=None):
        super().__init__(reason)
        self.reason = reason
        self.evidence = evidence
        # The controls on the screen closest to the one wanted, when an
        # escalation ladder (escalation.py) looked and was not sure: what a
        # person is offered instead of a bare "not found".
        self.options = options or []
        # The step's target was not on the screen (or was not one element) -
        # as opposed to the action itself failing. It is the only halt an
        # optional step is allowed to step over.
        self.missing = missing


def resolve(step, observation):
    """
    Find a step's target on a screen: (element, point, why not). One of the
    first two is set when it is found.
    """

    target = step.get("target") or {}
    elements = observation.get("elements") or []

    if target:
        path = target.get("path")
        if path:
            exact = [e for e in elements if e.get("path") == path]
            if len(exact) == 1:
                return exact[0], None, None

        role, name = target.get("role"), target.get("name")
        nth = target.get("nth")
        what = f"{role or 'element'} {el._quote(name or '')}" + (f" (#{nth})" if nth else "")

        if role or name:
            matches = [e for e in elements
                       if (not role or e.get("role") == role) and _norm(e.get("name")) == _norm(name)]

            # By its label alone, a heading or a paragraph reading the same is
            # not the control that was meant - not merely a worse candidate.
            if not role:
                matches = [e for e in matches if e.get("role") in ACTIVATING_ROLES]

            # A recording that could not name a control (two `textbox ''` in a login
            # form) says which one it was by its place among those that look the
            # same. That is a rule of the target, not a guess of the replay's - and
            # it is exactly as good as the page's order staying as it was.
            if nth:
                matches = matches[nth - 1:nth] if isinstance(nth, int) and nth >= 1 else []

            narrowers = [
                lambda e: "offscreen" not in (e.get("states") or []),
                lambda e: e.get("group") == core.active_group(observation),
            ]
            if target.get("window_title"):
                narrowers.insert(0, lambda e: title_matches(target["window_title"], e.get("window_title") or ""))

            for narrow in narrowers:
                if len(matches) > 1:
                    matches = [e for e in matches if narrow(e)] or matches

            if len(matches) == 1:
                return matches[0], None, None
            if matches:
                return None, None, f"{len(matches)} elements match {what}, and a replay does not guess which"

        return None, None, f"{what} is not on the screen"

    point = step.get("point")
    if point:
        size = observation.get("size")
        if step.get("screen") and size and list(step["screen"]) != list(size):
            return None, None, "the step was recorded as a point on a screen of a different size"
        window = _stable_title(step.get("window"))
        if window and not title_matches(window, observation.get("active_title") or ""):
            return None, None, f"the step was recorded as a point in {el._quote(window, 60)}, which is not in front"

        if step.get("look") and observation.get("frame") is not None:
            seen = visual.find(step["look"], observation["frame"])
            observation.setdefault("visual", {})[step.get("n")] = seen

            if seen["status"] == visual.MISSING:
                return None, None, LOOK_MISSING
            if seen["status"] == visual.AMBIGUOUS:
                return None, None, (f"{seen['matches']} places on the screen look like what was clicked, "
                                    "and a replay does not guess which")
            if seen["status"] == visual.FOUND:
                return None, seen["point"], None

        return None, point, None

    return None, None, "the recording has nothing to find this step's target by"


def _find(step, ladder=None):
    """
    Look until the step's target is on the screen, for up to FIND_SECONDS:
    (element, point, observation, repair). With a ladder, a target that never
    turns up as recorded is taken up its rungs before the step halts; `repair`
    is the ladder's answer when that is how it was found.
    """

    deadline = time.monotonic() + FIND_SECONDS
    last_kin = None

    while True:
        observation = core.observe()
        element, point, reason = resolve(step, observation)

        if element is not None or point is not None:
            return element, point, observation, None

        # What was clicked reads differently because this run typed something
        # else (the first result of another search): the same kind of thing in
        # the same place in order is what the person clicked. Only once two
        # looks at the screen agree on it, so a page still loading is not clicked.
        if reason == LOOK_MISSING and step.get("follows_change") and not step.get("consequential"):
            kin = _kin(step)
            seen = visual.find_kin(kin, observation.get("frame")) if kin else None
            if seen and seen["point"] == last_kin:
                observation["kin"] = seen
                return None, seen["point"], observation, None
            last_kin = seen["point"] if seen else None

        if time.monotonic() >= deadline:
            # Two candidates is not "not there": a replay does not guess
            # between them, and neither may an optional step.
            missing = "does not guess" not in reason

            if ladder is not None:
                repair = ladder.recover(step, observation, reason)
                if repair["element"] is not None:
                    return repair["element"], None, observation, repair
                raise _Halt(reason, _screen_brief(observation), missing=missing, options=repair["options"])

            raise _Halt(reason, _screen_brief(observation), missing=missing)

        time.sleep(POLL_SECONDS)


def _run_step(step, values, ladder=None, report=None):
    kind = step["kind"]

    if kind not in REPLAYABLE:
        raise _Halt(f"a replay cannot {kind.replace('_', ' ')}")

    if step.get("unresolved"):
        raise _Halt(f"the recording names a control it cannot identify: {step['unresolved']}")

    try:
        if kind == "open":
            core.open_target(step["open"], new_tab=step.get("new_tab", False))
            return {"status": "done", "observation": None}

        target = {}
        if kind in NEEDS_TARGET and not (step.get("target") or step.get("point")):
            # Nothing is clicked on a hope: a step recorded with neither a
            # control nor a position (an import that only had a CSS selector)
            # stops the run before it acts.
            raise _Halt("the recording has nothing to find this step's target by")

        if step.get("target") or step.get("point"):
            element, point, seen, repair = _find(step, ladder)
            target = {"path": element["path"]} if element is not None else {"point": tuple(point)}

            if element is None and seen.get("kin"):
                kin = seen["kin"]
                (report or progress.emit)(
                    f"Step {step['n']}: what was clicked reads differently now that the typed text has changed - "
                    f"clicking the {_ordinal(kin['ordinal'])} line of text in its colour and size in that column, "
                    f"as recorded, at {kin['point'][0]},{kin['point'][1]}")

            seen_look = (seen.get("visual") or {}).get(step.get("n")) if element is None else None
            if seen_look and seen_look["status"] == visual.FOUND and seen_look["offset"] != (0, 0):
                (report or progress.emit)(f"Step {step['n']}: what was clicked has moved {seen_look['offset'][0]:+d},"
                              f"{seen_look['offset'][1]:+d} px since it was recorded - clicking where it is now")

            if ladder is not None and element is not None:
                if repair:
                    ladder.repair(step, element, seen, repair["how"], repair["confidence"])
                else:
                    ladder.found(step, element, seen)

        files = step.get("files")
        if step.get("param") and values.get(step["param"]):
            val = values.get(step["param"])
            files = val if isinstance(val, list) else [val]

        return core.act(
            kind,
            text=values.get(step.get("param")) if kind == "type" else None,
            keys=step.get("keys"),
            direction=step.get("direction"),
            amount=step.get("amount"),
            option=step.get("option"),
            append=bool(step.get("append")),
            end=tuple(step["end"]) if step.get("end") else None,
            files=files,
            **target,
        )
    except ComputerError as exc:
        raise _Halt(str(exc))


def _await_checks(checks, observation, values):
    """Wait, up to EXPECT_SECONDS, for a step's checks: (unmet checks, last observation)."""

    observation = observation or core.observe()
    missing = unmet(checks, observation, values)
    deadline = time.monotonic() + EXPECT_SECONDS

    while missing and time.monotonic() < deadline:
        time.sleep(POLL_SECONDS)
        observation = core.observe()
        missing = unmet(checks, observation, values)

    return missing, observation


# Why a point click did not run when its picture was not found (resolve). A
# person may still say the spot is right (skill_task's "click anyway").
def _kin(step):
    """
    What kind of thing a point step clicked (visual.make_kin), read from the
    frame its recording kept; None when there is no such frame any more. Read
    once per run and kept on the run's copy of the step.
    """

    if "kin" in step:
        return step["kin"]

    kin = None
    session, index = step.get("session"), step.get("event")
    if session and index is not None:
        try:
            _, events = recording.load(session)
            event = next((e for e in events if e.get("i") == index), None)
            png = _Frames(session).png(event.get("before")) if event else None
            if png:
                width, height, rgb = visual.decode_png(png)
                kin = visual.make_kin(rgb, width, height, step.get("point"))
        except Exception as exc:  # best effort: without it the step halts, as it always has
            log(f"replay: could not read what step {step.get('n')} clicked: {exc}")
            kin = None

    step["kin"] = kin
    return kin


def _ordinal(n):
    n = int(n)
    if 10 <= n % 100 <= 20:
        return f"{n}th"
    return f"{n}{ {1: 'st', 2: 'nd', 3: 'rd'}.get(n % 10, 'th') }"


def _first_change(program, values):
    """
    The first step that types something other than what was recorded, or
    None. After it, what the screen shows may read differently (the results of
    another search), and a point click's look is not expected to match.
    """

    steps = []
    for param in program.get("params") or []:
        recorded = param.get("example") if param.get("example") is not None else param.get("default")
        value = values.get(param["name"])
        if recorded is None or value is None or isinstance(recorded, list) or isinstance(value, list):
            continue
        if str(value) != str(recorded):
            steps.append(param["step"])
    return min(steps) if steps else None


LOOK_MISSING = ("what was around the click when it was recorded is not on the screen "
                "(it looks different, or has moved too far), so a replay does not click there")


def _fit_screen(steps, report):
    """
    Put the screen back to the size the run's coordinate clicks were recorded
    at, when it is another size now and the display allows it.

    A screen's size follows whoever last viewed it: noVNC's remote resize
    makes it the size of that person's browser. A demonstration recorded at
    1527x727 then met a 1280x720 screen after a restart and halted at its
    first click ("a point on a screen of a different size"), although the
    same size again lays the same window out pixel for pixel. Only a click
    recorded as a point needs this; a step found by its control does not,
    and steps recorded at different sizes are left to halt as before.
    """

    sizes = {tuple(s["screen"]) for s in steps if s.get("point") and s.get("screen")}
    if len(sizes) != 1:
        return

    wanted = sizes.pop()
    try:
        if tuple(screen.size()) == wanted:
            return
        fitted = screen.set_size(*wanted)
    except Exception as exc:
        log(f"Could not check or set the screen size: {exc}")
        return

    if fitted:
        report(f"Set the screen to {wanted[0]}x{wanted[1]}, the size this was recorded at, so its clicks land where they did.")
        # Windows re-lay themselves out after a resize; the first step's own
        # wait for its target covers the rest.
        time.sleep(1)


def run(program, params=None, start=1, report=None, ladder=None):
    """
    Run a program from step `start` and say how it ended: {"outcome",
    "text", "step", "resume_from", "reason", "evidence", "done",
    "verified", "unverified", "total", "missing", "session", "options",
    "healed", "judged"}.

    With a `ladder` (escalation.Ladder) a control that is not where it was
    recorded is healed, grounded by the engine or offered to a person as
    numbered `options`, and a result that is not there exactly may be judged
    by the engine; without one, it halts, as it always has.
    """

    report = report or progress.emit
    if ladder is not None:
        ladder.attach(report)
    steps = program.get("steps") or []
    total = len(steps)
    given = {name: str(value) for name, value in (params or {}).items()}
    start = max(1, int(start or 1))

    values, absent = {}, []
    for param in program.get("params") or []:
        if param["step"] < start:
            continue
        if param["name"] in given:
            values[param["name"]] = given[param["name"]]
        elif param["secret"]:
            absent.append(param)
        else:
            values[param["name"]] = param.get("default") or ""

    counts = {"done": 0, "verified": 0, "unverified": 0, "skipped": 0}

    def finish(outcome, step=None, reason="", resume_from=None, evidence="", options=None):
        result = dict(
            counts, outcome=outcome, total=total, step=step["n"] if step else None,
            reason=reason, resume_from=resume_from, evidence=evidence,
            missing=[p["name"] for p in absent], session=program.get("session"),
            options=options or [], healed=len(ladder.healed) if ladder else 0,
            judged=len(ladder.judged) if ladder else 0, trail=list(ladder.trail) if ladder else [],
            repairs=ladder.summary() if ladder else "", model_calls=ladder.model_calls if ladder else 0,
        )
        result["text"] = _summary_text(program, result, step)
        return result

    if not steps:
        return finish(HALTED_BEFORE_EFFECT, reason="the recording has no steps a replay can run")
    if start > total:
        return finish(HALTED_BEFORE_EFFECT, reason=f"it has only {total} steps")

    if absent:
        first = next(s for s in steps if s["n"] == absent[0]["step"])
        needed = ", ".join(f"{p['label']} ({p['name']})" for p in absent)
        return finish(HALTED_BEFORE_EFFECT, first, resume_from=start,
                      reason=f"it needs {needed}, which the recording did not keep - give it when starting the replay")

    # A value the field cannot take is refused now, not typed into it.
    wrong = constraints.violations([p for p in program.get("params") or [] if p["step"] >= start and p["name"] in given],
                                   given)
    if wrong:
        param, why = wrong[0]
        first = next(s for s in steps if s["n"] == param["step"])
        return finish(HALTED_BEFORE_EFFECT, first, resume_from=start,
                      reason=f"the value for {param['label']} ({param['name']}) is wrong: {why}")

    _fit_screen([s for s in steps if s["n"] >= start], report)
    changed_at = _first_change(program, values)

    for step in steps:
        if step["n"] < start:
            continue

        if changed_at is not None and step["n"] > changed_at and step.get("look"):
            step = dict(step, follows_change=True, session=step.get("session") or program.get("session"))

        report(f"Replay step {step['n']}/{total}: {step['summary']}")

        if step["kind"] == "manual":
            return finish(NEEDS_HUMAN, step, reason=step.get("reason"), resume_from=step["n"] + 1)

        audit.origin = {"session": program.get("session"), "step": step["n"]}
        try:
            result = _run_step(step, values, ladder, report)
        except _Halt as halt:
            # An optional step (a skill learned that it only sometimes
            # happens) that is not on the screen simply did not happen this
            # time. Nothing else is stepped over: an optional step whose
            # action fails, or whose target is ambiguous, still halts.
            if step.get("optional") and halt.missing:
                counts["skipped"] += 1
                report(f"Left out step {step['n']}: it only sometimes happens and is not on the screen")
                continue

            outcome = HALTED if counts["done"] else HALTED_BEFORE_EFFECT
            return finish(outcome, step, reason=halt.reason, resume_from=step["n"], evidence=halt.evidence,
                          options=halt.options)
        finally:
            audit.origin = None

        if result["status"] == "needs_approval":
            text = result["text"]
            if text.startswith(approval.MARKER):
                text = text[len(approval.MARKER):].strip()
            return finish(NEEDS_APPROVAL, step, reason=text, resume_from=step["n"])

        counts["done"] += 1

        if not step.get("expect"):
            counts["unverified"] += 1
            continue

        missing, observation = _await_checks(step["expect"], result.get("observation"), values)

        if missing:
            described = [describe_check(c) for c in missing]

            # The screen may show the step did what it was for without showing
            # exactly what was recorded. An engine's yes is a judgement, not a
            # check: the step counts as one the run could not verify.
            if ladder is not None and ladder.judge(step, observation, described):
                counts["unverified"] += 1
                report(f"Step {step['n']}: its recorded result is not there exactly, but the screen shows it did its job")
                continue

            reason = "expected " + " and ".join(described) + ", which never appeared"
            evidence = _screen_brief(observation)
            if step.get("consequential"):
                return finish(RECONCILIATION_REQUIRED, step, reason=reason, evidence=evidence)
            return finish(HALTED, step, reason=reason, resume_from=step["n"] + 1, evidence=evidence)

        counts["verified"] += 1
        if ladder is not None:
            ladder.confirm(step)

    return finish(VERIFIED if counts["unverified"] == 0 else COMPLETED_UNVERIFIED)


def _summary_text(program, result, step):
    name = el._quote(program.get("title") or program.get("session") or "the recording", 80)
    total = result["total"]
    where = f"step {step['n']} of {total} ({step['summary']})" if step else "the first step"
    evidence = f" Now {result['evidence']}." if result.get("evidence") else ""
    outcome = result["outcome"]

    if result.get("skipped"):
        ran = f"{result['done']} of {total} steps ran ({result['skipped']} optional, not on the screen, left out)"
    else:
        ran = f"all {total} steps ran" if result["done"] == total else f"steps {total - result['done'] + 1}-{total} ran"

    repairs = f" {result['repairs']}" if result.get("repairs") else ""

    if outcome == VERIFIED:
        used = "" if result.get("model_calls") else " No model was used."
        return f"Replayed {name}: {ran}, and every step's expected result was on the screen.{repairs}{used}"

    if outcome == COMPLETED_UNVERIFIED:
        verified, unverified = result["verified"], result["unverified"]
        confirmed = f"{verified} {'was' if verified == 1 else 'were'} confirmed on the screen"
        return (f"Replayed {name}: {ran}. {confirmed}; {unverified} left nothing a replay can check, "
                f"so look at the result before relying on it.{repairs}")

    if outcome == HALTED_BEFORE_EFFECT:
        return f"Did not replay {name}: stopped at {where} before doing anything, because {result['reason']}.{evidence}"

    if outcome == HALTED:
        return (f"Stopped replaying {name} at {where}: {result['reason']}.{evidence} "
                f"{result['done']} step(s) ran before it, and nothing after it was done.")

    if outcome == RECONCILIATION_REQUIRED:
        return (f"While replaying {name}, {where} ran, but {result['reason']}.{evidence} "
                "It may or may not have taken effect - check before doing it again. The replay will not retry it.")

    if outcome == NEEDS_APPROVAL:
        return f"{result['reason']} (replaying {name}, {where})"

    return (f"Replaying {name}: {where} is one the recording kept private ({result['reason']}), so a person has to "
            "do it. Do it on the desktop's screen, then reply here and the replay carries on from the next step.")


# ============================================================
# Reading recordings, for an engine
# ============================================================

def search(query="", limit=8):
    """Recordings for `query`, best first: demonstrations ahead of tasks, newer ahead of older."""

    wanted = {w for w in WORD.findall((query or "").lower()) if w not in STOP_WORDS}
    ranked = []

    for meta in recording.sessions():
        if not meta.get("event_count"):
            continue

        words = set(WORD.findall(f"{meta.get('title') or ''} {meta.get('description') or ''}".lower()))
        score = len(wanted & words)

        if wanted and not score:
            continue

        if meta.get("kind") == "demonstration":
            score += 0.5

        ranked.append((score, meta.get("started_at") or 0, meta))

    ranked.sort(key=lambda item: (item[0], item[1]), reverse=True)
    return [meta for _, _, meta in ranked[:limit]]


def render_text(meta, program, budget=3500):
    """A recording as an engine reads it: its parameters and each step, within `budget` characters."""

    kinds = {"demonstration": "a demonstration", "task": "a task", "adhoc": "actions run by hand"}
    actors = {"human": "a person", "agent": "an agent"}
    who = " and ".join(actors.get(a, a) for a in meta.get("actors") or []) or "no one"

    lines = [
        f"Recording {program['session']} {el._quote(program.get('title') or '(untitled)', 80)}: "
        f"{kinds.get(meta.get('kind'), 'a recording')} by {who}, {len(program['steps'])} steps."
    ]

    if meta.get("description"):
        lines.append(el._quote(meta["description"], 240))

    if program["params"]:
        described = []
        for param in program["params"]:
            if param["secret"]:
                described.append(f"{param['name']} (secret - must be given)")
            else:
                described.append(f"{param['name']} (recorded as {el._quote(param['default'] or '', 40)})")
        lines.append("Parameters: " + ", ".join(described))

    # What an engine should know before it chooses to replay this (lint.py and
    # workflow.py read this module, so they are imported here rather than above).
    from decurix_agent.computer import lint, workflow

    parts = workflow.build(program)
    if len(parts) > 1:
        lines.append("Parts: " + " | ".join(
            f"{p['n']}. {p['place']} (steps {p['first']}-{p['last']})"
            + (f", done when {p['done_when'][0]}" if p["done_when"] else "") for p in parts))

    verdict = lint.lint(program)
    if verdict["grade"] != lint.READY:
        lines.append(el._quote(f"Readiness: {verdict['grade']}. {verdict['summary']}", 320))

    secrets = "".join(f' --param {p["name"]}="..."' for p in program["params"] if p["secret"])
    footer = f"Run it with no model: gui replay {program['session']}{secrets} [--param name=value] [--from N]"

    used = sum(len(line) + 1 for line in lines) + len(footer) + 40
    shown = 0

    for step in program["steps"]:
        line = f"{step['n']}. {step['summary']}"
        if step.get("consequential"):
            line += " [stops for approval]"
        if step.get("expect"):
            line += " -> then " + ", ".join(describe_check(c) for c in step["expect"])
        if used + len(line) + 1 > budget:
            break
        lines.append(line)
        used += len(line) + 1
        shown += 1

    if shown < len(program["steps"]):
        lines.append(f"(+{len(program['steps']) - shown} more steps)")

    lines.append(footer)
    return "\n".join(lines)
