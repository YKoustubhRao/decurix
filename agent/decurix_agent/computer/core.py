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
from decurix_agent.computer import approval, atspi, audit, browser, cdp, grounder, launchers, privacy, screen, session, volatility, xinput
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ActedError, ComputerError


# ============================================================
# observe / act / verify
#
# The one action layer every engine shares. Claude Code reaches it as MCP
# tools (mcp_server.py) and sees the screenshots; the Ollama loop and TheBot's
# operator reach it as the `gui` shell command (cli.py) and read the element
# list as text. Whichever door an action comes through, it passes the same
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

    tmp = STATE_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data), encoding="utf-8")
    tmp.replace(STATE_FILE)


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


def _wait_for_settle(reference):
    """
    Wait until the screen stops changing, for at most SETTLE_SECONDS.

    A click that opens a dialog or loads a page is not finished when the
    event is delivered; reading the screen straight away would report
    "nothing changed" about an action that worked.
    """

    deadline = time.monotonic() + SETTLE_SECONDS
    time.sleep(0.15)
    last = screen.grab()
    stable_since = time.monotonic()

    while time.monotonic() < deadline:
        time.sleep(0.15)
        current = screen.grab()
        if screen.changed_fraction(last, current) > 0.002:
            last = current
            stable_since = time.monotonic()
        elif time.monotonic() - stable_since >= 0.35:
            break

    return last


def _window_id_for(element):
    title = element.get("window_title") or ""
    windows = screen.windows()
    for w in windows:
        if w["title"] == title:
            return w["id"]
    for w in windows:
        if title and (title in w["title"] or w["title"] in title):
            return w["id"]
    return None


def _focus_window(element):
    window_id = _window_id_for(element)
    if window_id is not None:
        xinput.activate(window_id)


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
        FRONT_FILE.write_text(json.dumps({"task": _task_now(), "at": time.time(), "id": front["id"],
                                          "title": front["title"], "known": [w["id"] for w in wins]}),
                              encoding="utf-8")
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
        TASK_WINDOWS_FILE.write_text(json.dumps(record), encoding="utf-8")
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


def _goto_sheet_cell(element, cell=None):
    """
    Move a spreadsheet's cell cursor to an element's cell (or to `cell`, a
    coordinate on the same sheet), by coordinate.

    Through LibreOffice's Name Box (Ctrl+Shift+F5, the cell reference,
    Enter): the Name Box itself is not exposed over AT-SPI, and a click at a
    cell's reported position lands a row off (see atspi.place_caret).
    """

    _focus_window(element)
    xinput.key("Escape")
    xinput.key("ctrl+shift+F5")
    time.sleep(0.2)
    xinput.type_text(cell or element["name"])
    xinput.key("Return")
    time.sleep(0.2)


# ============================================================
# A block of cells typed into a spreadsheet's grid in one action
#
# Found live (2026-10-19): a five-row order sheet and a register's four headers were typed one cell per gui
# command - two commands a cell, about ten seconds each - because a Tab in typed text was refused (rightly, for a
# form: it carries the rest into the next field). In a sheet's grid a Tab is the next cell and a new line the next
# row, and every cell can be read back. So text with Tabs or new lines aimed at a sheet's cell is entered row by row
# from that cell, and then each cell is read back and compared with what was typed: a cell the app changed (its
# AutoInput completing "Maggi" to "Maggi 12-pack", a number read as a date, a formula showing an error) is reported,
# and the command fails verification. Form fields keep the one-field rule.
# ============================================================

CELL_NAME = re.compile(r"^\$?([A-Za-z]{1,3})\$?([0-9]{1,7})$")
# What a spreadsheet shows for a formula it could not work out.
FORMULA_ERROR = re.compile(r"^(?:#[A-Z][A-Z0-9/!?]*[!?]?|Err:\d+)$")


def _column_number(letters):
    n = 0
    for ch in letters.upper():
        n = n * 26 + ord(ch) - 64
    return n


def _column_letters(n):
    out = ""
    while n:
        n, r = divmod(n - 1, 26)
        out = chr(65 + r) + out
    return out


def _sheet_rows(text):
    """A block's rows of cells: lines split at Tabs (a final new line ends the block, it adds no row)."""

    return [line.split("\t") for line in text.rstrip("\n").split("\n")]


def _is_block(text):
    """Whether typed text is a block of cells (a Tab between cells or a new line between rows), not one value."""

    return bool(text) and ("\t" in text or "\n" in text.rstrip("\n"))


def _sheet_start(element, before):
    """
    The sheet cell a block starts at: the cell aimed at, or - with no target - the focused cell of the sheet in the
    active window. None when the typing does not go into a spreadsheet's grid.
    """

    if element is not None:
        return element if (element.get("handle") or {}).get("sheet_cell") else None
    focused = [e for e in before.get("elements", []) if (e.get("handle") or {}).get("sheet_cell")
               and "focused" in (e.get("states") or []) and e.get("window_title") == before.get("active_title")]
    return focused[0] if len(focused) == 1 else None


def _block_cells(start, rows):
    """Each cell of a block: (coordinate, row offset, column offset, text), from the start cell's coordinate."""

    m = CELL_NAME.match((start.get("name") or "").strip())
    if not m:
        raise ComputerError(f"{el.describe(start)} has no cell coordinate to start a block from - type into a cell "
                            "by its ref (gui observe lists them as cell \"A1\")")
    column, row = _column_number(m.group(1)), int(m.group(2))
    return [(f"{_column_letters(column + j)}{row + i}", i, j, value)
            for i, cells in enumerate(rows) for j, value in enumerate(cells)]


def _type_sheet_block(element, text):
    """Enter a block of cells row by row: the Name Box to each row's first cell, the cells with a Tab between, Enter."""

    rows = _sheet_rows(text)
    first = _block_cells(element, rows)
    for i, cells in enumerate(rows):
        coordinate = next(c for c, ri, cj, _ in first if ri == i and cj == 0)
        _goto_sheet_cell(element, coordinate)
        for j, value in enumerate(cells):
            if j:
                xinput.key("Tab")
            if value:
                xinput.type_text(value)
        xinput.key("Return")


# ---- The same, in a viewer of an app running elsewhere (a Windows spreadsheet shown through the host-app viewer) ----
#
# Found live (2026-10-19): real Excel through the viewer took 68 actions and 26 minutes for two columns of formulas,
# cell by cell through the Name Box, and saved nothing. There the cells are read through the app's accessibility and
# listed by the viewer as `row "D2 = 640"` (empty cells left out), and keys reach the app as a person's would. A block
# is typed where the app's cursor is, as a person types a table: each cell's text, Tab between cells, Enter at the end
# of a row - a spreadsheet takes Enter after a run of Tabs back to the column the run began in, one row down. The
# person's clipboard on the host is never used (winhost.Session.Paste: it is theirs). The cells are then read back from
# the viewer: the block's top-left is where the cells that changed begin.

# A cell as the viewer lists it: its address, then " = " and what it shows (an empty cell is not listed).
RELAYED_CELL = re.compile(r"^\$?([A-Z]{1,3})\$?([0-9]{1,7})(?: = (.*))?$", re.S)
# Pauses that let an app far away take a cell's characters before the key that moves on (a key can overtake text).
RELAYED_CELL_PAUSE = 0.3
RELAYED_ROW_PAUSE = 0.6
# How many more looks the read-back waits for when the viewer's copy of the app is still behind.
RELAYED_READ_TRIES = 5


def _relayed_page(observation):
    """Whether the page in front is a viewer of an app running elsewhere."""

    return any(v in ((observation or {}).get("page_url") or "") for v in RELAYED_VIEWERS)


def _relayed_cells(observation):
    """The spreadsheet cells a viewer lists, {address: what it shows}."""

    cells = {}
    for e in (observation or {}).get("elements", []):
        if e.get("source") != "cdp" or e.get("role") not in ("row", "gridcell", "cell"):
            continue
        m = RELAYED_CELL.match((e.get("name") or "").strip())
        if m:
            cells[m.group(1) + m.group(2)] = (m.group(3) or "").strip()
    return cells


def _type_relayed_block(text):
    """Type a block where the app's cursor is: each cell's text, Tab between cells, Enter after each row."""

    for cells in _sheet_rows(text):
        _type_relayed_row(cells)


def _type_relayed_row(cells):
    for j, value in enumerate(cells):
        if j:
            xinput.key("Tab")
            time.sleep(RELAYED_CELL_PAUSE)
        if value:
            xinput.type_text(value)
            time.sleep(RELAYED_CELL_PAUSE)
    xinput.key("Return")
    time.sleep(RELAYED_ROW_PAUSE)


def _read_back_relayed(before_cells, after_cells, text, start_cell=None):
    """
    Compare a typed block with the cells a viewer lists after it. Returns (span, formulas, wrong): where it went, what
    each formula came to, and each cell that does not show what was typed. Nothing changed is all wrong. The block
    starts at `start_cell` when the cell was named, else where the cells that changed begin.
    """

    def place(address):
        m = CELL_NAME.match(address)
        return _column_number(m.group(1)), int(m.group(2))

    changed = [a for a in after_cells if after_cells[a] != before_cells.get(a)]
    changed += [a for a in before_cells if a not in after_cells]
    if not changed:
        return "", [], ["no cell of the sheet changed - the keys did not reach the sheet (click the starting cell, or "
                        "go to it by the Name Box and press Enter, then type the block)"]
    if start_cell:
        start = {"name": start_cell.upper()}
    else:
        column = min(place(a)[0] for a in changed)
        row = min(place(a)[1] for a in changed)
        start = {"name": f"{_column_letters(column)}{row}"}
    cells = _block_cells(start, _sheet_rows(text))
    formulas, wrong = [], []
    for address, _, _, typed in cells:
        if not typed.strip():
            continue
        shown = after_cells.get(address, "")
        if typed.strip().startswith("="):
            formulas.append(f"{address} {typed.strip()} -> {el._quote(shown, 40)}")
        if not _cell_holds(typed, shown):
            wrong.append(f"{address} shows {el._quote(shown, 60) if shown else 'nothing (or is outside the part read)'} "
                         f"for {el._quote(typed, 60)}")
    if cells and len(_sheet_rows(text)) > 1:
        runs_on = _table_runs_on(lambda address: (after_cells.get(address) or "").strip(), cells)
        if runs_on:
            wrong.append(NOTE + runs_on)
    return _block_span(cells), formulas, wrong


def _block_span(cells):
    """Where a block's cells went ("D1:E3"), or "" for none."""

    return f"{cells[0][0]}:{max(cells, key=lambda c: (c[1], c[2]))[0]}" if cells else ""


def _block_report(opening, found, formulas, failed_lead):
    """
    What a block's result says, and which of its findings failed it: `found` holds cells that do not hold what was
    typed (they make the command end 4) and NOTE lines (a table that goes on below the block, which is only said).
    Returns (the text, the failures).
    """

    notes = [w[len(NOTE):] for w in found if w.startswith(NOTE)]
    failed = [w for w in found if not w.startswith(NOTE)]
    text = (f"\n{opening} Read back: "
            + ("every cell holds what was typed." if not failed else
               failed_lead + "; ".join(failed[:12]) + (f" (+{len(failed) - 12} more)" if len(failed) > 12 else ""))
            + (("\nFormulas came to: " + "; ".join(formulas[:12])) if formulas else "")
            + ("".join(f"\nNote: {n}" for n in notes)))
    return text, failed


