import re
import time

from decurix_agent.computer import approval, privacy, recording


# ============================================================
# Ingest: a recording made somewhere else, read into ours
#
# Decurix records a person on a desktop's own screen. A recording that already
# exists - a browser flow someone captured in Chrome's DevTools Recorder, which
# Puppeteer Replay, Playwright's importers and Browser Use's Workflow Use all
# read as well - should not have to be done again to be replayed here. This
# turns one into a Decurix recording (recording.py: events.jsonl, no frames), and
# from there everything already works: it compiles into a program, replays with
# no model, teaches a skill, is linted, exported.
#
# What it keeps is what a replay can act on. A step is found again by what
# the recording says about its control, so a selector that names an accessible
# label (`aria/Sign in[role="button"]`, the recorder's first choice) becomes a
# role and a name. A step it can only say how to find with a CSS or XPath
# selector is kept as a step with no target - the replay halts there and lint
# says so - rather than guessed at: nothing here can see the page. What is not
# a step a replay takes (hover, scroll, a script check) is listed as skipped,
# and a wait for an element becomes what the step before it is expected to
# leave on the screen, which is what replay.py checks.
#
# Secrets are the reason this runs through privacy.py like a live recording
# does: the recorder writes what was typed into a password field into its
# JSON, verbatim, and it must not survive the import.
# ============================================================

CHROME_RECORDER = "chrome-recorder"
BROWSERGYM = "browsergym"
OPENHANDS = "openhands"
WORKFLOW_DEMO = "workflow-demo"
FORMATS = (CHROME_RECORDER, BROWSERGYM, OPENHANDS, WORKFLOW_DEMO)

MAX_STEPS = 2000

# Seconds between imported steps on the recording's clock: their real timing is
# not in the file, only their order.
STEP_SECONDS = 0.5

ARIA = re.compile(r'^(?P<name>.*?)(?:\[role="(?P<role>[^"]*)"\])?$', re.DOTALL)

# Roles that say nothing about what kind of control it is.
GENERIC_ROLES = {"", "generic", "statictext", "text", "none", "presentation", "paragraph"}

MODIFIERS = {"Control": "ctrl", "Alt": "alt", "Shift": "shift", "Meta": "super"}

# The names a DOM key has in the X11 keysym vocabulary the recorder and replay use.
KEYS = {
    "Enter": "Return", "Backspace": "BackSpace", "Escape": "Escape", "Tab": "Tab", "Delete": "Delete",
    "ArrowUp": "Up", "ArrowDown": "Down", "ArrowLeft": "Left", "ArrowRight": "Right",
    "Home": "Home", "End": "End", "PageUp": "Prior", "PageDown": "Next", "Insert": "Insert", " ": "space",
}
KEYS.update({f"F{n}": f"F{n}" for n in range(1, 13)})

IGNORED = {"setViewport", "close", "emulateNetworkConditions", "keyUp"}


class IngestError(ValueError):
    """A file that is not a recording this can read."""


def detect(data):
    """The format of parsed JSON, or None."""

    steps = data.get("steps") if isinstance(data, dict) else None

    if isinstance(data, dict) and ("workflow" in data or "apps" in data):
        return WORKFLOW_DEMO

    if isinstance(steps, list) and steps and all(isinstance(s, dict) for s in steps[:50]):
        if all(isinstance(s.get("type"), str) for s in steps[:50]):
            return CHROME_RECORDER
        if any(s.get("kind") in ("open", "upload", "click", "type", "select", "drag") for s in steps[:50]):
            return WORKFLOW_DEMO
        # BrowserGym / AgentLab: a step is an action string and what it was chosen from.
        if all(isinstance(s.get("action"), str) for s in steps[:50]):
            return BROWSERGYM

    events = data if isinstance(data, list) else next(
        (data[k] for k in ("history", "trajectory", "events") if isinstance(data, dict) and isinstance(data.get(k), list)), None)

    if isinstance(events, list) and events and all(isinstance(e, dict) for e in events[:50]):
        # OpenHands: an action is {"action": "run", "args": {...}}, an observation carries "observation".
        if any(isinstance(e.get("args"), dict) and isinstance(e.get("action"), str) or "observation" in e for e in events[:50]):
            return OPENHANDS
        # A bare list of BrowserGym steps.
        if all(isinstance(e.get("action"), str) for e in events[:50]):
            return BROWSERGYM

    return None


def _target(step):
    """
    (role, name) of what a step acted on, from the best of its selectors, or
    (None, None). The recorder lists alternatives, each a list of strings (one
    per shadow root or frame crossed); the last string is the control itself.
    """

    aria = text = None

    for group in step.get("selectors") or []:
        selector = group[-1] if isinstance(group, list) and group else group

        if not isinstance(selector, str):
            continue
        if selector.startswith("aria/") and aria is None:
            aria = selector[len("aria/"):]
        elif selector.startswith("text/") and text is None:
            text = selector[len("text/"):]

    if aria:
        match = ARIA.match(aria)
        name = (match.group("name") or "").strip()
        role = (match.group("role") or "").strip().lower()

        if name:
            return (None if role in GENERIC_ROLES else role), name

    if text:
        return None, text.strip()

    return None, None


