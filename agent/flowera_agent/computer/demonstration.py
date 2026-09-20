import json
import os
import queue
import select
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

from flowera_agent import logging_utils, progress
from flowera_agent.computer import approval, audit, core, privacy, processing, recording, screen, session
from flowera_agent.computer import elements as el
from flowera_agent.computer.errors import ComputerError
from flowera_agent.logging_utils import log


# ============================================================
# Recording a person's demonstration
#
# openadapt-capture records what a person does on a desktop - their input,
# the screen, and the accessibility element under each action - so it can be
# learned from and replayed. This is that for a Flowera desktop, written in
# the format an agent's actions are recorded in (recording.py):
#
# - Input is read as XInput2 raw events, which the X server delivers for
#   every device whichever window has focus. Each event names the device it
#   came from, and that is what tells the person from the agent on a shared
#   screen: someone using the desktop through noVNC arrives as Xvnc's
#   "TigerVNC pointer/keyboard", while every xdotool action an engine takes
#   arrives as "Virtual core XTEST pointer/keyboard". XTEST input is dropped,
#   so an agent working during a demonstration is never recorded as the
#   person. The client announces XInput 2.2: only from 2.1 on are raw events
#   delivered while another client holds a grab (an open menu, a drag).
# - Raw events become actions with openadapt-capture's rules (processing.py)
#   once input has been quiet for QUIET_SECONDS.
# - Each action gets the frame grabbed as its first raw event arrived, and the
#   element under it from the last observation of a settled screen - only if
#   the screen still looked like that observation when the action began.
#   Otherwise the action is kept as a point, and the event says why.
# - Secret fields are handled as they are for an agent (privacy.py), and a
#   person can pause for a private step, such as signing in: no input and no
#   frame is kept until they resume, which records a "manual" step that a
#   replay stops at and hands back to a person.
#
# The recorder runs as a process of its own (python3 -m
# flowera_agent.computer.demonstration <session>), started and signalled by
# the agent's HTTP API, so a crash in X or AT-SPI code cannot take the agent
# down with it.
# ============================================================

ACTIVE_FILE = recording.ROOT / "active-demonstration.json"
READY_FILE = recording.ROOT / "active-demonstration.ready"
LOG_FILE = "/tmp/flowera-demonstration.log"

# Input quiet this long ends a run of actions: long enough to hold a
# double-click and a typed word together, short enough to keep up.
QUIET_SECONDS = 1.2

# How long the screen must stay still before it is read again.
SETTLED_SECONDS = 0.5

# Throttle idle screen polling so background checks do not cause X11 lag.
LOOK_INTERVAL = 1.0

# More of the screen than this changed since the last observation, and its
# elements no longer describe what the person acted on.
STALE_FRACTION = 0.02

START_SECONDS = 10
STOP_SECONDS = 30

TEXT_ENTRY_ROLES = {"textbox", "combobox", "spinbutton", "cell", "document", "paragraph", "terminal"}

_process = None


# ============================================================
# Control, from the agent's HTTP API
# ============================================================

