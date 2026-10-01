import time

from decurix_agent.computer import browser
from decurix_agent.computer import elements as el
from decurix_agent.computer import session
from decurix_agent.computer.errors import ComputerError


# ============================================================
# AT-SPI: GTK, XFCE and LibreOffice windows
#
# Walks every accessible application's showing windows and turns the widgets
# worth addressing into elements. Two things keep the walk bounded, and both
# matter on real apps rather than in theory:
#
# - Subtrees that are not SHOWING are skipped. A closed GTK menu keeps every
#   one of its items in the tree; mousepad alone is 300+ nodes, most of them
#   menu items nobody can see.
# - Nodes that MANAGE_DESCENDANTS with an enormous child count are not
#   descended into. A LibreOffice Calc sheet reports itself as a table of a
#   billion cells, created on request; walking it never finishes. Its used,
#   visible cells are read through the Table interface instead
#   (_sheet_cells). Writer's document sets the same flag with a handful of
#   children - its visible paragraphs - and is walked normally.
# ============================================================

MAX_NODES_PER_WINDOW = 4000
MAX_DEPTH = 40
WALK_SECONDS = 6

TOP_LEVEL_ROLES = {"frame", "dialog", "window", "alert", "file chooser"}

# Roles listed for reading even though they cannot be acted on.
READABLE_ROLES = {"label", "heading", "paragraph", "status", "alert", "notification", "caption"}

# Widget actions that mean "do the thing a click would do", best first.
CLICK_ACTIONS = ("click", "press", "activate", "jump", "toggle", "open")

TEXT_ROLES = {"textbox", "document", "paragraph", "cell", "terminal"}

# Never listed: structure a person does not act on.
SKIPPED_ROLES = {"separator", "filler", "panel", "scroll bar", "viewport"}

# Roles still worth listing without a name.
UNNAMED_OK = {"textbox", "document", "cell", "terminal", "combobox", "spinbutton", "slider"}

# A node that MANAGES_DESCENDANTS with more children than this is a
# spreadsheet-sized virtual table; fewer (a Writer document's visible
# paragraphs) is walked like anything else.
MAX_MANAGED_CHILDREN = 500

_atspi = None


def _lib():
    global _atspi

    if _atspi is not None:
        return _atspi

    session.ensure()

    try:
        import gi
        gi.require_version("Atspi", "2.0")
        from gi.repository import Atspi
    except (ImportError, ValueError):
        raise ComputerError(
            "the accessibility layer is not installed on this desktop "
            "(python3-gi, gir1.2-atspi-2.0); rebuild the desktop images"
        )

    _atspi = Atspi
    return _atspi


def _states(Atspi, node):
    try:
        state_set = node.get_state_set()
    except Exception:
        return set(), []

    has = state_set.contains
    S = Atspi.StateType

    flags = {
        "showing": has(S.SHOWING),
        "manages": has(S.MANAGES_DESCENDANTS),
        "single_line": has(S.SINGLE_LINE),
        "editable": has(S.EDITABLE),
        "focusable": has(S.FOCUSABLE),
    }

    shown = []
    if has(S.FOCUSED):
        shown.append("focused")
    if has(S.CHECKABLE) or has(S.CHECKED):
        shown.append("checked" if has(S.CHECKED) else "unchecked")
    if has(S.SELECTED):
        shown.append("selected")
    if has(S.EXPANDABLE):
        shown.append("expanded" if has(S.EXPANDED) else "collapsed")
    if not has(S.ENABLED) and not has(S.SENSITIVE):
        shown.append("disabled")

    return flags, shown


def _text(Atspi, node, limit=200):
    try:
        count = Atspi.Text.get_character_count(node)
        if count <= 0:
            return ""
        return Atspi.Text.get_text(node, 0, min(count, limit)) or ""
    except Exception:
        return ""