def _element(step):
    role, name = _target(step)

    if not (role or name):
        return None

    return {"role": role, "name": name, "source": "cdp"}


def _key(name, held):
    """A DOM key and the modifiers held as the name a replay presses, or None."""

    key = KEYS.get(name, name)
    mods = [MODIFIERS[m] for m in ("Control", "Alt", "Shift", "Meta") if m in held]

    if len(key) == 1 and key.isalpha():
        key = key.lower()

    return "+".join(mods + [key])


def _typed(element, value):
    """(text kept, why withheld): the same rules a person typing into the field is held to."""

    # A field the recorder only knew by its label is judged as the text field
    # it almost certainly is, so "Password" is caught.
    judged = dict(element or {})
    if not judged.get("role"):
        judged["role"] = "textbox"

    text, why = privacy.withhold_text(judged, value)

    if text is not None and len(text) > privacy.MAX_TEXT:
        return None, f"longer than {privacy.MAX_TEXT} characters"

    return text, why


def from_chrome_recorder(data):
    """
    A Chrome DevTools Recorder / Puppeteer Replay export as {"title", "screen",
    "events", "skipped", "ignored"}: events in this project's recording format.
    """

    steps = data.get("steps")
    if not isinstance(steps, list) or not steps:
        raise IngestError("that file has no steps in it")
    if len(steps) > MAX_STEPS:
        raise IngestError(f"that recording has more than {MAX_STEPS} steps")

    events, skipped, screen, ignored = [], [], None, 0
    url, held = "", []

    def skip(i, step, why):
        skipped.append({"step": i + 1, "type": step.get("type"), "why": why})

    def expectation(event):
        return event.setdefault("expect", {})

    for i, step in enumerate(steps):
        kind = step.get("type")

        if kind == "setViewport":
            width, height = step.get("width"), step.get("height")
            if isinstance(width, int) and isinstance(height, int) and width > 0 and height > 0:
                screen = [width, height]
            ignored += 1
            continue

        if kind in IGNORED:
            if kind == "keyUp":
                held = [m for m in held if m != step.get("key")]
            ignored += 1
            continue

        base = {"actor": "human", "outcome": "done", "imported": {"step": i + 1, "type": kind}}
        if url:
            base["url"] = url

        if kind == "navigate":
            target = step.get("url")
            if not isinstance(target, str) or not target:
                skip(i, step, "it has no address")
                continue

            url = privacy.reduce_url(target)
            event = dict(base, kind="open", target=url, url=url, new_tab=False)
            expectation(event)["url"] = url
            events.append(event)
            _asserted(event, step)
            continue

        if kind in ("click", "doubleClick"):
            element = _element(step)
            if kind == "doubleClick":
                name = "double_click"
            else:
                name = {"secondary": "right_click", "auxiliary": "middle_click"}.get(step.get("button"), "click")

            event = dict(base, kind=name, element=element or {})
            if element is None:
                event["imported"]["selectors"] = _selector_texts(step)
            events.append(event)
            _asserted(event, step)
            continue

        if kind == "change":
            element = _element(step)
            value = step.get("value")

            if not isinstance(value, str):
                skip(i, step, "it has no value")
                continue

            text, why = _typed(element, value)
            event = dict(base, kind="type", element=dict(element or {}), text=text)

            # What the field should hold afterwards is what a replay checks the typing by
            # (replay.compile_program turns it into a check on the value it is given).
            if text is not None and element and element.get("role") and element.get("name"):
                event["expect"] = {"values": [{"role": element["role"], "name": element["name"], "value": text}]}

            if why:
                event["withheld"] = {"text": why}
                if privacy.secret_field(dict(element or {}, role=(element or {}).get("role") or "textbox")):
                    event["element"]["secret"] = why
            if element is None:
                event["imported"]["selectors"] = _selector_texts(step)
                event["imported"]["why"] = "it can only be found by a CSS or XPath selector, which a replay cannot use"

            events.append(event)
            continue

        if kind == "keyDown":
            name = step.get("key")

            if name in MODIFIERS:
                if name not in held:
                    held.append(name)
                ignored += 1
                continue

            others = [m for m in held if m != "Shift"]
            if len(name or "") == 1 and not others:
                # A printable character: its effect arrives as a change step.
                ignored += 1
                continue

            events.append(dict(base, kind="key", keys=[_key(name, held)]))
            continue

        if kind == "waitForElement":
            element = _element(step)
            if element and element.get("role") and events and step.get("operator") in (None, ">=", "=="):
                appeared = expectation(events[-1]).setdefault("appeared", [])
                appeared.append({"role": element.get("role") or "", "name": element["name"]})
            else:
                skip(i, step, "it waits for something with no role and label a replay can look for")
            continue

        skip(i, step, {
            "hover": "a replay has no step for hovering",
            "scroll": "a replay finds its target wherever it is on the page",
            "waitForExpression": "it checks with a script, which a replay does not run",
            "customStep": "it is a custom step of the tool that recorded it",
        }.get(kind, f"a {kind} step is not one a replay takes"))

    for event in events:
        expect = event.get("expect")
        if expect and not any(expect.values()):
            event.pop("expect")

        event["summary"] = _summary(event)

    return {
        "title": str(data.get("title") or "").strip()[:200],
        "screen": screen,
        "events": events,
        "skipped": skipped,
        "ignored": ignored,
    }