def _sheet_cell_named(before, cell):
    """
    An element for a cell of the sheet in the active window, named by its coordinate ("D1"), made from a cell the
    look listed (a sheet lists only its visible cells, and an empty column may have none). None with no such sheet.
    """

    cells = [e for e in before.get("elements", []) if (e.get("handle") or {}).get("sheet_cell")
             and e.get("window_title") == before.get("active_title") and CELL_NAME.match(e.get("name") or "")]
    if not cells:
        return None
    ref = next((e for e in cells if "focused" in (e.get("states") or [])), cells[0])
    have, want = CELL_NAME.match(ref["name"]), CELL_NAME.match(cell)
    dc = _column_number(want.group(1)) - _column_number(have.group(1))
    dr = int(want.group(2)) - int(have.group(2))
    handle = dict(ref["handle"], row=ref["handle"].get("row", 0) + dr, column=ref["handle"].get("column", 0) + dc)
    return dict(ref, name=cell.upper(), ref=None, value="", states=[], handle=handle,
                path=(ref.get("path") or "").rsplit(" > ", 1)[0] + f' > cell "{cell.upper()}"')


def _name_boxes(observation):
    """
    The viewer's Name Box elements, the editable text field first: the viewer lists the box twice (a combobox and the
    textbox inside it), and the app takes a value only in its text field. Found live (2026-10-19): the combobox was
    picked, and every block failed with "not a field of the app" while the textbox took "D1:E26" when typed by hand.
    """

    boxes = [e for e in (observation or {}).get("elements", []) if e.get("source") == "cdp"
             and e.get("role") in ("textbox", "combobox") and (e.get("name") or "").strip().lower() == "name box"]
    return sorted(boxes, key=lambda e: e.get("role") != "textbox")


def _goto_relayed_cell(boxes, cell):
    """Put an app's cursor on a cell, in a viewer of a spreadsheet running elsewhere: its Name Box, then Enter."""

    enabled = [b for b in boxes if "disabled" not in (b.get("states") or [])]
    if not enabled:
        raise ComputerError(f"this app shows no Name Box to go to {cell} by (or it is greyed out: a cell is being "
                            "edited or a dialog is open) - nothing was typed")
    last = None
    for box in enabled:
        try:
            cdp.set_in_app(box, cell.upper())
            break
        except ComputerError as exc:
            last = exc
            if not NOT_SET.search(str(exc)):
                raise
    else:
        raise ComputerError(f"could not go to {cell} by the Name Box: {last} - nothing was typed")
    time.sleep(RELAYED_CELL_PAUSE)
    xinput.key("Return")
    time.sleep(RELAYED_ROW_PAUSE)


# How long a block may take to type into an app far away, as seconds: its keys are relayed one by one (a 25-row block
# of formulas was 2,100 of them, about 0.09 s each) and the operator kills a command at 180. Found live (2026-10-19):
# the command was killed at the 24th row's turn, nothing was read back, and "exit 124" was read past. Past this the
# block stops at a row boundary and says what was not typed.
RELAYED_BLOCK_SECONDS = float(os.environ.get("DECURIX_GUI_BLOCK_SECONDS", "110"))


def _type_relayed_rows(before, start_cell, text, budget=None, clock=time.monotonic):
    """
    Enter a block into a spreadsheet running elsewhere, row by row from `start_cell`. First Escape: it takes this
    page's keyboard out of any stand-in field (the keys then go where the app's cursor is) and ends an edit in the app.
    Each row is begun at its first cell through the Name Box - which also drops any selected range (in a selection Tab
    and Enter move inside it: found live, a block typed over a selected D1:E26 came out a row off, with a circular
    reference) - then each cell's text is typed as keys, a real Tab key between cells and Enter after the last. A
    cell's value is never set with Tabs in it: a Tab in a set value is a character.
    """

    budget = RELAYED_BLOCK_SECONDS if budget is None else budget
    began = clock()
    xinput.key("Escape")
    time.sleep(RELAYED_ROW_PAUSE)
    boxes = _name_boxes(before)
    if boxes and all("disabled" in (b.get("states") or []) for b in boxes):
        # Still editing a cell, or a dialog is open: one more look after the Escape.
        boxes = _name_boxes(observe())
    m = CELL_NAME.match(start_cell)
    column, row = _column_number(m.group(1)), int(m.group(2))
    rows = _sheet_rows(text)
    for i, cells in enumerate(rows):
        if i and clock() - began > budget:
            return i  # whole rows only: what was typed is read back, and the rest is named
        _goto_relayed_cell(boxes, f"{_column_letters(column)}{row + i}")
        _type_relayed_row(cells)
    return len(rows)


def _squash(text):
    """Text as compared after typing: spaces collapsed, the dashes and quotes AutoCorrect makes undone (case kept)."""

    return " ".join((text or "").translate(_AUTOCORRECTED).split())


def _numeric(text):
    """A cell's text as a number when it is one, written any usual way (1,250.00, ₹78, 9%); else None."""

    import decimal

    t = re.sub(r"[\s,]", "", text or "")
    # The currency is a token of its own, its dot included: "Rs.78" is 78, never .78.
    t = re.sub(r"(?i)^(?:rs\.?|inr|rupees?|usd|eur|gbp|[^\w+\-.])+", "", t)
    percent = t.endswith("%")
    t = t.rstrip("%")
    if not re.fullmatch(r"[+-]?(?:\d+\.?\d*|\.\d+)", t):
        return None
    # A leading zero is part of an identifier, not of a number: "0012345" is not 12345 (a sheet that drops it has
    # changed the value).
    if re.match(r"[+-]?0\d", t):
        return None
    value = decimal.Decimal(t)
    return value / 100 if percent else value


def _cell_holds(typed, shown):
    """Whether a cell shows what was typed into it (a formula: whether it shows a result rather than an error)."""

    typed, shown = (typed or "").strip(), (shown or "").strip()
    if typed.startswith("="):
        return not FORMULA_ERROR.match(shown)
    if _squash(typed) == _squash(shown):
        return True
    a, b = _numeric(typed), _numeric(shown)
    return a is not None and b is not None and (a == b or (typed.strip().endswith("%") != shown.endswith("%")
                                                            and (a * 100 == b or a == b * 100)))


# A row label that says it totals what is above it (the table ends before it).
_TOTAL_ROW = re.compile(r"^\s*(?:grand\s+|sub[\s-]*)?totals?\b", re.I)
# What a read-back puts first on a finding that is a warning to read, not a cell that failed (the command still ends 0).
NOTE = "NOTE: "
# How far below a block the table beside it is followed.
_RUNS_ON_LIMIT = 400


def _table_runs_on(shown, cells):
    """
    A sentence when the table to the left of a block of cells goes on below the block's last row, else "". Found live
    (2026-10-19, real Excel): a block of formulas stopped one row short of a table of 24 rows, and was read back as
    "every cell holds what was typed" - the read-back only compared what was typed with what was typed. `shown(address)`
    is a cell's text ("" for none); the table's own label column (A) says where a Total row ends it. `cells` are the
    block's (_block_cells).
    """

    first_column = _column_number(CELL_NAME.match(cells[0][0]).group(1))
    last_row = max(int(CELL_NAME.match(c[0]).group(2)) for c in cells)
    if first_column < 2:
        return ""
    left = _column_letters(first_column - 1)
    if not shown(f"{left}{last_row}"):
        return ""
    row = last_row
    while row < last_row + _RUNS_ON_LIMIT and shown(f"{left}{row + 1}") and not _TOTAL_ROW.match(shown(f"A{row + 1}") or ""):
        row += 1
    if row == last_row:
        return ""
    return (f"the table to its left (column {left}) goes on to row {row}, but this block ends at row {last_row} - "
            f"rows {last_row + 1} to {row} were NOT filled")


def _read_back_block(start, text):
    """
    Read every cell of a typed block back from the sheet. Returns (lines, wrong): what each formula came to and
    each cell that does not hold what was typed (or could not be read).
    """

    rows = _sheet_rows(text)
    cells = _block_cells(start, rows)
    handle = start.get("handle") or {}
    shown_lines, wrong = [], []
    for coordinate, i, j, typed in cells:
        if not typed.strip():
            continue
        shown = atspi.sheet_cell_text(start, handle.get("row", 0) + i, handle.get("column", 0) + j)
        if shown is None:
            wrong.append(f"{coordinate} could not be read back")
            continue
        if typed.strip().startswith("="):
            shown_lines.append(f"{coordinate} {typed.strip()} -> {el._quote(shown, 40)}")
        if not _cell_holds(typed, shown):
            wrong.append(f"{coordinate} shows {el._quote(shown, 60)} for {el._quote(typed, 60)}")

    def shown_at(address):
        m = CELL_NAME.match(address)
        return (atspi.sheet_cell_text(start, int(m.group(2)) - 1, _column_number(m.group(1)) - 1) or "").strip()

    runs_on = _table_runs_on(shown_at, cells) if len(rows) > 1 else ""
    if runs_on:
        wrong.append(NOTE + runs_on)
    return shown_lines, wrong


# Windows whose tables are shapes on a page (a slide, a drawing), edited a cell at a time with F2.
DRAWN_TABLES = re.compile(r"LibreOffice (?:Impress|Draw)\b")

# What LibreOffice's AutoCorrect does to typed text, undone before comparing: dashes and curly quotes.
_AUTOCORRECTED = str.maketrans({"–": "-", "—": "-", "‘": "'", "’": "'", "“": '"',
                                "”": '"'})


def _slide_words(text):
    return re.sub(r"\s+", "", (text or "").translate(_AUTOCORRECTED)).lower()


def _keep_slide_text(element, text, append):
    """
    Read back what was typed on a slide, and type it again slowly when the keys arrived out of order. Found live
    (2026-10-15): on a busy worker Impress took "Northwind Traders - prepared for the leadership team" as "Ns -
    preparedort for thwhined Tlraedeardership" - the shape re-laid itself out as the keys came and the caret went back
    each time; the same keys on an idle worker came out right. Only the first line is compared: each line of a
    bulleted text is a paragraph of its own.
    """

    want = _slide_words(text.split("\n")[0])
    if not want:
        return

    def kept():
        have = None
        for _ in range(6):
            time.sleep(0.5)
            have = atspi.text_now(element)
            if have is None:
                return None
            if want in _slide_words(have):
                return True
        return have

    have = kept()
    if have is True or have is None:
        return
    if append:
        raise ComputerError(f"the text on the slide came out as {have[:120]!r}, not what was typed; select it, "
                            "and type it again")
    xinput.key("ctrl+a")
    time.sleep(0.3)
    xinput.type_text(text, delay=90)
    have = kept()
    if have is True or have is None:
        return
    raise ComputerError(f"the text on the slide came out as {have[:120]!r} even typed a second time, slowly; it "
                        "needs correcting before going on")

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
PARTIAL_DOWNLOAD = (".crdownload", ".part", ".download")


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