def _box(Atspi, node):
    try:
        ext = node.get_extents(Atspi.CoordType.SCREEN)
    except Exception:
        return None

    if ext.width <= 0 or ext.height <= 0 or ext.x < -10000 or ext.y < -10000:
        return None

    return [ext.x, ext.y, ext.width, ext.height]


def _actions(Atspi, node):
    try:
        count = Atspi.Action.get_n_actions(node)
        return [Atspi.Action.get_action_name(node, i) for i in range(count)]
    except Exception:
        return []


def observe(skip_apps=()):
    """
    Return (elements, groups) for every showing window of every accessible app
    other than those named in `skip_apps` (compared lowercase).
    """

    Atspi = _lib()

    try:
        desktop = Atspi.get_desktop(0)
        app_count = desktop.get_child_count()
    except Exception as exc:
        raise ComputerError(f"could not reach the accessibility bus: {exc}")

    found = []
    groups = []
    deadline = time.monotonic() + WALK_SECONDS

    for app_index in range(app_count):
        try:
            app = desktop.get_child_at_index(app_index)
            app_name = app.get_name() or ""
            pid = app.get_process_id()
        except Exception:
            continue

        if app is None or app_name.lower() in skip_apps:
            continue

        try:
            window_count = app.get_child_count()
        except Exception:
            continue

        for window_index in range(window_count):
            if time.monotonic() > deadline:
                break

            try:
                window = app.get_child_at_index(window_index)
                role = window.get_role_name()
            except Exception:
                continue

            if window is None:
                continue

            flags, _ = _states(Atspi, window)
            if not flags.get("showing"):
                continue

            title = browser.canonical_title(window.get_name() or "")
            group_id = f"atspi:{app_name}:{window_index}:{title}"
            kind = "dialog" if role in ("dialog", "alert", "file chooser") else "window"

            groups.append({
                "id": group_id,
                "title": f"{app_name} {kind} {el._quote(title, 80)}",
                "window_title": title,
                "pid": pid,
                "source": "atspi",
            })

            before = len(found)
            bars = []
            # An editor marks unsaved changes with a leading "*"; it is still
            # the same window, and its widgets must keep their refs.
            stable_title = title.lstrip("*").strip()
            _walk(
                Atspi, window, [app_index, window_index],
                [f"{app_name} {kind} {el._quote(stable_title, 60)}"],
                group_id, title, app_name, pid, found, deadline, bars,
            )
            name_fields_by_layout(found[before:])
            el.dedupe_paths(found[before:])

            if app_name == "soffice" and role == "frame":
                _correct_libreoffice_boxes(found[before:], bars)

    return found, groups


def libreoffice_offset(bars):
    """
    How far above where they are drawn LibreOffice reports its self-drawn
    widgets, in pixels (0 when they are reported where they are).

    LibreOffice reports the extents of everything VCL paints in a document
    window - the document, table cells, toolbars, sheet tabs - by an offset
    that depends on its state: measured on the same desktop, a freshly opened
    Calc reported them 25 px (the menu bar's height) too high, the pointer
    merely crossing the window made them right, and a no-op window move made
    them wrong again. An earlier desktop had them 27 px too high, the frame's
    y. The frame's own extents cannot say which: it reported (0,0) while the
    window was at (0,36). The native GTK menu bar is always right, though, and
    VCL's first toolbar is drawn directly below it - so a toolbar reported
    above the menu bar's bottom edge is off by exactly the overlap. That is
    measured every observation, so it holds whichever state the app is in.
    `bars` is [(raw role, box)] of the window's menu and tool bars.
    """

    menus = [box for role, box in bars if role == "menu bar" and box and box[3] > 0]
    tools = [box for role, box in bars if role == "tool bar" and box and box[3] > 0]

    if not menus or not tools:
        return 0

    menu_bottom = max(box[1] + box[3] for box in menus)
    top = min(box[1] for box in tools)

    # A toolbar can only be above the menu bar if it is misreported; one or
    # two pixels is a border, not an offset.
    return menu_bottom - top if menu_bottom - top > 2 else 0


