import collections
import concurrent.futures
import json
import os
import queue
import re
import select
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

from decurix_agent import logging_utils, progress
from decurix_agent.computer import approval, audit, browser, cdp, core, privacy, processing, recording, screen, session
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log


# ============================================================
# Recording a person's demonstration
#
# openadapt-capture records what a person does on a desktop - their input,
# the screen, and the accessibility element under each action - so it can be
# learned from and replayed. This is that for a Decurix desktop, written in
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
# decurix_agent.computer.demonstration <session>), started and signalled by
# the agent's HTTP API, so a crash in X or AT-SPI code cannot take the agent
# down with it.
# ============================================================


ACTIVE_FILE = recording.ROOT / "active-demonstration.json"
READY_FILE = recording.ROOT / "active-demonstration.ready"
LOG_FILE = logging_utils.log_file("decurix-demonstration.log")

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

# How long End keeps reading input that was already made before it (see
# _read_input), at most. The rest of STOP_SECONDS is for writing it down.
DRAIN_SECONDS = 12

# Keys within this long of the last one are the same burst of typing: only the
# first of a burst has the screen captured with it. A capture per keystroke
# (a full-screen grab on the input thread) put the recorder 10-20 s behind a
# person typing a phrase on a 1-vCPU desktop - measured: End then came before
# " breeding season" and Enter had been read, and they were lost.
KEY_BURST_SECONDS = 1.0
# After this long with no key, every key event is read and the server's Caps Lock state is the recorder's again.
LOCK_RESYNC_SECONDS = 1.0

# A control whose own pixels (inside its border, where a focus ring or hover outline is not) differ by no more than
# this between the last look and the moment it was pressed is still the control that look saw there (_element).
CONTROL_UNCHANGED = 0.02
CONTROL_INSET = 3
# How many recent looks are kept to tell whether a password field was on the screen while text went into a field
# that could not be told (_secret_around).
LOOKS_KEPT = 20

# How far behind the input reader may be before a frame it grabs is not the moment of the press (Recorder._lag).
LATE_SECONDS = 0.5
LATE_NOTE = "it was read after the screen had already changed, so what was pressed is not known"
# A date/time field as a look lists it (cdp.DATE_ROLES), and the values such a field holds.
DATE_STATES = {"date", "date and time", "time", "month", "week"}
DATE_VALUE = re.compile(r"\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2})?|\d{2}:\d{2}|\d{4}-\d{2}|\d{4}-W\d{2}")

# A pause that ends on the screen it began on held no private step: the person paused to write a Reason (the toolbar
# pauses while one is composed), or pressed Stop and Continue with nothing in between. Recording a "manual" step for it
# made every run stop and ask a person to do something by hand that was never done - found teaching an application task whose
# Reason note came out as "29. a private step done by hand". Below this share of the screen, nothing changed (a clock,
# a caret).
UNCHANGED_SCREEN = 0.01

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
    recording.write_whole(ACTIVE_FILE, json.dumps(data))


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
            [sys.executable, "-m", "decurix_agent.computer.demonstration", session_id],
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

    exit_code = _process.poll()
    reason = (recording.meta(session_id) or {}).get("reason")
    if not reason:
        reason = (f"the recorder exited during startup (exit code {exit_code}); see {LOG_FILE}"
                  if exit_code is not None else
                  f"timed out waiting for input capture after {START_SECONDS} seconds; see {LOG_FILE}")

    if exit_code is None:
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
CAPS_LOCK = 0xFFE5


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


# Chords that work on the text of the field the keyboard is in and leave it
# there: select all, copy, cut, paste, undo, redo, and moving or selecting
# with the arrow keys.
EDITING_KEYS = {"a", "c", "v", "x", "z", "y", "left", "right", "up", "down", "home", "end", "backspace", "delete"}


def _edits_in_place(key):
    parts = str(key).lower().split("+")
    return len(parts) > 1 and set(parts[:-1]) <= {"ctrl", "control", "shift"} and parts[-1] in EDITING_KEYS


def _focus_after(action, element, focus):
    """
    Where keys go after `action`: (known, element). Clicking a field focuses
    it; clicking anything else, Tab, Return, Escape or a shortcut moves focus
    somewhere unknown; typing leaves it where it was - and so does a chord
    that only edits (found live: a date replaced with Ctrl+A and then typed
    was recorded as typed "into the screen", twice in one demonstration).
    """

    kind = action["kind"]

    if kind in ("click", "double_click"):
        return True, element if element is not None and element["role"] in TEXT_ENTRY_ROLES else None

    if kind == "key":
        keys = action.get("keys") or []
        if any(("+" in k and not _edits_in_place(k)) or k in FOCUS_KEYS for k in keys):
            return True, None
        return (True, element) if focus[0] else focus

    if kind == "type":
        return (True, element) if focus[0] or element is not None else focus

    return True, None