def _downloads_note(before, wait=DOWNLOAD_WAIT_SECONDS):
    """
    What an action downloaded: the file that arrived in the download folder, its size, and - when the browser named it
    "x (1).pdf" because "x.pdf" was already there - whether it is the same file again. Found live
    (2026-10-21): a click on a chat attachment's Download link said only that the link was focused; the engine went
    looking in the browser's download history and a file manager, had clicked the first attachment twice without
    knowing, and saved the second copy of invoice 197 under invoice 198's name.
    """

    # The screen has already settled: nothing new within a second means nothing was downloaded; a file still being
    # written is waited for up to `wait`.
    start = time.time()
    while True:
        now = _downloads_snapshot()
        new = [n for n, v in now.items() if before.get(n) != v]
        partial = [n for n in new if n.endswith(PARTIAL_DOWNLOAD)]
        waited = time.time() - start
        if (new and not partial) or (not new and waited >= 1) or waited >= wait:
            break
        time.sleep(0.5)
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
                    "insecure file\"; it then arrives in " + str(_downloads_dir()) + ".")
        return "\nStill downloading: " + ", ".join(partial[:3]) + "." if partial else ""
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
    note = "\nDownloaded: " + "; ".join(lines) + "."
    # A file that is not what its name says (a web page saved as .pdf) is said at once, not first at the final check.
    try:
        from decurix_agent import produced
        faults = produced.named_kind_faults([folder / n for n in done[:4]])
    except Exception:  # noqa: BLE001 - a note, never a reason for the action to fail
        faults = []
    if faults:
        note += " But " + "; ".join(faults) + "."
    return note


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
    Ctrl+S then saves nothing or a copy - or an empty document over the file with Save As. Found live (round 5 P1 on pc3,
    2026-10-20, CS): Word held the memo from an earlier attempt, the tools rewrote it, and Word's stale and later blank
    copy was saved over it - the memo ended with 0 headings.
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
    """What a replay's committing press holds that a person checks before it goes (_form_check, listing="ask"): the
    press is put to them through the approval gate rather than refused. (No check returns one since review 2026-11-03:
    a replay's sourced save goes, an unsourced one is refused.)"""


# The values a replay running in this process types from the person's demonstration because today's request gives
# none for them (replay.run sets it for the run): the person's own, never "in no source".
TAUGHT_VALUES = set()


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
# ("Accept": an accounting application's save, found live 2026-10-26 - its duplicate check had never run.)
_SAVE_CONTROL = re.compile(r"(?i)\b(?:submit|save|create|add|onboard|post|confirm|place|register|book|finish|accept)\b")
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
_NEW_RECORD_PAGE = re.compile(r"(?i)page is now \S*/(?:new|create|add)\b|\b(?:new|create|add)\b[^\"]{0,20}(?:record|employee|"
                              r"order|customer|invoice|entry)")
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


def already_entered(events):
    """
    (what it is, the system's number) when the new record being saved repeats one this task already saved: the values
    typed since the last save, against those of each earlier save (an email, or its first two words). None otherwise -
    and never for a form that changes an existing record. Found live (round 5 P4, 2026-10-21): a joiner saved with the
    wrong department was entered again, and the portal held him twice.

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
    mine = _identity(current)
    if not mine or not new_page:
        return None
    my_refs = _record_refs(pairs)
    for theirs, number, their_refs, _ in saved:
        # Two bills to one customer share a name, never their own numbers (found live, 2026-10-25: a month's second bill
        # for a customer was refused as the first one again).
        if my_refs and their_refs and not (my_refs & their_refs):
            continue
        if theirs and theirs[0] == mine[0] and (theirs[1] & mine[1] if mine[0] == "mail" else theirs[1] == mine[1]):
            what = ", ".join(sorted(mine[1])) if mine[0] == "mail" else " ".join(w.title() for w in mine[1])
            return what, number
    return None


def _current_form(events):
    """[(field label, value)] put into the record's form being filled: since it opened afresh or the last save."""

    current = []
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        if _FORM_START.search(change) or _FORM_START.search("page is now " + str(event.get("target") or "")):
            current = []
        label = str((event.get("element") or {}).get("name") or "")
        if event.get("kind") == "type" and event.get("text"):
            current.append((label, str(event["text"])))
        if event.get("kind") == "select" and event.get("option"):
            current.append((label, str(event["option"])))
        if event.get("kind") in ("click", "key") and _saved(change) and current:
            current = []
    return current


_LABEL_NOISE = {"of", "the", "a", "no", "number", "dd", "mm", "yyyy", "rs", "usd", "inr", "per", "recruiter", "status"}


def _words_of(label):
    return {w for w in re.findall(r"[a-z]+", re.sub(r"\(.*?\)", " ", str(label or "").lower())) if w not in _LABEL_NOISE}


def _same_value(a, b):
    """Two values of one field, the same whatever their form: case, spaces, punctuation, a date's format, a phone's +91."""

    import datetime
    def norm(v):
        if isinstance(v, (datetime.date, datetime.datetime)):
            return ("date", v.strftime("%Y-%m-%d"))
        t = str(v if v is not None else "").strip()
        m = re.fullmatch(r"(\d{1,2})[/.\- ](\d{1,2}|[A-Za-z]{3})[a-z]*[/.\- ](\d{4})", t)
        if m:
            months = {k: i for i, k in enumerate(("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct",
                                                  "nov", "dec"), 1)}
            mo = int(m.group(2)) if m.group(2).isdigit() else months.get(m.group(2).lower()[:3], 0)
            return ("date", f"{int(m.group(3)):04d}-{mo:02d}-{int(m.group(1)):02d}")
        m = re.fullmatch(r"(\d{4})-(\d{2})-(\d{2})(?:[ T].*)?", t)
        if m:
            return ("date", f"{m.group(1)}-{m.group(2)}-{m.group(3)}")
        digits = re.sub(r"\D", "", t)
        if len(digits) >= 7 and len(digits) >= len(re.sub(r"\s", "", t)) - 4:
            return ("digits", digits[-10:])
        return ("text", re.sub(r"[^a-z0-9]", "", t.lower()))
    return norm(a) == norm(b) and norm(a)[1] != ""


def crossed_values(form, tables):
    """
    The values put into a record's form that are ANOTHER record's in the source table (one row per record) and not
    this one's: [(field, typed, whose, own)]. The record is the row its typed email, else its first two words, names.
    Nothing when the record or the field's column cannot be told. Found live (round 5 P4, 2026-10-21): Rohit
    Kulkarni was saved with Mohammed Arif Khan's department and job title.
    """

    values = [v for _, v in form]
    mails = [v.strip().lower() for v in values if re.fullmatch(r"\S+@\S+\.\S+", v.strip())]
    words = [v.strip().lower() for v in values if re.fullmatch(r"[A-Za-z][A-Za-z .'-]{1,40}", v.strip())][:2]
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
            if col is None or col >= len(own) or _same_value(typed, own[col]):
                continue
            others = [r for r in rows if r is not own and col < len(r) and _same_value(typed, r[col])]
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
            mark.write_text("1", encoding="utf-8")
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


def _unsourced_fields(fields):
    """
    [(label, value)] of a form's fields whose value the tools typed during this task and that states a fact (an
    address, a date, a code, a figure of 100 or more) no source holds - the request, its files, the person's messages,
    the system's pages read, the work's recorded working (provenance, harness audit 2026-10-21). A value the
    application filled in itself is not the worker's and is never judged here.
    """

    from decurix_agent import long_task, task_context
    from decurix_agent.computer import evidence
    typed = {" ".join(str(v).split()) for v in evidence.entered()}
    mine = [(label, value) for label, value in fields if " ".join(str(value or "").split()) in typed]
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
        made_up = filler.invented(found["fields"], task_context.current_request(), files)
        unsourced = _unsourced_fields(found["fields"])
        if listing == "ask":
            # A replay types what the person demonstrated where today's request gives nothing (replay.run): their own
            # value, not one made up. Any other value no source holds is refused below, like an engine's.
            unsourced = [(label, value) for label, value in unsourced if not _taught_value(value)]
        made_up += unsourced
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
    ledger_lines = []
    judged = _ledger_check(found["fields"])
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
        note_file.write_text(json.dumps({"key": key, "at": time.time(), "ref": element.get("ref") or "",
                                         "task": task_context.current()}), encoding="utf-8")
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
    fields = []
    for e in (observation or {}).get("elements") or []:
        if e.get("role") not in FILLED_ROLES or (group and e.get("group") != group) or e is element:
            continue
        value = " ".join(str(e.get("value") or "").split())
        if not value or privacy.secret_field(e) or privacy.credential_in(value) or _FILE_NAME_VALUE.fullmatch(value):
            continue
        if approval.searches(e) or _FILTER_FIELD.match(str(e.get("name") or "")):
            # A list's search box and filters say what is shown, not what a press saves (review 2026-11-03: a list
            # page's filters were checked as the record a "Create" there would save).
            continue
        fields.append([" ".join(str(e.get("name") or e.get("role") or "").split())[:60], value[:80]])
    title = (element or {}).get("window_title") or (observation or {}).get("active_title") or "the window"
    return {"form": str(title)[:80], "fields": fields}


def _commit_keys(keys):
    """(Enter, Ctrl+S): whether these keys press Enter, and whether they save (an application's Ctrl+S)."""

    combos = [xinput.normalize_combo(k).lower() for k in keys or () if k and k.strip()]
    return (any(c in ("return", "enter", "kp_enter") for c in combos), any(c == "ctrl+s" for c in combos))


def _commit_check(kind, element, keys, before, checked=False, text=None, listing=True):
    """
    The pre-save checks (_form_check: a record entered twice, a placeholder, an invented or unsourced value, the
    declared record's ledger, the listing of a full form) for every action that commits what a form or a page holds,
    whichever way it is pressed: a click or double-click on a <form>'s submit control or on any control whose label
    says it saves, submits, sends, pays or posts (approval.commits) - a CDP element or an AT-SPI one, found by ref, by
    name or by --see; Enter in a form's field that submits it; Ctrl+S on a web page. Returns the text that stops the
    action, or "" to go on. Found in review (2026-11-02): the checks ran only for a single `gui click` on a CDP
    <form type=submit>, so a double-click, --see, the act tool, an AT-SPI press, an Enter and every button an
    application draws with divs (the `gui record` ledger among them) went past them.
    """

    if kind in ("click", "double_click") and element is not None:
        form = cdp.submit_form_fields(element, min_fields=1) if element.get("source") == "cdp" else None
        if form is None and not approval.commits(element):
            return ""
        if form is None and element.get("source") == "cdp" \
                and _OPENS_OR_ACCEPTS.match(approval.label_words(element.get("name"))) \
                and not approval.DIALOG_IN_PATH.search(element.get("path") or "") \
                and approval.press_effect(element) != "asks":
            return ""
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
            line = text.split("\n", 1)[0]
            form = dict(form, fields=[f for f in form.get("fields") or [] if f[0] != label]
                        + ([[label, line[:80]]] if line.strip() and not privacy.secret_field(field) else []))
        submit = {"name": form.get("submit") or "Submit", "ref": "", "path": field.get("path") or ""}
        return _form_check(submit, found=form, listing=listing, confirm=(
            "Enter would send it, and an Enter is not how a full form is sent: press the form's own submit control "
            "(gui click REF), which lists the form once more and is confirmed with --checked."))
    if save and before.get("page_url") and not _relayed_page(before):
        # (Ctrl+S on a web page saves its record: the checks that stop a wrong record stand; the listing of a full
        # form is the Save control's, since a page saved only by its keys has no control to confirm it on.)
        save_control = {"name": "Save", "ref": "", "path": (field or {}).get("path") or "",
                        "group": (field or {}).get("group")}
        return _form_check(save_control, found=_page_fields(before, field), listing=False)
    return ""