def _correct_libreoffice_boxes(elements, bars):
    """
    Move LibreOffice's self-drawn widgets to where they are actually drawn
    (libreoffice_offset). The menu bar's items are native and left alone;
    dialogs are separate windows and never passed here.
    """

    dy = libreoffice_offset(bars)

    if not dy:
        return

    for element in elements:
        if element.get("bar") == "menu bar" or not element.get("box"):
            continue
        box = element["box"]
        element["box"] = [box[0], box[1] + dy, box[2], box[3]]


def _walk(Atspi, root, index_path, path, group_id, window_title, app_name, pid, found, deadline, bars=None):
    stack = [(root, index_path, path, 0, None, "", "")]
    visited = 0
    seen = set()

    while stack:
        node, indices, segments, depth, bar, parent_role, parent_name = stack.pop()
        visited += 1

        if visited > MAX_NODES_PER_WINDOW or time.monotonic() > deadline:
            return

        try:
            raw_role = node.get_role_name()
            name = (node.get_name() or "").strip()
        except Exception:
            continue

        flags, shown_states = _states(Atspi, node)

        if depth > 0 and not flags.get("showing"):
            continue

        role = el.normalize_role(raw_role)
        if role == "textbox" and not flags.get("editable") and raw_role == "text":
            role = "text"

        if not name and role in el.INTERACTIVE_ROLES:
            name = _label_of(Atspi, node)
        # The text field inside an editable GTK combo box ("Search for" in a
        # find dialog) has no name of its own; the combo box carries it.
        if not name and role == "textbox" and parent_role == "combobox":
            name = parent_name

        box = _box(Atspi, node)
        actions = _actions(Atspi, node)

        try:
            count = node.get_child_count()
        except Exception:
            count = 0

        value = ""
        if role in TEXT_ROLES or raw_role in ("paragraph", "heading"):
            value = _text(Atspi, node).strip()

        # A LibreOffice Writer table cell is named by its coordinate ("B1");
        # its words are in the paragraph inside it.
        if role == "cell" and not value:
            value = _child_text(Atspi, node)

        interactive = (role in el.INTERACTIVE_ROLES or bool(actions)) and role not in SKIPPED_ROLES
        readable = role in READABLE_ROLES and (name or value) and parent_role != "cell"
        # A spreadsheet that creates cells on request: listed, never walked.
        sheet = flags.get("manages") and count > MAX_MANAGED_CHILDREN

        # GTK tree views nest a cell per renderer inside a cell per column;
        # the outer one only repeats its children's text. An empty cell with
        # nothing inside is an icon renderer. A LibreOffice cell keeps its
        # words in a paragraph child, so an empty one still counts.
        if role == "cell" and count and _first_child_role(node) == "table cell":
            interactive = False
        if role == "cell" and not name and not value and not count:
            interactive = False

        # A button with no name cannot be asked for, and a model shown
        # "e26 toggle" learns nothing from it; fields are the exception, since
        # "the text box" is still a real thing to type into.
        if interactive and not name and role not in UNNAMED_OK:
            interactive = False

        # The taskbar lists each window as a button named with its title; a
        # browser window's is reported the way its window is (browser.py), so
        # a recorded click on it replays whichever browser build is installed.
        if name and browser.is_browser_title(name):
            name = browser.canonical_title(name)

        key = (role, name, tuple(box) if box else None)

        if depth > 0 and box and (interactive or readable or sheet) and key not in seen:
            seen.add(key)

            if sheet and not name:
                name = raw_role

            found.append({
                "role": "sheet" if sheet and role in ("table", "document") else role,
                "name": name or (value[:120] if role in ("paragraph", "heading") else ""),
                "value": "" if role in ("paragraph", "heading") else value,
                "states": shown_states,
                "box": box,
                "path": " > ".join(segments + [f"{role} {el._quote(name, 40)}"]),
                "source": "atspi",
                "group": group_id,
                "window_title": window_title,
                "app": app_name,
                "bar": bar,
                "handle": {
                    "indices": indices,
                    "raw_role": raw_role,
                    "actions": actions,
                    "single_line": bool(flags.get("single_line")),
                    "editable": bool(flags.get("editable")),
                    "pid": pid,
                },
            })

        if sheet:
            _sheet_cells(Atspi, node, indices, segments + [f"sheet {el._quote(name, 40)}"],
                         group_id, window_title, app_name, pid, found, deadline)
            continue

        if depth >= MAX_DEPTH:
            continue

        # Named containers become part of their children's path, so the same
        # "OK" button in two dialogs has two different paths.
        child_segments = segments
        if depth > 0 and name and raw_role in (
            "panel", "filler", "tool bar", "menu bar", "page tab", "table",
            "list", "tree", "form", "section", "scroll pane", "split pane",
            "dialog", "internal frame", "group", "grouping", "page tab list",
        ):
            child_segments = segments + [f"{el.normalize_role(raw_role)} {el._quote(name, 40)}"]

        child_bar = bar
        if raw_role in ("tool bar", "menu bar", "status bar"):
            child_bar = name or raw_role
            if bars is not None and raw_role != "status bar":
                bars.append((raw_role, box))

        # Pushed in reverse so they are visited, and listed, in reading order.
        for child_index in reversed(range(min(count, 500))):
            try:
                child = node.get_child_at_index(child_index)
            except Exception:
                continue
            if child is not None:
                stack.append((child, indices + [child_index], child_segments, depth + 1, child_bar, role, name))


