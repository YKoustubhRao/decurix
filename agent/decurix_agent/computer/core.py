import base64
import json
import os
import re
import shutil
import time
import urllib.parse
from contextlib import contextmanager
from pathlib import Path

from decurix_agent import config, filelock, progress
from decurix_agent.computer import approval, atspi, audit, browser, cdp, grounder, identity, launchers, privacy, screen, session, xinput
from decurix_agent.computer import screen_call
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ActedError, ComputerError
from decurix_agent.computer import selecting, sheets

# Three parts of this module live in their own (follow-up O-10): select's machinery in selecting.py, blocks of
# cells and slide text in sheets.py, and the comparison of two looks in changes.py. What this module, other
# modules and tests still read through core is re-exported here by name. Those modules never import core: what
# they call back here is handed over below (use_core), each looked up in core when called, so a test that
# patches core.observe, core._task_now or core._focus_window patches what they read.
from decurix_agent.computer.changes import (  # noqa: F401 - re-exported
    compare, expectations, summarize, text_change,
)
from decurix_agent.computer.selecting import (  # noqa: F401 - re-exported
    _choose_radio, _decorated_number, _DOCUMENT_WORDS, _editor_opened_by, _in_documents, _in_place_of,
    _named_within, _NOT_A_CHOICE, _note_failed_search, _offered, _offers_nothing, _own_label, _person_said,
    _resolved, _searches, _select_custom, _select_datalist, _select_native_by_id, _selected, _typed_in_place_of,
    CUSTOM_OPEN_SECONDS, CUSTOM_OPTIONS_SECONDS, EDITOR_SECONDS, FAILED_SEARCH_FILE, forget_failed_search,
    MAX_OFFERED_KEPT, MAX_SEARCHES, MIN_LIST_ANSWER, NOT_A_FIELD, OFFERED_SHOWN, SUBSTITUTE, TYPING_ROLES,
)
from decurix_agent.computer.sheets import (  # noqa: F401 - re-exported
    _block_cells, _block_report, _block_span, _cell_holds, _goto_sheet_cell, _is_block, _keep_slide_text, _numeric,
    _read_back_block, _read_back_relayed, _relayed_cells, _relayed_page, _sheet_cell_named, _sheet_rows,
    _sheet_start, _type_relayed_block, _type_relayed_rows, _type_sheet_block, CELL_NAME, DRAWN_TABLES, NOT_SET,
    NOTE, RELAYED_PRESSABLE, RELAYED_READ_TRIES, RELAYED_SETTABLE, RELAYED_VIEWERS,
)

selecting.use_core(observe=lambda *args, **kwargs: observe(*args, **kwargs), _task_now=lambda: _task_now())
sheets.use_core(observe=lambda *args, **kwargs: observe(*args, **kwargs),
                _focus_window=lambda element: _focus_window(element))


# ============================================================
# observe / act / verify
#
# The one action layer the engine and the agent share. TheBot's operator
# reaches it as the `gui` shell command (cli.py) and reads the element list as
# text; the agent's own tasks (replays, skills) call it directly. Whichever door an action comes through, it passes the same
# approval gate and lands in the same audit trail, because both live here and
# not in any engine.
#
# Every action is observe -> act -> observe. The first observation is what
# resolves the target (so a ref always means what is on screen *now*) and is
# the "before" of the audit record; the second is the "after", and the
# difference between them is what the engine is told happened.
# ============================================================

STATE_DIR = config.DECURIX_DIR / "computer"
STATE_FILE = STATE_DIR / "observation.json"

CHROMIUM_APPS = browser.APP_NAMES

SETTLE_SECONDS = 4.0