def _ledger_check(form_fields):
    """
    The declared record (records.py) against the form about to be saved: None when no record is open; else
    (what stops the click or "", whether the diff is clean enough to save without a listing, advisory lines). A field
    holding a value the record does not stops the click; the record's scope absent from the window stops it once.
    """

    from decurix_agent import task_context
    from decurix_agent.computer import evidence, records

    try:
        rec = records.open_record(records.load(), " | ".join(str(v) for _, v in form_fields if v), form=form_fields)
    except Exception:  # noqa: BLE001 - a ledger that cannot be read judges nothing
        return None
    if rec is None:
        return None
    blocking, advisory, matched = records.presave(rec, form_fields)
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
        records.note_presave(rec, blocking)
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


def _perform(kind, element, point, text, keys, direction, amount, option, append, end=None, files=None, ids=None):
    """Carry out one action; return which backend did it. `ids`: the identifiers a select resolves the option by first."""

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
            if cdp.type_text(element, text, replace=not append) is not False:
                return "cdp"
            # The page threw the text away (cdp.type_text): type it the way
            # a person does - click into the field, then real keys.
            _focus_window(element)
            cdp.click(element)
            time.sleep(0.3)
            if not append:
                xinput.key("ctrl+a")
            xinput.type_text(text)
            return "xdotool"

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
            _selected.update(name=None, why="")
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
            cdp.set_file_input_files(element, files)
            return "cdp"
        raise ComputerError("upload is currently supported for browser elements")

    raise ComputerError(f"unknown action {kind!r}")


# Page elements typing can never go into.
NOT_A_FIELD = {"row", "button", "link", "heading", "image", "img", "tab", "menuitem", "option",
               "checkbox", "radio", "listitem", "list", "table", "navigation"}

# What typing does go into, for naming the fields a refusal points at.
TYPING_ROLES = {"textbox", "document", "combobox", "searchbox", "spinbutton", "password text", "entry", "editbar"}


# How long a custom dropdown has to show its options once clicked, and once typed into.
CUSTOM_OPTIONS_SECONDS = 6.0
# The least a list is given to answer a search before two equal readings of it count as its answer.
MIN_LIST_ANSWER = 2.0
CUSTOM_OPEN_SECONDS = 1.5

# What a dropdown's list shows: its options, and in many kits (the web app) plain links.
_SUGGESTION_ROLES = ("option", "menuitem", "listitem", "link")

# Entries a list offers besides the choices: they make or search, never pick.
# (A line of that kind often opens with an icon: a character that is no letter.)
_NOT_A_CHOICE = re.compile(r"^[\W_]*(create|add|search more|advanced search|new|filter(?:ed)? by|no (?:results|matches))\b", re.I)


def _offers_nothing(before, after, field=None):
    """Whether typing into a suggesting field left a list whose new entries only create or search - none to pick."""

    known = {e.get("path") for e in (before or {}).get("elements") or []}
    offered = [e for e in (after or {}).get("elements") or []
               if e.get("path") not in known and e.get("role") in _SUGGESTION_ROLES and e.get("name")]
    if offered and all(_NOT_A_CHOICE.match(e["name"]) for e in offered):
        return True
    # A list that was open before the typing shows nothing "new": judge what it holds now. Found live - the
    # field had been clicked first, so its create and search lines were already on the page, and the typing
    # that emptied the list of every real entry was not seen to have left none.
    shown = _list_under(field, after)
    return bool(shown) and all(_NOT_A_CHOICE.match(e["name"]) for e in shown)


# How far below a field its list of suggestions may reach.
LIST_REACH = 420


def _list_under(field, observation):
    """
    The options of the list a field has open: those drawn right under it (or
    over it), in its own column. A page holds other options too - every
    <select> on a form has its own, wherever it is - and found live they made
    an emptied list of suggestions look as if it still offered something.
    """

    box = (field or {}).get("box")
    if not box:
        return []
    left, top, right, bottom = box[0], box[1], box[0] + box[2], box[1] + box[3]
    out = []
    for e in (observation or {}).get("elements") or []:
        b = e.get("box")
        if e.get("role") != "option" or not e.get("name") or not b or "offscreen" in (e.get("states") or []):
            continue
        if b[0] < right and b[0] + b[2] > left and bottom - 12 <= b[1] <= bottom + LIST_REACH:
            out.append(e)
    if out:
        return out
    # No room below: a list drawn over its field ends at the field's top.
    return [e for e in (observation or {}).get("elements") or []
            if e.get("role") == "option" and e.get("name") and e.get("box")
            and "offscreen" not in (e.get("states") or [])
            and e["box"][0] < right and e["box"][0] + e["box"][2] > left
            and top - LIST_REACH <= e["box"][1] and e["box"][1] + e["box"][3] <= top + 12
            and top - (e["box"][1] + e["box"][3]) <= 160]


# How many of a suggesting field's entries are named after typing into it.
OFFERED_SHOWN = 8


def _offered(after, field=None):
    """The entries a suggesting field's list shows now that can be picked - not its "create" or "search" lines."""

    # Options only: a page's links and menu items are there whatever is typed.
    return [e for e in _list_under(field, after) if not _NOT_A_CHOICE.match(e["name"])]


def _words(text):
    return set(re.findall(r"\w+", (text or "").lower()))


def _pick(candidates, option):
    """The one candidate labelled `option`: exact, else starting with it, else holding all its words."""

    want = option.strip().lower()
    wanted = _words(option)
    tests = (lambda n: n == want, lambda n: n.startswith(want), lambda n: wanted and wanted <= _words(n))

    for test in tests:
        hits = [e for e in candidates if test((e.get("name") or "").strip().lower())]
        if len(hits) == 1:
            return hits[0]
        if len(hits) > 1:
            raise ComputerError(f"several options fit {option!r}: " + ", ".join(sorted({h['name'] for h in hits})[:8])
                                + " - pick one yourself")
    return None


_PLAIN_NUMBER = re.compile(r"[+-]?\d[\d,]*(?:\.\d+)?")


_DECORATED_NUMBER = re.compile(r"(?:[^\w\s]{1,3}\s*|[A-Za-z]{1,3}\.?\s+|[A-Za-z]{1,3}\.)?[-+]?[\d.,]+(?:\s*[^\w\s]{1,3}|\s+[A-Za-z]{1,4})?")


def _decorated_number(text, held):
    """Whether `text` is a number written with something a number field does not take, for a field that held a plain one."""

    text, held = (text or "").strip(), str(held or "").strip()
    if not held or not _PLAIN_NUMBER.fullmatch(held) or _PLAIN_NUMBER.fullmatch(text):
        return False
    # A decoration is a sign or a unit beside the number ("Rs 3,306", "3306 INR", "15 kg"), never a code whose letters
    # are joined to its digits ("INV3306", "ABC-3306"): found live (2026-10-26), a bill number typed into a field that
    # had held the form's default "1" drew "a number field takes digits only", and the worker asked the person.
    return bool(_DECORATED_NUMBER.fullmatch(text))


def _stem(word):
    """A word as compared across a singular and a plural: "Chairs" and "Chair" are one."""

    word = word.lower()
    return word[:-1] if len(word) > 3 and word.endswith("s") and not word.endswith("ss") else word


def _singular(word):
    """A plural word in the singular, as typed into a search ("Mugs" -> "Mug", "T-shirts" -> "T-shirt"); else as it is."""

    return word[:-1] if _stem(word) != word.lower() else word