SHEET_MAX_ROWS = 60
SHEET_MAX_COLUMNS = 26


def _sheet_cells(Atspi, table, indices, segments, group_id, window_title, app_name, pid, found, deadline):
    """
    List the used part of a spreadsheet's visible area as cell elements.

    A Calc sheet cannot be walked, but it answers "what is at row r, column c"
    for any cell. The visible rows and columns are found first (a cell that is
    not SHOWING is scrolled out of view), then every visible cell is read, and
    the listing is cut to the rectangle that holds content plus one empty row
    and column beyond it - which is where a model is usually asked to type.
    """

    S = Atspi.StateType

    def cell_at(r, c):
        try:
            cell = Atspi.Table.get_accessible_at(table, r, c)
        except Exception:
            return None
        if cell is None:
            return None
        try:
            if not cell.get_state_set().contains(S.SHOWING):
                return None
        except Exception:
            return None
        return cell

    rows = 0
    while rows < SHEET_MAX_ROWS and cell_at(rows, 0) is not None:
        rows += 1
    columns = 0
    while columns < SHEET_MAX_COLUMNS and cell_at(0, columns) is not None:
        columns += 1

    grid = {}
    last_row = last_col = -1

    for r in range(rows):
        if time.monotonic() > deadline:
            break
        for c in range(columns):
            cell = cell_at(r, c)
            if cell is None:
                continue
            text = _text(Atspi, cell).strip()
            grid[(r, c)] = (cell, text)
            if text:
                last_row, last_col = max(last_row, r), max(last_col, c)

    for r in range(min(rows, last_row + 2)):
        for c in range(min(columns, last_col + 2)):
            entry = grid.get((r, c))
            if entry is None:
                continue
            cell, text = entry
            name = (cell.get_name() or "").strip()
            _, states = _states(Atspi, cell)
            found.append({
                "role": "cell",
                "name": name,
                "value": text,
                "states": [s for s in states if s in ("focused", "selected")],
                "box": _box(Atspi, cell),
                "path": " > ".join(segments + [f"cell {el._quote(name, 12)}"]),
                "source": "atspi",
                "group": group_id,
                "window_title": window_title,
                "app": app_name,
                "bar": None,
                "sheet_row": r + 1,
                "handle": {
                    "indices": indices,
                    "raw_role": "table cell",
                    "actions": [],
                    "sheet_cell": True,
                    "row": r,
                    "column": c,
                    "pid": pid,
                },
            })