def _holds(entry, element, after):
    """
    {"role", "name", "value"} when the settled screen shows the field of a
    typed or chosen value holding exactly that value, else None. Never for
    text that was withheld, added to what a field already held, or put into
    a field with no name to find it by again.
    """

    if after is None or element is None or not element.get("name"):
        return None

    if entry["kind"] == "type" and not entry.get("append") and not entry.get("withheld"):
        wanted = entry.get("text")
    elif entry["kind"] == "select":
        wanted = entry.get("option")
    else:
        return None

    wanted = " ".join((wanted or "").split())
    if not wanted:
        return None

    def same_place(seen):
        # The same tab or window: a page keeps its tab when its title changes
        # (saving an order retitled the page the field was on).
        if seen.get("group") and seen.get("group") == element.get("group"):
            return True
        return (seen.get("window_title") or "") == (element.get("window_title") or "")

    for seen in after.get("elements") or []:
        if seen.get("role") == element.get("role") and seen.get("name") == element.get("name") and same_place(seen) \
                and " ".join((seen.get("value") or "").split()) == wanted:
            return {"role": seen["role"], "name": seen["name"], "value": entry["text"] if entry["kind"] == "type" else entry["option"]}

    return None


# Why an action has no element, by where it landed (Recorder._hit's "where").
WHERE_NOTES = {
    "desktop": "it was on the bare desktop, not in a window",
    "browser": "it was on the browser's own bars, which are found again by how they look",
    "list": "it was in a list the browser drew over its page",
}


