import re

from decurix_agent.computer.errors import ComputerError


# ============================================================
# Interface elements: one shape for every backend
#
# AT-SPI (GTK, XFCE, LibreOffice) and the Chrome DevTools Protocol describe
# the same things in different words. Both are normalised here into one plain
# dict, so an engine addresses "button Send" the same way whichever toolkit
# drew it:
#
#   ref     short handle an engine types back ("e12"), stable across
#           observations for as long as the element's path is unchanged
#   role    normalised role ("button", "textbox", "checkbox", ...)
#   name    accessible name - the label a person would read
#   value   current contents of a field, if any
#   states  "focused", "checked", "disabled", ...
#   box     [x, y, w, h] on screen, or None
#   path    where it sits: window, then named containers, then itself
#   source  "atspi" or "cdp"; group is the window/tab it belongs to
#   handle  backend-private locator, never shown to an engine
# ============================================================

ROLE_ALIASES = {
    "push button": "button",
    "toggle button": "toggle",
    "check box": "checkbox",
    "check menu item": "menuitem",
    "radio menu item": "menuitem",
    "menu item": "menuitem",
    "radio button": "radio",
    "radio": "radio",
    "text": "textbox",
    "entry": "textbox",
    "password text": "textbox",
    "searchbox": "textbox",
    "combo box": "combobox",
    "popupbutton": "combobox",
    "spin button": "spinbutton",
    "page tab": "tab",
    "table cell": "cell",
    "gridcell": "cell",
    "columnheader": "header",
    "table column header": "header",
    "rowheader": "header",
    "list item": "listitem",
    "tree item": "treeitem",
    "treeitem": "treeitem",
    "menuitemcheckbox": "menuitem",
    "menuitemradio": "menuitem",
    "switch": "checkbox",
    "document text": "document",
    "document spreadsheet": "document",
    "document frame": "document",
    "rootwebarea": "page",
    "statictext": "text",
    "status bar": "status",
    "file chooser": "dialog",
    "alertdialog": "dialog",
}

# What can be acted on, after normalisation.
INTERACTIVE_ROLES = {
    "button", "toggle", "checkbox", "radio", "textbox", "combobox", "listbox",
    "option", "menu", "menuitem", "tab", "link", "slider", "spinbutton",
    "listitem", "treeitem", "icon", "cell", "header", "terminal",
}


def normalize_role(role):
    role = (role or "").strip().lower()
    return ROLE_ALIASES.get(role, role)