# The fields a label beside or above can name, when the toolkit left them nameless.
LAYOUT_NAMED_ROLES = {"textbox", "combobox", "spinbutton", "password text"}
# How far a label may sit to the left of its field, and above it.
LABEL_GAP_LEFT = 160
LABEL_GAP_ABOVE = 24


def name_fields_by_layout(elements):
    """
    Name a nameless field after the label on its row just to its left (or,
    failing that, just above it), when the application never tied the two
    together with LABELLED_BY.

    Found in GnuCash's New Invoice dialog: Customer, Job and Billing ID are
    three nameless text boxes in one panel, so all three had the path
    `panel "Billing Information" > textbox ""` and a recorded "type 4501
    into the Billing ID" could only be found again by its order among them -
    which changes the moment a customer is chosen and the Job row appears. A
    person reads the label beside a field; so does this. A field with labels
    on both sides of a tie, or none close enough, keeps no name rather than
    a guessed one. Changes the elements' name and path in place.
    """

    labels = [e for e in elements if e.get("role") == "label" and (e.get("name") or "").strip() and e.get("box")]

    for field in elements:
        if field.get("role") not in LAYOUT_NAMED_ROLES or (field.get("name") or "").strip() or not field.get("box"):
            continue

        label = _label_beside(field["box"], labels) or _label_above(field["box"], labels)
        if not label:
            continue

        name = label["name"].strip().rstrip(":").strip()
        if not name:
            continue
        field["name"] = name
        field["label_from"] = "layout"
        role = field["role"]
        empty = f"{role} {el._quote('', 40)}"
        if field.get("path", "").endswith(empty):
            field["path"] = field["path"][: -len(empty)] + f"{role} {el._quote(name, 40)}"


def _label_beside(box, labels):
    x, y, w, h = box
    middle = y + h / 2
    best = []
    for label in labels:
        lx, ly, lw, lh = label["box"]
        gap = x - (lx + lw)
        if not -4 <= gap <= LABEL_GAP_LEFT:
            continue
        if abs((ly + lh / 2) - middle) > max(6, h / 2):
            continue
        best.append((gap, label))
    return _closest(best)


def _label_above(box, labels):
    x, y, w, h = box
    best = []
    for label in labels:
        lx, ly, lw, lh = label["box"]
        gap = y - (ly + lh)
        if not -2 <= gap <= LABEL_GAP_ABOVE:
            continue
        if abs(lx - x) > 24:
            continue
        best.append((gap, label))
    return _closest(best)


def _closest(candidates):
    """The nearest label, or None when two are equally near (a guess either way)."""

    if not candidates:
        return None
    candidates.sort(key=lambda c: c[0])
    if len(candidates) > 1 and abs(candidates[0][0] - candidates[1][0]) < 2 \
            and candidates[0][1]["name"] != candidates[1][1]["name"]:
        return None
    return candidates[0][1]


def _first_child_role(node):
    try:
        child = node.get_child_at_index(0)
        return child.get_role_name() if child is not None else ""
    except Exception:
        return ""


def _label_of(Atspi, node):
    """The name of the label a widget is LABELLED_BY - "Name:" for a file name field."""

    try:
        for relation in node.get_relation_set() or []:
            if relation.get_relation_type() != Atspi.RelationType.LABELLED_BY:
                continue
            for i in range(relation.get_n_targets()):
                target = relation.get_target(i)
                label = (target.get_name() or "").strip().rstrip(":")
                if label:
                    return label
    except Exception:
        pass
    return ""


def _child_text(Atspi, node):
    parts = []
    try:
        for i in range(min(node.get_child_count(), 20)):
            child = node.get_child_at_index(i)
            text = _text(Atspi, child).strip()
            if text:
                parts.append(text)
    except Exception:
        pass
    return " ".join(parts)