def _report_to_chat(session_id, task_id, event, before, after):
    """
    Show one recorded event in the chat, as a step of the task it was
    recorded for - the Recorder's own actions (via _report below) and a
    note() added outside the recorder subprocess alike.
    """

    if not task_id:
        return

    try:
        before_png = before.png(audit.THUMBNAIL_SCALE) if before is not None else None
        after_png = after.png(audit.THUMBNAIL_SCALE) if after is not None else None
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
        # End asked for (SIGTERM): input already made is still read, then
        # stopping is set (_read_input).
        self.stop_requested = threading.Event()
        self.stopping = threading.Event()
        self.paused = False
        self.pause_requested = False
        self.resume_requested = False
        self.latest = None
        self._poll_frame = None
        self._stable_since = 0.0
        self._last_scroll = 0.0
        # The smallest gap seen between this machine's clock and the X server's (_lag).
        self._clock_offset = None
        self._shift = set()
        self._caps = False
        self._lock_known = False
        self._keys_held = set()
        self._pause = None
        self._last_look = 0.0
        self._last_key = 0.0
        # Each look's time and whether a secret field was on the screen in it (_seen).
        self._looks = collections.deque(maxlen=LOOKS_KEPT)
        # Frames are grabbed off the input thread (_grab_later).
        self._grabs = None

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

        self._sync_lock()

        self.input.screen().root.xinput_select_events([(
            xinput.AllMasterDevices,
            xinput.RawKeyPressMask | xinput.RawKeyReleaseMask | xinput.RawButtonPressMask | xinput.RawButtonReleaseMask,
        )])
        self.input.flush()

    def _sync_lock(self):
        """Take Caps Lock's state from the server: right only when no key event is waiting to be read."""

        from Xlib import X

        try:
            self._caps = bool(self.input.screen().root.query_pointer().mask & X.LockMask)
            self._lock_known = True
        except Exception as exc:
            log(f"Could not read Caps Lock: {exc}")

    def _agent_key(self, evtype, data):
        """An agent's (XTEST) key: never recorded, but a Caps Lock it presses still changes what letters type."""

        from Xlib.ext import xinput

        if evtype == xinput.RawKeyPress:
            keycode = int(data.detail)
            if self.input.keycode_to_keysym(keycode, 0) == CAPS_LOCK:
                self._caps = not self._caps

    def _grab(self):
        try:
            return screen.grab(display=self.grabber)
        except ComputerError:
            return None

    def _grab_later(self):
        """
        The frame of the moment an input arrived, grabbed on a thread of its own. Found live (2026-10-14): grabbing a
        whole screen on the input thread for each press put the reader seconds behind on a busy desktop, and the
        pointer - which raw events do not carry, so it is read as each one is handled - had moved on by the time a
        release was read: a click on a field was recorded as a drag to the next button, and the typing after it as
        typed into nothing.
        """

        if getattr(self, "_grabs", None) is None:
            return self._grab()
        return self._grabs.submit(self._grab)

    def _read_input(self):
        from Xlib import X
        from Xlib.ext import ge, xinput

        root = self.input.screen().root
        drain_until = None

        while not self.stopping.is_set():
            try:
                readable = select.select([self.input], [], [], 0.3)[0]
                got = False
                if self.stop_requested.is_set() and drain_until is None:
                    drain_until = time.monotonic() + DRAIN_SECONDS
                while self.input.pending_events():
                    if drain_until is not None and time.monotonic() > drain_until:
                        break
                    got = True
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
                        self._agent_key(event.evtype, event.data)
                        continue
                    raw = self._raw(event.evtype, event.data, root)
                    if raw is not None and not self.paused:
                        self.raw.put(raw)
                    elif raw is not None:
                        self._input_while_paused = True
                        if event.evtype == xinput.RawKeyPress:
                            self._typed_while_paused = True

                # End: everything the person did before pressing it is already
                # on this connection, however far behind reading it is. Read it
                # all, then stop - stopping at once dropped the end of every
                # recording whose last actions had not been read yet.
                if drain_until is not None:
                    if (not got and not readable) or time.monotonic() > drain_until:
                        self.stopping.set()
                        return
                elif (not got and not self._lock_known and not self._keys_held
                      and time.time() - self._last_key > LOCK_RESYNC_SECONDS):
                    self._sync_lock()
            except Exception as exc:
                log(f"Stopped reading input: {exc}")
                self.stopping.set()
                return

    def _raw(self, evtype, data, root):
        from Xlib.ext import xinput

        now = time.time()
        lag = self._lag(now, getattr(data, "time", 0))

        if evtype in (xinput.RawButtonPress, xinput.RawButtonRelease):
            pointer = root.query_pointer()  # first: the frame is grabbed later, the pointer must be read now
            event = {
                "type": "press" if evtype == xinput.RawButtonPress else "release",
                "t": now, "button": int(data.detail), "x": pointer.root_x, "y": pointer.root_y,
                "happened": now - lag,
            }
            if lag > LATE_SECONDS:
                event["late"] = True
            if event["type"] == "press":
                scroll = event["button"] in processing.SCROLL_BUTTONS
                if not scroll or now - self._last_scroll > processing.SCROLL_GAP_SECONDS:
                    event["frame"] = self._grab_later()
                if scroll:
                    self._last_scroll = now
            return event

        keycode = int(data.detail)
        down = evtype == xinput.RawKeyPress
        keysyms = [self.input.keycode_to_keysym(keycode, index) for index in range(4)]
        base = next((k for k in keysyms if k), 0)
        name = _key_name(base, keycode)

        # Raw events carry no modifier state. Shift is followed from its own
        # keys, and so is Caps Lock: Xvnc types a capital by toggling Lock
        # around the letter, and its presses arrive in order with the letter's.
        # The server's state is read only when nothing is waiting to be read
        # (_sync_lock: at the start, so a Lock left on by an earlier session
        # does not invert every letter, and after a quiet second). Found live
        # (2026-10-13): reading it back at each key read it after Xvnc had
        # toggled Lock off again - always, on a reader running behind - and
        # "Cafe" typed over VNC was recorded as "cafe".
        if name in ("Shift_L", "Shift_R"):
            (self._shift.add if down else self._shift.discard)(name)
        if name == "Caps_Lock" and down:
            self._caps = not self._caps
        if down:
            self._lock_known = False

        char = None
        if down:
            letter = _char(base) is not None and _char(base).isalpha()
            if bool(self._shift) != (self._caps and letter):
                char = _char(keysyms[1] or base)
            else:
                char = _char(base)

        event = {"type": "key_down" if down else "key_up", "t": now, "key": name, "char": char}

        if down:
            if not self._keys_held and now - self._last_key > KEY_BURST_SECONDS:
                event["frame"] = self._grab_later()
            self._keys_held.add(name)
        else:
            self._keys_held.discard(name)
        self._last_key = now

        return event

    def _lag(self, now, server_ms):
        """
        How long ago (seconds) an input the reader is handling now really happened, from the X server's own timestamp
        on the event. The smallest gap seen between the two clocks is "on time"; anything more is how far behind the
        reader is. A busy desktop reads input seconds late, and a frame grabbed then is not the moment of the press.
        """
        if not isinstance(server_ms, int) or isinstance(server_ms, bool) or not server_ms:
            return 0.0
        offset = now * 1000.0 - server_ms
        if getattr(self, "_clock_offset", None) is None or offset < self._clock_offset:
            self._clock_offset = offset
        return max(0.0, (offset - self._clock_offset) / 1000.0)

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
        grabbed_at = time.time()

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
            self.latest = core.observe(remember=False, frame=frame)
            self.latest["seen_at"] = grabbed_at
            self._seen(self.latest)
        except ComputerError as exc:
            log(f"Could not read the screen: {exc}")

        self._poll_frame = None

    def _element(self, action, observation, frame, focus, located=None):
        """
        The element an action was on, and why there is none when there is not.
        `located` is what the screen itself said the input was on, the moment
        it was made (a native Windows worker: _hit for a press, _front for a
        key) - when there is one, it is the answer. Otherwise the element is
        worked out from the last look at the screen: `focus` is (known,
        element), where keys go as far as the actions before this one in the
        same run tell - known is False at the start of a run, when only the
        observation can say.
        """

        if located is not None:
            if action["kind"] in ("type", "key"):
                return located["element"], None
            if located["element"] is not None:
                return located["element"], None
            return None, WHERE_NOTES.get(located.get("where") or "")

        if observation is None:
            return None, "the screen had not been read yet"

        # Read late, and the last look at the screen was taken after the press: what is under the pointer now may be
        # what the press made. Found live on a 1-vCPU desktop: a click on the web app's Save, read 20 s late, was recorded
        # as the "Submit" the saved form shows in its place - a replay would have submitted the invoice.
        read_after = (observation.get("seen_at") or 0) > (action.get("happened") or 0)
        if action["kind"] not in ("type", "key") and action.get("late") and read_after:
            return None, LATE_NOTE

        fresh = frame is None or screen.changed_fraction(observation["frame"], frame) <= STALE_FRACTION

        if action["kind"] in ("type", "key"):
            known, focused = focus
            if known:
                return focused, None
            if not fresh:
                return None, None
            return next((e for e in observation["elements"] if "focused" in e.get("states", [])), None), None

        element = el.at_point(el.unoccluded(observation["elements"], observation.get("windows"), *action["point"],
                                            popups=observation.get("popups")),
                              *action["point"])

        if not fresh:
            # Something else on the screen changed since the last look (what was just typed, a suggestion list,
            # a clock): the control under the press is still the one that look saw when its own pixels did not
            # change. Found live (2026-10-14): a click on a drop-down a second after typing into the field above
            # it, and a click on Sign in after typing the password, were recorded as bare points.
            if element is not None and _unchanged_control(element, observation["frame"], frame):
                return element, None
            return None, "the screen was still changing when this happened"

        if element is not None:
            return element, None

        # The browser's DOM hit-test is authoritative when a VNC/window-scale
        # mismatch makes its cached accessibility box miss the raw X11 point.
        # It still returns a normal named element, so the recording remains
        # replayable by role/name and the browser stays fully visible.
        browser_visible = any(
            element.get("source") == "cdp" for element in observation["elements"]
        ) or browser.is_browser_title(observation.get("active_title"))
        if browser_visible and cdp.available():
            element = cdp.element_at_screen_point(*action["point"], observation["elements"])
            if element is not None:
                return element, None

        return None, None

    def _entry(self, action, element, note, observation, located=None):
        kind = action["kind"]
        point = action.get("point")
        text, withheld = privacy.withhold_text(element, action.get("text")) if kind == "type" else (None, None)

        # Fail closed: text whose field cannot be told, typed while a secret
        # field is on the screen, may have gone into that field. "On the
        # screen" is any look from the one before the typing on: found live
        # (2026-10-14), a password typed into a field that could not be told
        # was kept in plain text because the look this run was judged by had
        # been taken after Sign in had already taken the page away. Decided
        # before the summary is worded: built first, the summary kept the
        # text this withholds (review 2026-11-03, D-02).
        if kind == "type" and text is not None and element is None \
                and (privacy.secret_boxes((observation or {}).get("elements")) or self._secret_around(action["t"])):
            text, withheld = None, "typed while a password field was on screen, into a field that could not be told"

        # Where it happened: what the screen said at the moment, else the last look.
        if located is not None:
            window, url = located.get("window") or "", located.get("url") or ""
        else:
            window = (observation or {}).get("active_title")
            url = core._context(observation).get("url") if observation else None

        summary = approval.describe_action(kind, element, action.get("text"), action.get("keys"), point, withheld=withheld)
        if kind == "select":
            summary = f"select {el._quote(action.get('option') or '', 60)} in {el.describe(element)}"

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
            "window": privacy.kept_text(window),
            "url": privacy.reduce_url(url) if url else (None if observation is None and located is None else ""),
            "screen": (observation or {}).get("size"),
            "duration": round(action["t_end"] - action["t"], 3),
            "summary": summary,
        }

        if kind == "select":
            entry["option"] = privacy.kept_text(action.get("option"))
        if kind == "upload":
            entry["files"] = list(action.get("files") or [])
            names = ", ".join(os.path.basename(f) for f in entry["files"])
            entry["summary"] = f"upload {names} to {el.describe(element) if element else 'the page'}"

        # The program the window belongs to: what a replay opens when the
        # window is not there (a recording begun with the app already open
        # has no step that opened it).
        if located is not None and located.get("app"):
            entry["app"] = located["app"]

        if kind == "type":
            # What a person types goes in at the caret. Replayed, it replaces
            # the field only where that is the same thing: a field that was empty.
            #
            # The element comes from the last settled look at the screen, which
            # is usually taken after the typing: the field then reads as exactly
            # what was typed, and that is an empty field that has been typed
            # into, not one that had something in it (which reads as more). A
            # field asked as the typing began (`located`) holds what it held
            # before it: measured without that, a look taken part way through
            # "Acme Ltd" read "Acme Lt", and an empty field was kept as one
            # every later run should add to.
            value = " ".join((element.get("value") or "").split()) if element is not None else ""
            typed = " ".join((action.get("text") or "").split())
            before_typing = located is not None and element is located.get("element")
            empty_field = element is not None and element["role"] in ("textbox", "combobox", "spinbutton") \
                and (not value or (value == typed and not before_typing))
            entry["append"] = not empty_field
            # A date field takes its keys segment by segment in the browser's own order ("10062026" for 6 October);
            # what the person entered is the date it now holds, in the field's fixed format - which is what a
            # replay can put back (cdp.type_text / datefields). The keys themselves would be refused there.
            if element is not None and set(element.get("states") or ()) & DATE_STATES and not before_typing \
                    and DATE_VALUE.fullmatch(value) and not withheld:
                entry["text"] = value
                entry["append"] = False
                entry["summary"] = approval.describe_action(kind, element, value, action.get("keys"), point)

        if withheld:
            entry["withheld"] = {"text": withheld}
        if note:
            entry["element_missing"] = note
        if note == LATE_NOTE:
            # Its frames show the screen after the press, so no picture of "what was there" is taken from them.
            entry["late"] = True

        return entry

    # A page's drop-down (<select>) draws its list outside the page: the click
    # that chooses an option lands on nothing the page knows of. Measured: it
    # was recorded as the page text under the list ("Low", when "India" was
    # chosen), and a replay then clicked that. What the person did is choose
    # an option, and the drop-down itself says which - so the click that
    # opened it is held back until the choosing is over, and the pair is
    # recorded as one `select`, which a replay sets without opening anything.

    def _flush(self, buffer):
        buffer = [_with_frame(event) for event in buffer]
        actions = processing.merge(buffer)
        if not actions:
            return

        before = self.latest
        settled = core._wait_for_settle(None)
        settled_at = time.time()

        # One reading, of the settled frame. It used to read the whole screen
        # twice and keep only the second: ~10 s per click on a 1-vCPU desktop,
        # and every action after it waited its turn to reach the chat.
        try:
            after = core.observe(remember=False, frame=settled)
            after["seen_at"] = settled_at
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
        # Keys go where the last run of actions left them: a field it clicked into. Found live (2026-10-14): a
        # person clicked a field, paused a moment longer than QUIET_SECONDS, and typed - the typing, a run of its
        # own, was recorded as typed into nothing.
        carried = getattr(self, "_carried_focus", None)
        focus = (True, carried) if carried is not None else (False, None)
        ready = []

        for action in actions:
            frame = action.pop("frame", None)
            hit, front = action.pop("hit", None), action.pop("focus", None)
            located = front if action["kind"] in ("type", "key") else hit

            element, note = self._element(action, before, frame, focus, located)
            focus = _focus_after(action, element, focus)

            # Keys that follow a click in the same run go where the click put
            # the keyboard, and are in the window the click was in.
            if located is None and action["kind"] in ("type", "key") and ready and ready[-1]["located"] is not None:
                located = dict(ready[-1]["located"], element=element)

            ready.append({"action": action, "element": element, "note": note, "located": located, "frame": frame})

        self._carried_focus = focus[1] if focus[0] else None

        ready = _keyboard_choices(ready, after)
        ready = _dates_entered(ready, after)
        ready = self._file_chooser(ready, after, before)

        for index, item in enumerate(ready):
            action, element, frame = item["action"], item["element"], item["frame"]
            following = ready[index + 1]["frame"] if index + 1 < len(ready) else None

            entry = self._entry(action, element, item["note"], before, item["located"])

            # The observation after a run of actions describes what the last
            # of them left; the ones before it are followed straight away by
            # the next, whose first frame is their "after".
            if index == len(ready) - 1 and before is not None and after is not None:
                if overtaken:
                    entry["change_missing"] = "the next action came before the screen could be read"
                else:
                    change = core.compare(before, after)
                    entry["change"] = core.summarize(change)
                    entry["expect"] = core.expectations(change)

            # A field that holds exactly what was typed into it, or a drop-down
            # that shows the option chosen, once the screen has settled: seen,
            # so a replay can check for it with this run's own value.
            held = _holds(entry, element, after)
            if held is not None:
                values = entry.setdefault("expect", {}).setdefault("values", [])
                if not any(v.get("name") == held["name"] and v.get("role") == held["role"] for v in values):
                    values.append(held)

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
            self._seen(after)

    def _file_chooser(self, ready, after, before=None):
        """
        A person choosing a file for a page goes through the browser's own "Open File" dialog: a click on the
        page's file control, then clicks and keys in a window the page knows nothing of. Found live (2026-10-14):
        that was recorded as those clicks and keys, which a replay cannot use to give another run's file (and, for
        the dialog's own controls, cannot find again). What the person did is upload a file to that control. The
        click that opened the chooser is held, so is everything done inside it, and once it closes with a file
        chosen the run is recorded as one `upload` of that file (by its full path) - which a replay does without
        opening any dialog, and `/learn` asks about like any value. A chooser closed with no file records nothing.
        """

        held = getattr(self, "_chooser", None)
        open_now = after is None or _chooser_in_front(after)
        if held is None:
            for index, item in enumerate(ready):
                # A press the recorder could not name (the screen was still changing) is on the file control when
                # its point is inside that control's box in the last look. Found live (2026-10-14).
                if item["action"]["kind"] == "click" and not _file_control(item["element"]):
                    at = _file_control_at(item["action"].get("point"), before) or _file_control_at(item["action"].get("point"), after)
                    if at is not None:
                        item = dict(item, element=at)
                if item["action"]["kind"] == "click" and _file_control(item["element"]):
                    if not open_now and _chosen(item["element"], after) is None:
                        if index < len(ready) - 1 or not _chooser_appears():
                            continue  # a click on the control that opened nothing
                        open_now = True
                    self._chooser = held = {"item": item, "typed": [], "names": []}
                    rest = ready[index + 1:]
                    ready = ready[:index]
                    break
            else:
                return ready
        else:
            rest, ready = ready, []

        # What was done inside the chooser: typed paths and the names clicked in its list.
        index = 0
        while index < len(rest):
            item = rest[index]
            element = item["element"] or {}
            # Back on the page: the chooser has closed. Keys that closed it (Enter, Alt+O) land on the control
            # that opened it, once focus is back there - they are part of the choosing, not the page's.
            on_opener = item["action"]["kind"] in ("key", "type") and element.get("path") == (held["item"]["element"] or {}).get("path")
            if element.get("source") == "cdp" and not open_now and not on_opener:
                break
            if item["action"]["kind"] == "type" and item["action"].get("text"):
                held["typed"].append(item["action"]["text"])
            elif element.get("name"):
                held["names"].append(element["name"])
            index += 1
        after_chooser = rest[index:]

        if open_now:
            return ready  # still choosing
        self._chooser = None
        opener = held["item"]
        shown = _chosen(opener["element"], after)
        if shown is None:
            return ready + after_chooser  # closed with nothing chosen
        files = _chosen_paths(shown, held["typed"], held["names"])
        upload = dict(opener, action=dict(opener["action"], kind="upload", files=files))
        return ready + [upload] + after_chooser

    def _seen(self, observation):
        """Remember when a look was taken and whether it showed a secret field."""

        if observation is not None:
            self.__dict__.setdefault("_looks", collections.deque(maxlen=LOOKS_KEPT)).append((observation.get("seen_at") or time.time(),
                                bool(privacy.secret_boxes(observation.get("elements")))))

    def _secret_around(self, at):
        """Whether a secret field was on the screen in the last look before `at` or any look since."""

        looks = sorted(getattr(self, "_looks", ()))
        before = [n for n, (seen, _) in enumerate(looks) if seen <= at]
        start = before[-1] if before else 0
        return any(secret for _, secret in looks[start:])

    def _report(self, event, before, after):
        """Show one recorded action in the chat, as a step of the task it was recorded for."""

        _report_to_chat(self.session_id, self.task_id, event, before, after)

    def _paused(self):
        active = _read_active() or {}
        self._carried_focus = None  # what is done while paused is not seen
        before = self.latest
        self._pause = {
            "reason": active.get("pause_reason") or "a private step",
            "frame": before["frame"] if before else None,
            "masks": privacy.secret_boxes(before["elements"]) if before else [],
            "window": (before or {}).get("active_title"),
        }
        self.latest = None
        self._input_while_paused = False
        self._typed_while_paused = False
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

        # Nothing the person did reached the screen while paused: there is no private step, whatever the screen did
        # meanwhile. Found live (2026-10-14): a one-second pause to write a Reason, while the page an earlier click
        # opened was still loading, was recorded as "a private step done by hand" that every run stopped at.
        if not getattr(self, "_input_while_paused", True):
            if after is not None:
                self.latest = after
                self._seen(after)
            log("Resumed (no input from the person while paused, so no private step)")
            return

        # Typing while paused is a private step however little it shows: a password is a few dots, well under
        # UNCHANGED_SCREEN of the screen. Found live (2026-10-21): the password typed during a pause left
        # no manual step, so the learned skill logged in with no password and every replay stopped at the login.
        if not getattr(self, "_typed_while_paused", False) and after is not None and pause["frame"] is not None and \
                screen.changed_fraction(pause["frame"], after.get("frame")) < UNCHANGED_SCREEN:
            self.latest = after
            self._seen(after)
            log("Resumed (nothing changed on the screen while paused, so no private step)")
            return

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
        self._seen(after)
        log("Resumed")

    def _process(self):
        buffer = []
        last_input = 0.0

        while True:
            try:
                item = self.raw.get(timeout=0.25)
                buffer.append(item)
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
                # Whatever the reader put in before it said stop.
                while True:
                    try:
                        item = self.raw.get_nowait()
                    except queue.Empty:
                        break
                    buffer.append(item)
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

        signal.signal(signal.SIGTERM, lambda *_: self.stop_requested.set())
        signal.signal(signal.SIGUSR1, self._on_pause)
        signal.signal(signal.SIGUSR2, self._on_resume)

        self._grabs = concurrent.futures.ThreadPoolExecutor(max_workers=1, thread_name_prefix="grab")
        reader = threading.Thread(target=self._read_input, daemon=True)
        reader.start()

        # Input is being captured now. The first full screen/accessibility
        # observation can take longer than start()'s deadline on a busy
        # desktop, so it must not hold up the ready handshake.
        READY_FILE.write_text(str(os.getpid()), encoding="utf-8")
        log(f"Recording demonstration {self.session_id}; ignoring input from devices {sorted(self.agent_devices)}")

        return self._record(reader)

    def _record(self, reader):
        """From input being taken to the recording being closed: the same on both systems."""

        try:
            first = core.observe(remember=False)
            # A person may act while this first look is in progress. Its
            # result can then describe the screen *after* that action, rather
            # than the screen the action's captured frame shows.
            with self.raw.mutex:
                if not self.raw.queue:
                    self.latest = first
            self._seen(first)
        except ComputerError as exc:
            log(f"Could not read the screen at the start: {exc}")

        self._process()
        reader.join(timeout=2)

        # This process ends here; its last chat steps must not end with it,
        # and they must reach the chat before record_task's reply does.
        progress.flush()

        recording.update(self.session_id, status="complete", ended_at=time.time())
        log(f"Finished demonstration {self.session_id}")
        return 0