def _own_name(label):
    """
    The words of an entry's own name. A list often shows an entry as its name
    and then its description, which begins with the name again ("Leased Line
    Leased Line, Services"): the part said twice is the name.
    """

    tokens = [_stem(t) for t in re.findall(r"\w+", label or "")]
    for size in range(len(tokens) // 2, 0, -1):
        if tokens[:size] == tokens[size:2 * size]:
            return tokens[:size]
    return tokens


def _own_label(label):
    """An entry's own name as the list writes it ("USB-C Monitor" of "USB-C Monitor USB-C Monitor, Products")."""

    spans = [m.span() for m in re.finditer(r"\w+", label or "")]
    size = len(_own_name(label))
    if not spans or size >= len(spans):
        return (label or "").strip()
    return label[:spans[size - 1][1]].strip()


def _named_within(label, text):
    """
    Whether an entry's whole name lies within `text`: every word of the name
    is a word of the text, and the name has at least two. A document describes
    a thing in more words than the application's record of it has - "Executive
    A4 Printing Paper 75 GSM (Box of 10 Reams)" for a record named "A4
    Printing Paper" - so the record is found inside the description, never the
    other way round. One shared word proves nothing and is not enough.
    """

    name = set(_own_name(label))
    return len(name) >= 2 and name <= {_stem(w) for w in re.findall(r"\w+", text or "")}


# ============================================================
# Nothing else in its place
#
# When a list has no entry for what was asked, the only honest moves are the
# same thing under the list's own name, or a question. Found live: a document
# named a company the application did not have; the search said so and listed
# what there was; the engine then selected another company from that list and
# saved the record under it. Its final check caught it - after the record
# existed. So a search that found nothing is remembered for its field, and an
# entry that shares no word with what was asked is refused there until a
# person has answered (watchers calls forget_failed_search on a reply).
# ============================================================

FAILED_SEARCH_FILE = config.DECURIX_DIR / "computer" / "failed-search.json"
FAILED_SEARCH_SECONDS = 1800
# Words that say what kind of body or thing something is, not which one.
GENERIC_WORDS = {"ltd", "limited", "pvt", "private", "inc", "llp", "llc", "co", "company", "corp", "corporation", "plc",
                 "gmbh", "the", "and", "of", "for", "with", "new", "group", "services", "service", "products", "product"}


def _telling_words(text):
    return {w for w in (_stem(t) for t in re.findall(r"\w+", text or "")) if len(w) >= 3 and w not in GENERIC_WORDS}


def _note_failed_search(element, wanted):
    try:
        FAILED_SEARCH_FILE.parent.mkdir(parents=True, exist_ok=True)
        FAILED_SEARCH_FILE.write_text(json.dumps({"at": time.time(), "task": _task_now(), "field": element.get("name") or "",
                                                  "role": element.get("role") or "", "wanted": wanted}), encoding="utf-8")
    except OSError:
        pass


def forget_failed_search():
    """A person has answered, or the search has since found its entry: what failed before no longer binds."""

    try:
        FAILED_SEARCH_FILE.unlink()
    except OSError:
        pass


def _in_place_of(element, entry_name):
    """
    What was searched for in `element` (a field) and not found, when
    `entry_name` shares no telling word with it - i.e. the entry would stand in
    for something it is not; else None.
    """

    try:
        kept = json.loads(FAILED_SEARCH_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if time.time() - float(kept.get("at") or 0) > FAILED_SEARCH_SECONDS:
        return None
    # Of the task it happened in only. Found live: the next task could not select its own, correct record,
    # because the task before it had searched that field for something else and found nothing.
    if kept.get("task") != _task_now():
        return None
    if element is not None and (kept.get("field"), kept.get("role")) != (element.get("name") or "", element.get("role") or ""):
        return None
    wanted = kept.get("wanted") or ""
    if _telling_words(wanted) & _telling_words(" ".join(_own_name(entry_name)) or entry_name):
        return None
    if _person_said(wanted, entry_name):
        return None
    if _in_documents(entry_name):
        return None
    return wanted


_DOCUMENT_WORDS = {}


def _in_documents(entry_name):
    """
    Whether one of the documents this task's request brought names `entry_name`
    (every telling word of its own name is in the document). Such an entry is
    this run's value, not something in its place. Found live (2026-10-13): a
    worker read a remittance's addressee ("To: Example Inc - Accounts
    Receivable") as the paying customer, searched for it, found nothing - and
    the guard then refused the payer the remittance itself names, so the
    worker could not correct its own misreading and asked a person.
    """

    words = _telling_words(" ".join(_own_name(entry_name)) or entry_name)
    if not words:
        return False
    try:
        from decurix_agent import task_context
        from decurix_agent.tools import office
        paths = task_context.current_files()[:4]
    except Exception:
        return False
    for path in paths:
        if path not in _DOCUMENT_WORDS:
            try:
                _DOCUMENT_WORDS[path] = _telling_words(office.read_document(path).get("text") or "")
            except Exception:
                _DOCUMENT_WORDS[path] = set()
        if words <= _DOCUMENT_WORDS[path]:
            return True
    return False


def _typed_in_place_of(element, text):
    """What was searched for in a field that looks records up and not found, when `text` typed into it now is another thing; else None."""

    if element is None or not text or element.get("role") not in _SEARCHED_ROLES:
        return None
    return _in_place_of(element, text)


def _person_said(wanted, entry_name):
    """
    Whether the person's own words for this task name `entry_name` for what
    was searched for: one sentence of theirs that holds both. Found live: a
    person taught "a van, a truck and a lorry are all the kind Goods Vehicle";
    the document said lorry, the list had no lorry, and the entry they had named
    for it was refused as "another thing in its place" - so the worker asked
    them what they had already said. A list's entry nobody named for it is
    still refused.
    """

    try:
        from decurix_agent import guidance
        said = guidance.persons_words()
    except Exception:
        return False

    entry = " ".join(re.findall(r"\w+", (entry_name or "").lower()))
    own = " ".join(_own_name(entry_name))
    # The words that are the searched value's own, not the entry's: the sentence must tie one of them to the entry.
    telling = _telling_words(wanted) - _telling_words(entry_name)
    if not said or not entry or not telling:
        return False

    for sentence in re.split(r"[.;!?\n]+|\",\s*\"", said):
        words = re.findall(r"\w+", sentence.lower())
        plain, stemmed = " " + " ".join(words) + " ", " " + " ".join(_stem(w) for w in words) + " "
        if (f" {entry} " in plain or (own and f" {own} " in stemmed)) and telling & set(stemmed.split()):
            return True
    return False


SUBSTITUTE = ("{field} has no entry for {wanted!r} - the search for it found none - and {entry!r} shares no word with "
              "it: that is another thing, not this one under another name, and nothing was chosen. Do not put a "
              "different record in its place. If what is asked for is not in the list, stop and ask the person (HELP), "
              "saying what the document names and what the list offers")


# What select chose, when it was not an entry spelt as it was asked for: act() says so in its result.
_selected = {"name": None, "why": ""}

# How many searches a suggesting field is given before select says what it offered.
MAX_SEARCHES = 10
# A value's head searched as runs of its words only up to this many words; beyond it, by its distinctive words.
MAX_RUN_WORDS = 6
MAX_WORD_SEARCHES = 3
# Words that name nothing on their own.
FILLER_WORDS = {"about", "after", "their", "there", "these", "those", "which", "where", "while", "under", "other",
                "between", "within", "without", "through", "including", "against", "during", "before"}


def _searches(option):
    """
    What to type into a searching dropdown, most specific first: the value,
    what comes before its first bracket or comma, its first two words, its
    first word. A list searches for the typed text as one piece, so a variant
    written in another order ("Desk (White, Aluminium)" for the web app's "Desk
    (Aluminium, White)") finds nothing until the search is shorter; the pick is
    still judged against the whole value (_pick).
    """

    words = option.split()
    head = re.split(r"[(,]", option)[0].strip()
    tries = [option.strip(), head]
    # Runs of a short head's words from inside it, longest first: a bill writes "Dedicated Internet Leased Line (ILL)
    # 1 Gbps" where the application's item is "Internet Leased Line" - found live, every search began with "Dedicated" and
    # the item was never offered, so the engine asked a person for something the list had.
    hw = head.split()
    if len(hw) <= MAX_RUN_WORDS:
        for n in range(len(hw) - 1, 1, -1):
            for i in range(1, len(hw) - n + 1):
                run = " ".join(hw[i:i + n])
                tries.append(run)
                # ...and in the singular right after it: a list matches text as a piece, so an order's "coffee
                # mugs" never finds the record "Coffee Mug" (found live, 2026-10-13).
                if _singular(hw[i + n - 1]) != hw[i + n - 1]:
                    tries.append(" ".join(hw[i:i + n - 1] + [_singular(hw[i + n - 1])]))
    # Then its most distinctive words alone, longest first. A long line holds the system's short name somewhere in
    # the middle, and a list searches for a piece of text, not for words: found live, "Professional Fees for the
    # Statutory Audit & Compliance Filing Q2" never found "Statutory Audit & Compliance" (the "&" breaks
    # every run of words), and the engine, offered nothing, clicked an unrelated entry of the open list.
    # A word keeps the punctuation inside it ("T-shirts"): stripped to "Tshirt" it matched nothing (found live).
    bare = lambda w: re.sub(r"^\W+|\W+$", "", w)
    distinct = sorted({w for w in (bare(w) for w in hw) if len(re.sub(r"\W", "", w)) >= 5 and w.lower() not in FILLER_WORDS},
                      key=lambda w: (-len(w), hw.index(next(x for x in hw if bare(x) == w))))
    # Searched in the singular: a list matches the typed text as a piece, and a document's "Monitors" is not in a
    # record named "Monitor" (found live: the plural found nothing, and the record was one letter away).
    tries += [_singular(w) for w in distinct[:MAX_WORD_SEARCHES]]
    tries += [" ".join(words[:2]), words[0] if words else ""]
    out = []
    for t in tries:
        if len(t) >= 3 and t not in out:
            out.append(t)
    return out[:MAX_SEARCHES]


_SEARCHED_ROLES = ("combobox", "textbox", "searchbox")
EDITOR_SECONDS = 3.0


def _editor_opened_by(element):
    """The typable field of the page that has the keyboard after `element` (not a field itself) was pressed, or None."""

    deadline = time.monotonic() + EDITOR_SECONDS
    while True:
        for e in observe()["elements"]:
            if (e.get("source") == "cdp" and e.get("role") in _SEARCHED_ROLES and e.get("path") != element.get("path")
                    and "focused" in (e.get("states") or [])):
                return e
        if time.monotonic() >= deadline:
            return None
        time.sleep(0.3)


def _choose_radio(element, option):
    """
    Choose `option` among the radio buttons of the group `element` is in: the one named it, else the one whose name
    it starts with, else the one radio whose whole name lies within it ("Contractor (12 months)" -> Contractor) - never
    one of two.
    """

    group = (element.get("path") or "").rsplit(" > ", 1)[0]
    radios = [e for e in observe(remember=False)["elements"]
              if e.get("role") == "radio" and (e.get("path") or "").rsplit(" > ", 1)[0] == group]
    want = " ".join(option.lower().split())
    words = lambda s: " " + re.sub(r"[^a-z0-9]+", " ", s.lower()).strip() + " "
    hit = ([e for e in radios if " ".join((e.get("name") or "").lower().split()) == want]
           or [e for e in radios if (e.get("name") or "").lower().startswith(want)]
           or [e for e in radios if words(e.get("name") or "").strip() and words(e.get("name") or "") in words(option)])
    if len(hit) != 1:
        offered = ", ".join(el._quote(e.get("name") or "") for e in radios) or "none"
        raise ComputerError(f"no single radio button in that group is {el._quote(option)} - it offers {offered}")
    if element.get("source") == "cdp":
        cdp.click(hit[0])
        return "cdp"
    x, y = el.center(hit[0])
    xinput.click(x, y)
    return "xdotool"


# The resolution facts the select being made wrote (resolve.py): act() puts them in its summary. One line each.
_resolved = []
# Fields in which a search is sent with Enter when typing alone showed nothing (a page that answers a query once sent);
# never a combobox, whose Enter takes the highlighted entry.
_QUERY_ROLES = ("textbox", "searchbox")
# How many name searches a list page is sent after the identifier found nothing: each is a page load.
MAX_SENT_NAME_SEARCHES = 4


def _keep_fact(track, ident, line):
    """One resolution fact: kept for the gate (evidence note), for the summary (_resolved) and against a second one."""

    from decurix_agent.computer import resolve

    track["facts"][ident] = line
    resolve.note(line)
    _resolved.append(line)


def _wait_page_change(before, seconds=None):
    """Whether the page's elements changed within `seconds` of a query sent (paths, as `before` has them)."""

    deadline = time.monotonic() + (CUSTOM_OPTIONS_SECONDS if seconds is None else seconds)
    while True:
        if {e.get("path") for e in observe()["elements"]} != before:
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.3)


def _row_entries(now):
    """The entries of a page's table rows (a link beside cells): what a list page offers once a query was sent to it."""

    from decurix_agent.computer import resolve

    cells = [e for e in now if e.get("role") in ("cell", "gridcell", "rowheader") and e.get("box")]
    return [e for e in now if e.get("source") == "cdp" and e.get("box") and e.get("role") in _SUGGESTION_ROLES
            and not _NOT_A_CHOICE.match(e.get("name") or "") and any(resolve.same_row(e, c) for c in cells)]


def _matched_by_identifier(field, elements, ids, track):
    """
    Choose, among `elements`, the one record holding one of `ids` (resolve.matches): True when it was chosen - or
    found, when its row has nothing to press - and its fact kept. Two records holding one identifier are not chosen
    between: the ambiguity is kept and raised. False when none holds any. A field's own value never counts.
    """

    from decurix_agent.computer import resolve

    pool = [e for e in elements if e.get("source") == "cdp" and e.get("role") not in TYPING_ROLES
            and e.get("path") != field.get("path")]
    for ident in ids:
        if track["facts"].get(ident):
            continue
        found = resolve.matches(pool, ident)
        if not found:
            continue
        if len(found) > 1:
            _keep_fact(track, ident, resolve.ambiguous_line(ident, [name for _, name in found]))
            raise ComputerError(f"{el.describe(field)}: {len(found)} records hold {ident} - "
                                + ", ".join(el._quote(name, 60) for _, name in found)
                                + ". One identifier, two records: none was chosen. Use the one the documents or the "
                                "person name; when neither does, ask the person which (HELP) and tell them the "
                                "application holds two.")
        entry, name = found[0]
        if entry is not None:
            cdp.click(entry)
        _selected.update(name=name, why=f"matched by identifier {ident}")
        _keep_fact(track, ident, resolve.matched_line(ident, name))
        forget_failed_search()
        return True
    return False


def _select_datalist(element, option, ids=None):
    """
    A field with a <datalist>: the browser draws its list outside the page, so nothing "appears" to search. Its entries
    are read through the page instead; the one that is `option` (or the one entry holding one of `ids`, or the one
    entry whose own name lies within the value) is typed exactly, and the field keeps it. "cdp" when done; None when
    the field has no datalist. Found live (2026-10-26): searched for "Sales@18%" in such a field, the shorter searches
    left "Sales@18" in it and the application refused the save.
    """

    from decurix_agent.computer import resolve

    try:
        entries = cdp.datalist_options(element)
    except (ComputerError, KeyError, TypeError):
        return None
    if entries is None:
        return None
    want = " ".join(option.split()).casefold()
    pick = None
    for ident in ids or []:
        holders = list(dict.fromkeys(e for e in entries if resolve.holds(e, ident)))
        if len(holders) == 1:
            pick = holders[0]
            line = resolve.matched_line(ident, pick)
            resolve.note(line)
            _resolved.append(line)
            break
        if len(holders) > 1:
            line = resolve.ambiguous_line(ident, holders)
            resolve.note(line)
            _resolved.append(line)
            raise ComputerError(f"{el.describe(element)}: {len(holders)} entries hold {ident} - " + ", ".join(el._quote(h, 60) for h in holders))
    if pick is None and ids:
        # The list shows names only; the identifier was resolved on the application's list page (a resolution fact
        # names the record): that record's entry is the one. Found live (2026-10-26): the worker asked the picker for
        # a group it had read off a row and was offered the real ledger.
        from decurix_agent import task_context
        for ident in ids:
            found = resolve.resolution(task_context.current(), ident)
            if found.get("status") == "matched" and found.get("name"):
                match = [e for e in entries if " ".join(e.split()).casefold() == " ".join(found["name"].split()).casefold()]
                if match:
                    pick = match[0]
                    _resolved.append(found["line"])
                    break
    if pick is None:
        exact = [e for e in entries if " ".join(e.split()).casefold() == want]
        pick = exact[0] if exact else None
    if pick is None:
        within = [e for e in entries if _named_within(e, option)]
        if len(within) == 1:
            pick = within[0]
    if pick is None:
        offered = ", ".join(el._quote(e, 50) for e in entries[:OFFERED_SHOWN]) + (f" (+{len(entries) - OFFERED_SHOWN} more)" if len(entries) > OFFERED_SHOWN else "")
        how = (f" The list shows names, not identifiers: resolve {', '.join(ids)} on the application's own list page "
               f"first (gui select <its Find field> \"name\" --id {ids[0]}), then choose the record it names here." if ids else
               " Choose one of them by its exact name if it is the same thing; if none is, ask.")
        raise ComputerError(f"{el.describe(element)} offers no entry that is {el._quote(option, 60)}. Its entries: "
                            f"{offered or 'none'}.{how}")
    cdp.type_text(element, pick)
    _selected.update(name=pick, why="an entry of the field's own list" if not _resolved else _resolved[-1])
    return "cdp"


def _select_native_by_id(element, option, ids):
    """
    In a native <select>, choose the one option whose label holds one of `ids`: "cdp" when chosen. None when the
    element is no <select> (the drawn list is searched instead) or no label holds any of them (the choice by name
    follows; the whole list was read, so the fact says none holds it). Two labels holding one identifier are not
    chosen between.
    """

    from decurix_agent.computer import resolve

    try:
        labels = cdp.native_options(element)
    except (ComputerError, KeyError, TypeError):
        return None
    if not labels:
        return None
    for ident in ids:
        holders = [label for label in labels if resolve.holds(label, ident) and not resolve._NOT_A_RECORD.search(label)]
        names = list(dict.fromkeys(" ".join(label.split()) for label in holders))
        if not names:
            continue
        if len(names) > 1:
            line = resolve.ambiguous_line(ident, names)
            resolve.note(line)
            _resolved.append(line)
            raise ComputerError(f"{el.describe(element)}: {len(names)} options hold {ident} - "
                                + ", ".join(el._quote(n, 60) for n in names)
                                + ". One identifier, two records: none was chosen. Use the one the documents or the "
                                "person name; when neither does, ask the person which (HELP).")
        cdp.select_option(element, holders[0])
        _selected.update(name=names[0], why=f"matched by identifier {ident}")
        line = resolve.matched_line(ident, names[0])
        resolve.note(line)
        _resolved.append(line)
        return "cdp"
    for ident in ids:
        line = resolve.none_line(ident, [ident], [option], labels)
        resolve.note(line)
        _resolved.append(line)
    return None


def _select_custom(element, option, ids=None):
    """
    Pick `option` in a dropdown a page drew itself (_select_in_list). With `ids` - the identifiers a document gives
    the record (a tax number, an account number, an email) - the list is searched by them first and an entry whose
    text holds one is the record whatever its name; what each identifier resolved to is kept as one fact
    (resolve.py: matched / no record holds it / ambiguous / unanswered), for the engine's summary and for the save
    gate (_unsearched_record). Found live (2026-10-26, twice): searched by the document's name alone, an existing
    party was made again.
    """

    ids = list(dict.fromkeys(" ".join(str(i).split()) for i in (ids or []) if str(i).strip()))
    _resolved.clear()
    if not ids:
        return _select_in_list(element, option, [], None)
    from decurix_agent.computer import resolve

    track = {"searched": {i: [] for i in ids}, "name_searches": [], "offered": [], "answered": False, "facts": {}}

    def close():
        for ident in ids:
            if track["facts"].get(ident) or not track["searched"][ident]:
                continue  # resolved already, or never searched for: nothing to say about it
            if track["answered"]:
                line = resolve.none_line(ident, track["searched"][ident], track["name_searches"], track["offered"])
            else:
                line = resolve.unanswered_line(ident, track["searched"][ident], el.describe(element))
            _keep_fact(track, ident, line)

    try:
        _select_in_list(element, option, ids, track)
    except ComputerError as exc:
        close()
        if _resolved:
            raise ComputerError(f"{exc}\n" + "\n".join(_resolved)) from exc
        raise
    close()


def _select_in_list(element, option, ids, track):
    """
    Pick `option` in a dropdown a page drew itself (no <select>: OrangeHRM's
    "-- Select --", the web app's customer and product fields, most component kits)
    the way a person does: open it - typing the value, then shorter searches,
    when it only suggests once typed into - then click the matching entry
    among what appeared. Only what appeared counts, so a link elsewhere on the
    page with the same words is never taken; "Create ..." and "Search
    More..." are never choices; two entries that fit equally are not guessed
    between. Live, typing a product and clicking the next field left the web app's
    line with no product at all.
    """

    def refresh(now):
        nonlocal element
        if not element.get("path"):
            return
        fresh = next((e for e in now if e.get("path") == element["path"]
                      and e.get("role") == element.get("role") and e.get("name") == element.get("name")), None)
        if fresh is not None:
            element = fresh

    def expanded():
        now = observe()["elements"]
        refresh(now)
        me = next((e for e in now if e.get("path") == element.get("path")), None)
        return now, bool(me and "expanded" in (me.get("states") or []))

    # A list already open (an earlier try left it) would count as on the page
    # before the click, so none of its entries would count as appearing.
    now, is_open = expanded()
    # Escape can close the enclosing row editor as well as its dropdown
    # (the web app). Use already-visible options instead of dismissing the list.
    known = {e["path"] for e in now if not (is_open and e["role"] in ("option", "menuitem"))}
    replaced = _in_place_of(element, option)
    if replaced:
        raise ComputerError(SUBSTITUTE.format(field=el.describe(element), wanted=replaced, entry=option))
    if not is_open:
        cdp.click(element)
    if element["role"] not in _SEARCHED_ROLES:
        # A cell of a table that becomes its editor when pressed: the field
        # that took the keyboard is the one to search. Found live: select
        # aimed at a row's "Item" cell (plain text until pressed) searched
        # nothing, reported the list as it opens - eight entries of thirteen -
        # as all there was, and the task stopped for a record the list had.
        editor = _editor_opened_by(element)
        if editor is not None:
            element = editor
    from decurix_agent.computer import resolve

    # (search, the identifier it is for or None): the identifiers' searches first, whole to digits, then the name's.
    searches = [(s, None) for s in _searches(option)] if element["role"] in _SEARCHED_ROLES else []
    if ids and element["role"] in _SEARCHED_ROLES:
        searches = [(s, ident) for ident in ids for s in resolve.searches(ident)] + searches
    deadline = time.monotonic() + CUSTOM_OPEN_SECONDS
    seen = []
    # Entries whose own name lies within the value, with the search that showed each.
    within, typed, last_names, typed_at = {}, None, None, 0.0
    # A page that answers a query once sent (Enter in a search box): found out when typing alone showed nothing.
    submitted, can_send = False, bool(ids) and element["role"] in _QUERY_ROLES
    # The page's rows before anything was typed: a list page's search box narrows rows that were there already, so
    # none of them "appears" - a change in them is the list's answer.
    rows_before = None
    _selected.update(name=None, why="")

    def offered(now):
        return [e for e in now if e["path"] not in known and e["role"] in _SUGGESTION_ROLES
                and e.get("source") == "cdp" and e.get("box")
                and not _NOT_A_CHOICE.match(e.get("name") or "")]

    def entries(now):
        # Once a query was sent, the page's rows are its answer: they were there before too, so none "appeared".
        return _row_entries(now) if submitted else offered(now)

    def send(now):
        # Enter, in a field whose form only asks (cdp.submit_query); then the page's answer. False when not sent.
        if not cdp.submit_query(element):
            return False
        if _wait_page_change({e.get("path") for e in now}) and track is not None:
            track["answered"] = True
        return True

    while True:
        now = observe()["elements"]
        refresh(now)
        fresh = [e for e in now if e["path"] not in known and e.get("source") == "cdp" and e.get("box")]
        if track is not None and fresh and not submitted:
            track["answered"] = True  # the list answered (entries, or its own "create" or "no results" line)
        if ids and element["role"] in _QUERY_ROLES:
            rows = tuple(e.get("name") for e in _row_entries(now))
            if rows_before is None:
                rows_before = rows
            elif track is not None and typed is not None and rows != rows_before:
                track["answered"] = True  # a list page narrowed its rows to the search: that is its answer
        candidates = entries(now)
        # Everything any search offered, in order: the engine chooses among real entries when none is the value.
        seen += [n for n in (e.get("name") or "" for e in candidates) if n and n not in seen]
        if track is not None:
            track["offered"] = list(seen)
        # An entry whose text holds the document's identifier is the record, whatever its name (resolve.py). Among
        # what appeared; or, once a page's search box was typed into or sent, among the whole page: a list page's
        # rows were there before the search narrowed them, and a row holding the identifier is a record holding it.
        whole_page = submitted or (typed is not None and element["role"] in _QUERY_ROLES)
        if ids and _matched_by_identifier(element, now if whole_page else fresh, ids, track):
            return
        hit = _pick(candidates, option)
        if hit is not None:
            cdp.click(hit)
            forget_failed_search()
            return
        for entry in candidates:
            if _named_within(entry["name"], option):
                # The latest search that showed it: the list as it first opened is not one that can be typed again.
                within[entry["name"]] = typed if typed is not None else within.get(entry["name"])

        # A list that reads the same twice running has answered the search: no need to wait it out.
        names = tuple(e.get("name") for e in candidates)
        # (Not before the list has had time to answer at all: on a slow host the old list reads the same twice.)
        settled = typed is not None and names == last_names and time.monotonic() - typed_at >= MIN_LIST_ANSWER
        last_names = names
        if time.monotonic() > deadline or settled:
            if can_send and typed is not None and not fresh and not submitted:
                # Typing alone showed nothing: a search box of a page that answers a query once sent. Sent (only a
                # form that asks, never one that saves), the page after it is the list's answer.
                can_send = False
                if send(now):
                    submitted = True
                    # Each page load costs seconds: the name is searched for in its most telling forms only - its
                    # first forms (whole, head, runs from inside it) and its first word, which a list page's search
                    # finds a trade name's relative by.
                    by_name = [s for s in searches if not s[1]]
                    searches = ([s for s in searches if s[1]]
                                + list(dict.fromkeys(by_name[:MAX_SENT_NAME_SEARCHES - 1] + by_name[-1:])))
                    deadline, last_names, typed_at = time.monotonic() + CUSTOM_OPTIONS_SECONDS, None, time.monotonic()
                    continue
            if not searches:
                break
            typed, ident = searches.pop(0)
            if track is not None:
                (track["searched"][ident] if ident else track["name_searches"]).append(typed)
            cdp.type_text(element, typed)
            if submitted:
                send(now)
            typed_at = time.monotonic()
            deadline, last_names = typed_at + CUSTOM_OPTIONS_SECONDS, None
        time.sleep(0.3)

    # No entry is spelt as the value is - but exactly one entry of all the
    # searches has a name that lies wholly within it: the application's record
    # of the thing the value describes. Found live, again and again: handed a
    # document's line for a value, an engine searched with the wrong words,
    # clicked entries by where it expected them and asked a person for a
    # record the list had. Two such entries are not chosen between.
    if len(within) == 1:
        (name, search), = within.items()
        # Brought back by its own name first - that is what the list holds -
        # then by the search that showed it. Found live on a slow host: a
        # list answered one search after the next had been typed, the entry
        # was remembered under a search that does not show it, and select
        # said "no option" for a record it had just seen.
        own = _own_label(name) if typed is not None else ""
        again = [s for s in dict.fromkeys([own, search]) if s] or [None]
        for search in again:
            if search is not None:
                cdp.type_text(element, search)
                if submitted:
                    send(now)
            deadline = time.monotonic() + CUSTOM_OPTIONS_SECONDS + (MIN_LIST_ANSWER if search else 0)
            while True:
                now = observe()["elements"]
                refresh(now)
                entry = next((e for e in entries(now) if e.get("name") == name), None)
                if entry is not None:
                    try:
                        cdp.click(entry)
                    except ComputerError:
                        # The list was drawn again between the look and the click: look again.
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(0.3)
                        continue
                    _selected.update(name=name, why="its whole name is part of that text and no other entry's is: it is the record for it - go on")
                    forget_failed_search()
                    return
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.3)

    # Leave the page as it was found: an open list covers what is under it.
    if expanded()[1]:
        cdp.close_list(element)
    if seen:
        message = (f"{el.describe(element)} has no option {option!r}. Searching for it and for parts of it, it "
                   f"offers: " + ", ".join(seen[:30]) + ". If one of these is the same thing under this system's own "
                   "name, select it by that name; if none is, say so")
        if len(within) > 1:
            message += (". More than one of them is named within that text (" + ", ".join(sorted(within)[:6])
                        + "), so none was taken")
        # A value written with its variant ("Stool (Navy)") against an entry
        # that is only its general item ("Stool", shown with its code as
        # "StoolD_0045"): say so, and what selecting it leads to.
        head = re.split(r"[(,]", option)[0].strip()
        general = [n for n in seen if head and head != option.strip() and n.lower().startswith(head.lower())]
        if len(general) == 1:
            message += (f". {general[0]!r} starts with {head!r}: it is likely the general item that {option!r} is a "
                        f"variant of - select {head!r} (or that entry), then choose the variant in what the page asks next")
        _note_failed_search(element, option)
        raise ComputerError(message)
    if element["role"] not in _SEARCHED_ROLES:
        # Nothing was searched, so nothing is known about what the list has.
        raise ComputerError(f"{el.describe(element)} is not a field and pressing it opened no list: nothing was "
                            f"searched for {option!r}, so this says nothing about whether it exists. Press the place "
                            "where the value goes (a row of a table opens its fields when pressed), look at the "
                            "screen, and select in the field that appears, by its ref")
    _note_failed_search(element, option)
    if element.get("source") == "cdp" and element.get("role") in _SEARCHED_ROLES:
        # The last, shorter search must not stay in the field (found live, 2026-10-26: "Sales@18" was left where
        # "Sales@18%" was asked for, and the application refused the save): the value asked for is put back whole.
        try:
            cdp.type_text(element, option)
        except ComputerError:
            pass
    raise ComputerError(f"{el.describe(element)} showed no options for {option!r}")


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


