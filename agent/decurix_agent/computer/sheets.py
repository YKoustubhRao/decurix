"""
Text typed into a spreadsheet's grid or onto a slide in one action, and read back (docs/computer-use.md).

Split out of core.py (follow-up O-10): core observes, acts and verifies; this module holds a block of cells typed
from one cell (rows by new lines, cells by Tabs) and read back cell by cell (_type_sheet_block, _read_back_block),
the same through a viewer of an app running elsewhere (RELAYED_VIEWERS; _type_relayed_rows, _read_back_relayed), and
a slide's text kept as typed (_keep_slide_text).

core imports this module and re-exports what other modules and tests read through it. This module never imports core:
the look at the screen and bringing a window to the front are core's, handed over at its import (use_core).
"""

import os
import re
import time

from decurix_agent.computer import atspi, cdp, xinput
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


# ============================================================
# What this module calls back in core
#
# The look at the screen (observe) and bringing a window to the front (_focus_window) live in core, which imports this module; so core hands them over at its import (use_core) instead of being
# imported here. Each is looked up in core when called: a test that patches core.observe or core._focus_window patches what this module
# reads.
# ============================================================

_CORE = {}


def use_core(**calls):
    """Take the calls this module makes back into core (observe, _focus_window) from core at its import."""

    _CORE.update(calls)


def observe(*args, **kwargs):
    """The screen as it is now (core.observe)."""

    return _CORE["observe"](*args, **kwargs)


def _focus_window(element):
    """Bring an element's window to the front (core._focus_window)."""

    return _CORE["_focus_window"](element)


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


# Viewers that show an app running elsewhere (a Windows app on the host, a phone's app): a field there is a stand-in
# whose value comes from a later read of the real app's controls.
RELAYED_VIEWERS = ("/host-gui/view", "/phone-app/view")
# What in such a viewer is pressed through the app itself (cdp.press_in_app) rather than clicked.
RELAYED_PRESSABLE = ("button", "tab", "menuitem", "link", "checkbox", "radio", "treeitem")
# ...and what is given its text through the app itself (cdp.set_in_app) rather than typed key by key.
RELAYED_SETTABLE = ("textbox", "combobox", "searchbox", "spinbutton")
# What a viewer or the bridge says when it set nothing: only then is the text typed instead.
NOT_SET = re.compile(r"\bNOSET\b|not a field of the app|cannot set a value")