def _with_frame(event):
    """An input event with the frame grabbed for it, once that grab is done (Recorder._grab_later)."""

    frame = event.get("frame")
    if isinstance(frame, concurrent.futures.Future):
        try:
            frame = frame.result(timeout=30)
        except Exception as exc:
            log(f"Could not grab the screen for an input: {exc}")
            frame = None
        event = dict(event, frame=frame)
    return event


def _unchanged_control(element, before, after):
    """Whether a control's own pixels, inside its border, are the same in two frames."""

    box = element.get("box")
    if not box or len(box) != 4:
        return False
    x, y, w, h = box
    if w <= 2 * CONTROL_INSET + 4 or h <= 2 * CONTROL_INSET + 4:
        return False
    inner = [x + CONTROL_INSET, y + CONTROL_INSET, w - 2 * CONTROL_INSET, h - 2 * CONTROL_INSET]
    return screen.changed_in(before, after, inner) <= CONTROL_UNCHANGED


# The window a browser (or a desktop app) shows to pick a file: "Open File", "Open Files", "Select file(s) to upload".
CHOOSER_TITLE = re.compile(r"^\s*(open|select|choose|upload|attach)\s+(an?\s+)?(files?|documents?)(\s+to\s+upload)?\s*$"
                           r"|^\s*file\s+upload\s*$", re.I)