def _read_active():
    try:
        return json.loads(ACTIVE_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


def _write_active(data):
    ACTIVE_FILE.parent.mkdir(parents=True, exist_ok=True)
    tmp = ACTIVE_FILE.with_name(ACTIVE_FILE.name + ".tmp")
    tmp.write_text(json.dumps(data), encoding="utf-8")
    tmp.replace(ACTIVE_FILE)


def _clear_active():
    for path in (ACTIVE_FILE, READY_FILE):
        try:
            path.unlink()
        except OSError:
            pass


def _alive(pid):
    # A recorder this process started is its child, and a child that exited
    # stays a zombie - still "there" to kill(0) - until it is reaped.
    if _process is not None and _process.pid == pid:
        return _process.poll() is None

    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except (OSError, TypeError):
        return False


def _close(session_id, status, reason):
    try:
        current = recording.meta(session_id)
        if current is not None and current.get("status") == "recording":
            recording.update(session_id, status=status, ended_at=time.time(), reason=reason)
    except (OSError, ValueError) as exc:
        log(f"Could not close demonstration {session_id}: {exc}")


def status():
    """
    The demonstration being recorded: {"session", "title", "started_at",
    "paused", "pause_reason", "events"} - or None when there is none.
    """

    active = _read_active()
    if not active:
        return None

    if not _alive(active.get("pid", 0)):
        _close(active.get("session", ""), "interrupted", "the recorder stopped unexpectedly")
        _clear_active()
        return None

    meta = recording.meta(active["session"]) or {}

    return {
        "session": active["session"],
        "title": meta.get("title", ""),
        "task_id": meta.get("task_id", ""),
        "started_at": active.get("started_at"),
        "paused": bool(active.get("paused")),
        "pause_reason": active.get("pause_reason", ""),
        "events": meta.get("event_count", 0),
    }


def start(title="", description="", task_id=""):
    """
    Start recording a demonstration; returns its meta once the recorder is
    listening. `task_id` is the chat task it is being recorded for ("/record",
    record_task.py): the recorder reports each action there as a chat step.
    """

    global _process

    if status() is not None:
        raise ComputerError("a demonstration is already being recorded on this desktop - stop it first")

    session_id = recording.new_demonstration_id()
    recording.ensure(session_id, title=title or "Untitled demonstration", description=description, task_id=task_id)

    _clear_active()

    env = session.app_env()
    package_root = str(Path(__file__).resolve().parents[2])
    env["PYTHONPATH"] = package_root + (os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else "")

    with open(LOG_FILE, "a", encoding="utf-8") as output:
        _process = subprocess.Popen(
            [sys.executable, "-m", "flowera_agent.computer.demonstration", session_id],
            env=env,
            cwd=package_root,
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

    _write_active({"session": session_id, "pid": _process.pid, "started_at": time.time(), "paused": False})

    deadline = time.monotonic() + START_SECONDS
    while time.monotonic() < deadline and _process.poll() is None:
        try:
            if READY_FILE.read_text(encoding="utf-8").strip() == str(_process.pid):
                return recording.meta(session_id)
        except OSError:
            pass
        time.sleep(0.1)

    reason = (recording.meta(session_id) or {}).get("reason") or "the recorder did not start"

    if _process.poll() is None:
        _process.kill()

    _close(session_id, "failed", reason)
    _clear_active()

    raise ComputerError(f"could not start recording: {reason}")


def _running():
    active = _read_active()
    if not active or not _alive(active.get("pid", 0)):
        raise ComputerError("no demonstration is being recorded")
    return active


def pause(reason=""):
    """Stop keeping anything - input or frames - until resume(). For a private step."""

    active = _running()
    active.update(paused=True, pause_reason=reason or "a private step")
    _write_active(active)
    os.kill(active["pid"], signal.SIGUSR1)
    return recording.meta(active["session"])


def resume():
    active = _running()
    active.update(paused=False)
    _write_active(active)
    os.kill(active["pid"], signal.SIGUSR2)
    return recording.meta(active["session"])


def note(text):
    """
    Add a person's written explanation of what they are doing right now to
    the demonstration being recorded - a "note", sitting at its point in the
    timeline. Never replayed (replay.compile_program skips "note" events),
    and never guessed at: text that looks like a credential is withheld
    exactly as a typed field's would be, since a person explaining themselves
    can paste the wrong thing too.

    Callable whether the recording is currently active or already paused -
    the toolbar pauses before opening its text box, so nothing on the screen
    is kept while a reason is composed, but a note can just as well be added
    to a pause already in effect (e.g. Stop, before something private) without
    disturbing it.

    Always ends paused, with this note's own summary as the reason - even
    when a pause was already in effect, refreshing its reason to this note's
    text rather than whatever it started with. Writing a note is therefore
    never itself what resumes capturing; only an explicit resume() (the
    toolbar's Continue) is - which matters when the pause it is added to
    predates the note and is about to cover something the note must not.

    Goes straight into the session - recording.append already serialises
    every writer with a file lock - rather than through the recorder
    subprocess: reading the screen for the note's frame needs none of that
    process's own XInput2 connections.
    """

    active = _running()

    text = (text or "").strip()
    if not text:
        raise ComputerError("say what you're doing")

    observation = None
    try:
        observation = core.observe(remember=False)
    except ComputerError as exc:
        log(f"Could not read the screen for a note: {exc}")

    masks = privacy.secret_boxes((observation or {}).get("elements"))
    kept, withheld_reason = privacy.withhold_text(None, text)

    entry = {
        "actor": "human",
        "kind": "note",
        "outcome": "done",
        "target": "",
        "text": kept,
        "window": privacy.kept_text((observation or {}).get("active_title")),
        "summary": privacy.shown_text(kept, withheld_reason, limit=200),
    }

    if withheld_reason:
        entry["withheld"] = {"text": withheld_reason}

    session_id = active["session"]
    frame = _masked((observation or {}).get("frame"), masks)

    event = recording.append(session_id, entry, before=frame)

    task_id = (recording.meta(session_id) or {}).get("task_id", "")
    _report_to_chat(session_id, task_id, event, frame, None)

    # Writing a reason is itself stepping away from the demonstration, the
    # same as a private step: (re-)pause, so nothing after it is kept until
    # the person resumes. Reuses the note's own summary as the pause_reason,
    # so the "manual" event resume() eventually writes reads naturally ("a
    # private step, not recorded: <what they said>") rather than whatever
    # reason the pause started with.
    pause(entry["summary"])

    return event


def stop():
    """Stop the demonstration and wait for its last actions to be written; returns its meta."""

    active = _read_active()
    if not active:
        raise ComputerError("no demonstration is being recorded")

    pid = active.get("pid", 0)

    if _alive(pid):
        os.kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + STOP_SECONDS
        while _alive(pid) and time.monotonic() < deadline:
            time.sleep(0.2)
        if _alive(pid):
            os.kill(pid, signal.SIGKILL)
            _close(active["session"], "incomplete", "the recorder did not finish writing in time")

    _close(active["session"], "interrupted", "the recorder stopped before it finished")
    _clear_active()

    return recording.meta(active["session"])


def recover():
    """
    At agent startup: close any demonstration left "recording" by a recorder
    that is gone - the desktop restarted in the middle of it.
    """

    active = _read_active()

    if active and not _alive(active.get("pid", 0)):
        _clear_active()
        active = None

    for found in recording.sessions():
        if found.get("kind") == "demonstration" and found.get("status") == "recording":
            if not active or active.get("session") != found["id"]:
                _close(found["id"], "interrupted", "the desktop restarted while it was being recorded")


# ============================================================
# The recorder process
# ============================================================

# Names of the keys that type nothing, by keysym. python-xlib's own
# keysym_to_string returns None for every one of these (measured: Shift_L and
# Caps_Lock both came back None on a desktop), and a replay presses keys by
# these names through xdotool.
KEY_NAMES = {
    0xFF08: "BackSpace", 0xFF09: "Tab", 0xFF0D: "Return", 0xFF13: "Pause", 0xFF14: "Scroll_Lock",
    0xFF1B: "Escape", 0xFFFF: "Delete", 0xFF50: "Home", 0xFF51: "Left", 0xFF52: "Up",
    0xFF53: "Right", 0xFF54: "Down", 0xFF55: "Prior", 0xFF56: "Next", 0xFF57: "End",
    0xFF61: "Print", 0xFF63: "Insert", 0xFF67: "Menu", 0xFF7F: "Num_Lock", 0xFF8D: "KP_Enter",
    0xFFE1: "Shift_L", 0xFFE2: "Shift_R", 0xFFE3: "Control_L", 0xFFE4: "Control_R",
    0xFFE5: "Caps_Lock", 0xFFE7: "Meta_L", 0xFFE8: "Meta_R", 0xFFE9: "Alt_L", 0xFFEA: "Alt_R",
    0xFFEB: "Super_L", 0xFFEC: "Super_R", 0xFE03: "ISO_Level3_Shift", 0xFE20: "ISO_Left_Tab",
    0x20: "space",
}
KEY_NAMES.update({0xFFBE + n: f"F{n + 1}" for n in range(12)})
KEY_NAMES.update({0xFFB0 + n: f"KP_{n}" for n in range(10)})


def _key_name(keysym, keycode):
    from Xlib import XK

    if keysym in KEY_NAMES:
        return KEY_NAMES[keysym]

    char = _char(keysym)
    if char is not None and char.isprintable() and not char.isspace():
        return char.lower() if char.isalpha() else (XK.keysym_to_string(keysym) or char)

    return XK.keysym_to_string(keysym) or f"keycode_{keycode}"


def _char(keysym):
    """What a keysym types, or None for a key that types nothing."""

    if 0x20 <= keysym <= 0x7E or 0xA0 <= keysym <= 0xFF:
        return chr(keysym)
    if keysym & 0xFF000000 == 0x01000000:
        return chr(keysym & 0x00FFFFFF)
    return None


def _masked(frame, boxes):
    return frame.masked(boxes) if frame is not None else None


# Keys that send focus somewhere a recording cannot follow.
FOCUS_KEYS = {"Tab", "ISO_Left_Tab", "Return", "KP_Enter", "Escape"}


def _focus_after(action, element, focus):
    """
    Where keys go after `action`: (known, element). Clicking a field focuses
    it; clicking anything else, Tab, Return, Escape or a shortcut moves focus
    somewhere unknown; typing leaves it where it was.
    """

    kind = action["kind"]

    if kind in ("click", "double_click"):
        return True, element if element is not None and element["role"] in TEXT_ENTRY_ROLES else None

    if kind == "key":
        keys = action.get("keys") or []
        if any("+" in k or k in FOCUS_KEYS for k in keys):
            return True, None
        return (True, element) if focus[0] else focus

    if kind == "type":
        return (True, element) if focus[0] or element is not None else focus

    return True, None


THUMBNAIL_SCALE = audit.THUMBNAIL_SCALE


def _report_to_chat(session_id, task_id, event, before, after):
    """
    Show one recorded event in the chat, as a step of the task it was
    recorded for - the Recorder's own actions (via _report below) and a
    note() added outside the recorder subprocess alike.
    """

    if not task_id:
        return

    try:
        before_png = before.png(THUMBNAIL_SCALE) if before is not None else None
        after_png = after.png(THUMBNAIL_SCALE) if after is not None else None
    except Exception as exc:
        log(f"Could not encode step screenshots: {exc}")
        before_png = after_png = None

    progress.emit_action(
        event.get("summary") or event.get("kind", "action"),
        {
            "id": f"{session_id}-{event.get('i', '')}",
            "kind": event.get("kind"),
            "target": event.get("target"),
            "outcome": event.get("outcome"),
            "change": event.get("change"),
            "reason": event.get("reason"),
            "session": session_id,
            "index": event.get("i"),
        },
        before_png,
        after_png,
        task_id=task_id,
    )


class Recorder:

    def __init__(self, session_id):
        self.session_id = session_id
        # Set when a person pressed record in the chat for a task: each action
        # is then a step of that task, as an agent's would be.
        self.task_id = (recording.meta(session_id) or {}).get("task_id", "")
        self.raw = queue.Queue()
        self.stopping = threading.Event()
        self.paused = False
        self.pause_requested = False
        self.resume_requested = False
        self.latest = None
        self._poll_frame = None
        self._stable_since = 0.0
        self._last_scroll = 0.0
        self._shift = set()
        self._caps = False
        self._keys_held = set()
        self._pause = None
        self._last_look = 0.0

    # ---- X input, on its own thread and its own connections ----

    def _select_input(self):
        from Xlib import XK, display
        from Xlib.ext import xinput
        from Xlib.protocol import rq

        # Key names like "Shift_L" and "Return" live in keysym groups python-xlib
        # does not load on its own; without them every named key reads as None.
        for group in ("miscellany", "xkb", "latin1"):
            XK.load_keysym_group(group)

        self.input = display.Display()
        self.grabber = display.Display()

        if not self.input.has_extension(xinput.extname):
            raise ComputerError("the desktop's X server has no XInput extension")

        self.opcode = self.input.display.get_extension_major(xinput.extname)

        version = xinput.XIQueryVersion(
            display=self.input.display, opcode=self.opcode, major_version=2, minor_version=2,
        )
        if (version.major_version, version.minor_version) < (2, 1):
            raise ComputerError(f"the X server speaks XInput {version.major_version}.{version.minor_version}; 2.1 is needed")

        # python-xlib parses XInput's device events but not its raw ones.
        raw_event = rq.Struct(
            xinput.DEVICEID("deviceid"),
            rq.Card32("time"),
            rq.Card32("detail"),
            xinput.DEVICEID("sourceid"),
            rq.Card16("valuators_len"),
            rq.Card32("flags"),
            rq.Pad(4),
        )
        self.raw_types = (xinput.RawKeyPress, xinput.RawKeyRelease, xinput.RawButtonPress, xinput.RawButtonRelease)
        for evtype in self.raw_types:
            self.input.ge_add_event_data(self.opcode, evtype, raw_event)

        devices = self.input.xinput_query_device(xinput.AllDevices).devices
        self.agent_devices = {d.deviceid for d in devices if "XTEST" in str(d.name)}
        log("Input devices: " + ", ".join(f"{d.deviceid}={d.name}" for d in devices))

        self.input.screen().root.xinput_select_events([(
            xinput.AllMasterDevices,
            xinput.RawKeyPressMask | xinput.RawKeyReleaseMask | xinput.RawButtonPressMask | xinput.RawButtonReleaseMask,
        )])
        self.input.flush()

    def _grab(self):
        try:
            return screen.grab(display=self.grabber)
        except ComputerError:
            return None

    def _read_input(self):
        from Xlib import X
        from Xlib.ext import ge

        root = self.input.screen().root

        while not self.stopping.is_set():
            try:
                select.select([self.input], [], [], 0.3)
                while self.input.pending_events():
                    event = self.input.next_event()
                    # Xvnc maps keysyms a VNC client sends onto spare keycodes
                    # as they arrive, and says so with MappingNotify. With a
                    # stale keymap its capitals and punctuation read as
                    # unknown keys - measured: "Human Co" became four runs
                    # split by "keycode_66", and lost its capital.
                    if event.type == X.MappingNotify:
                        self.input.refresh_keyboard_mapping(event)
                        continue
                    if event.type != ge.GenericEventCode or getattr(event, "evtype", None) not in self.raw_types:
                        continue
                    if event.data.sourceid in self.agent_devices:
                        continue
                    raw = self._raw(event.evtype, event.data, root)
                    if raw is not None and not self.paused:
                        self.raw.put(raw)
            except Exception as exc:
                log(f"Stopped reading input: {exc}")
                self.stopping.set()
                return

    def _raw(self, evtype, data, root):
        from Xlib import X
        from Xlib.ext import xinput

        now = time.time()

        if evtype in (xinput.RawButtonPress, xinput.RawButtonRelease):
            pointer = root.query_pointer()
            event = {
                "type": "press" if evtype == xinput.RawButtonPress else "release",
                "t": now, "button": int(data.detail), "x": pointer.root_x, "y": pointer.root_y,
            }
            if event["type"] == "press":
                scroll = event["button"] in processing.SCROLL_BUTTONS
                if not scroll or now - self._last_scroll > processing.SCROLL_GAP_SECONDS:
                    event["frame"] = self._grab()
                if scroll:
                    self._last_scroll = now
            return event

        keycode = int(data.detail)
        down = evtype == xinput.RawKeyPress
        keysyms = [self.input.keycode_to_keysym(keycode, index) for index in range(4)]
        base = next((k for k in keysyms if k), 0)
        name = _key_name(base, keycode)

        # Raw events carry no modifier state. Shift is followed from its own
        # keys. Caps Lock is read back from the server whenever a key goes down
        # with nothing held: Xvnc types a capital by toggling Lock around the
        # letter, and a Lock state left on by an earlier session inverted every
        # letter of a demonstration that assumed it started off.
        if name in ("Shift_L", "Shift_R"):
            (self._shift.add if down else self._shift.discard)(name)
        if name == "Caps_Lock" and down:
            self._caps = not self._caps
        elif down and not self._keys_held:
            self._caps = bool(root.query_pointer().mask & X.LockMask)

        char = None
        if down:
            letter = _char(base) is not None and _char(base).isalpha()
            if bool(self._shift) != (self._caps and letter):
                char = _char(keysyms[1] or base)
            else:
                char = _char(base)

        event = {"type": "key_down" if down else "key_up", "t": now, "key": name, "char": char}

        if down:
            if not self._keys_held:
                event["frame"] = self._grab()
            self._keys_held.add(name)
        else:
            self._keys_held.discard(name)

        return event

    # ---- actions, on the main thread ----

    def _look(self):
        """Read the screen again once it has changed and settled."""

        now = time.monotonic()
        if now - self._last_look < LOOK_INTERVAL:
            return
        self._last_look = now

        try:
            frame = screen.grab()
        except ComputerError:
            return

        now = time.monotonic()

        if self.latest is not None and screen.changed_fraction(self.latest["frame"], frame) <= 0.002:
            self._poll_frame = None
            return

        if self._poll_frame is None or screen.changed_fraction(self._poll_frame, frame) > 0.002:
            self._poll_frame, self._stable_since = frame, now
            return

        if now - self._stable_since < SETTLED_SECONDS:
            return

        try:
            self.latest = core.observe(remember=False)
            self.latest = core.observe(remember=False, frame=frame)
        except ComputerError as exc:
            log(f"Could not read the screen: {exc}")

        self._poll_frame = None

    def _element(self, action, observation, frame, focus):
        """
        The element an action was on, and why there is none when there is not.
        `focus` is (known, element): where keys go, as far as the actions
        before this one in the same run tell - known is False at the start of
        a run, when only the observation can say.
        """

        if observation is None:
            return None, "the screen had not been read yet"

        fresh = frame is None or screen.changed_fraction(observation["frame"], frame) <= STALE_FRACTION

        if action["kind"] in ("type", "key"):
            known, focused = focus
            if known:
                return focused, None
            if not fresh:
                return None, None
            return next((e for e in observation["elements"] if "focused" in e.get("states", [])), None), None

        if not fresh:
            return None, "the screen was still changing when this happened"

        return el.at_point(observation["elements"], *action["point"]), None

    def _entry(self, action, element, note, observation):
        kind = action["kind"]
        point = action.get("point")
        text, withheld = privacy.withhold_text(element, action.get("text")) if kind == "type" else (None, None)

        entry = {
            "actor": "human",
            "kind": kind,
            "outcome": "done",
            "target": el.describe(element) if element else (f"{point[0]},{point[1]}" if point else ""),
            "element": privacy.public_element(element),
            "point": point,
            "end": action.get("end"),
            "text": text,
            "keys": action.get("keys") or [],
            "direction": action.get("direction"),
            "amount": action.get("amount"),
            "window": privacy.kept_text((observation or {}).get("active_title")),
            "url": privacy.reduce_url(core._context(observation).get("url")) if observation else None,
            "screen": (observation or {}).get("size"),
            "duration": round(action["t_end"] - action["t"], 3),
            "summary": approval.describe_action(kind, element, action.get("text"), action.get("keys"), point,
                                                withheld=withheld),
        }

        # Fail closed: text whose field cannot be told, typed while a secret
        # field is on the screen, may have gone into that field.
        if kind == "type" and text is not None and element is None \
                and privacy.secret_boxes((observation or {}).get("elements")):
            text, withheld = None, "typed while a password field was on screen, into a field that could not be told"

        if kind == "type":
            # What a person types goes in at the caret. Replayed, it replaces
            # the field only where that is the same thing: a field that was empty.
            #
            # The element comes from the last settled look at the screen, which
            # is usually taken after the typing: the field then reads as exactly
            # what was typed, and that is an empty field that has been typed
            # into, not one that had something in it (which reads as more).
            value = " ".join((element.get("value") or "").split()) if element is not None else ""
            typed = " ".join((action.get("text") or "").split())
            empty_field = element is not None and element["role"] in ("textbox", "combobox", "spinbutton") \
                and (not value or value == typed)
            entry["append"] = not empty_field

        if withheld:
            entry["withheld"] = {"text": withheld}
        if note:
            entry["element_missing"] = note

        return entry

    def _flush(self, buffer):
        actions = processing.merge(buffer)
        if not actions:
            return

        before = self.latest
        settled = core._wait_for_settle(None)

        try:
            after = core.observe(remember=False)
            after = core.observe(remember=False, frame=settled)
        except ComputerError as exc:
            log(f"Could not read the screen after an action: {exc}")
            after = None

        # Reading the screen takes a second or two, and a person does not wait
        # for it. If their next input landed before it finished, the reading
        # shows that input's effect too - measured: a sign-in click's "Welcome
        # back" recorded as what the typing before it did - so it is not
        # recorded as anyone's result.
        looked_until = time.time()
        with self.raw.mutex:
            overtaken = any(event["t"] < looked_until for event in self.raw.queue)

        masks = privacy.secret_boxes((before or {}).get("elements")) + privacy.secret_boxes((after or {}).get("elements"))
        focus = (False, None)

        for index, action in enumerate(actions):
            frame = action.pop("frame", None)
            following = actions[index + 1].get("frame") if index + 1 < len(actions) else None

            element, note = self._element(action, before, frame, focus)
            entry = self._entry(action, element, note, before)
            focus = _focus_after(action, element, focus)

            # The observation after a run of actions describes what the last
            # of them left; the ones before it are followed straight away by
            # the next, whose first frame is their "after".
            if index == len(actions) - 1 and before is not None and after is not None:
                if overtaken:
                    entry["change_missing"] = "the next action came before the screen could be read"
                else:
                    change = core.compare(before, after)
                    entry["change"] = core.summarize(change)
                    entry["expect"] = core.expectations(change)

            after_frame = following if following is not None else (after["frame"] if after else settled)

            before_kept = _masked(frame or (before or {}).get("frame"), masks)
            after_kept = _masked(after_frame, masks)
            event = entry

            try:
                event = recording.append(self.session_id, entry, at=action["t"], before=before_kept, after=after_kept)
            except (OSError, ValueError) as exc:
                log(f"Could not record an action: {exc}")

            self._report(event, before_kept, after_kept)

        if after is not None:
            self.latest = after

    def _report(self, event, before, after):
        """Show one recorded action in the chat, as a step of the task it was recorded for."""

        _report_to_chat(self.session_id, self.task_id, event, before, after)

    def _paused(self):
        active = _read_active() or {}
        before = self.latest
        self._pause = {
            "reason": active.get("pause_reason") or "a private step",
            "frame": before["frame"] if before else None,
            "masks": privacy.secret_boxes(before["elements"]) if before else [],
            "window": (before or {}).get("active_title"),
        }
        self.latest = None
        log(f"Paused: {self._pause['reason']}")

    def _resumed(self):
        pause, self._pause = self._pause, None
        if pause is None:
            return

        core._wait_for_settle(None)

        try:
            after = core.observe(remember=False)
        except ComputerError as exc:
            log(f"Could not read the screen on resuming: {exc}")
            after = None

        masks = pause["masks"] + privacy.secret_boxes((after or {}).get("elements"))

        entry = {
            "actor": "human",
            "kind": "manual",
            "outcome": "done",
            "reason": pause["reason"],
            "target": "",
            "window": privacy.kept_text(pause["window"]),
            "screen": (after or {}).get("size"),
            "summary": f"a private step, not recorded: {pause['reason']}",
        }

        if after is not None:
            entry["change"] = privacy.kept_text(f"after it, {el._quote(after.get('active_title') or 'no window', 80)} is in front")
            entry["expect"] = {"active_window": privacy.kept_text(after.get("active_title")) or None,
                               "url": privacy.reduce_url(after.get("page_url")) or None}

        before_kept = _masked(pause["frame"], masks)
        after_kept = _masked((after or {}).get("frame"), masks)
        event = entry

        try:
            event = recording.append(self.session_id, entry, before=before_kept, after=after_kept)
        except (OSError, ValueError) as exc:
            log(f"Could not record the private step: {exc}")

        # The frame from before the pause, and the screen after it: nothing
        # from during it was ever kept.
        self._report(event, before_kept, after_kept)

        self.latest = after
        log("Resumed")

    def _process(self):
        buffer = []
        last_input = 0.0

        while True:
            try:
                buffer.append(self.raw.get(timeout=0.25))
                last_input = time.monotonic()
                continue
            except queue.Empty:
                pass

            if self.pause_requested:
                self.pause_requested = False
                self._flush(buffer)
                buffer = []
                self._paused()
            elif self.resume_requested:
                self.resume_requested = False
                self._resumed()
            elif self.stopping.is_set():
                self._flush(buffer)
                return
            elif buffer and processing.idle(buffer) and time.monotonic() - last_input >= QUIET_SECONDS:
                self._flush(buffer)
                buffer = []
            elif not buffer and not self.paused:
                self._look()

    def _on_pause(self, *_):
        self.paused = True
        self.pause_requested = True

    def _on_resume(self, *_):
        self.paused = False
        self.resume_requested = True

    def run(self):
        session.ensure()

        try:
            self._select_input()
        except Exception as exc:
            recording.update(self.session_id, status="failed", ended_at=time.time(),
                             reason=f"cannot read this desktop's input: {exc}")
            log(f"Cannot record: {exc}")
            return 1

        signal.signal(signal.SIGTERM, lambda *_: self.stopping.set())
        signal.signal(signal.SIGUSR1, self._on_pause)
        signal.signal(signal.SIGUSR2, self._on_resume)

        try:
            self.latest = core.observe(remember=False)
        except ComputerError as exc:
            log(f"Could not read the screen at the start: {exc}")

        reader = threading.Thread(target=self._read_input, daemon=True)
        reader.start()

        READY_FILE.write_text(str(os.getpid()), encoding="utf-8")
        log(f"Recording demonstration {self.session_id}; ignoring input from devices {sorted(self.agent_devices)}")

        self._process()
        reader.join(timeout=2)

        # This process ends here; its last chat steps must not end with it,
        # and they must reach the chat before record_task's reply does.
        progress.flush()

        recording.update(self.session_id, status="complete", ended_at=time.time())
        log(f"Finished demonstration {self.session_id}")
        return 0


def main(argv=None):
    logging_utils.log_to_file(LOG_FILE)
    args = sys.argv[1:] if argv is None else argv

    if len(args) != 1 or not recording.valid(args[0]) or not args[0].startswith("demo-"):
        print("usage: python3 -m flowera_agent.computer.demonstration demo-<id>")
        return 2

    return Recorder(args[0]).run()


if __name__ == "__main__":
    sys.exit(main())