def center(element):
    box = element.get("box")
    if not box:
        return None
    x, y, w, h = box
    return (x + w // 2, y + h // 2)


def _quote(text, limit=80):
    text = " ".join(str(text).split())
    if len(text) > limit:
        text = text[: limit - 1] + "…"
    return '"' + text.replace('"', "'") + '"'


def describe(element):
    """`button "Send" (e4)` - how an element is named back to an engine."""

    if element is None:
        return "the screen"

    label = f"{element['role']} {_quote(element.get('name') or '')}"
    if element.get("row"):
        label += f" in row {element['row']}"
    if element.get("ref"):
        label += f" ({element['ref']})"
    return label


# ============================================================
# Refs
# ============================================================

def assign_refs(elements, previous):
    """
    Give each element a ref, reusing the previous observation's ref for any
    element at the same path.

    Numbering elements 1..N afresh on every observation would renumber
    everything after a single row is inserted, and an engine that had just
    read "e12 button Save" would click something else. Keyed by path, a ref
    means the same element for as long as that element is still there.
    """

    by_path = {}
    next_ref = 1

    if previous:
        next_ref = previous.get("next_ref", 1)
        for old in previous.get("elements", []):
            by_path.setdefault(old["path"], old["ref"])

    used = set()

    for element in elements:
        ref = by_path.get(element["path"])

        if ref is None or ref in used:
            ref = f"e{next_ref}"
            next_ref += 1

        element["ref"] = ref
        used.add(ref)

    return next_ref


def dedupe_paths(elements):
    """Suffix [2], [3]... onto paths that would otherwise collide."""

    seen = {}

    for element in elements:
        base = element["path"]
        count = seen.get(base, 0) + 1
        seen[base] = count
        if count > 1:
            element["path"] = f"{base}[{count}]"


# ============================================================
# Finding the element an engine meant
# ============================================================

REF_RE = re.compile(r"^e\d+$")
POINT_RE = re.compile(r"^\s*(\d{1,5})\s*[, ]\s*(\d{1,5})\s*$")


def parse_point(target):
    match = POINT_RE.match(target or "")
    if not match:
        return None
    return int(match.group(1)), int(match.group(2))


def _inside(box, outer):
    if not box or not outer:
        return False
    cx, cy = box[0] + box[2] / 2, box[1] + box[3] / 2
    return outer[0] <= cx <= outer[0] + outer[2] and outer[1] <= cy <= outer[1] + outer[3]


def find(elements, ref=None, role=None, name=None, path=None, active_group=None):
    """
    Resolve a target against a fresh observation, or raise a ComputerError
    that tells the engine what to do instead.
    """

    if ref:
        for element in elements:
            if element["ref"] == ref:
                return element
        raise ComputerError(
            f"{ref} is no longer on the screen (the window or page changed). "
            "Run observe again and use the new refs."
        )

    if path:
        # Paths aren't guaranteed unique across two windows that share a
        # title (dedupe_paths only dedupes within one window's own slice) -
        # take the first match silently and a future caller could act on
        # the wrong window's element. Raise the same way the role+name
        # branch below does on genuine ambiguity, rather than guessing.
        matches = [element for element in elements if element["path"] == path]

        if not matches:
            raise ComputerError(f"no element at path {path!r}. Run observe again.")

        if len(matches) > 1:
            listed = "; ".join(describe(e) for e in matches[:8])
            raise ComputerError(f"{len(matches)} elements share path {path!r} - use one of these refs: {listed}")

        return matches[0]

    if not role and not name:
        raise ComputerError("say which element: a ref like e12, or a role and name")

    wanted_role = normalize_role(role) if role else None
    candidates = [
        e for e in elements
        if wanted_role is None or e["role"] == wanted_role
    ]

    if name:
        wanted = " ".join(name.split()).lower()
        tiers = [
            [e for e in candidates if " ".join((e.get("name") or "").split()).lower() == wanted],
            [e for e in candidates if (e.get("name") or "").lower().startswith(wanted)],
            [e for e in candidates if wanted in (e.get("name") or "").lower()],
        ]
        matches = next((tier for tier in tiers if tier), [])
    else:
        matches = candidates

    if len(matches) > 1:
        interactive = [e for e in matches if e["role"] in INTERACTIVE_ROLES]
        matches = interactive or matches

    if len(matches) > 1 and active_group:
        in_active = [e for e in matches if e.get("group") == active_group]
        matches = in_active or matches

    # While a dialog is open over a page, its "Confirm" is the one meant - the
    # page's own (an the web app quotation's, which turns it into an order) sits under
    # the dialog and cannot be reached. Only when exactly one match is inside
    # the dialog on top.
    if len(matches) > 1:
        order = {id(e): i for i, e in enumerate(elements)}
        dialogs = [e for e in elements if e["role"] in ("dialog", "alertdialog") and e.get("box")
                   and "offscreen" not in (e.get("states") or [])]
        if dialogs:
            top = dialogs[-1]
            # Inside it: drawn within its box and after it in the page's order
            # (a modal's box can be the whole viewport, its backdrop included).
            inside = [e for e in matches if e.get("group") == top.get("group") and _inside(e.get("box"), top["box"])
                      and order.get(id(e), -1) > order[id(top)]]
            if len(inside) == 1:
                matches = inside

    if len(matches) > 1:
        visible = [e for e in matches if "offscreen" not in e.get("states", [])]
        matches = visible or matches

    if not matches:
        what = " ".join(p for p in (role, _quote(name) if name else "") if p)
        raise ComputerError(f"nothing on the screen matches {what}. Run observe to see what is there.")

    if len(matches) > 1:
        listed = "; ".join(describe(e) for e in matches[:8])
        raise ComputerError(f"{len(matches)} elements match - use one of these refs: {listed}")

    return matches[0]


def unoccluded(elements, windows, x, y, popups=None):
    """
    The elements that can be what a press at x,y lands on: not those of a
    window that another window covers at that point. A look at the screen
    lists every window's controls, the ones behind as readily as the ones in
    front - found live, a click on an Impress slide was taken as a click on
    the Calc sheet of a window left open behind it, and the worker never
    reached its slide. `windows` is observe's list, topmost last; an element
    whose window is not in it (a menu) is kept. `popups` are the boxes of open
    menus and drop-down lists, which are over every window: at a point inside
    one, only what lies within it counts (found live: GIMP's File menu hung
    over a viewer window behind GIMP, and a click on "Open..." was taken for
    that window's hidden "Name" button, thirty times).
    """

    for px, py, pw, ph in popups or []:
        if px <= x < px + pw and py <= y < py + ph:
            def inside(element, px=px, py=py, pw=pw, ph=ph):
                box = element.get("box")
                return bool(box) and px <= box[0] and py <= box[1] and box[0] + box[2] <= px + pw \
                    and box[1] + box[3] <= py + ph
            # The menu is over every window, whichever window its items say they belong to.
            return [element for element in elements if inside(element)]

    order = {}
    for index, window in enumerate(windows or []):
        if window.get("title"):
            order[window["title"]] = index

    def covered(element):
        index = order.get(element.get("window_title") or "")
        if index is None:
            return False
        for above in (windows or [])[index + 1:]:
            box = above.get("box")
            if box and box[0] <= x < box[0] + box[2] and box[1] <= y < box[1] + box[3]:
                return True
        return False

    return [element for element in elements if not covered(element)]


def at_point(elements, x, y, windows=None, popups=None):
    """The smallest element whose box contains a screen point, or None. With `windows` (observe's list, topmost
    last) and `popups` (open menus), the elements of a window covered at that point are left out."""

    if windows or popups:
        elements = unoccluded(elements, windows, x, y, popups)
    best = None
    best_area = None

    # A dialog drawn over a page takes every point inside it: what the page
    # has at that spot is underneath and cannot be pressed. Found live: with a
    # form open in a dialog, a point on the dialog was reported as a link of
    # the page behind it ("430,371 is link ...") and a scroll over the dialog
    # as a scroll of that link.
    order = {id(e): i for i, e in enumerate(elements)}
    dialogs = [e for e in elements if e["role"] in ("dialog", "alertdialog") and e.get("box")
               and "offscreen" not in (e.get("states") or [])]
    top = dialogs[-1] if dialogs else None
    if top is not None:
        tx, ty, tw, th = top["box"]
        if not (tx <= x < tx + tw and ty <= y < ty + th):
            top = None

    for element in elements:
        if top is not None and element is not top and (
                element.get("group") != top.get("group") or order[id(element)] < order[id(top)]):
            continue
        box = element.get("box")
        if not box or "offscreen" in element.get("states", []):
            continue
        bx, by, bw, bh = box
        if bx <= x < bx + bw and by <= y < by + bh:
            area = bw * bh
            interactive = element["role"] in INTERACTIVE_ROLES
            key = (0 if interactive else 1, area)
            if best is None or key < best_area:
                best, best_area = element, key

    return best


# ============================================================
# Rendering for an engine
# ============================================================

def element_line(element, coords=False):
    line = f"{element['ref']} {element['role']}"

    if element.get("name"):
        line += " " + _quote(element["name"])

    value = element.get("value")
    if value not in (None, "") and value != element.get("name"):
        line += " value=" + _quote(value, 60)
        if len(" ".join(str(value).split())) > 60:
            # The engine cannot see the rest, and must know there is a rest (gui read).
            lines = str(value).strip().count("\n") + 1
            line += f" ({len(value)} chars{f', {lines} lines' if lines > 1 else ''} - gui read {element['ref']} shows all)"

    states = [s for s in element.get("states", []) if s != "offscreen"]
    if element.get("row"):
        # Which line of a table of entries this field belongs to (cdp._ROW_NUMBERS).
        states = [f"row {element['row']}"] + states
    if states:
        line += " [" + ", ".join(states) + "]"

    if "offscreen" in element.get("states", []):
        # A page's element is brought into view by acting on it. Found live: told "scroll to reach", an engine
        # scrolled a form by hand, lost the fields above, clicked at guessed points and gave the form up for
        # another way in - leaving half of it unfilled.
        line += " (out of view - use its ref, no scrolling needed)" if element.get("source") == "cdp" else " (scroll to reach)"

    if coords:
        point = center(element)
        if point:
            line += f" @{point[0]},{point[1]}"

    return line


# Roles folded into one line when they sit in a toolbar, menu bar or nav.
BAR_ROLES = {"button", "toggle", "menu", "menuitem", "link", "tab"}


def _group_lines(members, coords):
    """
    Yield (line, element_count) for one window's elements.

    Runs of buttons in the same toolbar, menu bar or navigation are folded
    into a single line. LibreOffice Writer's toolbars alone are ~70 buttons;
    listed one per line they filled the whole budget before the document
    itself was reached, and a model asked to fill in a form was shown every
    formatting button and none of the form.
    """

    run = []

    def fold_key(element):
        if element.get("sheet_row"):
            return f"sheet row {element['sheet_row']}"
        if element.get("bar") and element["role"] in BAR_ROLES:
            return element["bar"]
        return None

    def flush():
        if not run:
            return None
        bar = fold_key(run[0])
        if run[0].get("sheet_row"):
            # Spreadsheet cells are addressed by their coordinate, which is
            # also their name, so the line is the row itself: A2="Rent" ...
            cells = [f"{e['name']}={_quote(e.get('value') or '', 40)}" + ("[focused]" if "focused" in e.get("states", []) else "") for e in run]
            count = len(run)
            run.clear()
            return f"{bar}: " + " ".join(cells), count
        items = []
        for e in run:
            item = f"{e['ref']} {_quote(e.get('name') or '', 30)}"
            states = [s for s in e.get("states", []) if s in ("checked", "disabled", "selected")]
            if states:
                item += "[" + ",".join(states) + "]"
            if coords and center(e):
                item += "@{},{}".format(*center(e))
            items.append(item)
        line = f"{_quote(bar, 30)} bar: "
        shown = 0
        for item in items:
            if len(line) + len(item) > 700:
                break
            line += ("" if shown == 0 else ", ") + item
            shown += 1
        if shown < len(items):
            line += f", … (+{len(items) - shown} more, observe --window to list)"
        count = len(run)
        run.clear()
        return line, count

    for element in members:
        key = fold_key(element)
        if run and key != fold_key(run[0]):
            yield flush()
        if key:
            run.append(element)
            continue
        yield element_line(element, coords=coords), 1

    if run:
        yield flush()


def group_matches(group, wanted):
    """
    Whether `wanted` names this group: part of its title, or of the window
    title it is shown in - with or without the browser's " - Chromium" - so
    the title an observation itself prints ("Invoices - Chromium") finds the
    tab it came from (whose group title is 'Chromium tab "Invoices" ...').
    """

    needle = (wanted or "").strip().lower()
    if not needle:
        return True
    names = [group.get("title") or "", group.get("window_title") or ""]
    haystacks = [n.lower() for n in names if n]
    if any(needle in h for h in haystacks):
        return True
    bare = re.sub(r"\s+[-\u2013\u2014]\s+[^-\u2013\u2014]+$", "", needle).strip()
    return bool(bare) and bare != needle and any(bare in h for h in haystacks)


def render(observation, budget=3500, coords=False, only_group=None):
    """
    The screen as text: which windows are open, then each window's elements,
    the active window first, cut off at `budget` characters.

    The budget is not cosmetic. The Ollama harness shows a model at most 4000
    characters of any one command's output, and every character is prompt a
    CPU has to read before the model answers - so the element list is capped
    before it can be truncated somewhere less careful.
    """

    lines = []
    width, height = observation["size"]
    active = observation.get("active_title") or "(none)"

    lines.append(f"Screen {width}x{height}. Active window: {_quote(active, 100)}")

    titles = [w["title"] for w in observation.get("windows", []) if w.get("title")]
    if titles:
        lines.append("Open windows: " + ", ".join(_quote(t, 60) for t in reversed(titles)))

    apps = observation.get("apps") or []
    if apps:
        lines.append("Apps on this desktop (an icon is opened by its name: gui open \"NAME\"): "
                     + ", ".join(_quote(name, 50) for name in apps))

    for note in observation.get("notes", []):
        lines.append("Note: " + note)

    groups = observation.get("groups", [])
    if only_group:
        chosen = [g for g in groups if group_matches(g, only_group)]
        if chosen:
            groups = chosen
        else:
            # A filter that matches nothing must not turn a full screen into
            # "no accessible elements" - live, the engine then clicked a list
            # row by its picture and opened the wrong purchase order.
            lines.append(f"Note: no window matches {_quote(only_group, 60)}; showing every window.")

    used = sum(len(line) + 1 for line in lines)
    hidden = 0

    for group in groups:
        members = [e for e in observation["elements"] if e.get("group") == group["id"]]
        if not members:
            continue

        header = f"== {group['title']} =="

        if used + len(header) + 1 > budget:
            hidden += len(members)
            continue

        lines.append(header)
        used += len(header) + 1

        for line, count in _group_lines(members, coords):
            if used + len(line) + 1 > budget:
                hidden += count
                continue
            lines.append(line)
            used += len(line) + 1

    if not any(line.startswith("== ") for line in lines):
        lines.append("(no accessible elements found - the screen may only have the desktop on it)")

    if hidden:
        lines.append(
            f"({hidden} more elements not shown. `observe --window <title>` lists one window in full.)"
        )

    return "\n".join(lines)