NO_FILE = re.compile(r"^\s*(no\s+files?\s+(chosen|selected)|choose\s+files?|browse\.*)?\s*$", re.I)
MANY_FILES = re.compile(r"^\s*\d+\s+files?\s*$", re.I)
# Where a person keeps a file they are about to upload, looked in when the chooser does not say the folder.
LOOK_FOR_FILES_IN = ("work", "Downloads", "Desktop", "Documents", "Pictures")
# How long a file chooser may take to come up after its control is clicked.
CHOOSER_APPEAR_SECONDS = 5


def _file_control(element):
    """Whether an element is a page's file input (what opens the browser's file chooser)."""

    return ((element or {}).get("handle") or {}).get("input_type", "").lower() == "file"


def _file_control_at(point, observation):
    """The file control whose box holds `point` in a look, or None."""

    if not point or not observation:
        return None
    x, y = point
    for element in observation.get("elements") or []:
        box = element.get("box")
        if _file_control(element) and box and box[0] <= x <= box[0] + box[2] and box[1] <= y <= box[1] + box[3]:
            return element
    return None


def _chooser_in_front(observation):
    return bool(CHOOSER_TITLE.match((observation or {}).get("active_title") or ""))


def _chooser_appears(wait=None):
    """
    Whether a file chooser comes to the front within a few seconds. The screen can settle before the browser's
    dialog is up (found live, 2026-10-14: the look after the click was taken ~2 s before it appeared), so a click on a
    file control that seems to have opened nothing is given that long.
    """

    deadline = time.time() + (CHOOSER_APPEAR_SECONDS if wait is None else wait)
    while True:
        try:
            if any(w.get("active") and CHOOSER_TITLE.match(w.get("title") or "") for w in screen.windows()):
                return True
        except Exception:
            return False
        if time.time() >= deadline:
            return False
        time.sleep(0.3)