# Viewers that show an app running elsewhere (a Windows app on the host, a phone's app): a field there is a stand-in
# whose value comes from a later read of the real app's controls.
RELAYED_VIEWERS = ("/host-gui/view", "/phone-app/view")
# What in such a viewer is pressed through the app itself (cdp.press_in_app) rather than clicked.
RELAYED_PRESSABLE = ("button", "tab", "menuitem", "link", "checkbox", "radio", "treeitem")
# ...and what is given its text through the app itself (cdp.set_in_app) rather than typed key by key.
RELAYED_SETTABLE = ("textbox", "combobox", "searchbox", "spinbutton")
# What a viewer or the bridge says when it set nothing: only then is the text typed instead.
NOT_SET = re.compile(r"\bNOSET\b|not a field of the app|cannot set a value")


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
        return None
    if not any(s in str(exc).lower() for s in STALE_NODE):
        return None
    fresh = observe()
    try:
        again = el.find(fresh["elements"], ref=ref, role=role, name=name, path=path, active_group=active_group(fresh),
                        gone=fresh.get("gone"))
    except ComputerError:
        return None
    # ...and in the same window: the same tab, the same title (an approval judged for one document's control never
    # passes to another's).
    if again is None or again.get("role") != element.get("role") or again.get("name") != element.get("name")             or again.get("group") != element.get("group") or again.get("window_title") != element.get("window_title"):
        return None
    return again


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

    _refuse_number_arrows(kind, point)
    _refuse_unsafe_save(kind, element, keys, before)
    downloads_before = _downloads_snapshot() if kind in ("click", "double_click", "key") else None
    acted_at = time.time()
    require = None
    if check_form:
        # Every committing action an engine or a replay takes, however it is pressed (_commit_check). A replay's
        # (check_form="replay") listing of today's values is put to the person instead (AskFirst).
        held = _commit_check(kind, element, keys, before, checked=check_form == "checked", text=text,
                             listing="ask" if check_form == "replay" else True)
        if isinstance(held, AskFirst):
            require = str(held)
        elif held:
            raise ComputerError(held)

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

    if not decision.allowed:
        text = approval.request_text(decision.request)
        entry.update({
            "outcome": "needs_approval",
            "reason": decision.reason,
            "summary": f"{described} - paused for approval: {decision.reason}",
        })
        audit.record(entry, before=before["frame"], masks=masks)
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
                backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end, files=files, ids=ids)
            except ComputerError as exc:
                again = _redrawn(exc, element, ref, role, name, path)
                if again is None:
                    raise
                element = again
                backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end, files=files, ids=ids)
    except ComputerError as exc:
        entry.update({
            "outcome": "failed",
            "reason": str(exc),
            "summary": f"{described} - failed: {exc}",
        })
        audit.record(entry, before=before["frame"], masks=masks)
        raise
    except OSError as exc:
        # A connection that broke mid-action (a browser tab that crashed: BrokenPipeError on its DevTools socket) is a
        # failed action, recorded like any other - it may have half happened. Found in review (2026-11-02): it left
        # `gui` with exit 0 and no audit step.
        reason = (f"the connection to the app broke while acting ({type(exc).__name__}: {exc}) - it may or may not "
                  "have happened; observe before doing anything again")
        entry.update({"outcome": "failed", "reason": reason, "summary": f"{described} - failed: {reason}"})
        audit.record(entry, before=before["frame"], masks=masks)
        raise ActedError(reason) from exc

    settled = _wait_for_settle(before["frame"])
    after = observe()
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
    if kind == "type" and element is not None and text and not append and not relayed \
            and element.get("role") not in ("paragraph", "heading", "cell", "document"):
        again = _same_field(element, after)
        if again is not None and _typing_lost(again, text):
            time.sleep(RETYPE_PAUSE)
            _perform(kind, again, point, text, keys, direction, amount, option, append, end, files=files)
            settled = _wait_for_settle(before["frame"])
            after = observe()
            retyped = True

    if kind == "type" and element is not None and text and not append and element.get("role") == "textbox"             and not relayed:
        retained = _same_field(element, after)
        if retained is not None and _typing_lost(retained, text):
            reason = (f"{target} did not retain the requested text after retrying. Observe the field again and "
                      "commit it or use its picker; do not assume the value was entered.")
            entry.update({"outcome": "failed", "backend": backend, "reason": reason,
                          "summary": described + " - failed: " + reason})
            audit.record(entry, before=before["frame"], after=after["frame"] or settled,
                         masks=masks + privacy.secret_boxes(after["elements"]))
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

    audit.record(entry, before=before["frame"], after=after["frame"] or settled,
                 masks=masks + privacy.secret_boxes(after["elements"]))

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
    if kind in ("click", "key") and _saved(change_text):
        note += _ledger_readback(after, change_text)
    if downloads_before is not None:
        note += _downloads_note(downloads_before)

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