def _summary(event):
    """
    The line a step is shown as - the recorder's own wording (approval.describe_action),
    so an imported step reads like one recorded here. A step that could only be
    found by a CSS or XPath selector says which, because that is why it will halt.
    """

    kind = event["kind"]

    if kind == "open":
        return f"open {event['target']}"

    element = event.get("element") or {}
    shown = {"role": element.get("role") or "control", "name": element.get("name") or element.get("hint") or ""} \
        if (element.get("role") or element.get("name")) else None
    if shown and element.get("nth"):
        # A control known by its place among identical ones says so: "textbox "Username" (2nd)".
        shown["name"] += f" (#{element['nth']})"

    if shown is None and (event.get("imported") or {}).get("selectors"):
        return f"{kind.replace('_', ' ')} {event['imported']['selectors'][0]} (a selector a replay cannot use)"

    line = approval.describe_action(kind, shown, event.get("text"), event.get("keys"), None,
                                    withheld=(event.get("withheld") or {}).get("text"))

    if kind == "select" and event.get("option"):
        line = line.replace("select ", f"select {event['option']!r} in ", 1)

    return line


def _selector_texts(step):
    """The selectors a step could only be found by, for a person reading why it will halt."""

    found = []

    for group in step.get("selectors") or []:
        selector = group[-1] if isinstance(group, list) and group else group
        if isinstance(selector, str):
            found.append(selector[:120])

    return found[:4]


def _asserted(event, step):
    """What the recorder saw happen after a step (`assertedEvents`), as what it should leave."""

    for asserted in step.get("assertedEvents") or []:
        if not isinstance(asserted, dict) or asserted.get("type") != "navigation":
            continue

        expect = event.setdefault("expect", {})
        if isinstance(asserted.get("url"), str):
            expect["url"] = privacy.reduce_url(asserted["url"])
        if isinstance(asserted.get("title"), str) and asserted["title"].strip():
            expect["active_window"] = asserted["title"].strip()


def import_recording(data, title=None, fmt=None):
    """
    Save what `data` (a parsed export) records as a new demonstration and say
    what became of it: {"session", "format", "imported", "skipped", "ignored"}.
    """

    fmt = fmt or detect(data)

    if fmt == CHROME_RECORDER:
        parsed = from_chrome_recorder(data)
        parsed.setdefault("source_steps", len(data["steps"]))
    elif fmt == WORKFLOW_DEMO:
        from decurix_agent.computer import ingest_workflows
        parsed = ingest_workflows.from_workflow_demo(data)
    elif fmt in (BROWSERGYM, OPENHANDS):
        from decurix_agent.computer import ingest_agents

        parsed = (ingest_agents.from_browsergym if fmt == BROWSERGYM else ingest_agents.from_openhands)(data)
    else:
        raise IngestError("that is not a recording this can read - export it from Chrome's DevTools Recorder, "
                          "or give it a BrowserGym / AgentLab or OpenHands trajectory, as JSON")

    if not parsed["events"]:
        raise IngestError("nothing in that recording is a step a replay can take")

    session_id = recording.new_demonstration_id()
    # A task's own words can carry a credential ("log in with the password ..."); a title is public.
    name = privacy.withhold_spoken_secrets((title or parsed["title"] or "An imported recording").strip()[:200])

    recording.ensure(session_id, title=name, description=f"Imported from a {fmt} export.",
                     source={"format": fmt, "steps": parsed["source_steps"], "skipped": len(parsed["skipped"])},
                     screen=parsed["screen"])

    started = time.time()
    for i, event in enumerate(parsed["events"]):
        recording.append(session_id, event, at=started + i * STEP_SECONDS)

    recording.update(session_id, status="complete", ended_at=started + len(parsed["events"]) * STEP_SECONDS)

    return {
        "session": session_id,
        "format": fmt,
        "imported": len(parsed["events"]),
        "skipped": parsed["skipped"],
        "ignored": parsed["ignored"],
        "warnings": parsed.get("warnings") or [],
    }