def _chosen(element, after):
    """What a file control shows once the screen settled - the chosen file's name, "2 files" - or None for none."""

    path = (element or {}).get("path")
    now = next((e for e in (after or {}).get("elements") or [] if path and e.get("path") == path), None)
    value = (now or {}).get("value") or ""
    return None if now is None or NO_FILE.match(value) else value.strip()


def _chosen_paths(shown, typed, names, home=None):
    """
    The full paths of the files chosen: a path typed into the chooser, a folder typed and a name clicked in its
    list, or - when the chooser said neither - the file of that name a person keeps nearest to hand (newest first).
    A name nothing finds is kept as the name: the lesson asks about it like any file.
    """

    home = home or os.path.expanduser("~")
    wanted = [] if MANY_FILES.match(shown) else [shown]
    folders, found = [], []
    for text in typed:
        candidate = os.path.normpath(os.path.expanduser(text.strip()))
        if not os.path.isabs(candidate):
            candidate = os.path.join(home, candidate)
        if os.path.isfile(candidate):
            found.append(candidate)
        elif os.path.isdir(candidate):
            folders.append(candidate)
        elif os.path.isdir(os.path.dirname(candidate)):
            folders.append(os.path.dirname(candidate))
    for name in names + wanted:
        for folder in reversed(folders):
            candidate = os.path.join(folder, name)
            if os.path.isfile(candidate) and candidate not in found:
                found.append(candidate)
                break
    found = [f for f in found if not wanted or os.path.basename(f) == wanted[0]] or found
    if found:
        return found[:1] if wanted else found
    if not wanted:
        return []
    matches = []
    for top in LOOK_FOR_FILES_IN:
        for root, _dirs, files in os.walk(os.path.join(home, top)):
            if wanted[0] in files:
                path = os.path.join(root, wanted[0])
                try:
                    matches.append((os.path.getmtime(path), path))
                except OSError:
                    pass
    return [max(matches)[1]] if matches else wanted


