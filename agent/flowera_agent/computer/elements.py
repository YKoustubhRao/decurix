import re

from flowera_agent.computer.errors import ComputerError


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


def at_point(elements, x, y):
    """The smallest element whose box contains a screen point, or None."""

    best = None
    best_area = None

    for element in elements:
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

    states = [s for s in element.get("states", []) if s != "offscreen"]
    if states:
        line += " [" + ", ".join(states) + "]"

    if "offscreen" in element.get("states", []):
        line += " (scroll to reach)"

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

    for note in observation.get("notes", []):
        lines.append("Note: " + note)

    groups = observation.get("groups", [])
    if only_group:
        needle = only_group.lower()
        groups = [g for g in groups if needle in g["title"].lower()]

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