def text_change(old, new, spans=3, context=18):
    """
    What an edit did to a field's text, for an engine to check: the changed
    spans with a little of what stands around them. A whole-value quote cut
    at 40 characters showed the first line of a document and hid every change
    after it (found by the GUI benchmark: a word replaced in the third line of
    a file was invisible, and the engine reported every one replaced).
    """

    old, new = old or "", new or ""
    if len(old) <= 40 and len(new) <= 40:
        return f"{el._quote(old, 40)} -> {el._quote(new, 40)}"

    import difflib
    changes = [op for op in difflib.SequenceMatcher(None, old, new, autojunk=False).get_opcodes() if op[0] != "equal"]
    if not changes:
        return "unchanged"

    def quoted(text, i, j):
        start, end = max(0, i - context), min(len(text), j + context)
        piece = " ".join(text[start:end].replace("\n", " / ").split())
        return '"' + ("…" if start else "") + piece.replace('"', "'") + ("…" if end < len(text) else "") + '"'

    shown = []
    for tag, i1, i2, j1, j2 in changes[:spans]:
        line = old.count("\n", 0, i1) + 1
        shown.append(f"line {line}: {quoted(old, i1, i2)} -> {quoted(new, j1, j2)}")
    more = f" (+{len(changes) - spans} more changes)" if len(changes) > spans else ""
    return f"changed in {len(changes)} place{'s' if len(changes) != 1 else ''}: " + "; ".join(shown) + more


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