def _native_choice(element):
    """Whether an element is a page's own drop-down (<select>): a combobox nothing can be typed into."""

    handle = (element or {}).get("handle") or {}
    return (element or {}).get("role") == "combobox" and handle.get("editable") is False and element.get("source") == "cdp"


def _dates_entered(ready, after):
    """
    The keys a person typed into a date field are its segments in the browser's own order ("10062026"); what they
    entered is the date the field holds once the screen settled, read from the look after the run (the field's own
    look before it was taken before the typing when another action followed in the same run). Found live: of two
    dates typed one after the other, the first was kept as "10062026", which a replay cannot put back.
    """

    if after is None:
        return ready
    shown = {e.get("path"): e for e in after.get("elements") or [] if e.get("path")}
    out = []
    for item in ready:
        action, element = item["action"], item["element"] or {}
        now = shown.get(element.get("path")) if action["kind"] == "type" else None
        value = (now or {}).get("value") or ""
        if now is not None and set(now.get("states") or ()) & DATE_STATES and DATE_VALUE.fullmatch(value):
            item = dict(item, action=dict(action, text=value), element=dict(element, value=value, states=now.get("states")))
        out.append(item)
    return out


def _keyboard_choices(ready, after):
    """
    A person choosing in a page's drop-down with the keyboard - open it, type the first letters, Enter - is a choice,
    not typing: the keys only move which option is chosen. Found live (2026-10-13): "open the product list, type MUG,
    Enter" was kept as typing "MUG" into the field plus a Return, and the option it chose ("MUG-12 - Ceramic mug
    12oz") was nowhere in the recording. The run of keys is recorded as one `select` of the option the drop-down
    shows once the screen settled, and the click that opened it (in the same run) is folded into it, as a choice
    made with the mouse already is.
    """

    if after is None:
        return ready
    shown = {e.get("path"): e for e in after.get("elements") or [] if e.get("path")}
    out = []
    for item in ready:
        action, element = item["action"], item["element"]
        target = element or {}
        if action["kind"] == "type" and _native_choice(element):
            now = shown.get(target.get("path"))
            option = (now or {}).get("value")
            if option:
                if out and out[-1]["action"]["kind"] == "click" and (out[-1]["element"] or {}).get("path") == target.get("path"):
                    opened = out.pop()
                    action = dict(action, t=opened["action"]["t"])
                choice = {k: v for k, v in action.items() if k != "text"}
                choice.update(kind="select", option=option)
                out.append(dict(item, action=choice))
                continue
        if action["kind"] == "key" and out and out[-1]["action"]["kind"] == "select"                 and (out[-1]["element"] or {}).get("path") == target.get("path")                 and set(action.get("keys") or []) <= {"Return", "Enter", "KP_Enter"}:
            continue  # the Enter that closed the list is part of the choice
        out.append(item)
    return out


def main(argv=None):
    logging_utils.log_to_file(LOG_FILE)
    args = sys.argv[1:] if argv is None else argv

    if len(args) != 1 or not recording.valid(args[0]) or not args[0].startswith("demo-"):
        print("usage: python3 -m decurix_agent.computer.demonstration demo-<id>")
        return 2

    return Recorder(args[0]).run()


if __name__ == "__main__":
    sys.exit(main())