# ============================================================
# Acting on an element
# ============================================================

def resolve(element):
    """Walk an element's index path back to its live accessible, or None."""

    Atspi = _lib()
    handle = element["handle"]
    indices = handle["indices"]

    try:
        node = Atspi.get_desktop(0)
        for index in indices:
            node = node.get_child_at_index(index)
            if node is None:
                return None
        if handle.get("sheet_cell"):
            return Atspi.Table.get_accessible_at(node, handle["row"], handle["column"])
        if node.get_role_name() != handle["raw_role"]:
            return None
    except Exception:
        return None

    return node


def default_action(element):
    """
    Invoke the widget's own click-like action. Returns the action's name, or
    None when the widget has none (the caller then clicks its centre).
    """

    Atspi = _lib()
    node = resolve(element)

    if node is None:
        return None

    names = _actions(Atspi, node)

    for wanted in CLICK_ACTIONS:
        if wanted in names:
            try:
                if Atspi.Action.do_action(node, names.index(wanted)):
                    return wanted
            except Exception:
                return None

    return None


def place_caret(element, at_end=True):
    """
    Put the text caret inside a document element - a paragraph, or a table
    cell's paragraph - through the accessibility Text interface.

    Not a click, on purpose. LibreOffice reports the on-screen extents of
    everything it draws itself (the document, its cells, its toolbars) too
    high - a click at a cell's reported centre landed in the line above it.
    _correct_libreoffice_boxes compensates for the offset it can measure, but
    setting the caret needs no coordinates at all, so it cannot be wrong on a
    geometry nobody measured.
    """

    Atspi = _lib()
    node = resolve(element)

    if node is None:
        return False

    target = node

    if element["role"] == "cell":
        try:
            for i in range(node.get_child_count()):
                child = node.get_child_at_index(i)
                if child is not None and child.get_role_name() in ("paragraph", "heading"):
                    target = child
                    break
        except Exception:
            return False

    try:
        count = Atspi.Text.get_character_count(target)
        return bool(Atspi.Text.set_caret_offset(target, count if at_end else 0))
    except Exception:
        return False


def grab_focus(element):
    Atspi = _lib()
    node = resolve(element)

    if node is None:
        return False

    try:
        return bool(Atspi.Component.grab_focus(node))
    except Exception:
        return False


def select_option(element, option):
    """
    Pick an entry in a GTK combo box by its label, through the menu the combo
    opens. Returns the label chosen, or raises with the labels on offer.
    """

    Atspi = _lib()
    node = resolve(element)

    if node is None:
        raise ComputerError(f"{el.describe(element)} is no longer on the screen")

    wanted = option.strip().lower()
    labels = []

    def visit(current, depth):
        if depth > 4:
            return None
        try:
            for i in range(current.get_child_count()):
                child = current.get_child_at_index(i)
                if child is None:
                    continue
                label = (child.get_name() or "").strip()
                if child.get_role_name() in ("menu item", "list item", "option") and label:
                    labels.append(label)
                    if label.lower() == wanted:
                        return child
                hit = visit(child, depth + 1)
                if hit is not None:
                    return hit
        except Exception:
            return None
        return None

    item = visit(node, 0)

    if item is None:
        on_offer = ", ".join(labels[:20]) or "none found"
        raise ComputerError(f"{el.describe(element)} has no option {option!r}. Options: {on_offer}")

    try:
        Atspi.Selection.select_child(node, item.get_index_in_parent())
        return item.get_name()
    except Exception:
        pass

    names = _actions(Atspi, item)
    for wanted_action in CLICK_ACTIONS:
        if wanted_action in names:
            Atspi.Action.do_action(item, names.index(wanted_action))
            return item.get_name()

    raise ComputerError(f"could not select {option!r} in {el.describe(element)}")