def compare(before, after):
    """What differs between two observations."""

    old = {e["path"]: e for e in before["elements"]}
    new = {e["path"]: e for e in after["elements"]}

    # A clock is not something an action did: the panel's ticking over from
    # 10:41 to 10:42 would otherwise be reported, and recorded as an expected
    # result, under every action (volatility.py).
    appeared = [e for p, e in new.items() if p not in old and not volatility.only_a_moment(e.get("name"))]
    disappeared = [e for p, e in old.items() if p not in new and not volatility.only_a_moment(e.get("name"))]
    changed = []

    for p, e in new.items():
        prior = old.get(p)
        if prior is None:
            continue
        fields = []
        old_value, new_value = prior.get("value") or "", e.get("value") or ""
        if old_value != new_value and not (volatility.only_a_moment(old_value) and volatility.only_a_moment(new_value)):
            fields.append(("value", old_value, new_value))
        before_states = set(prior.get("states", [])) - {"offscreen"}
        after_states = set(e.get("states", [])) - {"offscreen"}
        if before_states != after_states:
            fields.append(("states", ", ".join(sorted(before_states)) or "-", ", ".join(sorted(after_states)) or "-"))
        if fields:
            changed.append((e, fields))

    old_windows = {w.get("id", w["title"]): w["title"] for w in before["windows"]}
    new_windows = {w.get("id", w["title"]): w["title"] for w in after["windows"]}

    if "frame" in before and isinstance(before["frame"], screen.Frame):
        pixels = screen.changed_fraction(before["frame"], after["frame"])
    else:
        pixels = None

    focused = next((e for e in after["elements"] if "focused" in e.get("states", [])), None)

    return {
        "pixels": pixels,
        "active_before": before.get("active_title"),
        "active_after": after.get("active_title"),
        "windows_opened": sorted(t for i, t in new_windows.items() if i not in old_windows),
        "windows_closed": sorted(t for i, t in old_windows.items() if i not in new_windows),
        "url_before": before.get("page_url"),
        "url_after": after.get("page_url"),
        "appeared": appeared,
        "disappeared": disappeared,
        "changed": changed,
        "focused": focused,
    }


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


def summarize(change, limit=6):
    """One short paragraph: what an engine needs to know about the result."""

    parts = []

    for title in change["windows_opened"]:
        parts.append(f"window {el._quote(title, 60)} opened")
    for title in change["windows_closed"]:
        parts.append(f"window {el._quote(title, 60)} closed")

    if change["active_before"] != change["active_after"] and not change["windows_opened"]:
        parts.append(f"active window is now {el._quote(change['active_after'] or '(none)', 60)}")

    if change.get("url_after") and change.get("url_before") != change.get("url_after"):
        parts.append(f"page is now {(privacy.reduce_url(change['url_after']) or '')[:100]}")

    for element, fields in change["changed"][:limit]:
        for field, old, new in fields:
            if field == "value" and privacy.secret_field(element):
                parts.append(f"{el.describe(element)} value changed")
            elif field == "value":
                parts.append(f"{el.describe(element)} value " + text_change(old, new))
            else:
                parts.append(f"{el.describe(element)} [{old}] -> [{new}]")

    if change["appeared"]:
        appeared = change["appeared"]
        # What a Save or a Submit answers with is a few words appearing - "Successfully
        # Saved", "Should be a valid date in yyyy-dd-mm format" - usually plain text
        # beside a close button. Listing only the button ("button × (+2 more)") hid the
        # validation errors of a form that never saved, and the engine reported its
        # fields as filled. So messages come first, and a handful of new elements is
        # listed whole; only a new page's hundreds are cut to the controls.
        messages = [e for e in appeared if e["role"] in ("alert", "status", "dialog")]
        texts = [e for e in appeared if e["role"] in ("text", "paragraph", "heading", "label")
                 and (e.get("name") or "").strip()]
        controls = [e for e in appeared if e["role"] in el.INTERACTIVE_ROLES]
        if len(appeared) <= SMALL_CHANGE:
            ordered = messages + texts + [e for e in appeared if e not in messages and e not in texts]
            shown_list = ordered
        elif len(appeared) <= PANEL_CHANGE and change.get("url_before") == change.get("url_after") \
                and not change["windows_opened"]:
            # A panel opened in place (a ribbon tab, a toolbar, a sidebar): every control in it is listed, so the
            # next press can be made by a ref that was shown. Found live (2026-10-16): after Excel's Insert tab,
            # six of 35 new controls were listed "(+29 more)", and the engine guessed a ref from their numbering -
            # it pressed "Insert Pie or Doughnut Chart" for a column chart.
            shown_list = messages + [e for e in texts if e["role"] == "heading"] + controls or appeared[:limit]
        else:
            shown_list = (messages + [e for e in texts if e["role"] == "heading"] + controls)[:limit] or appeared[:limit]
        shown = ", ".join(el.describe(e) for e in shown_list)
        more = len(appeared) - len(shown_list)
        parts.append(f"{len(appeared)} element(s) appeared: {shown}" + (f" (+{more} more)" if more > 0 else ""))

    if change["disappeared"]:
        parts.append(f"{len(change['disappeared'])} element(s) disappeared")

    if change.get("focused") is not None:
        parts.append(f"focus: {el.describe(change['focused'])}")

    if change["pixels"] is not None:
        if change["pixels"] < 0.001 and len(parts) <= 1:
            return "nothing visible changed" + (f"; {parts[0]}" if parts else "") + " - check whether the action reached its target."
        percent = change["pixels"] * 100
        parts.append(("under 1%" if percent < 1 else f"{round(percent)}%") + " of the screen changed")

    return "; ".join(parts) if parts else "nothing visible changed"


# Up to this many new elements are all listed in an action's result.
SMALL_CHANGE = 10
# Up to this many, appearing in place (no new page or window), every new control is listed.
PANEL_CHANGE = 60


# Roles worth checking for on a replay, besides the ones that can be acted on.
NOTABLE_ROLES = ("dialog", "alert", "status", "heading")


def expectations(change, limit=8):
    """
    What an action left on the screen, as data rather than a sentence: the
    recording keeps it with the action, and replay.py mines what to check
    from it. Secret fields, and values past privacy.MAX_TEXT, are left out.
    """

    def brief(element):
        return {
            "role": element["role"],
            "name": privacy.kept_text(element.get("name")),
            "path": privacy.kept_text(element.get("path")),
        }

    def notable(elements):
        return [
            brief(e) for e in elements
            if e.get("name") and not privacy.secret_field(e)
            and (e["role"] in el.INTERACTIVE_ROLES or e["role"] in NOTABLE_ROLES)
        ][:limit]

    values = []
    for element, fields in change["changed"]:
        for field, _, new in fields:
            if field == "value" and not privacy.secret_field(element) and privacy.kept_text(new) is not None:
                values.append(dict(brief(element), value=new))

    url_changed = change.get("url_after") and change.get("url_before") != change.get("url_after")
    window_changed = change["active_before"] != change["active_after"]

    return {
        "url": privacy.reduce_url(change["url_after"]) if url_changed else None,
        "active_window": privacy.kept_text(change["active_after"]) if window_changed else None,
        "windows_opened": [privacy.kept_text(t) for t in change["windows_opened"][:limit]],
        "windows_closed": [privacy.kept_text(t) for t in change["windows_closed"][:limit]],
        "appeared": notable(change["appeared"]),
        "disappeared": notable(change["disappeared"]),
        "values": values[:limit],
        "pixels": round(change["pixels"], 4) if change["pixels"] is not None else None,
    }


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
        needle = expect_text.lower()
        haystack = [after.get("active_title") or ""] + [w["title"] for w in after["windows"]]
        for e in after["elements"]:
            haystack.append(e.get("name") or "")
            haystack.append(e.get("value") or "")
        ok = any(needle in (h or "").lower() for h in haystack)
        detail = f"text {el._quote(expect_text)} is {'on' if ok else 'NOT on'} the screen"

    if role or name:
        try:
            match = el.find(after["elements"], role=role, name=name)
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
            return f"Opened {url} in Chromium (tab title {el._quote(title, 60)})."

        if any(browser.is_browser_title(t) for t in before_titles):
            raise ComputerError(
                "The browser is already open without its debugging port. Close it first "
                "(key ctrl+shift+q in its window), then open the page again."
            )

        session.launch(browser.launch_argv(url))
        _wait_until(cdp.available, BROWSER_START_SECONDS, "Chromium did not start")
        title = cdp.wait_ready(timeout=15, default_title=url)
        _raise_browser()
        return f"Started Chromium on {url} (tab title {el._quote(title, 60)})."

    path = Path(target).expanduser()
    lower = target.lower().strip()

    if lower in CHROMIUM_APPS:
        if any(browser.is_browser_title(t) for t in before_titles) or cdp.available():
            _raise_browser()
            return "Brought Chromium to the front."
        session.launch(browser.launch_argv("about:blank"))
        _wait_until(cdp.available, BROWSER_START_SECONDS, "Chromium did not start")
        _raise_browser()
        return "Started Chromium."

    if path.exists():
        if path.suffix.lower() in OFFICE_EXTENSIONS:
            argv = ["soffice", "--nologo", "--norestore", str(path)]
        elif path.suffix.lower() in IMAGE_EXTENSIONS and shutil.which("gimp"):
            argv = ["gimp", "--no-splash", str(path)]
        elif path.suffix.lower() == ".svg" and shutil.which("inkscape"):
            argv = ["inkscape", str(path)]
        elif path.is_dir():
            argv = ["thunar", str(path)]
        elif path.suffix.lower() in (".html", ".htm", ".pdf", ".eml"):
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
        parts = target.split()
        if not shutil.which(parts[0]):
            icons = sorted({entry["name"] for entry in launchers.entries()})
            raise ComputerError(
                f"don't know how to open {target!r}: give a URL, an existing file path, or one of "
                + ", ".join(sorted(APPS))
                + ("; or the name of an app on this desktop: " + ", ".join(icons) if icons else "")
            )
        argv = parts

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