def _load_state():
    try:
        return json.loads(STATE_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


def _save_state(observation, next_ref):
    STATE_DIR.mkdir(parents=True, exist_ok=True)

    data = {k: v for k, v in observation.items() if k != "frame"}
    data["next_ref"] = next_ref
    data["sample"] = base64.b64encode(observation["frame"].sample()).decode("ascii")

    # Shapes on: a key or code shown in an ordinary field is withheld; the values compared later are exempt by name.
    privacy.write_scrubbed(STATE_FILE, data, atomic=True, exempt=("value", "next_ref", "sample"))


@contextmanager
def _state_lock():
    """
    Guard the read-modify-write of the ref-continuity state, the same way
    approval.py's and recording.py's own stores already guard theirs. The
    atomic rename in _save_state only stops a torn read; it does nothing
    about two observe() calls (two MCP tool calls, or an MCP call racing
    the demonstration recorder's read-only core.observe(remember=False))
    both reading the same "previous" state and each computing their own ref
    assignments, where the second writer would silently discard the first's.
    """

    STATE_DIR.mkdir(parents=True, exist_ok=True)

    with open(STATE_DIR / "observation.lock", "w") as lock:
        filelock.exclusive(lock)
        yield


MAX_APPS_SHOWN = 10


def _desktop_apps():
    """
    The names of the apps that are icons on this desktop, as `open` takes
    them. Found live: with its app closed, an engine double-clicked guessed
    points around the icon, tried the app's address on this machine, then read
    the launcher's script - twelve commands before the window was open. An
    icon has no element to press; its name opens it.
    """

    try:
        names = [entry["name"] for entry in launchers.entries([launchers.PLACES[0]])]
    except Exception:  # a look must never fail for the sake of this line
        return []
    return list(dict.fromkeys(names))[:MAX_APPS_SHOWN]


# What a browser draws in place of a page whose process has died.
_CRASHED_PAGE = re.compile(r"aw,?\s*snap|something went wrong while displaying this webpage|"
                           r"this page isn.t responding|page unresponsive|out of memory", re.IGNORECASE)
CRASHED_PAGE_NOTE = ("THE PAGE IN THE BROWSER HAS CRASHED (the browser shows its own \"Aw, Snap!\" page in its "
                     "place). Nothing on it can be read or pressed, and anything typed into it that was not saved is "
                     "gone. Reload it - gui key F5 - then look again, and check what was saved before entering "
                     "anything a second time.")


def _dead_page_note(frame):
    """
    What to say when the browser's page does not answer because the page is
    dead, or None when the screen does not show that. Found live on a host
    short of memory: the page's process died, the browser drew "Aw, Snap!",
    and all a worker was told was that Chromium "did not answer Page.enable
    within 20s" - every look after that the same, with nothing to act on. The
    screen says what happened, in the browser's own words; they are read off
    it (only on this path - a look that works costs nothing more).
    """

    try:
        from decurix_agent.computer import ocr
        # The browser draws that page light on dark, which the reader (made for dark text on a light
        # page) sees as blank: it is read as it is and with its tones turned over.
        rgb = bytes(frame.rgb)
        shown = (ocr.text(ocr.read(frame.width, frame.height, rgb), limit=4000) + "\n"
                 + ocr.text(ocr.read(frame.width, frame.height, bytes(255 - c for c in rgb)), limit=4000))
    except Exception:
        # No reader on this desktop: the plain "did not answer" note is what there is.
        return None
    return CRASHED_PAGE_NOTE if _CRASHED_PAGE.search(shown or "") else None


@screen_call.routed("core.observe")
def observe(remember=True, frame=None):
    """
    Look at the screen: a screenshot, the open windows, and every accessible
    element in them. Saves the result as the reference for the next action -
    unless `remember` is False, for the demonstration recorder, which looks
    at the screen without moving an engine's refs out from under it.
    """

    session.ensure()

    if frame is None:
        frame = screen.grab()
    windows = screen.windows()
    active = next((w for w in windows if w["active"]), None)
    active_title = active["title"] if active else ""
    if remember:
        _task_windows(windows)  # the windows open at a task's first look are the ones it found, not its own

    found = []
    groups = []
    notes = []
    page_group = None

    if cdp.available():
        try:
            page_elements, page_group = cdp.observe()
            found.extend(page_elements)
            groups.append(page_group)
        except ComputerError as exc:
            notes.append(_dead_page_note(frame) or f"could not read the browser tab: {exc}")
    elif any(browser.is_browser_title(w["title"]) for w in windows):
        notes.append(
            "The browser is open without its debugging port, so its page cannot be read. "
            "Close it and use open <url> to reopen the page."
        )

    try:
        app_elements, app_groups = atspi.observe(
            skip_apps=CHROMIUM_APPS if page_group is not None else ()
        )
        found.extend(app_elements)
        groups.extend(app_groups)
    except ComputerError as exc:
        notes.append(str(exc))

    groups = _order_groups(groups, windows, active_title)
    _withhold_secret_values(found)

    if _human_check(found, page_group):
        notes.append(HUMAN_CHECK_NOTE)

    width, height = frame.width, frame.height

    with _state_lock():
        previous = _load_state()
        next_ref = el.assign_refs(found, previous)
        gone = _gone(previous, found, groups)

        behind = [g for g in groups if g.get("behind")]
        if behind:
            notes = list(notes) + [
                f"the controls of {el._quote(g.get('window_title') or g.get('title') or '', 60)} are from a read made "
                "before your last action (the app's picture is still being read again): where the cursor is and what "
                "the fields hold may have changed - observe again in a few seconds before relying on them."
                for g in behind]
        observation = {
            "behind": bool(behind),
            "time": time.time(),
            "size": [width, height],
            "active_title": active_title,
            "windows": [{"id": w["id"], "title": w["title"], "box": w["box"], "active": w["active"]} for w in windows],
            "popups": screen.popups(),
            "groups": groups,
            "elements": found,
            "notes": notes,
            "page_url": page_group.get("url") if page_group else "",
            "gone": gone,
            "frame": frame,
            "apps": _desktop_apps(),
        }

        if remember:
            _save_state(observation, next_ref)

    return observation


SECRET_VALUE = "[withheld]"


def _withhold_secret_values(elements):
    """
    Every secret field's value (privacy.secret_field: a password, a one-time code, a key) replaced where it is read, at
    the look itself, so no renderer, note or record can show it: the field is shown filled, by SECRET_VALUE. Found in
    review (2026-11-03, D-18): `gui observe` printed a text-type code or key field's value, and the evidence notes the
    final check reads kept it. The value is not kept anywhere else: acting types what it is given and reads the page
    itself, never this copy (review round 2: a raw copy in the handle was one more place a secret sat).
    """

    for e in elements:
        value = e.get("value")
        if value in (None, "", SECRET_VALUE):
            continue
        try:
            secret = privacy.secret_field(e)
        except Exception:  # noqa: BLE001 - not known to be safe to show: withheld
            secret = True
        if secret:
            e["value"] = SECRET_VALUE


# How many refs no longer on the screen are kept with where they were (_gone).
GONE_LIMIT = 600


def _gone(previous, found, groups):
    """
    {"refs": {ref: where it was seen}, "urls": {group: the address it shows now}}: the refs earlier looks printed that
    this one no longer has, with their role, name, window and document (elements.where_seen), for a ref an engine uses
    after its control was drawn anew (elements._same_named re-binds it only within that same window and document).
    """

    previous = previous or {}
    urls_then = {g.get("id"): g.get("url") or "" for g in previous.get("groups") or []}
    refs = dict(((previous.get("gone") or {}).get("refs")) or {})
    present = {e.get("ref") for e in found}
    for old in previous.get("elements") or []:
        if old.get("ref") and old["ref"] not in present:
            refs[old["ref"]] = el.where_seen(old, urls_then.get(old.get("group"), ""))
    for ref in present:
        refs.pop(ref, None)
    if len(refs) > GONE_LIMIT:
        refs = dict(list(refs.items())[-GONE_LIMIT:])
    return {"refs": refs, "urls": {g.get("id"): g.get("url") or "" for g in groups}}


def report_observation(observation):
    """
    Put an explicit look at the screen into the chat's step view.

    Only for an engine's own observe - act() observes twice per action, and
    those looks belong to the action's audit record, not to lines of their
    own.
    """

    count = len(observation["elements"])
    active = observation.get("active_title") or "no active window"
    progress.emit(f"looked at the screen: {el._quote(active, 60)}, {count} elements")


def _order_groups(groups, windows, active_title):
    """Active window first, then the rest topmost first."""

    stacking = [w["title"] for w in windows]

    def rank(group):
        title = group.get("window_title") or ""
        if title and title == active_title:
            return (0, 0)
        if title in stacking:
            return (1, -stacking.index(title))
        return (2, 0)

    return sorted(groups, key=rank)


def active_group(observation):
    groups = observation.get("groups") or []
    if groups and groups[0].get("window_title") == observation.get("active_title"):
        return groups[0]["id"]
    return None


def _context(observation):
    group = active_group(observation)
    url = ""
    for g in observation.get("groups", []):
        if g["id"] == group and g.get("source") == "cdp":
            url = g.get("url", "")
    return {"window": observation.get("active_title", ""), "url": url}


def _keyboard_field(observation):
    """
    The field keys would go to now, or None when it cannot be told: the one
    the look marked focused, else - in a browser - the one the page itself
    says has the keyboard. What Enter does depends on it (approval.assess).
    """

    field = next((e for e in observation["elements"] if "focused" in e.get("states", [])
                  and e.get("role") in TYPING_ROLES), None)
    if field is not None:
        return field

    if browser.is_browser_title(observation.get("active_title")) and cdp.available():
        try:
            return cdp.focused(observation["elements"])[0]
        except ComputerError:
            return None

    return None


# The settle wait (tuned live; docs/computer-use.md, "Waits"): settled when the screen has not changed by more than
# SETTLE_CHANGE (changed_fraction) for SETTLE_QUIET seconds, looked at every SETTLE_POLL seconds at most.
SETTLE_QUIET = 0.35
SETTLE_POLL = 0.15
SETTLE_CHANGE = 0.002
# How often the drawing reports are read (screen.damage_watch): a local read, no picture.
DAMAGE_POLL = 0.05


def _wait_for_settle(reference):
    """
    Wait until the screen stops changing, for at most SETTLE_SECONDS, and return a frame of it.

    A click that opens a dialog or loads a page is not finished when the
    event is delivered; reading the screen straight away would report
    "nothing changed" about an action that worked.

    The X server says what was drawn (screen.damage_watch), so a screen that is not changing costs no pictures: one is
    taken only when the drawing could amount to a change, and once at the end. Without the reports, or when they stop,
    screenshots are taken and compared as before (_settle_by_frames).
    """

    watch = screen.damage_watch()
    if watch is None:
        return _settle_by_frames()
    try:
        return _settle_by_damage(watch)
    except screen.DamageLost:
        pass
    finally:
        watch.close()
    return _settle_by_frames()


def _settle_by_frames():
    """The settle wait by pictures: a screenshot every SETTLE_POLL seconds, compared with the last that differed."""

    deadline = time.monotonic() + SETTLE_SECONDS
    time.sleep(0.15)
    last = screen.grab()
    stable_since = time.monotonic()

    while time.monotonic() < deadline:
        time.sleep(SETTLE_POLL)
        current = screen.grab()
        if screen.changed_fraction(last, current) > SETTLE_CHANGE:
            last = current
            stable_since = time.monotonic()
        elif time.monotonic() - stable_since >= SETTLE_QUIET:
            break

    return last


def _settle_by_damage(watch):
    """
    The settle wait by drawing reports, deciding what _settle_by_frames decides with fewer pictures.

    `hits` holds every point of the comparison grid (screen.grid_points) that may differ now from `last`, the picture
    the next one is compared with: the points drawn on since `last` was taken, and the points where the latest picture
    already differed from it. changed_fraction counts at most three channels per point, so while `hits` has no more
    than SETTLE_CHANGE of the grid's points the screen provably has not changed by more than SETTLE_CHANGE - nothing is
    taken. A caret's blink or the panel's clock is a point or two. When more was drawn a picture is taken (at most one
    each SETTLE_POLL seconds) and compared exactly as before; a screen that was redrawn the same is still quiet. The
    quiet time counts from the last drawing seen before a change. Raises screen.DamageLost when the reports stop.
    """

    deadline = time.monotonic() + SETTLE_SECONDS
    time.sleep(0.15)
    watch.take()  # what was drawn before the first look counts for nothing, as before
    width, height = reported = tuple(watch.size)
    limit = SETTLE_CHANGE * screen.grid_size(width, height)

    last = newest = None        # the picture compared with, and the latest picture taken
    hits = set()
    drawn_since_newest = False
    quiet_since = drawn_at = time.monotonic()
    grabbed_at = None
    resized = False             # the screen's size changed since the latest picture

    while time.monotonic() < deadline:
        time.sleep(DAMAGE_POLL)
        rects, size = watch.take()
        now = time.monotonic()
        if size is not None and tuple(size) != reported:
            resized, reported = True, tuple(size)
        if rects:
            drawn_since_newest, drawn_at = True, now
            hits |= screen.grid_points(width, height, rects)
        if resized or len(hits) > limit:
            if grabbed_at is not None and now - grabbed_at < SETTLE_POLL:
                continue        # may be changing: not quiet, and the next picture is due shortly
            current = screen.grab()
            grabbed_at = time.monotonic()
            newest, drawn_since_newest, was_resized, resized = current, False, resized, False
            if last is None or was_resized or (current.width, current.height) != (width, height) \
                    or screen.changed_fraction(last, current) > SETTLE_CHANGE:
                last, hits, quiet_since = current, set(), drawn_at
                if (current.width, current.height) != (width, height):
                    width, height = current.width, current.height
                    limit = SETTLE_CHANGE * screen.grid_size(width, height)
            else:
                hits = screen.differing_points(last, current)
                if grabbed_at - quiet_since >= SETTLE_QUIET:
                    break       # still drawing, but the same for long enough (as the pictures alone would decide)
            continue
        if now - quiet_since >= SETTLE_QUIET:
            break

    if newest is not None and not drawn_since_newest:
        return newest
    return screen.grab()


def _window_id_for(element):
    title = element.get("window_title") or ""
    windows = screen.windows()
    for w in windows:
        if w["title"] == title:
            return w["id"]
    # A near title is the same window drawn differently (a "*" for unsaved changes), never another name: other digits
    # or other words are another document ("Invoice 104" is inside "Invoice 1042"); and only one window may be meant
    # (review 2026-11-03, D-27).
    near = [w for w in windows if title and w["title"] and (title in w["title"] or w["title"] in title)
            and not identity.changes_identity(title, w["title"])]
    return near[0]["id"] if len(near) == 1 else None


def _focus_window(element):
    """Put the element's own window in front before keys go to it, or raise: keys sent when it cannot be found, or
    cannot be told apart from another, go to whatever is in front (D-27)."""

    window_id = _window_id_for(element)
    if window_id is not None:
        xinput.activate(window_id)
    elif element.get("window_title"):
        raise ComputerError(f"its window {el._quote(element['window_title'], 60)} cannot be found on its own on the "
                            "screen, so nothing was typed or pressed (it would go to whatever window is in front). "
                            "Bring that window forward (gui front) and observe again.")


# Where the keyboard was left by the last action of this task: the window in front then, and every window there was.
FRONT_FILE = STATE_DIR / "front.json"
# How long that stays the place keys go to.
FRONT_KEEP_SECONDS = 900


def _task_now():
    try:
        from decurix_agent import task_context
        return task_context.current() or ""
    except Exception:
        return ""


def _note_front():
    """Remember the window in front after an action, and which windows there were."""

    try:
        wins = screen.windows()
        front = next((w for w in wins if w.get("active")), None)
        if front is None:
            return
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(FRONT_FILE, {"task": _task_now(), "at": time.time(), "id": front["id"],
                                                       "title": front["title"], "known": [w["id"] for w in wins]})
    except Exception:  # a note for later must never fail the action
        pass


def _keys_home():
    """
    Before keys sent to no element: the window the last action left in front, when an older window has taken the
    keyboard since. Found live (2026-10-15): Impress, left open by an earlier task, took the focus while the worker
    filled TallyPrime's Company Creation screen; "Escape" and "Enter" went to Impress, and the worker reported that
    the focus "kept going to other apps" and gave up. A window that appeared after that action (a dialog the app
    opened) is where keys belong and is left alone. Returns the title keys were sent back to, or None.
    """

    try:
        note = json.loads(FRONT_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if note.get("task") != _task_now() or time.time() - note.get("at", 0) > FRONT_KEEP_SECONDS:
        return None
    try:
        wins = screen.windows()
    except Exception:
        return None
    front = next((w for w in wins if w.get("active")), None)
    home = next((w for w in wins if w["id"] == note.get("id")), None)
    if home is None or front is None or front["id"] == home["id"] or front["id"] not in note.get("known", []):
        return None
    xinput.activate(home["id"])
    return home["title"]


# ============================================================
# Windows a task found open, and keys sent to none of its own
#
# Found live (2026-10-19): a task that ran out of time left two spreadsheet windows open; the next task opened its
# own workbook, and its `gui type "=11,160"` (no target: the keys go where the keyboard is) replaced A1 of the old
# task's sheet. A window that was open before this task's first look is not where this task's keys go until the task
# has worked in it on purpose - acted on something in it, opened it, or brought it to the front. Keys with no target
# aimed at such a window are refused, with what to do instead. Replays and a person's own actions are not asked.
# ============================================================

TASK_WINDOWS_FILE = STATE_DIR / "task-windows.json"


def _task_windows(windows=None):
    """This task's record of windows: {task, inherited: [ids open at its first look], adopted: [ids]}, or None."""

    task = _task_now()
    if not task:
        return None
    try:
        record = json.loads(TASK_WINDOWS_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        record = None
    if not isinstance(record, dict) or record.get("task") != task:
        try:
            wins = screen.windows() if windows is None else windows
        except Exception:
            return None
        record = {"task": task, "inherited": [w["id"] for w in wins], "adopted": []}
        _save_task_windows(record)
    return record


def _save_task_windows(record):
    try:
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(TASK_WINDOWS_FILE, record)
    except OSError:
        pass


def _adopt_front():
    """The window in front now is one this task works in (it acted there, opened it, or brought it forward)."""

    try:
        record = _task_windows()
        front = next((w for w in screen.windows() if w.get("active")), None)
    except Exception:  # a note for later must never fail the action
        return
    if record is None or front is None or front["id"] in record["adopted"]:
        return
    record["adopted"].append(front["id"])
    _save_task_windows(record)


# How long keys wait for a window to come to the front (an app just opened) before they are refused.
FRONT_WAIT_SECONDS = 5.0


def _nothing_in_front(wait=0.0):
    """
    Whether no window is active, after waiting up to `wait` seconds for one: keys sent now go to the desktop itself.
    False when it cannot be told.
    """

    deadline = time.monotonic() + wait
    while True:
        try:
            wins = screen.windows()
        except Exception:
            return False
        if any(w.get("active") for w in wins):
            return False
        if time.monotonic() >= deadline:
            return True
        time.sleep(0.5)


def _left_from_before():
    """The title of the window in front when it was open before this task and the task has not worked in it, or ""."""

    record = _task_windows()
    if record is None:
        return ""
    try:
        front = next((w for w in screen.windows() if w.get("active")), None)
    except Exception:
        return ""
    if front is None or front["id"] not in record["inherited"] or front["id"] in record["adopted"]:
        return ""
    return front.get("title") or "an untitled window"

# The prompt a presentation shows in an empty placeholder (LibreOffice Impress, PowerPoint).
PLACEHOLDER_PROMPT = re.compile(r"^\s*click to add (?:a |an )?(?:title|subtitle|text|notes|outline|table|chart|picture|image)\b",
                                re.IGNORECASE)

# The name a description of a control opens with: its capitalised words ("Recommended Charts in the Insert ribbon",
# "the OK button"), up to the first word that is not.
DESCRIBED_NAME = re.compile(r"^\s*(?:(?:the|a|an)\s+)?[\"']?((?:[A-Z0-9][\w&'.+-]*)(?:\s+[A-Z0-9][\w&'.+-]*){0,4})")


# What a host app's viewer says when its controls were read from the picture by a vision model (hostgui.html).
VISION_READ = "Controls read from the picture by the vision model"


def _read_by_vision(observation):
    """Whether a look's controls are a vision model's estimate of a picture, not the app's own."""

    return any(e.get("name") == VISION_READ for e in (observation or {}).get("elements", []))


def _keys_went_to(before, after):
    """
    When typing with no target went into a field the page lists, which one, and
    what it holds now: ", which is textbox "Name Box" (e12), now "A20Total"".
    Found live (2026-10-16): in Excel the engine typed a cell's address into the
    Name Box, left out the Enter, and typed "Total" with no ref; the result said
    only "into the focused element", and it went on as if the cell had it.
    """

    field = next((e for e in before.get("elements", []) if "focused" in (e.get("states") or [])
                  and e.get("role") in ("textbox", "combobox", "searchbox", "spinbutton")
                  and (e.get("name") or "").strip()), None)
    # Not for an app read from its picture: there the field "with the cursor" is a vision model's estimate, and it named
    # the wrong field as often as the right one (found live, 2026-10-16: "Search" for TallyPrime's ledger Name).
    if field is None:
        return ""
    if privacy.secret_field(field):
        return ""
    now = _same_field(field, after) or field
    value = (now.get("value") or "").strip()
    held = f", now {el._quote(value, 60)}" if value else ""
    return f", which is {el.describe(field)}{held} - the keys went to that field, not to anything behind it"


def _names_another(description, name):
    """
    The control name a description opens with, when the control found has a name
    that does not hold every word of it ("Recommended Charts" found as
    "Recommended PivotTables"), else "". A description that names nothing (it
    opens in lower case) and a control with no name decide nothing.
    """

    match = DESCRIBED_NAME.match(description or "")
    if not match or not (name or "").strip():
        return ""
    wanted = match.group(1).strip(" .'\"")

    def words(text):
        return {w.lower().rstrip("s") for w in re.findall(r"[A-Za-z0-9]+", text)}

    asked = words(wanted)
    return wanted if asked and not asked <= words(name) else ""


def _inside_large(element, point):
    """
    Whether a click aimed at `point` means that point inside the element rather than the element itself: a point
    away from the middle of a control (a spreadsheet's grid, a canvas, a drawn area). Found live (2026-10-15): real
    Excel's whole grid is one control in its viewer, every click on a cell went to the grid's middle, and there it
    was covered - the worker could not select a single cell.
    """

    if point is None or not element or not element.get("box"):
        return False
    x, y, w, h = element["box"]
    # A control, not an area: a press anywhere on it is a press of it. An area is big both ways - a dialog's narrow,
    # tall folder pane is one too (found live: PowerPoint's Open dialog exposes its whole navigation pane as one
    # "Control Host", and every click on "Documents" in it was refused as a press of that).
    if w * h < 20000 or min(w, h) < 60:
        return False
    return abs(point[0] - (x + w / 2)) > 12 or abs(point[1] - (y + h / 2)) > 12


# A press that saves in an app shown in a viewer: Ctrl+S, or a dialog's Save, Replace or Yes (a replace prompt).
SAVE_CONTROL = re.compile(r"^\s*(?:save|replace|yes)\s*$", re.I)
SAVE_SETTLE_SECONDS = 4


def _saves(kind, element, keys):
    """Whether an action saves in an app: Ctrl+S, or a press on a control named Save, Replace or Yes."""

    if kind == "key":
        return any(xinput.normalize_combo(c).lower() == "ctrl+s" for c in keys or ())
    if kind in ("click", "double_click") and element is not None:
        return bool(SAVE_CONTROL.match(element.get("name") or ""))
    return False


def _shared_save_note(since, wait=SAVE_SETTLE_SECONDS):
    """
    After a save in an app of the person's computer, on a worker with a shared folder: which file of that folder it
    wrote, or plainly that none changed. Found live (2026-10-20, P1): a deck saved in PowerPoint was read back from a
    stale copy elsewhere, and the worker reported the save had not taken - the file had changed where asked.
    """

    from decurix_agent import shared

    if not shared.active():
        return ""
    deadline = time.time() + wait
    while True:
        changed = []
        for path in shared.walk():
            if path.name.startswith("~$"):
                continue  # an app's lock file, not what it saved
            try:
                if os.stat(path).st_mtime >= since - 1:
                    changed.append(path)
            except OSError:
                continue
        if changed or time.time() >= deadline:
            break
        time.sleep(0.5)
    if changed:
        named = "; ".join(f"{shared.to_host(str(p)) or p} (on this desktop {p})" for p in changed[:4])
        return f"\nSaved in the person's shared folder: {named}. Read it back from there."
    held = sorted({p.name[2:] for p in _shared_lock_files()})
    if held:
        return ("\nNo file in the person's shared folder changed with this save, and " + ", ".join(held[:4])
                + " is open in an app (its lock file ~$... is there): a save over a file that is open elsewhere does not "
                "take. Save in the app that has it open, or close it there first; then read the file back.")
    return ("\nNo file in the person's shared folder changed with this save. Before going on, look: a dialog may still "
            "be open (a replace or file-format question), the app may have saved somewhere else (another folder, a "
            "cloud location), or the file may be open in another program.")


DOWNLOAD_WAIT_SECONDS = 6
# Nothing new in the download folder this long after an action in the browser: it downloaded nothing (_downloads_note).
DOWNLOAD_QUIET_SECONDS = 1.0
PARTIAL_DOWNLOAD = (".crdownload", ".part", ".download")
# With the browser heard (cdp.DownloadWatch): no download begun, nothing loading or fetched, this long after the action
# and after the page's last fetch, means the action downloaded nothing. The settle wait and the look after the action
# take longer than this, so it is over by the time the note is made.
DOWNLOAD_SIGNAL_QUIET = 0.3
DOWNLOAD_SIGNAL_POLL = 0.05
# A download the browser says ended is in the folder within this (the browser renames its .crdownload as it ends).
DOWNLOAD_ARRIVE_SECONDS = 1.0


def _downloads_dir():
    return Path(os.path.expanduser("~")) / "Downloads"


def _downloads_snapshot():
    """{name: (size, mtime)} of the files in the browser's download folder, or {} when there is none."""

    try:
        return {e.name: (e.stat().st_size, e.stat().st_mtime) for e in os.scandir(_downloads_dir()) if e.is_file()}
    except OSError:
        return {}


def _same_bytes(a, b):
    import hashlib

    try:
        if os.path.getsize(a) != os.path.getsize(b):
            return False
        return hashlib.sha256(Path(a).read_bytes()).digest() == hashlib.sha256(Path(b).read_bytes()).digest()
    except OSError:
        return False


def _may_download(kind, element, point, before):
    """
    Whether this action can start a download: only the browser downloads, so only an action whose target is in the
    browser can. Decided by structure: the element's own source and application, else the window the click lands in
    or the keys go to. Unknown is in the browser (the folder is watched as before).
    """

    def in_browser(title):
        return browser.is_browser_title(title or "")

    if element is not None:
        if element.get("source") == "cdp":
            return True
        app = str(element.get("app") or "").lower()
        return app in browser.APP_NAMES or in_browser(element.get("window_title"))
    windows = (before or {}).get("windows") or []
    if kind in ("click", "double_click") and point is not None:
        try:
            under = [w for w in windows
                     if w["box"][0] <= point[0] < w["box"][0] + w["box"][2]
                     and w["box"][1] <= point[1] < w["box"][1] + w["box"][3]]
        except (KeyError, IndexError, TypeError):
            return True
        if under:
            return in_browser(under[-1].get("title"))   # topmost last (screen.windows)
        return bool((before or {}).get("page_url")) or any(in_browser(w.get("title")) for w in windows)
    # Keys with no target go to the window in front.
    active = (before or {}).get("active_title")
    return in_browser(active) if active else True


def _downloads_note(before, wait=DOWNLOAD_WAIT_SECONDS, since=None, watch=None, acted=None, remember=False):
    """
    _downloads_note says what an action downloaded (_downloads_waited). With `remember` (an action's own note), a file
    that arrived in the folder since the last report and is not this action's - a download a page began after the last
    action's note had ended - is said too (_unreported_downloads), and the folder as it is now is the new record.
    """

    note, now, reported = _downloads_waited(before, wait=wait, since=since, watch=watch, acted=acted)
    if remember:
        late = _unreported_downloads(now, reported)
        if late:
            note += "\n" + LATE_DOWNLOADS + _arrived_text(late, now) + "."
    return note


def _downloads_waited(before, wait=DOWNLOAD_WAIT_SECONDS, since=None, watch=None, acted=None):
    """
    What an action downloaded: the file that arrived in the download folder, its size, and - when the browser named it
    "x (1).pdf" because "x.pdf" was already there - whether it is the same file again. Found live
    (2026-10-21): a click on a chat attachment's Download link said only that the link was focused; the engine went
    looking in the browser's download history and a file manager, had clicked the first attachment twice without
    knowing, and saved the second copy of invoice 197 under invoice 198's name.

    `watch` (cdp.DownloadWatch, started before the action; closed here): what the browser said meanwhile. It only
    decides when to stop looking - the folder says what arrived. A download it heard begin is waited for until the
    browser says it ended and the file is in the folder (at most `wait`). With none heard, nothing loading or fetched
    since the action, the page alone (no new window, no frame of another site) and DOWNLOAD_SIGNAL_QUIET gone since
    the action (`acted`) and the page's last fetch, nothing was downloaded - without the quiet second. Whenever it
    cannot say (no watch, it stopped hearing, a window was opened, a fetch still on its way) the rules are the
    folder's alone, as they were.
    """

    # Nothing new within DOWNLOAD_QUIET_SECONDS of `since` (the action, when the caller gives it: the window then ran
    # while the screen settled and was looked at, instead of after it) means nothing was downloaded; with no `since`
    # the second starts now. A file still being written - the browser has begun the download - is waited for up to
    # `wait` from now, however slow the server.
    start = time.time()
    quiet_from = start if since is None else min(since, start)
    acted = quiet_from if acted is None else min(acted, start)
    heard, seen, ended_at = watch, None, None
    try:
        while True:
            now = _downloads_snapshot()
            new = [n for n, v in now.items() if before.get(n) != v]
            partial = [n for n in new if n.endswith(PARTIAL_DOWNLOAD)]
            clock = time.time()
            seen = heard.status() if heard is not None else None
            if seen is not None and not seen["began"] and (not seen["alive"] or seen["opened"]):
                # It may have missed what it did not hear (a download in a window just opened is that window's).
                heard = seen = None
            if seen is not None and seen["began"]:
                # The browser began one: waited for until it says it ended and the file is in the folder.
                if all(g in seen["ended"] for g in seen["began"]):
                    if new and not partial:
                        break
                    # Ended and not (yet) in the folder: renamed a moment later, or canceled.
                    ended_at = ended_at or clock
                    if clock - ended_at >= DOWNLOAD_ARRIVE_SECONDS:
                        break
                elif not seen["alive"] and new and not partial:
                    break  # no longer heard, and the folder has it
                if clock - start >= wait:
                    break
            elif seen is not None and not new and not seen["busy"] \
                    and clock - max(acted, seen["quiet_since"]) >= DOWNLOAD_SIGNAL_QUIET:
                if heard.alone():
                    break
                heard = seen = None  # another tab, or a frame of another site: the folder alone
                continue
            elif (new and not partial) or (not new and clock - quiet_from >= DOWNLOAD_QUIET_SECONDS) \
                    or clock - start >= wait:
                break
            if seen is not None:
                time.sleep(DOWNLOAD_SIGNAL_POLL)
            else:
                # Looked at again every 0.25 s, and once more just as the quiet second ends rather than up to 0.25 s
                # past it.
                time.sleep(0.5 if new else min(0.25, max(0.01, quiet_from + DOWNLOAD_QUIET_SECONDS - clock)))
    finally:
        if watch is not None:
            watch.close()
    done = sorted(n for n in new if not n.endswith(PARTIAL_DOWNLOAD))
    if not done:
        # A download the browser holds for a yes ("Unconfirmed 123.crdownload": a file from a site without https) never
        # arrives by waiting. Found live (2026-10-24): the worker saw the held download, guessed at its address, fetched
        # it from outside the browser and saved a web page under the bill's name; the next run spent 25 minutes on the
        # download bubble, whose Keep took about one press in two. The desktop's policy now lets a page that is itself
        # plain http download (agent/chrome-policies.json); this is for a browser it does not reach. The way that
        # worked every time, measured: the card's menu on chrome://downloads.
        held = [n for n in partial if n.lower().startswith("unconfirmed")]
        if held:
            return ("\nThe browser is holding this download until it is confirmed (" + ", ".join(held[:3]) + "): open "
                    "chrome://downloads with gui open, open the menu (three dots) on its card and choose \"Download "
                    "insecure file\"; it then arrives in " + str(_downloads_dir()) + ".", now, [])
        if partial:
            return "\nStill downloading: " + ", ".join(partial[:3]) + ".", now, []
        if seen is not None and seen["began"]:
            # The browser said it began one, and nothing arrived in the folder: said, not taken for nothing.
            names = ", ".join(sorted(n or "a file" for n in seen["began"].values())[:3])
            canceled = len(seen["ended"]) == len(seen["began"]) and all(v == "canceled" for v in seen["ended"].values())
            return (f"\nThe browser began downloading {names}, and it is not in {_downloads_dir()}"
                    + (": the browser canceled it." if canceled else " (yet)."), now, [])
        return "", now, []
    return "\nDownloaded: " + _arrived_text(done, now) + ".", now, done


# What a file that arrived after the last report is said with, by the next action or command (_unreported_downloads).
LATE_DOWNLOADS = "Downloaded since the last action: "
# The record of the download folder as last reported (names hashed: nothing of a name is kept).
DOWNLOADS_SEEN_FILE = STATE_DIR / "downloads-seen.json"


def _arrived_text(done, now):
    """
    _arrived_text says each file that arrived in the download folder: its path and size, and - when the browser named it
    "x (1).pdf" because "x.pdf" was already there - whether it is the same file again; a file that is not what its name
    says is said at once, not first at the final check.
    """

    lines = []
    folder = _downloads_dir()
    for name in done[:4]:
        path = folder / name
        line = f"{path} ({now[name][0]:,} bytes)"
        m = re.match(r"^(.*) \((\d+)\)(\.[^.]*)?$", name)
        if m:
            first = folder / (m.group(1) + (m.group(3) or ""))
            if first.exists():
                same = _same_bytes(path, first)
                line += (f" - the browser added \"({m.group(2)})\" because {first.name} was already there"
                         + ("; it is byte for byte the same file, so this downloaded that one again" if same else
                            "; its content differs from that file"))
        lines.append(line)
    text = "; ".join(lines) + (f" (and {len(done) - 4} more)" if len(done) > 4 else "")
    try:
        from decurix_agent import produced
        faults = produced.named_kind_faults([folder / n for n in done[:4]])
    except Exception:  # noqa: BLE001 - a note, never a reason for the action to fail
        faults = []
    if faults:
        text += ". But " + "; ".join(faults)
    return text


def _seen_key(name):
    import hashlib
    return hashlib.sha256(name.encode("utf-8", "surrogateescape")).hexdigest()[:24]


def _unreported_downloads(now, reported=()):
    """
    _unreported_downloads gives the files in the download folder snapshot `now` that arrived (or changed) since the
    folder was last recorded and are not in `reported` (said by this action's own note), and records `now` - complete
    files only, so a download still being written (.crdownload) is said once it has arrived. Nothing is said the first
    time (the folder as it is becomes the record). Every gui command begins with it (downloads_since_last) and every
    action's note ends with it, so a download a page began after an action's note had ended is said by the next one,
    once: none is lost, and none is said twice.
    """

    done = {n: [v[0], v[1]] for n, v in now.items() if not n.endswith(PARTIAL_DOWNLOAD)}
    try:
        with _state_lock():  # the read and the write of the record, one command at a time
            try:
                recorded = json.loads(DOWNLOADS_SEEN_FILE.read_text(encoding="utf-8")).get("files")
                recorded = {k: [size, mtime] for k, size, mtime in recorded}
            except (OSError, ValueError, TypeError, AttributeError):
                recorded = None
            privacy.write_scrubbed(DOWNLOADS_SEEN_FILE, {"files": [[_seen_key(n), v[0], v[1]] for n, v in done.items()]},
                                   atomic=True, exempt=("files",))
    except OSError:
        return []  # no record could be kept: nothing is said that could be said again
    if recorded is None:
        return []
    return sorted(n for n, v in done.items() if n not in reported and recorded.get(_seen_key(n)) != v)


def downloads_since_last():
    """downloads_since_last says the files that arrived in the download folder since the last action or command said
    what it had (_unreported_downloads): a line for the start of a gui command's output, or ""."""

    now = _downloads_snapshot()
    late = _unreported_downloads(now)
    return LATE_DOWNLOADS + _arrived_text(late, now) + "." if late else ""


def _shared_lock_files():
    """Office's owner files (~$name) in the shared folder: the files an app of the person's computer has open."""

    from decurix_agent import shared

    out = []
    if not shared.active():
        return out
    for folder, _dirs, names in os.walk(shared.link_path(), followlinks=True):
        out += [Path(folder) / n for n in names if n.startswith("~$")]
        if len(out) > 20:
            break
    return out


def _refuse_number_arrows(kind, point):
    """A click at a point on a number field's up/down arrows is refused: each one changes the field's value."""

    if kind not in ("click", "double_click") or point is None or not cdp.available():
        return
    number = cdp.number_field_at(*point)
    if number and number.get("arrows"):
        raise ComputerError(f"nothing was pressed: {point[0]},{point[1]} is on the up/down arrows of the number "
                            f"field {el._quote(number.get('label') or '', 60)} (now {number.get('value')!r}), and "
                            "each click there changes its value. Set it with gui type REF \"the number\". A list "
                            "of earlier entries that opens under a field is the browser's, not the form's: ignore it "
                            "(press Tab).")


# A document an app has open read-only, or a new blank one ("Document1", "Book1", "Presentation1"), as its title or a
# status element shows it.
_READ_ONLY_DOC = re.compile(r"\bRead-Only\b|\[Read-Only\]", re.I)
_BLANK_DOC = re.compile(r"^\s*(?:Document|Book|Presentation)\d+(?:\s*-\s*(?:Word|Excel|PowerPoint|Compatibility Mode))?\s*$")


def _refuse_unsafe_save(kind, element, keys, before):
    """
    A save in an app of the person's computer is refused when what is open there is read-only or a new blank document:
    Ctrl+S then saves nothing or a copy - or an empty document over the file with Save As. Found live (2026-10-20): a word
    processor held a memo from an earlier attempt, the tools rewrote it, and the app's stale and later blank copy was
    saved over it - the memo ended with 0 headings.
    """

    if not _relayed_page(before) or not _saves(kind, element, keys):
        return
    names = [str(e.get("name") or "") for e in (before or {}).get("elements", [])]
    title = str((before or {}).get("active_title") or "")
    if any(_READ_ONLY_DOC.search(n) for n in names + [title]):
        raise ComputerError("nothing was saved: the document open in the app is read-only (its title says Read-Only) - "
                            "a save there does not reach the file. Close it without saving, then open the file again from "
                            "its place (another app of this desktop may still hold it), and save only when it is not "
                            "read-only.")
    if any(_BLANK_DOC.match(n) for n in names + [title]):
        raise ComputerError("nothing was saved: the app has a new blank document open (Document1/Book1/Presentation1), "
                            "not the file - Ctrl+S would save an empty document. Open the file itself from its place "
                            "(File > Open, its full path), check it is the file, then save.")


def _beside_number_fields(target):
    """A point to scroll the page from that is not on a number field: the target's row at the page's left, then the
    middle of the screen."""

    width, height = screen.size()
    for candidate in ((max(10, target[0] - 300), target[1]), (width // 2, height // 2), (20, height // 2)):
        if not cdp.number_field_at(*candidate):
            return candidate
    return (20, height // 2)


# Where a form waiting to be confirmed is noted between gui commands (each runs as its own process): one note per task
# (_form_check_file), so a listing in one task is never the confirmation of a click in the next.
FORM_CHECK_FILE = Path(os.environ.get("TMPDIR", "/tmp")) / "decurix-form-check.json"
FORM_CHECK_SECONDS = 600


def _form_check_file():
    """This task's form-check note: FORM_CHECK_FILE with the task's id in its name (review 2026-11-02: one global file
    let a form listed in one task be sent by a --checked click in another). No task: FORM_CHECK_FILE itself."""

    from decurix_agent import task_context

    task = task_context.current()
    return FORM_CHECK_FILE.with_name(f"{FORM_CHECK_FILE.stem}.{task}{FORM_CHECK_FILE.suffix}") if task else FORM_CHECK_FILE


class AskFirst(str):
    """What a committing press holds that a person checks before it goes: the press is put to them through the approval
    gate rather than refused. Returned for a button whose form could not be read (_commit_check, D-26)."""


# The values a replay running in this process types from the person's demonstration because today's request gives
# none for them (replay.run sets it for the run): the person's own, never "in no source".
TAUGHT_VALUES = set()

# The steps the plan of the work, as it was put to the person, makes "with your OK first" (TheBot's operator sets it
# on every gui command of a goal whose plan said so: plan.go, guiEnv): a JSON list with one entry per such step, the
# words of the control it is made with, or "" when the plan did not know them.
PLAN_ASK_FIRST_ENV = "DECURIX_PLAN_ASK_FIRST"


def _plan_ask_first():
    """The controls the agreed plan makes with the person's OK first (PLAN_ASK_FIRST_ENV); [] when none. A value that
    cannot be read stands for a step whose control is not known: every committing press is held (unsure is asked)."""

    # (Read by its literal name: the screen broker passes a caller's variable only when the agent's code names it.)
    raw = os.environ.get("DECURIX_PLAN_ASK_FIRST", "").strip()
    if not raw:
        return []
    try:
        value = json.loads(raw)
    except ValueError:
        return [""]
    if not isinstance(value, list):
        return [""]
    return [" ".join(v.split()) if isinstance(v, str) else "" for v in value]


def _plan_hold(control):
    """
    Why a committing press (control: what _commit_check found it commits with, or None) is put to the person: the
    plan of the work they were shown says its step is made with their OK first, and the gate does not ask for it on its
    own (a local form's Save). Found live (2026-11-08): a plan read "Then, each with your OK first: Press Save bill",
    and the worker pressed Save bill after its own pre-save check, never asking - the person was told one thing and
    the worker did another. Held when a step's control names this one or this one names it as a whole phrase
    (approval.names_control), or a step's control is not known, or this control has no name. None otherwise.
    """

    if control is None:
        return None
    steps = _plan_ask_first()
    if not steps:
        return None
    label = " ".join(str(control.get("name") or "").split())[:60]
    if not label or any(not step or approval.names_control(step, control) or approval.names_control(label, {"name": step})
                        for step in steps):
        return (f'the plan of the work you agreed to makes "{label}" with your OK first' if label
                else "the plan of the work you agreed to makes this press with your OK first")
    return None


def _taught_value(value):
    """Whether a typed value is one the replay running now took from the person's demonstration (TAUGHT_VALUES)."""

    value = " ".join(str(value or "").split()).lower()
    return bool(value) and value in TAUGHT_VALUES


# A value that only holds a place: "TEST", "dummy", "xxx", "asdf" anywhere; "123", "abc" in a reference-like field.
PLACEHOLDER_ANYWHERE = re.compile(r"^\W*(?:test(?:ing)?|dummy|sample|placeholder|x{3,}|asdf\w*|qwerty|lorem ipsum.*|"
                                  r"tbd|to be (?:confirmed|decided))\W*$", re.I)
PLACEHOLDER_IN_REFERENCE = re.compile(r"^\W*(?:1234?5?6?|123|abc|xyz|0000?|1111?)\W*$", re.I)
REFERENCE_FIELD = re.compile(r"\b(?:po|ref|reference|number|no\.?|#|code|id|order|invoice|bill|receipt|voucher|customer|"
                             r"supplier|vendor|name|description|note|remarks?)\b", re.I)


def placeholder_fields(fields):
    """The filled fields of a form whose value only holds a place ("Customer PO: TEST"), as "label: value"."""

    out = []
    for label, value in fields or ():
        value = str(value or "").strip()
        if PLACEHOLDER_ANYWHERE.match(value) or (REFERENCE_FIELD.search(str(label or "")) and PLACEHOLDER_IN_REFERENCE.match(value)):
            out.append(f"{label}: {value}")
    return out


# A click that saves a new record, and what the application answers once it has.
# (The plain verbs of keeping a record, whatever the application: "Accept" keeps one too.)
_SAVE_CONTROL = re.compile(r"(?i)\b(?:submit|save|create|add|post|confirm|place|register|book|finish|accept)\b")
_SAVED_ANSWER = re.compile(r"(?i)/done/|/saved\b|/success|\bsaved\b|\bcreated\b|\bsubmitted\b|\badded\b")
# What an application says when it did not save: an answer with one of these is no proof of a save, whatever page it
# is on. Found live (2026-10-25): a form posted to ".../voucher/save" answered "ledger '<name>' does not exist"; the "/save"
# in the address was taken for a save, and the bill was then refused as "already saved" when it never was.
_SAVE_REFUSED = re.compile(r"(?i)\b(?:error|invalid|does not exist|doesn't exist|not found|is required|are required|"
                           r"failed|could not|cannot|must be|not saved|already exists|try again)\b")
# A field that holds a record's own number: two records whose numbers differ are two records, whatever else they share.
_REF_LABEL = re.compile(r"(?i)(?:^|\b)(?:no\.?|number|num|#|ref(?:erence)?|invoice|bill|voucher|document|doc)(?:\b|$)")


def _saved(change):
    """Whether an action's answer shows the record saved: a saved/created answer, and no error beside it."""

    return bool(_SAVED_ANSWER.search(change)) and not _SAVE_REFUSED.search(change)


# What an application says once it has removed a record.
_DELETED_ANSWER = re.compile(r"(?i)\b(?:deleted|removed|moved to (?:the )?(?:trash|bin))\b")


# The last part of an address that names one record (it holds a digit): ".../purchase-invoice/PINV-26-00011".
_RECORD_PAGE = re.compile(r"(?i)page is now (\S*/[^/\s;?#]*\d[^/\s;?#]*)/?(?=[\s;?#]|$)")


def _became_a_record(before, change):
    """Whether a press took the browser from a new record's page (".../new...", ".../new-purchase-invoice-x1y2") to a
    page that names one record, with no refusal beside it: how a web application answers a save that says no "saved".
    Found live (2026-11-08, ERPNext): every bill's save answered only "page is now .../PINV-26-00011", so no saved bill
    was ever read back against its declared record ("declared but never saved")."""

    url = str((before or {}).get("page_url") or "")
    if not url or _SAVE_REFUSED.search(change or ""):
        return False
    came_from_new = bool(_NEW_RECORD_PAGE.search("page is now " + url)) or bool(
        re.search(r"(?i)/new[-_][^/]*$", url.split("?")[0].rstrip("/")))
    landed = _RECORD_PAGE.search(change or "")
    return came_from_new and landed is not None and not _NEW_RECORD_PAGE.search(landed.group(0))         and not re.search(r"(?i)/new[-_]", landed.group(1))


def _released(save, answer, page):
    """Whether a delete's answer (or the page it was pressed on) is about this earlier save: the record's number or
    one of its own references named in it, or the delete pressed on the page the save led to."""

    _, number, refs, saved_page = save
    named = [r for r in ({number} | refs) if r and len(r) >= 3]
    if any(re.search(r"(?<![A-Z0-9])" + re.escape(r.upper()) + r"(?![A-Z0-9])", answer) for r in named):
        return True
    # (an answer that names another record is about that one, wherever it was pressed)
    return bool(saved_page) and page == saved_page and not _RECORD_NO.search(answer)


def _record_refs(pairs):
    """The values typed into a record's own number fields (No., Bill, Invoice, Voucher, Ref) that hold a digit."""

    return {str(v).strip().upper() for label, v in pairs if _REF_LABEL.search(str(label or "")) and re.search(r"\d", str(v))}
# A new record's page, told by its address (review 2026-11-03, D-29: a list of business nouns named it too).
_NEW_RECORD_PAGE = re.compile(r"(?i)page is now \S*/(?:new|create|add)\b")
_RECORD_NO = re.compile(r"\b[A-Z]{2,5}-\d{3,}\b")
# A new record's form opened at its start (".../new", not ".../new?step=2" of the same form).
_FORM_START = re.compile(r"(?i)page is now \S*/(?:new|create|add)(?:[?#](?![^\s;]*step=(?:[2-9]|\d\d))[^\s;]*)?(?=[\s;]|$)")


def _identity(values):
    """What identifies a record among the values put into its form: an email, else its first two words typed (a name)."""

    mails = {v.strip().lower() for v in values if re.fullmatch(r"\S+@\S+\.\S+", v.strip())}
    if mails:
        return ("mail", frozenset(mails))
    words = [v.strip().lower() for v in values if re.fullmatch(r"[A-Za-z][A-Za-z .'-]{1,40}", v.strip())]
    return ("name", tuple(words[:2])) if len(words) >= 2 else None


def _replayed_saves(events):
    """The scan already_entered makes, for possibly_entered: (saved, current, pairs, new_page)."""

    out = {}

    def keep(saved, current, pairs, new_page):
        out.update(saved=saved, current=current, pairs=pairs, new_page=new_page)
        return None

    already_entered(events, _scan=keep)
    return out.get("saved", []), out.get("current", []), out.get("pairs", []), out.get("new_page", False)


def already_entered(events, _scan=None):
    """
    (what it is, the system's number) when the new record being saved repeats one this task already saved: the values
    typed since the last save, against those of each earlier save (an email, or its first two words). None otherwise -
    and never for a form that changes an existing record. Found live (2026-10-21): a record saved with a wrong
    value was entered again, and the application held it twice.

    A save the application has since deleted no longer counts: found live (2026-10-31): a record saved with a misread
    value was deleted to be entered again, and every re-entry was refused as "already saved" though the application
    no longer held it.
    """

    saved, current, pairs, new_page, page = [], [], [], False, ""
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if event.get("kind") in ("click", "key") and saved:
            answer = urllib.parse.unquote_plus(change).upper()
            if _DELETED_ANSWER.search(answer) and not _SAVE_REFUSED.search(answer):
                saved = [s for s in saved if not _released(s, answer, page)]
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            current, pairs = [], []  # (a new record's form opened afresh: what was typed into an abandoned one is not this one's)
        label = str((event.get("element") or {}).get("name") or "")
        if event.get("kind") == "type" and event.get("text"):
            current.append(str(event["text"]))
            pairs.append((label, str(event["text"])))
        if event.get("kind") == "select" and event.get("option"):
            current.append(str(event["option"]))
            pairs.append((label, str(event["option"])))
        moved = re.search(r"page is now (\S+)", change)
        if moved:
            # Whether the form is a new record's is the page it is on now, not one visited earlier
            # (2026-10-21: after an "add invoice" page, the re-save of an existing return elsewhere was refused as a
            # second record).
            new_page = bool(_NEW_RECORD_PAGE.search("page is now " + moved.group(1).rstrip(";,")))
        elif _NEW_RECORD_PAGE.search(change) or _NEW_RECORD_PAGE.search(str(event.get("target") or "")):
            new_page = True
        if event.get("kind") in ("click", "key") and _saved(change) and current:
            number = _RECORD_NO.search(change)
            # (the page a save led to is the record's own only when it ends in its id: a list page holds others too)
            landed = moved.group(1).rstrip(";,").split("?")[0] if moved else ""
            saved.append((_identity(current), number.group(0) if number else "", _record_refs(pairs),
                          landed if re.search(r"/[^/]*\d[^/]*/?$", landed) else ""))
            current, pairs, new_page = [], [], False
        if moved:
            page = moved.group(1).rstrip(";,").split("?")[0]
    if _scan is not None:
        return _scan(saved, current, pairs, new_page)
    found = _same_saved(current, pairs, new_page, saved)
    return (found[0], found[1]) if found and found[2] else None


def _same_saved(current, pairs, new_page, saved):
    """
    (what it is, the system's number, decided) for the earlier save of this task the record being filled repeats, or
    None. decided: it is the same record by a mechanical identity - the same email, or the same reference typed into
    its own number field. A record that only shares the first words typed (a name) is not decided (False): two bills
    to one customer for one item share every word but are two records. Found in review (2026-11-03, D-12): that was
    refused as a duplicate, and the worker was told never to enter it.
    """

    mine = _identity(current)
    if not mine or not new_page:
        return None
    my_refs = _record_refs(pairs)
    for theirs, number, their_refs, _ in saved:
        # Two bills to one customer share a name, never their own numbers (found live, 2026-10-25: a month's second bill
        # for a customer was refused as the first one again).
        if my_refs and their_refs and not (my_refs & their_refs):
            continue
        if not theirs or theirs[0] != mine[0]:
            continue
        if mine[0] == "mail" and theirs[1] & mine[1]:
            return ", ".join(sorted(mine[1])), number, True
        if mine[0] == "name" and theirs[1] == mine[1]:
            return " ".join(w.title() for w in mine[1]), number, bool(my_refs & their_refs)
    return None


def possibly_entered(events):
    """(what it is, the system's number) when the record being filled shares only its name with one this task saved:
    evidence for the final check and the person, never a refusal (already_entered decides only on an identity)."""

    saved, current, pairs, new_page = _replayed_saves(events)
    found = _same_saved(current, pairs, new_page, saved)
    return (found[0], found[1]) if found and not found[2] else None


def _current_form(events):
    """
    [(field label, value)] put into the record's form being filled: since it opened afresh or the last save, each
    field once, with its latest value - a field typed again holds what was typed last (appended to, when the typing
    appended), and a select's entry is the one taken. Found in an eval probe (W0.3 slip, 2026-11-05): a slipped account
    corrected in place was still held against every later save, because each value ever typed into the form stayed in
    it - a worker doing what the refusal said was stuck.
    """

    current, where = [], {}
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            current, where = [], {}
        element = event.get("element") or {}
        label = str(element.get("name") or "")
        value = None
        if event.get("kind") == "type" and event.get("text"):
            value = str(event["text"])
        if event.get("kind") == "select" and (event.get("taken") or event.get("option")):
            value = str(event.get("taken") or event["option"])
        if value is not None:
            # (A field is the same field by its path on the page, else by its label.)
            field = str(element.get("path") or "") or "label:" + label
            if field in where:
                n = where[field]
                if event.get("kind") == "type" and event.get("append"):
                    value = current[n][1] + value
                current[n] = (label, value)
            else:
                where[field] = len(current)
                current.append((label, value))
        if event.get("kind") in ("click", "key") and _saved(change) and current:
            current, where = [], {}
    return current


_LABEL_NOISE = {"of", "the", "a", "no", "number", "dd", "mm", "yyyy", "rs", "usd", "inr", "per", "status"}


def _words_of(label):
    return {w for w in re.findall(r"[a-z]+", re.sub(r"\(.*?\)", " ", str(label or "").lower())) if w not in _LABEL_NOISE}


def _same_value(a, b, a_order=None, b_order=None):
    """Two values of one field, decidedly the same whatever their form (values.same_value is True): case, spaces,
    punctuation, a date's format under each side's date order, a phone's leading country code. Every digit of a number
    counts (W0.3: two account numbers that share their last 10 digits are two accounts), and a date whose day and
    month could be either way is never decided by its layout (W0.2, review round 2)."""

    from decurix_agent import values
    return values.same_value(a, b, a_order=a_order, b_order=b_order) is True


def crossed_values(form, tables):
    """
    The values put into a record's form that are ANOTHER record's in the source table (one row per record) and not
    this one's: [(field, typed, whose, own)]. The record is the row its typed email, else its first two words, names.
    Nothing when the record or the field's column cannot be told. Found live (2026-10-21): one person's record
    was saved with another person's department and job title.
    """

    values = [v for _, v in form]
    mails = [v.strip().lower() for v in values if re.fullmatch(r"\S+@\S+\.\S+", v.strip())]
    words = [v.strip().lower() for v in values if re.fullmatch(r"[A-Za-z][A-Za-z .'-]{1,40}", v.strip())][:2]
    from decurix_agent import task_context
    # What was typed is the application's; the table is the request's source (two date orders, review round 2).
    app, source = task_context.app_date_order(), task_context.source_date_order()
    out = []
    for header, rows in tables:
        cells = lambda row: [str(c).strip().lower() for c in row if c not in (None, "")]  # noqa: E731
        if mails:
            own = [r for r in rows if any(c == m for c in cells(r) for m in mails)]
        elif len(words) == 2:
            own = [r for r in rows if all(any(w == c or w in c.split() for c in cells(r)) for w in words)]
        else:
            continue
        if len(own) != 1:
            continue
        own = own[0]
        name_col = next((i for i, h in enumerate(header) if re.search(r"(?i)\bname\b", str(h or ""))), 0)
        for label, typed in form:
            if typed.strip().lower() in mails or typed.strip().lower() in words:
                continue
            lw = _words_of(label)
            col = next((i for i, h in enumerate(header) if h and lw and (_words_of(h) and (_words_of(h) <= lw
                                                                                          or lw <= _words_of(h)))), None)
            if col is None or col >= len(own) or _same_value(typed, own[col], app, source):
                continue
            others = [r for r in rows if r is not own and col < len(r) and _same_value(typed, r[col], app, source)]
            if others:
                whose = str(others[0][name_col] if name_col < len(others[0]) else "another record")
                out.append((str(label), typed, whose, "" if own[col] is None else str(own[col])))
    return out


def _source_files():
    """The request's own input tables: its attachments and the files in the folders it names (as BK3 reads them) -
    most requests name a folder ("everything is in the P3_payment_run folder") and attach nothing."""

    from decurix_agent import task_context
    files = list(task_context.current_files())
    try:
        from decurix_agent import long_task
        files += long_task.request_input_files(task_context.current_request(), ())
    except Exception:  # noqa: BLE001 - what cannot be listed is not read
        pass
    return [f for f in dict.fromkeys(str(x) for x in files)
            if Path(f).suffix.lower() in (".xlsx", ".xlsm", ".csv", ".tsv")]


def _crossed_record(element):
    """The refusal of a save whose form holds another record's values from the request's own table ("" to go on)."""

    if not _SAVE_CONTROL.search(str(element.get("name") or "")):
        return ""
    try:
        from decurix_agent import stage_facts, task_context
        from decurix_agent.computer import recording
        task_id = task_context.current()
        if not task_id:
            return ""
        _, events = recording.load(recording.session_for_task(task_id))
        tables = [(header, rows) for _, header, rows in stage_facts.tables_of(_source_files())]
    except (FileNotFoundError, OSError, ValueError, ImportError):
        return ""
    form = _current_form(events)
    found = crossed_values(form, tables) if form and tables else []
    if not found:
        return ""
    said = "; ".join(f"{field} is '{typed}' - that is {whose}'s; the source gives '{own}'"
                     for field, typed, whose, own in found[:6])
    return (f"Not pressed: this record's form holds another record's values ({said}). Put this record's own values "
            "from the source in those fields, then save.")


def _duplicate_record(element):
    """The refusal of a save that would enter, as a new record, one this task already saved ("" to go on)."""

    if not _SAVE_CONTROL.search(str(element.get("name") or "")):
        return ""
    try:
        from decurix_agent import task_context
        from decurix_agent.computer import recording
        task_id = task_context.current()
        if not task_id:
            return ""
        _, events = recording.load(recording.session_for_task(task_id))
    except (FileNotFoundError, OSError, ValueError, ImportError):
        return ""
    found = already_entered(events)
    if not found:
        maybe = possibly_entered(events)
        if maybe:
            # Not a refusal (D-12): the final check and the person see it.
            try:
                from decurix_agent.computer import evidence
                evidence.note(f"a record named {maybe[0]} was saved earlier in this task"
                              f"{f' ({maybe[1]})' if maybe[1] else ''}, and another with that name is being saved now - "
                              "two records, or the same one twice")
            except Exception:  # noqa: BLE001 - evidence is decoration here: the save is not stopped for it
                pass
        return ""
    what, number = found
    return (f"Not pressed: {what} is already saved in this application{f' ({number})' if number else ''} - entering it "
            "again makes a second record of the same one. Never enter a record twice. If the saved one is wrong and the "
            "application lets you correct it, correct that one; if it does not, write a line 'WRONG IN THE APPLICATION: "
            "<the record> - <what is wrong> - <what the person must fix by hand>' and leave it for the person.")


# A value that names one record among others: a long code of letters and digits (a tax, registration or account
# number) or an email.
_IDENTIFIER = re.compile(r"^(?:(?=[A-Za-z0-9]*\d)(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{10,20}|[^\s@]+@[^\s@]+\.[A-Za-z]{2,})$")
# A value that is a name (of a person, a firm): what a record of a party has and a document's own number is not.
_NAME_VALUE = re.compile(r"[A-Za-z][A-Za-z .,'&()-]{2,60}")
# The identifiers a task was stopped over (_unsearched_record), by why: each refusal is made once.
ID_REFUSALS_DIR = STATE_DIR / "id-refusals"


def _page_items(look):
    """
    What one noted look at the screen (evidence.note_look) showed as the page's own words, never a field's value:
    [(item, the words around it)] - each of its words outside fields, with the two before and after (a table's row),
    and each line of its listing that is not a field.
    """

    out = []
    words = re.search(r"outside its fields[^\n]*:\n(.*?)\nThe part in view", look, re.S)
    if words:
        items = [w.strip() for w in words.group(1).split(" | ")]
        out += [(w, " | ".join(items[max(0, i - 3):i + 3])) for i, w in enumerate(items) if w]
    listing = look.split("as the tool listed it:\n", 1)
    for line in listing[1].splitlines() if len(listing) == 2 else []:
        parts = line.split()
        # A window's or a tab's header and the page's address are not the page's words: a search typed into a list
        # page puts the identifier into its address ("list?q=<the identifier>"), and the gate read that as a record
        # holding it (2026-10-29: a new customer refused as "shown already" three times).
        if _HEADER_LINE.match(line) or "://" in line:
            continue
        if len(parts) > 1 and parts[1] not in TYPING_ROLES and " value=" not in line:
            out.append((line, line))
    return out


# The lines of a listing that frame the page rather than show it: the screen, its windows, a tab's header.
_HEADER_LINE = re.compile(r"^\s*(?:==|Screen \d|Active window:|Open windows:|Apps on this desktop)")


# A title in front of a name: not part of the name a record is known by.
_TITLE_RE = re.compile(r"^(?:mr|mrs|ms|miss|mx|dr|prof|shri|smt|sri|m/s|messrs)\.?\s+", re.I)


# The name field of a record of a party itself: "Name", "Ledger name", "Account name", "Full name" - not a field that
# names a party on a document ("Party A/c name", "Customer", "Supplier name").
_OWN_NAME_LABEL = re.compile(r"(?i)(?:ledger |account |full |legal |display |contact )?name\*?:?")


def plain_name(value):
    """A name as records are matched by it: titles set aside, case and punctuation too."""

    value = _TITLE_RE.sub("", str(value or "").strip())
    return " ".join(re.sub(r"[^a-z0-9 ]+", " ", value.lower()).split())


def same_named_record(events, looks):
    """
    For a new record of a party being saved (a new-record form with a name typed): the name a look of this task
    showed it listed under already - the same name once titles and punctuation are set aside - else "". Found live
    (2026-10-27): the books held a customer by name; the bill wrote the name with "Mr." in front; the worker searched
    the list, saw the row, and made a second customer with the title.
    """

    pairs, new_page = _new_record_fields(events)
    if not new_page or _record_refs(pairs):
        return ""  # (a document that quotes a party by name - a voucher, an order - is not a party record)
    # The party record's own name field ("Name", "Ledger name"), never a field that names a party on a document
    # ("Party A/c name", "Customer"): found live (2026-10-27), a voucher's save was refused as "a new party" because
    # its party field held a name the ledger list showed - the one the voucher was meant to use.
    names = [v.strip() for l, v in pairs if _OWN_NAME_LABEL.fullmatch(" ".join(l.split()))
             and _NAME_VALUE.fullmatch(v.strip()) and not _IDENTIFIER.match(v.strip())]
    if not names:
        return ""
    want = plain_name(names[0])
    if len(want) < 4:
        return ""
    for look in looks:
        for item, _ in _page_items(look):
            quoted = re.search(r'"([^"]+)"', item)
            text = quoted.group(1) if quoted else item
            for cell in [c.strip() for c in text.split(" | ")][:1]:
                if cell and plain_name(cell) == want:
                    return cell
    return ""


def _same_named_record(element):
    """The refusal of a save that makes a second party record under a name a list this task looked at shows already
    (same_named_record); "" to go on. It stands every time: a title or a short form on a document does not make
    another record, and only the person can say it is a different party."""

    if not _SAVE_CONTROL.search(str(element.get("name") or "")):
        return ""
    try:
        from decurix_agent import task_context
        from decurix_agent.computer import evidence, recording
        task_id = task_context.current()
        if not task_id:
            return ""
        _, events = recording.load(recording.session_for_task(task_id))
        shown = same_named_record(events, evidence.looks(task_id))
    except (FileNotFoundError, OSError, ValueError, ImportError):
        return ""
    if not shown:
        return ""
    return (f"Not pressed: this makes a new record of a party, and a list you looked at shows {el._quote(shown, 80)} "
            "already - the same name once the title is set aside. A title, a trade name or a short form on a document "
            "does not make another record: use that one as it is (choose it in the field; alter nothing on it), or ask the "
            "person (HELP) whether this is a different party - and only with their yes, create it.")


def _new_record_fields(events):
    """(pairs, new_page): the (label, value) typed or chosen into the form being saved, and whether the form is a
    new-record page (unsearched_identifier, same_named_record)."""

    pairs, new_page = [], False
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            pairs = []
        moved = re.search(r"page is now (\S+)", change)
        if moved:
            new_page = bool(_NEW_RECORD_PAGE.search("page is now " + moved.group(1).rstrip(";,")))
        elif _NEW_RECORD_PAGE.search(change) or _NEW_RECORD_PAGE.search(str(event.get("target") or "")):
            new_page = True
        label = str((event.get("element") or {}).get("name") or "")
        if event.get("kind") == "type" and event.get("text"):
            pairs.append((label, str(event["text"])))
        if event.get("kind") == "select" and event.get("option"):
            pairs.append((label, str(event["option"])))
        if event.get("kind") in ("click", "key") and _saved(change) and pairs:
            pairs, new_page = [], False
    return pairs, new_page


def unsearched_identifier(events, looks, known=()):
    """
    For a new record of a party being saved - a new-record form with a name typed and no number of its own (a
    customer, an employee; not an invoice, which only quotes one): [(identifier, words)] for each identifier typed
    into it - a code, an email (_IDENTIFIER), or any value in `known` (what this task resolved with --id, whatever its
    shape). words are the page's words around it when a look this task made showed it already (a record holds it);
    "" otherwise. [] when the form is not such a record. `looks` are the texts of the task's looks at the screen.
    Whether the identifier was resolved is not judged here: _unsearched_record reads the task's resolution facts
    (resolve.resolution). Found live (2026-10-26): a bill named its customer otherwise than the books did (a trade
    name, a title); the worker searched the books by the bill's name only, found nothing, and made a second account
    with the same tax number.
    """

    from decurix_agent.computer import resolve

    pairs, new_page = [], False
    known_keys = {resolve.key(k) for k in known if resolve.key(k)}
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            pairs = []
        moved = re.search(r"page is now (\S+)", change)
        if moved:
            new_page = bool(_NEW_RECORD_PAGE.search("page is now " + moved.group(1).rstrip(";,")))
        elif _NEW_RECORD_PAGE.search(change) or _NEW_RECORD_PAGE.search(str(event.get("target") or "")):
            new_page = True
        label = str((event.get("element") or {}).get("name") or "")
        if event.get("kind") == "type" and event.get("text"):
            pairs.append((label, str(event["text"])))
        if event.get("kind") == "select" and event.get("option"):
            pairs.append((label, str(event["option"])))
        if event.get("kind") in ("click", "key") and _saved(change) and pairs:
            pairs, new_page = [], False
    is_ident = lambda v: bool(_IDENTIFIER.match(v)) or resolve.key(v) in known_keys
    named = any(_NAME_VALUE.fullmatch(v.strip()) and not is_ident(v.strip()) for _, v in pairs)
    if not new_page or not named or _record_refs(pairs):
        return []
    items = [item for look in looks for item in _page_items(look)]
    out = []
    for label, value in dict.fromkeys(pairs):
        ident = value.strip()
        if not is_ident(ident):
            continue
        low = ident.lower()
        shown = next((around for item, around in items if low in item.lower()), "")
        out.append((ident, " ".join(shown.split())[:240] if shown else ""))
    return out


def _unsearched_record(element):
    """
    The refusal of a save that makes a new record holding an identifier this task has not resolved ("" to go on). A
    mechanical gate on the task's resolution facts (resolve.py, written by gui select --id): no fact - refused until
    resolved; a match - refused naming the record that holds it; none holds it - the save goes on. A look of this
    task that showed the identifier beside other words is a second trigger. Each refusal is made once per identifier
    and reason (ID_REFUSALS_DIR): the worker that saves again after it has decided, and the record says so.
    """

    if not _SAVE_CONTROL.search(str(element.get("name") or "")):
        return ""
    try:
        from decurix_agent import task_context
        from decurix_agent.computer import evidence, recording, resolve
        task_id = task_context.current()
        if not task_id:
            return ""
        _, events = recording.load(recording.session_for_task(task_id))
        found = unsearched_identifier(events, evidence.looks(task_id), resolve.identifiers(task_id))
    except (FileNotFoundError, OSError, ValueError, ImportError):
        return ""
    for ident, words in found:
        fact = resolve.resolution(task_id, ident)
        if fact["status"] == "none":
            continue  # resolved: no record holds it, so this is a new one
        why = fact["status"] or ("shown" if words else "unresolved")
        mark = ID_REFUSALS_DIR / f"{task_id}-{why}-{re.sub(r'[^A-Za-z0-9]', '_', ident)}"
        if mark.exists() and fact["status"] != "matched":
            continue  # stopped over once already: the worker has decided, and saves it as it is
        if mark.exists():
            # A record that holds the identifier exists: a second one is never the worker's to decide. Found live
            # (2026-10-27): refused once, the worker saved again and the customer got a second ledger. Only the
            # person's word on it (an answer, an approval) lets it through - and that comes back as "resolved by the
            # person" through gui select --id, not by saving again.
            return (f"Not pressed: this makes a new record holding {ident}, and the record "
                    f"{el._quote(fact['name'] or '', 90)} holds it already ({fact['line']}). A second record holding "
                    "the same identifier is never saved on the worker's own judgement: use that record (open it and "
                    "edit it if its name or address must change), or ask the person (HELP) whether this is a "
                    "different party - and only with their yes, create it.")
        try:
            ID_REFUSALS_DIR.mkdir(parents=True, exist_ok=True)
            privacy.write_scrubbed(mark, "1")
        except OSError:
            return ""  # a stop that cannot be kept to once is not made
        head = f"Not pressed: this makes a new record holding {ident}, "
        if fact["status"] == "matched":
            return (head + f"and this task resolved {ident}: the record {el._quote(fact['name'] or '', 90)} holds it already "
                    f"({fact['line']}). A record that holds the identifier is the one to use, whatever its name - a title, "
                    "a trade name or a short form on a document does not make another one: use that record (to change "
                    "it, open it and edit it), and say in the report how you matched it. Only if you can say why this is "
                    "a different one, save again.")
        if fact["status"] == "ambiguous":
            return (head + f"and this task found more than one record holding it ({fact['line']}): a third makes it worse. "
                    "Use one of them - ask the person which (HELP) if the documents do not say - and tell the person "
                    "the application holds two.")
        if words:
            return (head + f"and a page you looked at shows {ident} already: \"{words}\". A record that holds it already is "
                    "the one to use - a name written otherwise on a document (a title, a trade name, a short form) does "
                    f"not make another one. Resolve it first: gui select REF \"<the name as the document writes it>\" --id "
                    f"{ident} on the application's list of such records (its search or filter field) or on the field that "
                    "chooses one - it searches by the identifier and reports which record holds it. Use that record, and "
                    "say in the report how you matched it.")
        where = ("the field you searched showed nothing for it" if fact["status"] == "unanswered"
                 else f"nothing in this task resolved {ident}")
        return (head + f"but {where}. Resolve it first: gui select REF \"<the name as the document writes it>\" --id {ident} "
                "on the application's list of such records (its page with a search or filter field) or on the field that "
                "chooses one - it searches by the identifier first and reports whether a record holds it; a record that "
                "already holds it is the one to use, whatever its name. When it reports that none holds it, save again - "
                "it is then a new one.")
    return ""


def _entered_fields(fields):
    """
    [(label, value)] of a form's fields whose value this task's tools typed or chose exactly (evidence.entered): what
    the source check (_unsourced_fields) holds against the request's sources - an application's own default it never
    judges. Raises when the task's recording cannot be read (the caller makes that a check not made).
    """

    from decurix_agent.computer import evidence
    typed = {" ".join(str(v).split()) for v in evidence.entered()}
    return [(label, value) for label, value in fields if " ".join(str(value or "").split()) in typed]


# Events by which the worker puts something into the field it acts on (a paste is a key event on that field).
_INPUT_KINDS = ("type", "select", "key")
_PICK_ROLES = ("option", "listitem", "menuitem", "menuitemradio", "treeitem", "gridcell")


def _shown_by_app(task_id=None):
    """
    {field label: value} that the application itself showed in a field before any worker input into it: read from the
    observations kept with this task's actions (audit.keep_observation - each is the screen an action was resolved
    against, so the first one holding a field shows it before that action). A field the worker typed, chose, pasted
    or picked an entry into from then on is the worker's (review round 2, B9). Raises when the recording cannot be read.
    """

    import json as _json
    from decurix_agent import task_context
    from decurix_agent.computer import evidence, recording
    task_id = task_id or task_context.current()
    events = evidence._task_events(task_id)
    folder = recording._dir(recording.session_for_task(task_id)) / "obs"
    shown, touched, last = {}, set(), ""
    for event in events:
        name = str((event.get("element") or {}).get("name") or "")
        role = str((event.get("element") or {}).get("role") or "")
        if event.get("obs"):
            try:
                doc = _json.loads((folder / f"{event['obs']}.json").read_text(encoding="utf-8"))
            except (OSError, ValueError):
                doc = {}
            for element in doc.get("elements") or []:
                label, value = str(element.get("name") or ""), element.get("value")
                if label and value not in (None, "") and label not in touched and label not in shown:
                    shown[label] = str(value)
        if event.get("kind") in _INPUT_KINDS:
            touched.add(name or last)
        elif event.get("kind") in ("click", "double_click") and role in _PICK_ROLES and last:
            touched.add(last)  # an entry picked from a field's list (autocomplete) is put into that field
        if name and role not in _PICK_ROLES:
            last = name
    touched.discard("")
    return {label: value for label, value in shown.items() if label not in touched}


def _not_the_apps(fields):
    """The form's fields the filler check judges: every one, except a value the application itself showed in that
    field before the worker put anything there (_shown_by_app), compared by values.compare - a value the application
    reformatted or re-cased is the same value, so neither dodges the check."""

    from decurix_agent import values
    shown = _shown_by_app()
    return [(label, value) for label, value in fields
            if not (label in shown and values.same_value(value, shown[label]) is True)]


def _unsourced_fields(fields):
    """
    [(label, value)] of a form's fields whose value the tools typed during this task and that states a fact (an
    address, a date, a code, a figure of 100 or more) no source holds - the request, its files, the person's messages,
    the system's pages read, the work's recorded working (provenance, harness audit 2026-10-21). A value the
    application filled in itself is not the worker's and is never judged here.
    """

    from decurix_agent import long_task, task_context
    mine = _entered_fields(fields)
    if not mine:
        return []
    sources = long_task.work_sources(task_context.current_request())
    declared = _declared_values()
    # A value a record of this task declares from its document is that document's: the declaration checked it against
    # the source, or kept it as read off a picture (records.declare). Found live (2026-10-27): the figures of a
    # photographed bill, declared from the photo, were refused as "invented" at every save - no text holds a photo's
    # figures - and a mixed-rate bill could not be entered at all.
    return [(label, value) for label, value in mine
            if sources.unsourced(value) and not _declared_holds(value, declared)]


def _declared_holds(value, declared):
    from decurix_agent.computer import records
    return any(records.same_value(value, d) for d in declared)


def _declared_values():
    """The values the task's declared records hold as their intent (records.load)."""

    out = []
    try:
        from decurix_agent.computer import records
        for rec in records.load():
            for f in (rec.get("fields") or {}).values():
                if f.get("intent") and f.get("from") != "empty":
                    out.append(str(f["intent"]))
    except Exception:  # noqa: BLE001 - no ledger: nothing declared
        return []
    return out


def _form_check_pending():
    """The ref of the submit control whose form was listed for checking within FORM_CHECK_SECONDS and not confirmed
    since, else "". The note is written at the listing and cleared when the checked click goes through."""
    from decurix_agent import task_context

    try:
        noted = json.loads(_form_check_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return ""
    if (time.time() - float(noted.get("at") or 0) < FORM_CHECK_SECONDS and noted.get("key")
            and (noted.get("task") or "") == task_context.current()):
        return noted.get("ref") or "REF"
    return ""


def _form_check(element, checked=False, found=None, confirm=None, listing=True):
    """
    Before a form with several filled fields is sent by a click on its submit control: a click lists what the form
    holds and presses nothing; the click again with --checked, the form unchanged since that list, sends it. Found live
    (2026-10-20): an order saved with a quantity the worker had not meant - a list of the fields before Save is the
    one look that catches it. Returns the text that stops the click, or "" to go on.

    found: {"form", "fields"} when the caller has read what the press sends (a page with no <form>, Enter in a form's
    field - _commit_check); else the <form> of the submit control is read. confirm: how the listing is confirmed, when
    not by the same click with --checked (an Enter is never confirmed: the form's own submit control is pressed).

    listing="ask" (a replay's press): what would list the form, or a value no source of today's request holds, is
    returned as AskFirst - the press is put to the person with today's values through the approval gate (core.act),
    since a recording a person made is not their yes to the values a run types today. What is never sent (a record
    entered twice, a placeholder, a value the declared record does not hold) still stops it.
    """

    import hashlib

    from decurix_agent import task_context

    twice = (_duplicate_record(element) or _crossed_record(element) or _unsearched_record(element)
             or _same_named_record(element))
    if twice:
        return twice  # (never sent, --checked or not)
    if found is None:
        found = cdp.submit_form_fields(element, min_fields=1)
    if not found or not found.get("fields"):
        return ""
    probe = placeholder_fields(found["fields"])
    if probe:
        # Never sent, with or without --checked: a record saved to see what the application does is a real record.
        return (f"Not pressed: {el._quote(found['form'], 60)} holds a placeholder ({'; '.join(probe)}), so this would "
                "save a test record in a real application - which may then be invoiced or shipped, and many "
                "applications cannot delete it. Never save, submit or post a record to find out how an application "
                "behaves: learn the form by reading it (its fields, labels, options, help, an existing record's detail "
                "view), and save only the real records the request asks for. A form started by mistake is cancelled "
                "or left unsaved.")
    try:
        from decurix_agent import filler, task_context
        files = task_context.current_files()
        # Every field, except a value the application itself showed there before the worker acted (an edit form's
        # existing record, found in the GUI bench 2026-11-03): decided by the observations, not by what was typed -
        # a reformatted, re-cased, pasted or picked value is judged (review round 2, B9).
        made_up = filler.invented(_not_the_apps(found["fields"]), task_context.current_request(), files)
        unsourced = _unsourced_fields(found["fields"])
        if listing == "ask":
            # A replay types what the person demonstrated where today's request gives nothing (replay.run): their own
            # value, not one made up. Any other value no source holds is refused below, like an engine's.
            unsourced = [(label, value) for label, value in unsourced if not _taught_value(value)]
        made_up += unsourced
        # A record's number, reference or id the worker typed as words (records.wordy_identifiers): a document writing
        # them there points elsewhere; only the person can give the code (found live 2026-11-08).
        from decurix_agent.computer import records
        wordy = records.wordy_identifiers(_entered_fields(found["fields"]))
    except Exception as exc:  # noqa: BLE001
        # A check that could not be made is not a check that passed (review 2026-11-02): the press waits for a person.
        return (f"Not pressed: the check of where {el._quote(found['form'], 60)}'s values come from could not be made "
                f"({' '.join(str(exc).split())[:160]}), so nothing shows they are the request's. Ask the person (HELP) "
                "before this is saved.")
    lines = "\n".join(f"  {label}: {value}" for label, value in found["fields"])
    if made_up:
        # Never sent, with or without --checked (found live 2026-10-20: a joiner onboarded with phone 9999999999) - nor
        # by a replay: a value in no source is refused, never put to a person to wave through (review 2026-11-03).
        return filler.refusal(made_up, files)
    if wordy:
        # Never sent, with or without --checked, a replay's press too: the person is the one to give the code.
        return f"Not pressed: {el._quote(found['form'], 60)}: " + records.wordy_text(wordy)
    ledger_lines = []
    judged = _ledger_check(found["fields"], cut=found.get("cut") or (), unset=found.get("unset") or ())
    if judged is not None:
        held, clean, ledger_lines = judged
        if held:
            return held
        if clean:
            return ""  # the declared record is the check this listing stood in for: one click saves
    if listing == "ask":
        # A replay of the person's own steps whose every value is today's request's or the person's demonstration:
        # saving it is the taught task, not a new decision. Asking at every save (review 2026-11-02, second round) made
        # every taught skill wait on a person at each record - and taught them to say yes without reading
        # (review 2026-11-03). What commits the person to more than a record in their own system (pay, send, file,
        # sign, an irrevocable submit) is still the approval gate's to ask, at the press itself.
        return ""
    if len(found["fields"]) < cdp.FORM_CHECK_MIN_FIELDS or not listing:
        return ""  # a small form (a search, a sign-in) is sent at once
    key = hashlib.sha256(json.dumps([element.get("path") or element.get("name"), found], sort_keys=True).encode()).hexdigest()
    note_file = _form_check_file()
    try:
        noted = json.loads(note_file.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        noted = {}
    if (noted.get("task") or "") != task_context.current():
        noted = {}  # another task's listing confirms nothing here
    if confirm:
        # Not confirmed this way: the same press would only list it again. The form's own submit control is.
        return (f"Not pressed - {el._quote(found['form'], 60)} would be sent holding:\n{lines}\n"
                + ("".join(f"  ({line})\n" for line in ledger_lines)) + confirm)
    if checked and noted.get("key") == key and time.time() - float(noted.get("at") or 0) < FORM_CHECK_SECONDS:
        # Kept until the click is let through (act): a click the approval gate then stops is asked for, and the same
        # --checked click after the person's yes must not list the form again. Found live (2026-10-24): the note was
        # used up by the click the gate stopped, the approved click listed the form instead of pressing, and the
        # filing was put to the person five more times.
        return ""
    try:
        privacy.write_scrubbed(note_file, {"key": key, "at": time.time(), "ref": element.get("ref") or "",
                                                      "task": task_context.current()}, shapes=False)
    except OSError as exc:
        # Without the note the listing cannot be confirmed later, and letting the press through unlisted is the check
        # skipped (review 2026-11-02). It waits for a person instead.
        return (f"Not pressed: the list of what {el._quote(found['form'], 60)} would send could not be kept "
                f"({' '.join(str(exc).split())[:120]}). Ask the person (HELP) before this is saved.")
    return (f"Not pressed yet - {el._quote(found['form'], 60)} would be sent holding:\n{lines}\n"
            + ("".join(f"  ({line})\n" for line in ledger_lines))
            + "Check every value against what you meant to enter (a quantity, a price, every line). If all are right, "
            f"send gui click {element.get('ref') or 'REF'} --checked to save (the act tool: the same click with "
            "checked: true); if one is wrong, correct it first (gui type REF \"value\") - the form is listed again "
            "before it is sent.")


# A field that narrows what a list shows ("Filter by status", "Sort by").
_FILTER_FIELD = re.compile(r"^\s*(?:filter|sort|group by|show|search|find|page size|rows per page)\b", re.I)
# A control whose label opens a new record or accepts something, rather than saving what the page around it holds:
# with no <form> and no dialog around it, the page's other fields are not what it sends (review 2026-11-03: "Create"
# on a list page and "Accept" on a banner were refused over fields they never send).
_OPENS_OR_ACCEPTS = re.compile(r"^\s*(?:create|new|accept|agree)\b", re.I)

# A value that is a file's name or path (a save dialog's "Name" field): not a record's value, never judged as one.
_FILE_NAME_VALUE = re.compile(r"(?:[^\s@]*[/\\])?[^\s@/\\]+\.[A-Za-z][A-Za-z0-9]{1,4}")


def _page_fields(observation, element):
    """
    {"form", "fields": [[label, value]]}: the filled fields in the same window as a committing control that sits in no
    <form> - an application drawn with divs and spans, a desktop app's dialog. What a press there saves is what the
    window holds. Secrets and file names are left out.
    """

    group = (element or {}).get("group") or active_group(observation)
    fields, cut, unset = [], [], []
    for e in (observation or {}).get("elements") or []:
        if e.get("role") not in FILLED_ROLES or (group and e.get("group") != group) or e is element:
            continue
        if approval.searches(e) or _FILTER_FIELD.match(str(e.get("name") or "")):
            # A list's search box and filters say what is shown, not what a press saves (review 2026-11-03: a list
            # page's filters were checked as the record a "Create" there would save).
            continue
        label = " ".join(str(e.get("name") or e.get("role") or "").split())[:60]
        value = " ".join(str(e.get("value") or "").split())
        if not value and e.get("role") == "combobox":
            unset.append(label)  # a choice in its "no selection" state (records.presave), told by structure
        if not value or privacy.secret_field(e) or privacy.credential_in(value) or _FILE_NAME_VALUE.fullmatch(value):
            continue
        # Read whole up to cdp.FORM_VALUE_CHARS, a longer value marked cut: a cut read is never judged as the whole
        # (found live 2026-11-08: cut at 80 characters, a 126-character field could never be saved).
        value, part = cdp.cut_value(value)
        fields.append([label, value])
        if part:
            cut.append(label)
    title = (element or {}).get("window_title") or (observation or {}).get("active_title") or "the window"
    return {"form": str(title)[:80], "fields": fields, "cut": cut, "unset": unset}


def _commit_keys(keys):
    """(Enter, Ctrl+S): whether these keys press Enter, and whether they save (an application's Ctrl+S)."""

    combos = [xinput.normalize_combo(k).lower() for k in keys or () if k and k.strip()]
    return (any(c in ("return", "enter", "kp_enter") for c in combos), any(c == "ctrl+s" for c in combos))


def _commit_check(kind, element, keys, before, checked=False, text=None, listing=True, pressed=None):
    """
    The pre-save checks (_form_check: a record entered twice, a placeholder, an invented or unsourced value, the
    declared record's ledger, the listing of a full form) for every action that commits what a form or a page holds,
    whichever way it is pressed: a click or double-click on a <form>'s submit control or on any control whose label
    says it saves, submits, sends, pays or posts (approval.commits) - a CDP element or an AT-SPI one, found by ref, by
    name or by --see; Enter in a form's field that submits it; Ctrl+S on a web page. Returns the text that stops the
    action, or "" to go on. Found in review (2026-11-02): the checks ran only for a single `gui click` on a CDP
    <form type=submit>, so a double-click, --see, the act tool, an AT-SPI press, an Enter and every button an
    application draws with divs (the `gui record` ledger among them) went past them.

    pressed: a list the control this action commits with is appended to (the element, or the form's submit control an
    Enter or a Ctrl+S stands for), whatever the checks decide - the press the plan of the work may hold (_plan_hold).
    """

    def committing(control):
        if pressed is not None:
            pressed.append(control)

    if kind in ("click", "double_click") and element is not None:
        form = cdp.submit_form_fields(element, min_fields=1, unreadable=cdp.UNREADABLE) \
            if element.get("source") == "cdp" else None
        if form is cdp.UNREADABLE:
            if element.get("role") != "button" and not approval.commits(element):
                form = None
            else:
                # A button whose form could not be read may submit one: the checks cannot be made, so a person looks
                # first (review 2026-11-03, D-26 - "no form" and "could not read" were the same None).
                committing(element)
                label = " ".join(str(element.get("name") or "").split())[:60]
                return AskFirst(f'the form "{label}" may submit could not be read, so the checks made before a save '
                                "could not be made - look at what it holds before it goes")
        if form is None and not approval.commits(element):
            return ""
        if form is None and element.get("source") == "cdp" \
                and _OPENS_OR_ACCEPTS.match(approval.label_words(element.get("name"))) \
                and not approval.DIALOG_IN_PATH.search(element.get("path") or "") \
                and approval.press_effect(element) != "asks":
            return ""
        committing(element)
        return _form_check(element, checked=checked, found=form or _page_fields(before, element), listing=listing)
    typed_enter = kind == "type" and bool(text) and "\n" in text and element is not None
    if kind != "key" and not typed_enter:
        return ""
    enter, save = _commit_keys(keys) if kind == "key" else (True, False)
    field = element if element is not None else _keyboard_field(before)
    if enter and field is not None and field.get("source") == "cdp":
        form = cdp.enter_form_fields(field)
        if not form:
            return ""
        if typed_enter:
            # What the field will hold when its new line submits the form: the text typed before it.
            label = " ".join(str(field.get("name") or field.get("role") or "").split())[:60]
            line, part = cdp.cut_value(text.split("\n", 1)[0])
            form = dict(form, fields=[f for f in form.get("fields") or [] if f[0] != label]
                        + ([[label, line]] if line.strip() and not privacy.secret_field(field) else []),
                        cut=[c for c in form.get("cut") or [] if c != label] + ([label] if part else []))
        submit = {"name": form.get("submit") or "Submit", "ref": "", "path": field.get("path") or ""}
        committing(submit)
        return _form_check(submit, found=form, listing=listing, confirm=(
            "Enter would send it, and an Enter is not how a full form is sent: press the form's own submit control "
            "(gui click REF), which lists the form once more and is confirmed with --checked."))
    if save and before.get("page_url") and not _relayed_page(before):
        # (Ctrl+S on a web page saves its record: the checks that stop a wrong record stand; the listing of a full
        # form is the Save control's, since a page saved only by its keys has no control to confirm it on.)
        save_control = {"name": "Save", "ref": "", "path": (field or {}).get("path") or "",
                        "group": (field or {}).get("group")}
        committing(save_control)
        return _form_check(save_control, found=_page_fields(before, field), listing=False)
    return ""


def _ledger_check(form_fields, cut=(), unset=()):
    """
    The declared record (records.py) against the form about to be saved: None when no record is open; else
    (what stops the click or "", whether the diff is clean enough to save without a listing, advisory lines). A field
    holding a value the record does not stops the click; the record's scope absent from the window stops it once.
    cut: the form's labels whose values were read in part (cdp.FORM_VALUE_CHARS); unset: its choices left in their
    "no selection" state (records.presave).
    """

    from decurix_agent import task_context
    from decurix_agent.computer import evidence, records

    try:
        found = records.load()
        form_text = " | ".join(str(v) for _, v in form_fields if v)
        rec = records.open_record(found, form_text, form=form_fields)
    except Exception:  # noqa: BLE001 - a ledger that cannot be read judges nothing
        return None
    events = _task_events_now()
    if rec is None:
        # A new record's form holding the key of a record this task saved already, in that record's own key field: the
        # second save would make a second record (review round 9, B: a task tried again from the start saved its bill
        # twice, outside the ledger). Only its key field - a payment "Against bill X" after bill X is another record.
        done = records.saved_on_form(found, form_fields)
        if done is not None and _new_record_fields(events)[1]:
            return ("Not pressed: " + records.saved_again_text(done), False, [])
        return None
    blocking, advisory, matched = records.presave(rec, form_fields, cut=cut, unset=unset)
    # A field this task set on the form that the record does not declare: its value has no source on record. ADVISORY
    # at the save until it passes the replay gate (records.UNDECLARED_FIELD_BLOCKS); the final check holds it against
    # the saved record (records.gate_faults) - review round 9, A.
    undeclared = records.undeclared_task_fields(rec, form_fields, [label for label, _ in _current_form(events)])
    if undeclared:
        line = ("this task set " + ", ".join(f"{l}={v}" for l, v in undeclared[:6]) + " and no declaration covers it: "
                "declare it with its source (gui record), or ask the person (HELP) for it")
        (blocking if records.UNDECLARED_FIELD_BLOCKS else advisory).append(line)
    key = rec.get("key")
    if rec.get("scope"):
        try:
            now = observe(remember=False)
            missing = records.scope_missing(rec["scope"], now.get("active_title"), evidence.words_text(now, records.MAX_PAGE_TEXT))
        except Exception:  # noqa: BLE001
            missing = []
        if missing:
            # (Once per scope, not per record: nine records on pages that never name the client would be nine stops.)
            if records.stop_once(task_context.current(), f"scope-{rec['scope']}"):
                return (f"Not pressed: record {key} belongs in {el._quote(rec['scope'], 80)}, but the window "
                        f"({el._quote(now.get('active_title') or '', 80)}) shows none of: {', '.join(missing)}. Read "
                        "which entity, book or period is open and open the right one before saving; if this window is "
                        "right and only worded otherwise, save again.", False, [])
            advisory.append(f"the window does not show the scope words {', '.join(missing)}")
    try:
        records.note_presave(rec, blocking, undeclared=undeclared,
                             unset=records.undeclared_unset(rec, unset))
    except Exception:  # noqa: BLE001
        pass
    if blocking:
        return (f"Not pressed: the form does not hold what record {key} says - " + "; ".join(blocking)
                + ". Correct the field (gui type REF \"value\" / gui select REF \"option\"), then save again. If the "
                "record's own intent was misread, declare it again from its source (gui record): the declaration is "
                "checked against the document, so a value the document writes otherwise is refused there - never save "
                "a value the document does not give, nor a part of one." + (" Also: " + "; ".join(advisory) if advisory else ""), False, advisory)
    wanted = sum(1 for f in rec.get("fields", {}).values() if f.get("intent"))
    clean = matched >= max(1, (wanted + 1) // 2)
    return "", clean, ([f"record {key}: {matched} of {wanted} declared values match the form"] + advisory)


def _task_events_now():
    """The task's recorded events, oldest first; [] when there is no task or no record of it."""

    try:
        from decurix_agent import task_context
        from decurix_agent.computer import recording
        task_id = task_context.current()
        if not task_id:
            return []
        return recording.load(recording.session_for_task(task_id))[1]
    except (FileNotFoundError, OSError, ValueError, ImportError):
        return []


def records_created(events):
    """How many of the task's saves made a new record: a save answered as saved, pressed on a new record's page (its
    address .../new, /create, /add) - what the ledger's declared records must equal (review round 9, B)."""

    created, new_page = 0, False
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            new_page = True
        if event.get("kind") in ("click", "key") and _saved(change) and new_page:
            created += 1
            new_page = False
            continue
        moved = re.search(r"page is now (\S+)", change)
        if moved:
            new_page = bool(_NEW_RECORD_PAGE.search("page is now " + moved.group(1).rstrip(";,")))
        elif _NEW_RECORD_PAGE.search(change) or _NEW_RECORD_PAGE.search(str(event.get("target") or "")):
            new_page = True
    return created


def saved_twice(events):
    """
    {record number: how many saves} for each number that two or more saves of a new record in this task held in the
    record's own number field (_record_refs: a No., Bill, Invoice, Voucher or Ref field with a digit): one record saved
    twice, found with no declaration (review round 9, R9-B4: a task run again saved its bill a second time, and with no
    ledger nothing saw it). A save the application has since deleted no longer counts (already_entered).
    """

    saves, pairs, new_page, page = [], [], False, ""
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if event.get("kind") in ("click", "key") and saves:
            answer = urllib.parse.unquote_plus(change).upper()
            if _DELETED_ANSWER.search(answer) and not _SAVE_REFUSED.search(answer):
                saves = [s for s in saves if not _released(("", "", s[0], s[1]), answer, page)]
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            new_page, pairs = True, []
        label = str((event.get("element") or {}).get("name") or "")
        if event.get("kind") == "type" and event.get("text"):
            pairs.append((label, str(event["text"])))
        if event.get("kind") == "select" and event.get("option"):
            pairs.append((label, str(event["option"])))
        moved = re.search(r"page is now (\S+)", change)
        if event.get("kind") in ("click", "key") and _saved(change):
            if new_page:
                landed = moved.group(1).rstrip(";,").split("?")[0] if moved else ""
                saves.append((_record_refs(pairs), landed if re.search(r"/[^/]*\d[^/]*/?$", landed) else ""))
            pairs, new_page = [], False
        elif moved:
            new_page = bool(_NEW_RECORD_PAGE.search("page is now " + moved.group(1).rstrip(";,")))
        elif _NEW_RECORD_PAGE.search(change) or _NEW_RECORD_PAGE.search(str(event.get("target") or "")):
            new_page = True
        if moved:
            page = moved.group(1).rstrip(";,").split("?")[0]
    counts = {}
    for refs, _ in saves:
        for ref in refs:
            counts[ref] = counts.get(ref, 0) + 1
    return {ref: n for ref, n in counts.items() if n > 1}


def _ledger_readback(after, change_text):
    """After a save: the declared record read back off the page the save left (records.py); "" when none is open."""

    from decurix_agent.computer import evidence, records

    try:
        page = (evidence.fields_text(after, records.MAX_PAGE_TEXT, offscreen=True) + "\n"
                + evidence.words_text(after, records.MAX_PAGE_TEXT))
        rec = records.open_record(records.load(), page + "\n" + str(change_text or ""))
        if rec is None:
            return ""
        return "\n" + "\n".join(records.readback(rec, after.get("active_title"), page, change_text))
    except Exception as exc:  # noqa: BLE001 - a read-back that fails is said, never raised
        return f"\n(the record could not be read back: {exc})"


def _type_with_keys(element, text, append):
    """The page threw typed text away (cdp.type_text): type it the way a person does - click into the field, then real
    keys. Returns the backend."""

    _focus_window(element)
    cdp.click(element)
    time.sleep(0.3)
    if not append:
        xinput.key("ctrl+a")
    xinput.type_text(text)
    return "xdotool"


def _watch_downloads(element):
    """What the browser says of downloads while this action runs (cdp.watch_downloads), on the element's own tab when
    it is a page's element; None when it cannot say, and the download folder alone decides."""

    try:
        handle = (element or {}).get("handle") or {}
        target = handle.get("target_id") if (element or {}).get("source") == "cdp" else None
        return cdp.watch_downloads(target)
    except Exception as exc:  # noqa: BLE001 - only a quicker end to the wait; the folder decides without it
        from decurix_agent.logging_utils import log
        log(f"Could not watch for downloads: {exc}")
        return None


def _worker_point(kind, element, point):
    """
    Broker mode, the screen side: a widget read off the worker's accessibility bus (atspi._broker_bus) is the worker's
    word - its name, its role, its box. An action on one is done only where a window of the worker's own apps is shown:
    otherwise a widget the worker made up, laid over the browser, would have the gate asked about "Cancel" while the
    press landed on the browser's "Pay". Raises ComputerError, nothing done, when the point shows any other window.
    """

    from decurix_agent.computer import broker
    if element is None or element.get("source") != "atspi" or not broker.screen_side():
        return
    target = point or el.center(element)
    if target is None:
        return  # nothing is pressed at a point; the action says so itself
    worker = broker.worker_uid()
    if screen.owner_uid_at(*target) != worker and point is None:
        # Its window may be behind another: brought forward as an action on it would be, then looked at again.
        try:
            _focus_window(element)
            time.sleep(0.3)
        except ComputerError:
            pass
    if screen.owner_uid_at(*target) != worker:
        raise ComputerError(f"{el.describe(element)} is a control of an app of this desktop's worker, and the screen "
                            f"at {target[0]},{target[1]} shows a window that is not that app's - nothing was done. "
                            "gui observe again: the window may have moved or be behind another.")


def _perform(kind, element, point, text, keys, direction, amount, option, append, end=None, files=None, ids=None,
             later=None):
    """
    Carry out one action; return which backend did it. `ids`: the identifiers a select resolves the option by first.
    `later`, a list: typing into a browser field leaves in it the read-back of whether the field kept the text
    (cdp.type_text's check_later), for the caller to make once the screen has settled - and, when it was thrown away,
    to type it again with real keys (_type_with_keys). With no list the read-back is waited for here.
    """

    _worker_point(kind, element, point)

    if element is not None and element.get("handle", {}).get("sheet_cell"):
        if kind == "type":
            if text is None:
                raise ComputerError("nothing to type")
            if _is_block(text):
                _type_sheet_block(element, text)
                return "xdotool:sheet-block"
            _goto_sheet_cell(element)
            # Typing into a selected cell replaces it; Enter commits the entry.
            xinput.type_text(text)
            if not text.endswith("\n"):
                xinput.key("Return")
            return "xdotool:sheet"
        if kind in ("click", "double_click"):
            _goto_sheet_cell(element)
            if kind == "double_click":
                xinput.key("F2")
            return "xdotool:sheet"

    if kind in ("click", "double_click", "right_click"):
        count = 2 if kind == "double_click" else 1
        button_cdp = "right" if kind == "right_click" else "left"
        button_x = 3 if kind == "right_click" else 1

        if element is not None and element["source"] == "cdp" and kind == "click" and point is None \
                and element.get("role") in RELAYED_PRESSABLE and (element.get("name") or "").strip() \
                and _relayed(element, _load_state()):
            # A named control of an app shown in a viewer (a Windows app's ribbon, its File page, a dialog's buttons)
            # is pressed through the app's own accessibility: a posted click let Office's ribbon go by. When the app
            # cannot press it that way, it is clicked as before.
            try:
                cdp.press_in_app(element)
                return "relayed:pressed"
            except ComputerError as exc:
                if "BLOCKED" in str(exc):
                    # Its window waits on an open dialog: nothing behind a dialog can be pressed until it is closed.
                    raise ComputerError(f"{el.describe(element)} is behind an open dialog of the app, and nothing "
                                        "was pressed: finish the dialog first (its own buttons, e.g. its Save or "
                                        "Cancel) - the control you want is the dialog's.") from None

        if element is not None and element["source"] == "cdp" and not _inside_large(element, point):
            opened = cdp.click(element, button=button_cdp, count=count)
            return "cdp:file_chooser" if opened.get("file_chooser") else "cdp"

        # A list or table cell's own action is "activate" - open the file,
        # not select it - so a cell is clicked where it is drawn instead.
        # A click aimed at a point is a press there, whatever the look says is under it: an open menu is a window of
        # its own that a look may not list, and the control "under" the point is then the one behind the menu. Found
        # live (2026-10-15): "Export As..." in GIMP's open File menu was resolved to a checkbox behind it, the
        # checkbox's own action toggled it, and the menu item was never pressed - four times. Raising the control's
        # window would close the menu too.
        aimed = point is not None and element is not None and element.get("source") != "cdp"
        if element is not None and kind == "click" and element["role"] not in ("cell", "listitem", "treeitem", "icon") \
                and element.get("source") != "cdp" and not _inside_large(element, point) and not aimed:
            done = atspi.default_action(element)
            if done:
                return f"atspi:{done}"
            # A control that commits or that the gate stops is pressed only as the very widget that was looked at: one
            # that is no longer at its place (atspi.resolve) is not clicked where it was drawn, where another may now be.
            if (approval.commits(element) or approval.assess(kind, element) is not None) \
                    and not element.get("handle", {}).get("sheet_cell") and atspi.resolve(element) is None:
                raise ComputerError(f"{el.describe(element)} is no longer the control it was when the screen was read "
                                    "(the window redrew) - nothing was pressed. gui observe again and press it by its "
                                    "new ref.")

        target = point or (el.center(element) if element else None)
        if target is None:
            raise ComputerError(f"{el.describe(element)} has no position on screen to click")

        if element is not None and not aimed:
            _focus_window(element)

        xinput.click(target[0], target[1], button=button_x, repeat=count)
        return "xdotool"

    if kind == "type":
        if text is None:
            raise ComputerError("nothing to type")

        if element is not None and element["source"] == "cdp":
            if element.get("role") in RELAYED_SETTABLE and not any(c in text for c in "\r\n\t") \
                    and not privacy.secret_field(element) and _relayed(element, _load_state()):
                # A field of an app shown in a viewer (a Windows app) is set through the app's own accessibility, and
                # read back: typed key by key, the keys raced the app - "D20" into Excel's Name Box reached it as "D2"
                # with the Enter overtaking the "0", and a whole Total row landed in the wrong cell (found live,
                # 2026-10-16). A field the app will not set that way is typed into as before.
                try:
                    cdp.set_in_app(element, text, append=append)
                    return "relayed:set"
                except ComputerError as exc:
                    # Typed instead only when nothing was set. Once the app may hold the text, typing it as well wrote
                    # it twice (found live, 2026-10-16: "D20D20" in Excel's Name Box).
                    if not NOT_SET.search(str(exc)):
                        raise ComputerError(f"{exc} - the text may already be in the field: observe it before typing "
                                            "again") from None
            typed = cdp.type_text(element, text, replace=not append, check_later=later is not None)
            if typed is not False:
                if later is not None and callable(typed):
                    later.append(typed)
                return "cdp"
            return _type_with_keys(element, text, append)

        if element is not None:
            _focus_window(element)
            center = el.center(element)
            # Inside a document (a Writer table cell, a paragraph) focus is
            # not the caret: the caret is placed through the Text interface,
            # and a click at the element's centre is only the fallback.
            in_document = element["role"] in ("cell", "paragraph", "heading", "document")
            if element["role"] == "cell" and DRAWN_TABLES.search(element.get("window_title") or "") and center:
                # A table on a slide is a shape: a caret placed in its cell edits it for one key, and a click selects
                # the table, not the cell - found live (2026-10-15), "Region" became "R", the next value landed in
                # another cell, and a clicked cell took "Revenue" as "Rvenuee". A click, then F2 (edit the cell's
                # text), then the keys, is what a person does.
                xinput.click(*center)
                time.sleep(0.3)
                xinput.key("F2")
                time.sleep(0.3)
                if not append:
                    xinput.key("ctrl+a")
                xinput.type_text(text)
                # Not read back and retyped here: in a table, Ctrl+A selects past the cell, and a retype after it put
                # one cell's value across others (found live, 2026-10-16).
                return "xdotool:drawn-cell"
            if in_document and PLACEHOLDER_PROMPT.match(element.get("name") or "") and center:
                # A presentation placeholder's words are its prompt, not its text: a caret placed in them adds to
                # "Click to add Title" (found live, 2026-10-15, building a deck in Impress). A click on it is what
                # opens it for editing, empty.
                xinput.click(*center)
                time.sleep(0.3)
            elif in_document:
                if not atspi.place_caret(element) and center:
                    xinput.click(*center)
            elif not atspi.grab_focus(element) and center:
                xinput.click(*center)
            handle = element["handle"]
            single_line = handle.get("single_line") or handle.get("raw_role") in (
                "entry", "password text", "spin button", "combo box",
            )
            # Replacing means select-all first - which in a document would
            # select the whole document, so it is only done in a field.
            if not append and single_line and not in_document:
                xinput.key("ctrl+a")
        elif point is not None:
            xinput.click(*point)

        if element is not None and DRAWN_TABLES.search(element.get("window_title") or "") and len(text) > 1:
            # On a slide the first key starts editing the text, and the next ones arrive while that is still starting:
            # found live (2026-10-15), "Northwind Traders - prepared" came out "Nrthwind Traders - prepoared".
            xinput.type_text(text[0])
            time.sleep(0.6)
            xinput.type_text(text[1:])
            if element.get("source") != "cdp":
                _keep_slide_text(element, text, append)
            return "xdotool"
        xinput.type_text(text)
        return "xdotool"

    if kind == "key":
        if not keys:
            raise ComputerError("no key given")
        # All of them are read before the first is pressed: a list that fails half way (a word that is not a
        # key) would leave a field emptied by its ctrl+a and Backspace and nothing typed after.
        for combo in keys:
            xinput.normalize_combo(combo)
        if element is not None:
            _focus_window(element)
            if element["source"] != "cdp":
                atspi.grab_focus(element)
        if any(xinput.normalize_combo(c).lower() in ("up", "down", "page_up", "page_down", "prior", "next")
               for c in keys) and cdp.available():
            number = cdp.focused_number_field()
            if number:
                raise ComputerError(f"nothing was pressed: Up/Down in the number field {el._quote(number, 60)} steps "
                                    "its value. Set the value with gui type REF \"the number\"; to move on, press Tab.")
        if any(xinput.normalize_combo(c).lower() in ("return", "enter", "kp_enter") for c in keys) and cdp.available():
            # Enter in a form's line field submits the whole form (cdp.ENTER_SUBMITS): held back while the form still
            # has empty fields - nothing is pressed.
            held = cdp.focused_enter_would_submit()
            if held:
                raise cdp.EnterWouldSubmit(cdp.held_enter_message(held))
            # A form listed for checking and not yet confirmed is not sent with Enter either. Found live (2026-10-26):
            # after "Not pressed yet", Tab and Enter saved the record and the check never saw the confirmed click.
            pending = _form_check_pending()
            if pending:
                raise ComputerError("Not pressed - the form listed for checking is not confirmed yet: check its values "
                                    f"and send gui click {pending} --checked to save it; Enter does not stand in for "
                                    "the checked click.")
        for combo in keys:
            xinput.key(combo)
        return "xdotool"

    if kind == "scroll":
        direction = (direction or "down").lower()
        if element is not None and element["source"] == "cdp":
            cdp.scroll(element, direction, amount or 3)
            return "cdp"
        target = point or (el.center(element) if element else None)
        if target is None:
            width, height = screen.size()
            target = (width // 2, height // 2)
        if cdp.available() and cdp.number_field_at(*target):
            # The wheel over a number field changes its value, not the page (cdp.SET_NUMBER's note): the page is
            # scrolled from a point beside it instead.
            target = _beside_number_fields(target)
        if element is not None:
            _focus_window(element)
        xinput.scroll(target[0], target[1], direction, amount or 3)
        return "xdotool"

    if kind == "select":
        if element is None or not option:
            raise ComputerError("select needs a dropdown and the option to pick")
        if element.get("role") == "radio":
            return _choose_radio(element, option)
        if element.get("role") in ("spinbutton", "spin button") and re.fullmatch(r"\s*-?[\d.,]+\s*", option):
            # A number box has no options to pick from: choosing a number in it is typing it (found live
            # (2026-10-15): Impress's Insert Table "Number of rows" answered `select ... "5"` with "no option '5'").
            return _perform("type", element, None, option.strip(), None, None, None, None, False, None)
        if element["source"] == "cdp":
            _resolved.clear()
            _selected.update(name=None, why="", by_id=False, offered=None)
            if ids:
                chosen = _select_native_by_id(element, option, ids)
                if chosen:
                    return chosen
            try:
                label = cdp.select_option(element, option)
                if " ".join(str(label or "").split()).casefold() != " ".join(option.split()).casefold():
                    # Not the text asked for: the summary says which option was taken, so the engine and the check
                    # can object.
                    _selected.update(name=label, why="the one option of the list that fits it")
                try:
                    # What the list offered, for the ledger's check of whether anything given picks this option out
                    # among them (records.option_backed). Not read: the check takes it as unknown.
                    _selected["offered"] = cdp.native_options(element)
                except (ComputerError, KeyError, TypeError):
                    pass
                return "cdp"
            except ComputerError as exc:
                if "is not a dropdown" not in str(exc):
                    if _resolved:
                        raise ComputerError(f"{exc}\n" + "\n".join(_resolved)) from exc
                    raise
            if element.get("source") == "cdp":
                chosen = _select_datalist(element, option, ids)
                if chosen:
                    return chosen
            if element.get("role") == "combobox" and _relayed(element, _load_state()):
                # In a viewer of a Windows app the app's own list is asked first: what a page-style search sees
                # appear there is the app's other controls (found live: Excel's Sort dialog "Sort by" was answered
                # with the ribbon's "More Options" entries).
                _selected.update(name=cdp.choose_in_app(element, option), why="the app's list chose it")
                return "relayed:chosen"
            try:
                _select_custom(element, option, ids=ids)
            except ComputerError as exc:
                if "showed no options" not in str(exc) or not _relayed(element, _load_state()):
                    raise
                # A Windows app's drop-down list (Save As's "Save as type") is a window of its own whose entries the
                # viewer is not given: found live (2026-10-15) in real Excel, the list opened and no entry could be
                # chosen, and the worker asked the person to set the file type. Keys and clicks posted to a dialog do
                # not move its focus (typed words went to its folder pane, an Enter closed it), so the app's own
                # accessibility selects the entry.
                forget_failed_search()
                _selected.update(name=cdp.choose_in_app(element, option), why="the app's list chose it")
                return "relayed:chosen"
            return "cdp:custom"
        atspi.select_option(element, option)
        return "atspi"

    if kind == "drag":
        start = point or (el.center(element) if element else None)
        if start is None or not end:
            raise ComputerError("drag needs where to start (an element or a point) and a point to drop it at")
        if element is not None:
            _focus_window(element)
        xinput.drag(start[0], start[1], end[0], end[1])
        return "xdotool"

    if kind == "upload":
        if element is None or not files:
            raise ComputerError("upload needs a target element and one or more file paths")
        if element.get("source") == "cdp":
            # Broker mode: the browser is the screen user's and may not open the worker's files; it is given a copy
            # of each one the caller handed over (broker.local_copy), under its own name.
            from decurix_agent.computer import broker
            try:
                files = [broker.local_copy(f) for f in files]
            except broker.NotHanded as exc:
                raise ComputerError(f"nothing was uploaded: {exc}") from None
            cdp.set_file_input_files(element, files)
            return "cdp"
        raise ComputerError("upload is currently supported for browser elements")

    raise ComputerError(f"unknown action {kind!r}")


# How long to give a page that reset a field before typing into it again.
RETYPE_PAUSE = 1.0

# Typing aimed at no field whose result changed nothing on the screen did not land: no text field, cell or box had the
# keyboard. A short text can change too few pixels to show, so the note is only for texts of this many characters.
NOTHING_TYPED_MIN = 3
NOTHING_TYPED_NOTE = ("NOTHING WAS TYPED: no text field, table cell or box had the keyboard, so the text went nowhere. "
                      "Click into the cell/box/field you mean (or give its ref), then type, and look to see the text "
                      "show before going on.")


def _same_field(element, observation):
    """The field typed into, as a fresh observation has it (found by its path), or None."""

    if element.get("source") != "cdp" or not element.get("path"):
        return None
    try:
        again = el.find(observation["elements"], path=element["path"])
    except ComputerError:
        return None
    if again is None or again.get("role") != element.get("role"):
        return None
    return again


def _relayed(element, observation):
    """
    Whether an element belongs to a viewer of an app running elsewhere. What was typed into such a field reaches the
    real app at once, but the field reports it only after the next read of that app - found live (2026-10-15): in
    real Excel, every cell typed into looked "cleared", was typed a second time ("TotalTotal") and the sheet was
    corrupted.
    """

    if not element or element.get("source") != "cdp":
        return False
    for group in (observation or {}).get("groups") or []:
        if group.get("id") == element.get("group"):
            return any(v in (group.get("url") or "") for v in RELAYED_VIEWERS)
    return _relayed_page(observation)


def _typing_lost(field, text):
    """Whether a field no longer holds what was just typed into it. A password field shows no text, so it is never
    judged; nor is a field the page does not report a value for."""

    if field.get("role") in ("password", "password text") or privacy.secret_field(field):
        return False
    # A date field shows its date its own way ("10/06/2026" for "6 Oct 2026"); cdp.type_text checked it held.
    if set(field.get("states") or ()) & {"date", "date and time", "time", "month", "week"}:
        return False
    value = field.get("value")
    line = (text.split("\n")[0]).strip()
    if value is None or not line or line in value:
        return False
    # Numeric form fields commonly add grouping and decimal places after typing.
    # Treat that formatting as the same value, without hiding genuinely lost input.
    import decimal
    numeric = r"^[+-]?(?:\d+(?:,\d{3})*)(?:\.\d+)?$"
    wanted, shown = line.strip(), str(value).strip()
    if re.fullmatch(numeric, wanted) and re.fullmatch(numeric, shown):
        return decimal.Decimal(wanted.replace(",", "")) != decimal.Decimal(shown.replace(",", ""))
    return True


# What the browser says when the node an action was aimed at is no longer the one on the page.
STALE_NODE = ("is detached from document", "does not have a layout object", "no node with given id",
              "could not find node with given id")


def _redrawn(exc, element, ref, role, name, path):
    """
    The same control again, read afresh, when an action failed because the page drew it anew between the look and the
    act - or None. A page built by a framework re-draws a field the moment it is touched: the web app's Supplier box, live,
    failed `gui select` with "Node is detached from document" and the engine spent turns finding its way back. Only the
    same control counts (found by the same ref, role, name or path, and still the same role and name), so an approval
    judged for one control can never pass to another.
    """

    if element is None or element.get("source") != "cdp" or not (ref or role or name or path):
        return None, None
    if not any(s in str(exc).lower() for s in STALE_NODE):
        return None, None
    fresh = observe()
    try:
        again = el.find(fresh["elements"], ref=ref, role=role, name=name, path=path, active_group=active_group(fresh),
                        gone=fresh.get("gone"))
    except ComputerError:
        return None, None
    # ...and in the same window: the same tab, the same title (an approval judged for one document's control never
    # passes to another's).
    if again is None or again.get("role") != element.get("role") or again.get("name") != element.get("name") \
            or again.get("group") != element.get("group") or again.get("window_title") != element.get("window_title"):
        return None, None
    return again, fresh


def _changed_under(before, element, fresh, again):
    """
    Why the page a redrawn control sits on is not the one the action was judged on, or "": its address, the filled
    fields shown to the person (_what_it_acts_on) or every field the press sends (_what_it_sends) differ. Found in
    review (2026-11-03, D-06): the retry compared the control's role, name and window only, so a same-titled tab
    showing another record or amount was pressed under a grant made for the first. Unreadable is changed.
    """

    try:
        if (_context(fresh).get("url") or "") != (_context(before).get("url") or ""):
            return "its address changed"
        if _what_it_acts_on(fresh, again) != _what_it_acts_on(before, element):
            return "what is filled in beside it changed"
        if _what_it_sends(fresh, again) != _what_it_sends(before, element):
            return "what it would send changed"
    except Exception as exc:  # noqa: BLE001 - what the page holds now cannot be read: it is not known to be the same
        return f"what the page holds now could not be read ({type(exc).__name__})"
    return ""


# Controls whose press does something or chooses something. An engine that
# presses one says which, by its ref or name - see `named` in act().
PRESSED_ROLES = ("button", "link", "option", "menuitem", "listitem", "treeitem", "tab", "checkbox", "radio",
                 "switch", "toggle")


FILLED_ROLES = ("textbox", "searchbox", "combobox", "spinbutton")


def _what_it_acts_on(observation, element):
    """
    The filled fields in the same window as an action the gate stopped, for
    the person to see what it would send and where: "Send" alone said nothing
    of which chat the message was in. Found live (2026-10-16): a message was
    approved and sent to the wrong person, whose name was on the message box
    the whole time. Secret fields are never shown.
    """

    group = (element or {}).get("group") or active_group(observation)
    shown = []
    for e in observation.get("elements") or []:
        if e.get("role") not in FILLED_ROLES or (group and e.get("group") != group):
            continue
        value = " ".join((e.get("value") or "").split())
        if not value or privacy.secret_field(e) or privacy.credential_in(value):
            continue
        # A message about to be sent is read whole: cut at 80 characters, the person asked to approve it was shown
        # the first few words and refused until the worker pasted it into a question (2026-10-25).
        shown.append(f"{e['role']} {el._quote(e.get('name') or '', 60)} holds {el._quote(value, ACTS_ON_CHARS)}")
    # Every filled field is shown up to a length (the grant is bound to all of them, _what_it_sends): an amount in the
    # fourth field is as much what the person approves as the first three.
    out, used = [], 0
    for line in shown:
        if out and used + len(line) > ACTS_ON_TOTAL:
            out.append(f"and {len(shown) - len(out)} more filled field(s)")
            break
        out.append(line)
        used += len(line)
    return ("It would act on what is filled in: " + "; ".join(out)) if out else ""


# How much of a filled field's value an approval quotes (_what_it_acts_on), and of all of them together.
ACTS_ON_CHARS = 600
ACTS_ON_TOTAL = 3000

CHECKED_ROLES = ("checkbox", "radio", "switch")


def _what_it_sends(observation, element, enter_form=None):
    """
    Every filled field an action the gate stopped would send, for its grant (approval.sent_digest): each filled field
    of its window, a checked box among them, and the fields of the <form> a click or an Enter submits as the page holds
    them. A secret field is in it as filled, never by its value. Found in review (2026-11-02): the grant held the first
    three fields shown, so an amount in a later one could change under a yes.
    """

    group = (element or {}).get("group") or active_group(observation)
    out = []
    for e in (observation or {}).get("elements") or []:
        if group and e.get("group") != group:
            continue
        role = e.get("role")
        if role in FILLED_ROLES:
            value = " ".join(str(e.get("value") or "").split())
            if value:
                secret = privacy.secret_field(e) or privacy.credential_in(value)
                out.append([e.get("name") or role, "" if secret else value, bool(secret)])
        elif role in CHECKED_ROLES and "checked" in (e.get("states") or []):
            out.append([e.get("name") or role, "checked"])
    form = enter_form
    if form is None and element is not None and element.get("source") == "cdp" and element.get("handle"):
        form = cdp.submit_form_fields(element, min_fields=0)
    for label, value in (form or {}).get("fields") or []:
        out.append(["form " + str(label), value])
    return out


def _enter_submits(kind, element, keys, text, before):
    """
    The submit control an Enter in this action presses, as an element ({"role": "button", "name": its label, "form":
    what the form holds}), when the keys (or a typed new line) go to a page's single-line field whose <form> has one;
    else None. Enter there is a press on that control (cdp.FORM_FIELDS), and is put to the gate as one: found in review
    (2026-11-02), Enter in a form whose submit was "Pay" went past the gate, which looks at labels it is pressed on.
    """

    if kind == "key":
        if not _commit_keys(keys)[0]:
            return None
    elif not (kind == "type" and text and "\n" in text):
        return None
    field = element if element is not None else _keyboard_field(before)
    if not field or field.get("source") != "cdp" or not field.get("handle"):
        return None
    form = cdp.enter_form_fields(field)
    if not form:
        return None
    return {"role": "button", "name": form.get("submit") or "", "source": "cdp", "path": field.get("path") or "",
            "group": field.get("group"), "window_title": field.get("window_title"), "form": form}


SPACE_KEY = re.compile(r"(^|\+)(space|spacebar)$", re.IGNORECASE)


def _focused_control(before):
    """The button, link or menu entry that has the keyboard, as an element, or None: Return or Space on it presses it
    (review 2026-11-03: Tab to a "Pay" button and Return went past the gate, which looked at clicks only)."""

    found = next((e for e in before.get("elements") or [] if "focused" in (e.get("states") or [])
                  and e.get("role") in approval.ACTIVATING_ROLES), None)
    if found is not None:
        return found
    if browser.is_browser_title(before.get("active_title")) and cdp.available():
        try:
            field = cdp.focused(before.get("elements") or [])[0]
        except ComputerError:
            return None
        if field is not None and field.get("role") in approval.ACTIVATING_ROLES:
            return field
    return None


def _pressed_by_key(kind, element, keys, before, focus=None):
    """The control a Return or Space presses, when the keys go to a focused button/link (or are aimed at one); else None.
    A press after a key that may move the keyboard is not this one's: approval.assess asks for it (`focus`: the field
    that has the keyboard, when known)."""

    if kind != "key" or not keys:
        return None
    first = next((i for i, k in enumerate(keys) if approval.ENTER_KEY.search(k.strip()) or SPACE_KEY.search(k.strip())),
                 None)
    if first is None or approval.press_after_focus_move(keys[:first + 1], element or focus):
        return None
    if element is not None:
        return element if element.get("role") in approval.ACTIVATING_ROLES else None
    return _focused_control(before)


def _unresolved_press(kind, element, point, before):
    """
    (element, why) for a click at a point that resolved to no element: the control the page itself says is there
    (cdp.hit), else the line of text under it (OCR) taken as the label of a button; else (None, why) - a press on a spot
    nothing could name is put to the person, never let through (review 2026-11-03).
    """

    if kind not in ("click", "double_click") or element is not None or point is None:
        return None, None
    answered = False
    if before.get("page_url") and cdp.available():
        try:
            found, context = cdp.hit(point[0], point[1], before.get("elements") or [], controls_only=True)
            answered = context is not None
        except Exception:  # noqa: BLE001 - the page did not answer: look at the picture
            found = None
        if found is not None:
            return found, None
    try:
        from decurix_agent.computer import ocr
        line = ocr.line_at(ocr.read_frame(before.get("frame")), point) if ocr.available() else None
    except Exception:  # noqa: BLE001
        line = None
    if line and str(line.get("text") or "").strip():
        return {"role": "button", "name": line["text"].strip(), "window_title": before.get("active_title")}, None
    if answered:
        return None, None  # the page says no control is there: a click on plain page, nothing to name
    return None, (f"this presses the spot {point[0]},{point[1]}, which nothing on the screen names - a press there may "
                  "pay, send or file")


def _refuse_files_that_may_not_leave(files):
    """Raise when a file an upload would hand a page may not leave this desktop (privacy.file_may_leave: the worker's
    own state, a hidden folder, anything outside the work, shared and download folders). Found in review (2026-11-03,
    D-08): an upload sent any file on the desktop to any page; only the outbox and handoff asked this."""

    if not files:
        return files
    if isinstance(files, (str, bytes)):
        files = [files]
    # As the upload reads them (cdp.set_file_input_files): from this process's folder when not absolute.
    named = [os.path.abspath(os.path.expanduser(str(f))) for f in files]
    for f in named:
        _, why = privacy.file_may_leave(f)
        if why:
            raise ComputerError(f"{f} is not uploaded: {why}. Nothing was sent.")
    return named


@screen_call.routed("core.act")
def act(kind, ref=None, role=None, name=None, path=None, point=None,
        text=None, keys=None, direction=None, amount=None, option=None, append=False, end=None, files=None,
        see=None, replace_all=False, named=False, keys_from_task=False, cell=None, check_form=False, ids=None):
    """
    Do one thing on the screen and report what changed. `end` is the point a
    drag drops at. `see` describes the target in words for the grounding model
    to find on the screen (grounder.py), when nothing in the element list is it.
    `ids` are the identifiers (a tax number, an account number, an email) a
    select looks for first: an entry holding one is the record, whatever its name.

    Returns a dict: status ("done" | "needs_approval"), text (one
    paragraph for the engine), element, change, observation (the "after"
    observation, or the "before" one when nothing ran).
    """

    # One field per typing action. A Tab inside the text moves focus to the next
    # field, and what lands there is recorded - and withheld or not - as if it
    # were typed into this one: live, "Admin\tadmin123\n" put the password into
    # the username's step in plain text. Typing each field on its own lets the
    # password field be recognised as one.

    # A withheld mark is never typed: it is where a value was kept out of a copy (a resumed run's request, a note),
    # not the value. Into a password field it is a wrong password; into any other it is saved into the person's
    # record (review round 4).
    if text is not None and ("[withheld" in str(text) or "[held back" in str(text)):
        raise ComputerError("Not typed: the text holds a withheld mark ([withheld]), not the value it stands for. "
                            "Ask the person for this value (HELP:), and type what they give.")

    before = observe()

    element = None
    grounded = None
    if see:
        # A description that is simply an element's name ("cell B6", "Save") is that element: exact, and
        # free. Only what the list does not have is looked for in the picture.
        element = _named_by(see, before)
        if element is None:
            point = grounder.locate(before["frame"], see)
            grounded = see
    elif ref or role or name or path:
        try:
            element = el.find(before["elements"], ref=ref, role=role, name=name, path=path,
                              active_group=active_group(before), gone=before.get("gone"))
        except ComputerError as exc:
            # A control the element list does not have (an icon drawn on a canvas, a page whose tree is
            # empty, a toolkit with no accessibility) is looked for by eye, when a grounding model is set
            # up - only for a click by name, and only when nothing matched at all: two matches are a
            # choice for the engine, not for a picture.
            wanted = " ".join(p for p in (role, name) if p)
            if not (name and not ref and not path and kind in GROUNDED_KINDS
                    and str(exc).startswith("nothing on the screen matches") and grounder.available()):
                raise
            point = grounder.locate(before["frame"], wanted)
            grounded = wanted
    # A cell named by its coordinate ("gui type D1 ..."): in a sheet this desktop reads, that cell's element; in a
    # viewer of a spreadsheet running elsewhere, the cursor is put there by the app's Name Box before typing. Found
    # live (2026-10-19): `gui type D1 - <<'EOF'` with a block in real Excel was refused as "one field at a time".
    relayed_cell = None
    rows_entered = None  # how many rows of a block typed into an app far away were entered (the time budget may stop it)
    if cell:
        if not CELL_NAME.match(cell.strip()):
            raise ComputerError(f"{cell!r} is not a cell's coordinate (like D1)")
        element = _sheet_cell_named(before, cell.strip().replace("$", ""))
        if element is None:
            if _relayed_page(before) and _relayed_cells(before):
                relayed_cell = cell.strip().upper().replace("$", "")
            else:
                raise ComputerError(f"{cell} names a cell, but the window in front shows no spreadsheet whose cells "
                                    "can be read - bring the spreadsheet forward (gui front), or type by a ref")
    aimed_by_point = point is not None and element is None and not grounded
    if point is not None and element is None:
        element = el.at_point(before["elements"], point[0], point[1], before.get("windows"), before.get("popups"))

    # What looking found must be what the description named. Found live (2026-10-16): asked for "Recommended Charts in
    # the Insert ribbon", the grounding model put the point on the button beside it, "Recommended PivotTables", and
    # that was pressed - a dialog the worker never meant to open, and the task lost its way from there.
    if grounded and kind in ("click", "double_click", "right_click") and element is not None \
            and element.get("role") in PRESSED_ROLES and not _inside_large(element, point):
        wanted = _names_another(grounded, element.get("name") or "")
        if wanted:
            raise ComputerError(
                f"looking at the screen for {el._quote(grounded, 80)} found {el.describe(element)} at "
                f"{point[0]},{point[1]} - that is not \"{wanted}\", so nothing was clicked. If \"{wanted}\" is not in "
                "your last look, gui observe again (a ribbon, menu or panel lists its controls once it is shown) and "
                "press it by its ref.")

    # An engine's click on a control that has a name is made by that name
    # (`named`: the gui command and the MCP tools ask for it; a replay and a
    # person's own actions do not). A point says where, not what: found live,
    # an engine choosing from a list of suggestions clicked where it expected
    # an entry to be - and took another record once, the list's "create a new
    # one" line once, and a button it had taken for empty space once. Told
    # what is at the point, it clicks that by its ref, or sees it is not what
    # it meant. Fields, cells and bare points are still clicked where they are.
    if named and aimed_by_point and kind in ("click", "double_click", "right_click") and element is not None \
            and element.get("role") in PRESSED_ROLES and (element.get("name") or "").strip() and element.get("ref") \
            and not _inside_large(element, point):
        raise ComputerError(
            f"{point[0]},{point[1]} is {el.describe(element)} - nothing was clicked. A click by coordinates does "
            f"not say what you mean to press. If this is it, press it by its ref: gui {kind.replace('_', '-')} "
            f"{element['ref']}. If it is not, observe and choose the control you mean by its ref or name.")

    # An entry clicked in a list, after a search of that list found nothing for what was wanted, must not stand
    # in for it (see "Nothing else in its place").
    if named and kind == "click" and element is not None and element.get("role") == "option" \
            and not _NOT_A_CHOICE.match(element.get("name") or ""):
        replaced = _in_place_of(None, element.get("name") or "")
        if replaced:
            raise ComputerError(SUBSTITUTE.format(field="The list", wanted=replaced, entry=element.get("name")))

    # ...and neither may one typed into the field itself. Found live: a bill's line had no item in the
    # application; selecting another item in its place was refused, clicking one in the list was refused -
    # and the engine typed another item's name into the field and pressed Tab, which a field that looks
    # records up takes as a choice. The draft was saved with a database-hosting item on a server's line.
    # What is typed there again must still be the thing that was searched for.
    if named and kind == "type":
        replaced = _typed_in_place_of(element, text)
        if replaced:
            raise ComputerError(SUBSTITUTE.format(field=el.describe(element), wanted=replaced, entry=text))

    # In a viewer of an app running elsewhere (a Windows app read from its picture, a phone's app) a point IS where the
    # app's field is: the click reaches the app and the keys follow it. Found live (2026-10-15): refused here, a worker
    # filling TallyPrime spelled "Northwind Traders" out one key at a time and made a company named "N".
    if kind == "type" and point is not None and element is None and before.get("page_url") \
            and not _relayed_page(before):
        raise ComputerError("that point does not resolve to a browser field to type into - click the editor, "
                            "observe, then type into its field by ref; typing at an unresolved point can trigger "
                            "page shortcuts instead of entering text")

    # Keys sent to no element go where the keyboard is (see "Windows a task found open").
    untargeted = element is None and point is None

    # Rows of cells for a spreadsheet's grid: entered row by row and read back (see "A block of cells" above). With no
    # target, the block starts at the focused cell of the sheet in the active window.
    block = _sheet_start(element, before) if kind == "type" and _is_block(text) and point is None else None
    if block is not None and element is None:
        element = block
    # ...and in a viewer of a spreadsheet running elsewhere, typed with no target where the app's cursor is.
    relayed_block = (kind == "type" and block is None and untargeted and text is not None
                     and (relayed_cell is not None
                          or (_is_block(text) and _relayed_page(before) and bool(_relayed_cells(before)))))

    # One field per typing action: a Tab carries the rest into another field, and what lands there is recorded - and
    # withheld or not - as if typed into this one (live, "Admin\tadmin123\n" put a password into the username's step).
    # A table on a slide is the exception: Tab is how its cells are filled one after another, and none of them is a
    # password field (found live, 2026-10-16: filled cell by cell, the cells' refs moved as they changed and the values
    # scattered).
    if kind == "type" and block is None and not relayed_block and text and "\t" in text and not (
            (element is not None and element.get("role") == "cell"
             and DRAWN_TABLES.search(element.get("window_title") or ""))
            or (element is None and point is None and DRAWN_TABLES.search(before.get("active_title") or ""))):
        raise ComputerError("type one field at a time: a Tab in the text would carry the rest into another field. "
                            "Type into this field, then type the next value into its own field (by its ref). (Rows "
                            "of a spreadsheet's grid are typed in one go into a cell: gui type CELLREF "
                            "\"a\\tb\\nc\\td\" - the cell a sheet lists as cell \"A1\".)")
    # In a slide's table a line break is a second line inside the same cell, not the next row: a table typed with Tabs
    # between cells and line breaks between rows came out with every row crammed into the first cells (found live).
    if kind == "type" and block is None and not relayed_block and text and "\t" in text and "\n" in text:
        raise ComputerError("a table on a slide is filled with a Tab between every two cells, the end of a row "
                            "included: a line break would put the next row inside this cell. Nothing was typed - type "
                            "it again with Tabs only (\"Region\\tRevenue\\tNorth\\t812\").")

    keys = [k for k in (keys or []) if k and k.strip()]
    target = el.describe(element) if element else (f"{point[0]},{point[1]}" if point else "")

    # Typing replaces a field's text - and a field holding several lines is a document. Found by the GUI benchmark:
    # asked to replace a word in Mousepad, the engine ran `gui type e13 "color"` on the editor, the whole file became
    # "color", and an undo brought back only part of it. Replacing a whole document has to be said outright.
    if kind == "type" and element is not None and not append and not replace_all:
        held = element.get("value") or ""
        if "\n" in held.strip() and not privacy.secret_field(element):
            lines = held.strip().count("\n") + 1
            raise ComputerError(
                f"{target} holds {lines} lines of text, and typing into it replaces all of them; nothing was typed. "
                f"To change part of it use the app's own Find and Replace, or click where the change goes and "
                f"type with --append. gui read {element.get('ref') or 'REF'} shows the whole text. Only to replace "
                f"everything: gui type {element.get('ref') or 'REF'} --replace-all \"...\"")

    # Typing needs a field. Aimed at a page's row, button or link, the text goes
    # to whatever still has focus: live, a product name typed "into" an the web app
    # order row at a coordinate landed in the previous line's Unit Price.
    if kind == "type" and element is not None and element["role"] in NOT_A_FIELD and element.get("source") == "cdp":
        raise ComputerError(f"{el.describe(element)} is not a field to type into - click the cell or control that "
                            "opens a field, observe, then type into that field by its ref")

    # Typing with no target at all (no ref/role/name/path/point) goes to
    # whatever already has focus, and that field is the one whose secrecy
    # decides whether the text may be kept - before is accurate here because
    # nothing will change focus before typing. But when a point was given
    # and simply didn't resolve to a tracked element, _perform still clicks
    # there before typing, so the field that ends up focused is unknown
    # until after that click - substituting before's already-focused field
    # would describe the wrong element's secrecy. Leave field unresolved in
    # that case; privacy.withhold_text(None, ...) still catches text that is
    # itself shaped like a credential.
    field = element
    if kind == "type" and field is None and point is None:
        field = next((e for e in before["elements"] if "focused" in e.get("states", [])), None)
    kept, withheld = privacy.withhold_text(field, text) if kind == "type" else (None, None)
    if withheld and text:
        # What went into a password or code field is a secret from here on: kept out of every reply and step too.
        privacy.remember_value(text)
    described = approval.describe_action(kind, element, text, keys, point, withheld=withheld)

    entry = {
        "kind": kind,
        "target": target,
        "element": privacy.public_element(element),
        "point": list(point) if point else None,
        "end": list(end) if end else None,
        "text": kept,
        "append": bool(append) if kind == "type" else None,
        "files": [str(f) for f in files] if files else None,
        "keys": keys,
        "option": option,
        "direction": (direction or "down") if kind == "scroll" else None,
        "amount": amount if kind == "scroll" else None,
        "window": privacy.kept_text(before.get("active_title")),
        "url": privacy.reduce_url(_context(before).get("url")),
        "screen": before.get("size"),
    }

    if withheld:
        entry["withheld"] = {"text": withheld}

    masks = privacy.secret_boxes(before["elements"])

    context = _context(before)
    if kind == "key" and element is None:
        context["focused"] = _keyboard_field(before)
    # The dialogs open on the screen, for a yes-button whose own path does not name its dialog (approval.py).
    context["dialogs"] = [str(e.get("name") or "") for e in before.get("elements") or []
                          if str(e.get("role") or "").lower() in ("dialog", "alertdialog") and e.get("name")]
    if kind == "key" and approval.press_after_focus_move(keys, element or context.get("focused")):
        # Keys that move the keyboard and then press: any control on the screen may be the one pressed (approval.assess).
        context["controls"] = [{"role": e.get("role"), "name": e.get("name"), "path": e.get("path") or ""}
                               for e in before.get("elements") or []
                               if e.get("role") in approval.PRESS_ROLES and str(e.get("name") or "").strip()]

    def refused(guard, reason):
        # A guard's refusal is an event of the task like any action (outcome "refused", the guard and its reason),
        # withheld like every store, so the run's record and its bundle show which guard stopped what, and why
        # (W0.3 probe, 2026-11-05: four refused saves left no trace in the task's events).
        entry.update({"outcome": "refused", "guard": guard, "reason": str(reason),
                      "summary": f"{described} - refused ({guard}): {reason}"})
        try:
            audit.record(entry, before=before["frame"], masks=masks, observation=before)
        except Exception as exc:  # noqa: BLE001 - the refusal stands whether or not it could be recorded
            from decurix_agent.logging_utils import log
            log(f"Could not record a refused action: {exc}")
        raise ComputerError(str(reason))

    try:
        _refuse_number_arrows(kind, point)
        _refuse_unsafe_save(kind, element, keys, before)
        files = _refuse_files_that_may_not_leave(files)
    except ComputerError as exc:
        refused("action_rules", exc)
    # Only an action in the browser can download (_may_download): any other is not followed by a wait for one.
    downloads_before = _downloads_snapshot() if kind in ("click", "double_click", "key") \
        and _may_download(kind, element, point, before) else None
    acted_at = time.time()
    require = None
    if check_form:
        # Every committing action an engine or a replay takes, however it is pressed (_commit_check). A replay's
        # (check_form="replay") listing of today's values is put to the person instead (AskFirst).
        pressed = []
        held = _commit_check(kind, element, keys, before, checked=check_form == "checked", text=text,
                             listing="ask" if check_form == "replay" else True, pressed=pressed)
        if isinstance(held, AskFirst):
            require = str(held)
        elif held:
            refused("pre_save", held)
        # A press the plan of the work told the person is made with their OK first is put to them, as the gate puts
        # its own (_plan_hold).
        promised = _plan_hold(pressed[0] if pressed else None)
        if promised:
            require = f"{promised}; {require}" if require else promised

    # Enter that submits a form is a press on its submit control, and is asked for when that press would be
    # (_enter_submits).
    by_enter = _enter_submits(kind, element, keys, text, before)
    if by_enter is not None:
        why = approval.assess("click", by_enter, context=context)
        if why:
            label = " ".join(str(by_enter.get("name") or "").split())[:60]
            why = f'Enter here submits the form with its "{label}" control - {why}'
            require = f"{why}; {require}" if require else why

    # Return or Space on a focused button presses it; a click at a spot that resolved to no element presses whatever is
    # drawn there. Both are put to the gate as the press they are.
    keyed = _pressed_by_key(kind, element, keys, before, focus=context.get("focused")) if by_enter is None else None
    if keyed is not None:
        why = approval.assess("click", keyed, context=context)
        if why:
            label = " ".join(str(keyed.get("name") or "").split())[:60]
            why = f'{" ".join(keys)} on "{label}" presses it - {why}'
            require = f"{why}; {require}" if require else why
    if grounded and kind in ("click", "double_click") and element is None:
        # Found by eye for a description: the description is what it presses.
        spot, spot_why = {"role": "button", "name": grounded, "window_title": before.get("active_title")}, None
    else:
        spot, spot_why = _unresolved_press(kind, element, point, before)
    if spot is not None:
        why = approval.assess("click", spot, context=context)
        if why:
            require = f"{why}; {require}" if require else why
    elif spot_why:
        require = f"{spot_why}; {require}" if require else spot_why
    decision = approval.check(kind, element, text=text, keys=keys, point=point, context=context,
                              about=_what_it_acts_on(before, element), require=require,
                              sends=lambda: _what_it_sends(before, element if by_enter is None else by_enter,
                                                           enter_form=(by_enter or {}).get("form")))
    if decision.allowed and check_form == "checked":
        try:
            _form_check_file().unlink()  # the checked form is sent now (_form_check)
        except OSError:
            pass

    if not decision.allowed and not decision.request:
        # Refused without asking anyone (the gate could not check its words for secrets): a no to this one action,
        # said to the engine with the gate's reason - nothing waits on a person (review round 3, R2-B3).
        entry.update({"outcome": "refused", "reason": decision.reason,
                      "summary": f"{described} - refused by the approval gate: {decision.reason}"})
        audit.record(entry, before=before["frame"], masks=masks, observation=before)
        raise ComputerError(f"Not done: {decision.reason or 'the approval gate refused this action'}.")

    if not decision.allowed:
        text = approval.request_text(decision.request)
        entry.update({
            "outcome": "needs_approval",
            "reason": decision.reason,
            "summary": f"{described} - paused for approval: {decision.reason}",
        })
        audit.record(entry, before=before["frame"], masks=masks, observation=before)
        return {
            "status": "needs_approval",
            "text": "NEEDS_APPROVAL: " + text,
            "element": element,
            "change": None,
            "observation": before,
        }

    keys_home = None
    if element is None and point is None and kind in ("key", "type"):
        keys_home = _keys_home()
    if keys_from_task and untargeted and kind in ("key", "type") and _nothing_in_front(wait=FRONT_WAIT_SECONDS):
        # Found live (2026-10-19): Alt+F4 pressed with every window closed went to the desktop, which offered to log
        # out (and the next Alt+F4 went to that dialog). An app just opened may not have its window up yet: a few
        # seconds are waited first.
        raise ComputerError("no window is in front, so the keys would go to the desktop itself (Alt+F4 there asks to "
                            "log out) - nothing was pressed. If you just opened an app it is still starting: gui "
                            "observe again in a few seconds. Otherwise bring forward the window you mean (gui front, "
                            "gui open).")
    if keys_from_task and untargeted and kind in ("key", "type"):
        left = _left_from_before()
        if left:
            raise ComputerError(
                f"the keyboard is in {el._quote(left, 80)}, a window that was open before this task began and that "
                "this task has not worked in - nothing was typed or pressed. If it is the window you mean, bring it "
                "forward first (gui front \"a few words of its title\") or act on something in it by its ref; "
                "otherwise bring forward the window you are working in (gui open FILE brings an open document "
                "forward).")

    # Heard from the browser while the action runs: whether it began a download (_downloads_note).
    download_watch = _watch_downloads(element) if downloads_before is not None else None
    typing_later = []
    try:
        if relayed_block:
            if relayed_cell:
                rows_entered = _type_relayed_rows(before, relayed_cell, text)
            elif len(_sheet_rows(text)) > 1:
                # Rows typed where the cursor is depend on what is selected and on the app's Enter: refused, with the
                # form that is entered row by row from a named cell.
                raise ComputerError("several rows go into a sheet from a named cell, row by row - nothing was "
                                    "typed. Give the block's top-left cell: gui type D1 - <<'EOF' (the rows, real "
                                    "Tabs between cells) EOF")
            else:
                _type_relayed_block(text)
            backend = "relayed:block"
        else:
            try:
                backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end, files=files, ids=ids,
                                   later=typing_later)
            except ComputerError as exc:
                again, fresh = _redrawn(exc, element, ref, role, name, path)
                if again is None:
                    raise
                # The second press is judged like the first, on the page as it is now (D-06).
                changed = _changed_under(before, element, fresh, again)
                if changed:
                    raise ComputerError(f"the page changed under the action ({changed}) - nothing more was done. "
                                        "Observe and do it again.") from exc
                if check_form:
                    held = _commit_check(kind, again, keys, fresh, checked=check_form == "checked", text=text,
                                         listing="ask" if check_form == "replay" else True)
                    if held:
                        raise ComputerError(str(held)) from exc
                element = again
                backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end, files=files, ids=ids,
                                   later=typing_later)
    except ComputerError as exc:
        if download_watch is not None:
            download_watch.close()
        entry.update({
            "outcome": "failed",
            "reason": str(exc),
            "summary": f"{described} - failed: {exc}",
        })
        audit.record(entry, before=before["frame"], masks=masks, observation=before)
        raise
    except OSError as exc:
        if download_watch is not None:
            download_watch.close()
        # A connection that broke mid-action (a browser tab that crashed: BrokenPipeError on its DevTools socket) is a
        # failed action, recorded like any other - it may have half happened. Found in review (2026-11-02): it left
        # `gui` with exit 0 and no audit step.
        reason = (f"the connection to the app broke while acting ({type(exc).__name__}: {exc}) - it may or may not "
                  "have happened; observe before doing anything again")
        entry.update({"outcome": "failed", "reason": reason, "summary": f"{described} - failed: {reason}"})
        audit.record(entry, before=before["frame"], masks=masks, observation=before)
        raise ActedError(reason) from exc

    performed_at = time.time()
    settled = _wait_for_settle(before["frame"])
    if typing_later and not typing_later[0]():
        # The field threw the text away (cdp.type_text's read-back, made now that the screen has settled - a longer
        # wait than its own): typed again with real keys, as type_text's caller always did.
        backend = _type_with_keys(element, text, append)
        settled = _wait_for_settle(before["frame"])
    # The settled frame is the after-look's picture: the screen stopped changing on it, and a second full grab at once
    # is the same screen again (resource plan P1 #5: one full grab an action fewer).
    after = observe(frame=settled)
    _note_front()
    if not untargeted:
        _adopt_front()

    # A page still setting itself up re-draws its fields from what its form holds, and what was typed a moment before
    # (not yet committed - that happens when the field loses focus) is gone. Found live in the web app: typed into a new
    # invoice's date field within seconds of the page appearing, the same keys held in one run and were wiped within
    # half a second in the next, and a worker typed a bill's date four times without seeing it hold. Typed once more,
    # after a moment, the field keeps it.
    retyped = False
    relayed = _relayed(element, before)
    # Only a field is typed again: in a paragraph, a cell or a document a second typing adds a second copy (found
    # live: an Impress subtitle came out twice).
    # Never typed again when the text pressed Enter or Tab (a field that empties because Enter sent it looks like one the
    # page cleared, and the retype sent it twice), nor when a person approved this typing: one yes is one action (D-07).
    typed_keys = bool(text) and ("\n" in text or "\t" in text)
    if kind == "type" and element is not None and text and not append and not relayed and not typed_keys \
            and not getattr(decision, "approved", False) \
            and element.get("role") not in ("paragraph", "heading", "cell", "document"):
        again = _same_field(element, after)
        if again is not None and _typing_lost(again, text):
            time.sleep(RETYPE_PAUSE)
            _perform(kind, again, point, text, keys, direction, amount, option, append, end, files=files)
            settled = _wait_for_settle(before["frame"])
            after = observe(frame=settled)
            retyped = True

    if kind == "type" and element is not None and text and not append and element.get("role") == "textbox" \
            and not relayed and not typed_keys:
        retained = _same_field(element, after)
        if retained is not None and _typing_lost(retained, text):
            reason = (f"{target} did not retain the requested text after retrying. Observe the field again and "
                      "commit it or use its picker; do not assume the value was entered.")
            if download_watch is not None:
                download_watch.close()
            entry.update({"outcome": "failed", "backend": backend, "reason": reason,
                          "summary": described + " - failed: " + reason})
            audit.record(entry, before=before["frame"], after=after["frame"] or settled,
                         masks=masks + privacy.secret_boxes(after["elements"]), observation=before)
            raise ActedError(reason)

    block_note, unverified = "", []
    if block is not None and backend == "xdotool:sheet-block":
        rows = _sheet_rows(text)
        formulas, found = _read_back_block(element, text)
        block_note, unverified = _block_report(
            f"A block of {len(rows)} row(s) went into {_block_span(_block_cells(element, rows))}, each row from its "
            "first cell.", found, formulas,
            "these cells do NOT hold what was typed - correct each by its own gui type CELL: ")
    elif relayed_block:
        # The viewer's copy of the app catches up after the keys: read the cells back from a look that is not behind.
        for _ in range(RELAYED_READ_TRIES):
            if not after.get("behind"):
                break
            time.sleep(1.5)
            after = observe()
        rows = _sheet_rows(text)
        entered = len(rows) if rows_entered is None else rows_entered
        typed = "\n".join("\t".join(r) for r in rows[:entered])
        span, formulas, found = _read_back_relayed(_relayed_cells(before), _relayed_cells(after), typed,
                                                   start_cell=relayed_cell)
        if entered < len(rows):
            # The time budget stopped it (the operator would kill the command, and nothing would be read back).
            m = CELL_NAME.match(relayed_cell)
            next_cell = f"{m.group(1).upper()}{int(m.group(2)) + entered}"
            found.append(f"STOPPED after row {entered} of {len(rows)} (this takes too long to type through the "
                         f"viewer, a key at a time): the other {len(rows) - entered} row(s) were NOT typed - start "
                         f"again at {next_cell} with those rows; a column of one formula is typed once and filled "
                         "down (select the range with the Name Box, Ctrl+D) rather than row by row")
        block_note, unverified = _block_report(
            f"A block of {entered} of {len(rows)} row(s) was typed" + (f" ({span})" if span else "")
            + ", Tab between cells and Enter after each row.", found, formulas,
            "these cells do NOT hold what was typed - look at the sheet and correct them: ")

    change = compare(before, after)
    change_text = summarize(change)
    if _nothing_typed(kind, element, text, change_text, after) and not before.get("behind"):
        # Found live (2026-10-17): after PowerPoint's Insert Table the table is selected, not being edited, and four
        # typings in a row (with Tab between) went nowhere; each said only "nothing visible changed - check ...", and
        # the run ended with the table empty. Typing that changes nothing was not typed.
        change_text += " " + NOTHING_TYPED_NOTE

    entry.update({
        "outcome": "done",
        "backend": backend,
        "approved": decision.approved,
        "change": change_text,
        "expect": expectations(change),
        "summary": described + (" (approved)" if decision.approved else "") + f" - {change_text}",
    })
    if kind == "select" and backend == "cdp:custom" and _selected["name"]:
        # Which entry was taken is the fact of this action: the record of the task says so.
        entry["summary"] = (f"{described} - took the entry {el._quote(_selected['name'], 90)} for "
                            f"{el._quote(option or '', 90)} - {change_text}")
    if kind == "select" and _resolved:
        # What the identifiers resolved to is a fact of this action too (resolve.py).
        entry["summary"] += " - " + "; ".join(_resolved)
    if kind == "select" and backend in ("cdp", "cdp:custom", "relayed:chosen"):
        # The facts the ledger's pick check reads, as structure (review round 9, R9-B1: a plain pick's summary words
        # were read as an identifier's resolution, and the worker's guess backed itself): which entry was taken,
        # whether an identifier the documents give chose it, and what the list offered.
        if _selected["name"]:
            entry["taken"] = privacy.kept_text(" ".join(str(_selected["name"]).split())[:200])
        if _selected.get("by_id"):
            entry["resolved_by"] = "identifier"
        offered = _selected.get("offered")
        if isinstance(offered, list) and 0 < len(offered) <= MAX_OFFERED_KEPT:
            entry["offered"] = [" ".join(str(o).split())[:200] for o in offered]

    audit.record(entry, before=before["frame"], after=after["frame"] or settled,
                 masks=masks + privacy.secret_boxes(after["elements"]), observation=before)

    verb = {
        "click": "Clicked", "double_click": "Double-clicked", "right_click": "Right-clicked",
        "type": "Typed into", "key": "Pressed keys on", "scroll": "Scrolled",
        "select": "Selected in", "drag": "Dragged", "upload": "Uploaded to",
    }[kind]

    summary = f"{verb} {target or 'the focused window'}"
    if kind == "type":
        summary = f"Typed {privacy.shown_text(text, withheld)} into {target or 'the focused element'}"
        if not target:
            summary += _keys_went_to(before, after)
        if retyped:
            summary += " (the page cleared it a moment after the first typing, so it was typed again)"
    elif kind == "drag":
        summary = f"Dragged {target} to {end[0]},{end[1]}"
    elif kind == "key":
        summary = f"Pressed {' '.join(keys)}" + (f" on {target}" if target else "")
    elif kind == "select":
        summary = f"Selected {el._quote(option or '')} in {target}"
        if backend in ("cdp", "cdp:custom") and _selected["name"]:
            # Not the text asked for: say which entry it was and why, so the engine and the check can object.
            summary = (f"Selected the entry {el._quote(_selected['name'], 90)} in {target} for "
                       f"{el._quote(option or '', 90)} ({_selected['why']})")
        if backend == "relayed:chosen":
            summary = f"Selected the entry {el._quote(_selected['name'] or '', 90)} in {target}"
        if _resolved:
            # The resolution facts, as noted: the engine reads what each identifier resolved to.
            summary += " - " + "; ".join(_resolved)
    elif kind == "upload":
        files_str = ", ".join(os.path.basename(f) for f in (files or []))
        summary = f"Uploaded {files_str} to {target or 'the file input'}"
    if keys_home:
        summary += (f" (another window had taken the keyboard, so the keys went to \"{keys_home}\", where you "
                    "were working)")
    if decision.approved:
        summary += " (approved by the human)"

    note = ""
    if kind == "click" and element is not None and element.get("role") == "combobox" and _relayed(element, before):
        # Found live (2026-10-15): in real Excel's Save As, the worker opened "Save as type" and clicked where it
        # guessed "Excel Workbook" would be, chose Excel 97-2003, and saved "report.xlsx.xls".
        note = (f"\n{target} is a drop-down of an app running elsewhere: its open list is not shown here, so do not "
                f"click its entries by position. Choose one with gui select {element.get('ref') or 'REF'} "
                "\"the entry's name\".")
    if backend == "cdp:file_chooser":
        # cdp.click intercepted it: no dialog is on the screen, nothing was chosen.
        note = (f"\n{target} opens a file chooser; none was shown and no file was chosen. "
                "To attach a file, use the upload action on this same control with the file path(s).")
    if kind == "type" and element is not None and element.get("role") == "combobox" and _offers_nothing(before, after, _same_field(element, after) or element):
        # Found live entering a bill: the engine typed the bill's whole line ("Professional Fees for the Statutory
        # Audit & Compliance Filing Q2") into an Item field, the list offered only "Create a new Item" and
        # "Advanced Search", and it asked a person - for an item that select, searching parts of the text, finds.
        # The search an engine was told to make with `select`, made for it. Found live on a build that had the
        # hint below: the engine read it, opened the list's own "advanced search" instead, clicked around the
        # list and asked a person for a record the list had. Only the one entry whose own name lies within the
        # text is ever taken; when there is none, the text is put back as it was typed.
        field = _same_field(element, after) or element
        try:
            _select_custom(field, text)
        except ComputerError as exc:
            picked = None
            try:
                cdp.type_text(_same_field(element, observe()) or field, text)
            except ComputerError:
                pass
            note += (f"\nNothing in {target}'s list matches all of that text - it offers only to create or search. "
                     f"{exc}. Choose one of those by gui select {element.get('ref') or 'REF'} \"its name\" only if "
                     "it is plainly the same thing; if none is, ask.")
        else:
            picked = _selected["name"] or text
            note += (f"\nNothing in {target}'s list matched all of that text, so its parts were searched for and "
                     f"the entry {el._quote(picked, 90)} was selected ({_selected['why'] or 'it is that text'}). "
                     "The field now holds that entry - do not type into it again.")
    elif kind == "type" and element is not None and element.get("role") == "combobox" and _offered(after, _same_field(element, after) or element):
        # Found live: typing a word narrowed a list that was already open, so nothing "appeared" and the result
        # named none of its entries. The engine typed two more words over the one that had found the record,
        # then told a person the list offered nothing.
        offered = _offered(after, _same_field(element, after) or element)
        note += (f"\n{target}'s list now offers: "
                 + "; ".join(f"{e.get('ref') or '?'} {el._quote(e['name'], 70)}" for e in offered[:OFFERED_SHOWN])
                 + (f" (+{len(offered) - OFFERED_SHOWN} more)" if len(offered) > OFFERED_SHOWN else "")
                 + ". If one of them is the same thing, choose it now (gui click REF) - typing again replaces the "
                   "list.")
    if kind == "type" and element is not None and text and _decorated_number(text, element.get("value")):
        # Found live: a number typed with its currency sign into a field that had read "15,000.00" - the form kept the text until it
        # was saved, then read it as nothing, and the line was saved at zero.
        note += (f"\n{target} held a plain number ({el._quote(str(element.get('value')), 30)}) and was given "
                 f"{el._quote(text, 30)}. A number field takes digits (and a decimal point) only - a currency sign "
                 "or a unit can make it read as nothing when the form is saved. Type the bare number, and read the "
                 "field back after saving.")
    if grounded:
        # Where it was found, and what is there: the engine checks the model's answer against the element.
        found_on = f"; that is {el.describe(element)}" if element else "; nothing in the element list is there"
        summary = f"{summary} (found {el._quote(grounded, 80)} at {point[0]},{point[1]} by looking at the screen{found_on})"
    if kind in ("click", "double_click", "right_click") and point is not None and _on_taskbar(point):
        # Found live: an engine clicked 360,690 again and again for a form's row below the page's visible part,
        # and every click landed on the taskbar under the page.
        note += (f"\n{point[0]},{point[1]} is on the desktop's taskbar, below the windows. To reach something "
                 "below the visible part of a page, scroll it (gui scroll down 5) and observe again.")
    # A key with no target goes to whatever has the keyboard: Enter in a field commits that field, which is not another
    # field being changed behind it (found live: Enter in Excel's Name Box was reported as "changed a value you typed").
    if not _read_by_vision(before):  # a vision read's values are estimates: one that "changed" may only be read anew
        note += _overwritten_note(change, element if element is not None or kind != "key" else
                                  next((e for e in before.get("elements", []) if "focused" in (e.get("states") or [])),
                                       None))

    if _relayed_page(before) and _saves(kind, element, keys):
        note += _shared_save_note(acted_at)
    if kind in ("click", "key") and (_saved(change_text) or _became_a_record(before, change_text)):
        note += _ledger_readback(after, change_text)
    if downloads_before is not None:
        # The second in which a download must show starts at the action and runs while the screen settles and is looked
        # at - except when the action showed no result at all: that is the click whose answer may still be on its way
        # from a slow server, and it is given its second after the look, as before.
        nothing_seen = change_text.startswith("nothing visible changed")
        note += _downloads_note(downloads_before, since=None if nothing_seen else performed_at, watch=download_watch,
                                acted=performed_at, remember=True)

    return {
        "status": "done",
        "text": f"{summary}.\nWhat changed: {change_text}{note}{block_note}",
        "element": element,
        "change": change,
        "observation": after,
        # The screen as it was before the action: what a replay holds a step's expected result against, so a result
        # that was already there is not taken for the step's (replay.run).
        "before": before,
        # Cells of a typed block that do not hold what was typed: the gui command fails verification (exit 4).
        "unverified": unverified,
    }


def _overwritten_note(change, acted_on):
    """
    A warning when this action changed a field away from a value this task typed into it. Found live (2026-10-14):
    an order line's price was typed from the sheet, then the product was picked, the form filled in its catalogue
    price over it, and the order was saved at the wrong price - the result only said the price "changed".
    """

    from decurix_agent.computer import evidence

    try:
        typed = {" ".join(t.split()).casefold() for t in evidence.entered() if t and t.strip()}
    except Exception:
        return ""
    if not typed:
        return ""
    target = (acted_on or {}).get("path")
    lost = []
    for element, fields in change.get("changed") or []:
        if element.get("path") == target or privacy.secret_field(element):
            continue
        # The same field listed twice (a drop-down and its text box: Excel's "Name Box") is the field acted on.
        if acted_on and element.get("name") and element.get("name") == acted_on.get("name"):
            continue
        for field, old, new in fields:
            if field == "value" and old and " ".join(old.split()).casefold() in typed and \
                    " ".join(old.split()).casefold() != " ".join((new or "").split()).casefold():
                lost.append(f"{el.describe(element)} from {el._quote(old, 40)} to {el._quote(new or '', 40)}")
    if not lost:
        return ""
    return ("\nThis also changed " + "; ".join(lost[:3]) + " - a value you typed earlier in this task. The form "
            "fills such fields from the one you just set: if the request gives that value, type it again now and "
            "read it back.")


def _named_by(description, observation):
    """
    The one element a description names outright - by its name, or its role
    and name, as `gui click NAME` would find it - or None. "checkbox Replace
    all in" is the checkbox "Replace all in:"; two that fit are not chosen
    between.
    """

    words = " ".join((description or "").split()).strip("\"'")
    for prefix in ("the ", "a ", "an "):
        if words.lower().startswith(prefix):
            words = words[len(prefix):]
    if not words:
        return None

    tries = [{"name": words}]
    first, _, rest = words.partition(" ")
    if rest and any(e["role"] == el.normalize_role(first) for e in observation["elements"]):
        tries.insert(0, {"role": first, "name": rest.strip("\"'")})
    visible = [e for e in observation["elements"] if "offscreen" not in e.get("states", [])]
    for target in tries:
        try:
            return el.find(visible, active_group=active_group(observation), **target)
        except ComputerError:
            continue
    return None


# What a description is looked for on the screen for: clicks, and typing (the field to click into first).
GROUNDED_KINDS = ("click", "double_click", "right_click", "type")


def _on_taskbar(point):
    """Whether a screen point is on one of the desktop's own bars (screen.docks)."""
    try:
        boxes = screen.docks()
    except Exception:
        return False
    return any(x <= point[0] < x + w and y <= point[1] < y + h for x, y, w, h in boxes)


# ============================================================
# verify
# ============================================================

def _nothing_typed(kind, element, text, change_text, observation=None):
    """
    True for typing aimed at no field whose result changed nothing on the screen: it did not land anywhere. Never for
    an app read from its picture, or whose copy is still behind: there the typing lands and the look shows it only
    later - found live (2026-10-18) in TallyPrime, where the note said "nothing was typed" over text that had gone
    into the field, and the worker typed the same ledger name again ("Name already exists").
    """
    # Nor in a viewer of an app running elsewhere: a cell being edited there shows its text only once it is entered.
    # Found live (2026-10-19, real Excel): a formula said "NOTHING WAS TYPED" had gone into the cell being edited.
    if _read_by_vision(observation) or (observation or {}).get("behind") or _relayed_page(observation):
        return False
    return (kind == "type" and element is None and bool(text) and len(text) >= NOTHING_TYPED_MIN
            and change_text.startswith("nothing visible changed"))


def verify(expect_text=None, role=None, name=None):
    """
    Look at the screen again, report what changed since the last
    observation, and check an expectation if one is given.

    Returns {"ok": bool | None, "text": str, "observation": ...}; ok is
    None when nothing was expected.
    """

    previous = _load_state()
    after = observe()

    if previous is None:
        change_text = "no earlier observation to compare with"
    else:
        previous_frame = _SampleFrame(previous)
        prior = dict(previous)
        prior["frame"] = previous_frame
        change = compare(prior, after)
        if previous_frame.valid:
            change["pixels"] = previous_frame.fraction_against(after["frame"])
        change_text = summarize(change)

    ok = None
    detail = ""

    if expect_text:
        # Whole words, in order (identity.holds), in what the screen shows - never the values of fields that take
        # typing, which the worker may have typed itself. Found in review (2026-11-03, D-14): a substring over every
        # name and value passed `--text paid` on "Unpaid", and on a notes box the worker had filled.
        haystack = [after.get("active_title") or ""] + [w["title"] for w in after["windows"]]
        for e in after["elements"]:
            haystack.append(e.get("name") or "")
            if e.get("role") not in TYPING_ROLES:
                haystack.append(e.get("value") or "")
        ok = any(identity.holds(h, expect_text) for h in haystack)
        detail = f"text {el._quote(expect_text)} is {'on' if ok else 'NOT on'} the screen"

    if role or name:
        try:
            match = el.find(after["elements"], role=role, name=name, exact=True)
            hit = True
            found = el.describe(match)
        except ComputerError:
            hit = False
            found = ""
        ok = hit if ok is None else (ok and hit)
        what = " ".join(p for p in (role, el._quote(name) if name else "") if p)
        detail += ("; " if detail else "") + (f"found {found}" if hit else f"no {what} on the screen")

    text = f"Since the last look: {change_text}."
    if detail:
        text = f"{'PASS' if ok else 'FAIL'}: {detail}.\n" + text

    return {"ok": ok, "text": text, "observation": after}


class _SampleFrame:
    """The saved pixel sample of an earlier observation, standing in for a Frame."""

    def __init__(self, state):
        try:
            self.data = base64.b64decode(state.get("sample") or "")
        except ValueError:
            self.data = b""
        self.valid = bool(self.data)

    def fraction_against(self, frame):
        current = frame.sample()
        if len(current) != len(self.data) or not current:
            return None
        return sum(1 for a, b in zip(self.data, current) if abs(a - b) > 24) / len(current)


# ============================================================
# open
# ============================================================

URL_RE = re.compile(r"^(https?://|file://|www\.|localhost[:/]|127\.0\.0\.1[:/])", re.I)

APPS = {
    "libreoffice": ["soffice", "--nologo", "--norestore"],
    "writer": ["soffice", "--nologo", "--norestore", "--writer"],
    "calc": ["soffice", "--nologo", "--norestore", "--calc"],
    "impress": ["soffice", "--nologo", "--norestore", "--impress"],
    "mousepad": ["mousepad"],
    "text editor": ["mousepad"],
    "editor": ["mousepad"],
    "files": ["thunar"],
    "file manager": ["thunar"],
    "thunar": ["thunar"],
    "terminal": ["xfce4-terminal"],
    "chromium": [browser.COMMAND],
    "chrome": [browser.COMMAND],
    "google chrome": [browser.COMMAND],
    "browser": [browser.COMMAND],
    "web browser": [browser.COMMAND],
    "mail": ["thunderbird"],
    "email": ["thunderbird"],
    "thunderbird": ["thunderbird"],
    # What people call the apps these stand in for. A request says "in Excel" or "make a PowerPoint"; the
    # desktop's equivalents are LibreOffice's, GIMP and Inkscape.
    "excel": ["soffice", "--nologo", "--norestore", "--calc"],
    "microsoft excel": ["soffice", "--nologo", "--norestore", "--calc"],
    "spreadsheet": ["soffice", "--nologo", "--norestore", "--calc"],
    "word": ["soffice", "--nologo", "--norestore", "--writer"],
    "microsoft word": ["soffice", "--nologo", "--norestore", "--writer"],
    "powerpoint": ["soffice", "--nologo", "--norestore", "--impress"],
    "microsoft powerpoint": ["soffice", "--nologo", "--norestore", "--impress"],
    "gimp": ["gimp"],
    "image editor": ["gimp"],
    "photoshop": ["gimp"],
    "adobe photoshop": ["gimp"],
    "inkscape": ["inkscape"],
    "illustrator": ["inkscape"],
    "adobe illustrator": ["inkscape"],
}

OFFICE_EXTENSIONS = (".odt", ".ods", ".odp", ".docx", ".doc", ".xlsx", ".xls", ".pptx", ".csv", ".rtf")
IMAGE_EXTENSIONS = (".png", ".jpg", ".jpeg", ".gif", ".bmp", ".tif", ".tiff", ".webp", ".xcf")


# A page that asks whoever is there to prove they are a person. It is exactly
# what a worker is not: the worker stops and a person does it on the worker's
# screen. Found live: Google answered a search with its "unusual traffic" page,
# and the engine clicked at the "I'm not a robot" box seven times, by guessed
# coordinates, until it ran out of turns.
HUMAN_CHECK = re.compile(
    r"unusual traffic from your computer network|i(?:'|\u2019)?m not a robot|verify (?:that )?you(?:'| a)re (?:a )?human|"
    r"are you a robot|\bcaptcha\b|complete the security check|checking (?:if|that) the site connection is secure|"
    r"press (?:and|&) hold to confirm", re.IGNORECASE)

HUMAN_CHECK_NOTE = (
    "This page is a check that a person is there (a CAPTCHA). Do not try to pass it yourself: reply HELP asking the "
    "person to complete it on this worker's screen and to reply when it is done, then carry on with the task."
)


def _human_check(found, page_group):
    """Whether what is on the screen is a prove-you-are-human page."""

    texts = [(page_group or {}).get("url") or "", (page_group or {}).get("title") or ""]
    texts += [str(element.get("name") or "") for element in found[:200]]
    if "google.com/sorry/" in texts[0]:
        return True
    return any(HUMAN_CHECK.search(text) for text in texts)


@screen_call.routed("core.open_target")
def open_target(target, new_tab=False):
    """
    Open a web page, a document or an app, and wait for it to be on screen.
    Returns a one-line summary. Recorded in the audit trail like any action.
    """

    session.ensure()
    before = screen.grab()
    wanted = (target or "").strip()
    entry = {
        "kind": "open",
        "target": privacy.reduce_url(wanted) if URL_RE.match(wanted) else privacy.kept_text(wanted),
        "new_tab": new_tab,
    }

    try:
        summary = _open(target, new_tab)
    except ComputerError as exc:
        entry.update({"outcome": "failed", "reason": str(exc), "summary": f"open {entry['target']} - failed: {exc}"})
        audit.record(entry, before=before)
        raise

    after = screen.grab()
    active = next((w["title"] for w in screen.windows() if w["active"]), "")
    _note_front()
    _adopt_front()

    entry.update({
        "outcome": "done",
        "change": summary,
        "summary": f"open {entry['target']} - {summary}",
        "screen": [after.width, after.height],
        "expect": {"active_window": privacy.kept_text(active) or None},
    })
    audit.record(entry, before=before, after=after)

    return summary


# How long a cold Chromium may take to open its debugging port. It is ~3 s on
# an idle machine and was over 30 s on a loaded 8 GB host, where a shorter
# wait failed a task whose browser was about to be ready.
BROWSER_START_SECONDS = int(os.environ.get("DECURIX_BROWSER_START_SECONDS", "90"))


def _open(target, new_tab):
    target = (target or "").strip()
    if not target:
        raise ComputerError("say what to open: a URL, a file, or an app name")
    from decurix_agent.computer import broker
    if target.lower().startswith("file://"):
        # Broker mode: a local page of the worker's opens as a copy the browser (the screen user's) may read
        # (broker.local_copy); a file the caller did not hand over is not opened at all.
        try:
            target = "file://" + urllib.parse.quote(broker.local_copy(urllib.parse.unquote(target[len("file://"):])))
        except broker.NotHanded as exc:
            raise ComputerError(f"nothing was opened: {exc}") from None

    before = screen.windows()
    _task_windows(before)
    before_titles = {w["title"] for w in before}
    # A window that was not there is told by which window it is, not by its
    # title: a second window of an app that is open has the first one's title.
    before_ids = {w["id"] for w in before}

    if URL_RE.match(target):
        url = target if "://" in target else "http://" + target

        if cdp.available():
            title = cdp.navigate(url, new_tab=new_tab)
            _raise_browser()
            return f"Opened {broker.shown_url(url)} in Chromium (tab title {el._quote(title, 60)})."

        if any(browser.is_browser_title(t) for t in before_titles):
            raise ComputerError(
                "The browser is already open without its debugging port. Close it first "
                "(key ctrl+shift+q in its window), then open the page again."
            )

        session.launch(browser.launch_argv(url))
        _wait_until(cdp.available, BROWSER_START_SECONDS, "Chromium did not start")
        title = cdp.wait_ready(timeout=15, default_title=url)
        _raise_browser()
        return f"Started Chromium on {broker.shown_url(url)} (tab title {el._quote(title, 60)})."

    path = Path(target).expanduser()
    lower = target.lower().strip()

    if lower in CHROMIUM_APPS:
        if any(browser.is_browser_title(t) for t in before_titles):
            _raise_browser()
            return "Brought Chromium to the front."
        if cdp.available():
            # Running with no window: warmed for a task (browser.warm, --no-startup-window). Found live
            # (2026-11-07, broker mode): a person's click on the panel's Web Browser icon became this open, was told
            # "brought to the front", and nothing was on the screen. A window is opened instead.
            cdp.navigate("about:blank", new_tab=True)
            _raise_browser()
            return "Opened a Chromium window."
        session.launch(browser.launch_argv("about:blank"))
        _wait_until(cdp.available, BROWSER_START_SECONDS, "Chromium did not start")
        _raise_browser()
        return "Started Chromium."

    # Broker mode: a file of the worker's may not be one the screen user can see; one the caller handed over is there.
    if path.exists() or broker.handed(str(path)):
        if path.suffix.lower() in OFFICE_EXTENSIONS:
            argv = ["soffice", "--nologo", "--norestore", str(path)]
        elif path.suffix.lower() in IMAGE_EXTENSIONS and shutil.which("gimp"):
            argv = ["gimp", "--no-splash", str(path)]
        elif path.suffix.lower() == ".svg" and shutil.which("inkscape"):
            argv = ["inkscape", str(path)]
        elif path.is_dir():
            argv = ["thunar", str(path)]
        elif path.suffix.lower() in (".html", ".htm", ".pdf", ".eml"):
            if broker.handed(str(path)):
                return _open("file://" + str(path), new_tab)  # (the copy is made there, from what was handed over)
            return _open("file://" + str(path.resolve()), new_tab)
        else:
            argv = ["mousepad", str(path)]
    elif launchers.by_name(target) is None and launchers.missing_named(target):
        # An app the person asked this desktop to have and it does not: no stand-in for it, said plainly.
        raise ComputerError(
            launchers.missing_sentence(launchers.missing_named(target))
            + " Tell the person; do not do it in another app instead unless they say so."
        )
    elif lower in APPS and launchers.by_name(target) is None:
        # A stand-in only when the desktop has no app of that name: "Excel" is the real Excel when a person copied
        # it here ("Excel (Host)"), LibreOffice Calc otherwise.
        argv = APPS[lower]
    elif launchers.by_name(target) is not None:
        # An app that is an icon on this desktop, opened by its name: what a
        # double click on the icon does. Found live, an engine told to open an
        # app a person had copied here was refused, then double-clicked at
        # guessed points around the icon on every run.
        argv = launchers.argv(launchers.by_name(target))
    else:
        # Found in review (2026-11-03, D-05): anything else was split into words and run when its first word was a
        # program on PATH - a door to run any program with any arguments, which no guard of the shell looked at. Open
        # takes a URL, a file, an app of APPS or an icon of this desktop, and nothing else.
        icons = sorted({entry["name"] for entry in launchers.entries()})
        raise ComputerError(
            f"don't know how to open {target!r}: give a URL, an existing file path, or one of "
            + ", ".join(sorted(APPS))
            + ("; or the name of an app on this desktop: " + ", ".join(icons) if icons else "")
        )

    started = " ".join(argv)
    raises = launchers.by_name(target) is not None and not path.exists()
    front_before = next((w["id"] for w in before if w.get("active")), None)
    if "(host)" in target.lower():
        # An app of the person's computer opens the files the tools made as they are now: one of ours still holding
        # one of them (a stale copy from an earlier attempt) lets it go first (tool_made.py; CS, 2026-10-20).
        try:
            from decurix_agent import tool_made
            tool_made.held_by_our_apps()
        except Exception as exc:  # noqa: BLE001 - opening the app never fails on this
            from decurix_agent.logging_utils import log
            log(f"gui: could not release the tools' files before opening {target}: {exc}")
    session.launch(argv)

    def new_window():
        now = screen.windows()
        opened = [w for w in now if w["id"] not in before_ids and w["title"]]
        if opened or not raises:
            return opened
        # A launcher brings its app forward when it is open already, and opens nothing. Found live (2026-10-13):
        # a web app's launcher raised the app's window from behind the browser, and the open was reported as "no
        # new window appeared within 90 seconds" - the engine took the app for broken and tried addresses that
        # do not reach it.
        return [w for w in now if w.get("active") and w["id"] != front_before and w["title"]]

    opened = _wait_until(new_window, OPEN_SECONDS, None)
    if not opened:
        return (f"Started {started}, but no new window appeared within {OPEN_SECONDS} seconds. It may still be "
                "starting: wait and observe before trying anything else - starting it again opens it twice.")

    # A window is there before what it shows is: an app that is a page is titled by its address until the page
    # has loaded, and its controls are not on the screen yet. Found live on a slow host: told "Opened" while the
    # page was still blank, an engine found nothing to work with, closed the window and tried other ways in.
    window_id, title, same = opened[-1]["id"], opened[-1]["title"], 0
    deadline = time.monotonic() + LOAD_SECONDS
    while time.monotonic() < deadline and same < 3:
        time.sleep(1.0)
        now = next((w["title"] for w in screen.windows() if w["id"] == window_id), None)
        if now is None:
            break
        same = same + 1 if now == title and not _ADDRESS_TITLE.search(now) else 0
        title = now
    return f"Opened {el._quote(title, 80)}." + ("" if same >= 3 else " It may still be loading: observe before acting.")


# How long an app is given to show its window, and what it shows to load.
OPEN_SECONDS = 90
LOAD_SECONDS = 40
# A window titled by an address: a page that has not loaded yet (host_/path, http...).
_ADDRESS_TITLE = re.compile(r"^\s*(https?://|[\w.-]+\.[a-z]{2,}(:\d+)?[_/]|[\w.-]+_/)", re.I)


def bring_to_front(title):
    """
    Put an open window in front of the others, by a few words of its title:
    what a click on its taskbar button does. The engine's own view of the
    screen has no taskbar - found live, with one app left in front of another
    it clicked around the top of the screen looking for one, for ten minutes.
    """

    wanted = " ".join((title or "").lower().split())
    if not wanted:
        raise ComputerError("say which window: a few words of its title")

    windows = [w for w in screen.windows() if w["title"]]
    exact = [w for w in windows if w["title"].lower() == wanted]
    partial = [w for w in windows if wanted in w["title"].lower()]
    if not exact and not partial and wanted in ("browser", "chromium", "edge", "chrome"):
        partial = [w for w in windows if browser.is_browser_title(w["title"])]
    found = exact or partial

    if not found:
        raise ComputerError("no open window is titled like that. Open windows: "
                            + (", ".join(el._quote(w["title"], 60) for w in windows) or "none"))
    if len(found) > 1:
        raise ComputerError("more than one window is titled like that: "
                            + ", ".join(el._quote(w["title"], 60) for w in found) + ". Say more of the title.")

    xinput.activate(found[0]["id"])
    time.sleep(0.4)
    _adopt_front()
    return f"Brought {el._quote(found[0]['title'], 80)} to the front."


def _raise_browser():
    for w in screen.windows():
        if browser.is_browser_title(w["title"]):
            xinput.activate(w["id"])
            return


def _wait_until(probe, seconds, failure):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = probe()
        if result:
            return result
        time.sleep(0.4)
    if failure:
        raise ComputerError(failure)
    return None

