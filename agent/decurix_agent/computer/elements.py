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
#           observations for as long as it is the same control (assign_refs)
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

_SUFFIX = re.compile(r"\[\d+\]$")


def _base(path):
    """A path without the [2], [3]... that dedupe_paths adds: what it is, not which of its kind."""

    return _SUFFIX.sub("", path or "")


def _document(element, urls):
    """The document an element is in, for a page's element (its tab and the address the tab shows); None for an app's,
    whose path already begins with its app and window."""

    if element.get("source") != "cdp":
        return None
    group = element.get("group") or ""
    return (group, (urls or {}).get(group, ""))


def _node(element, urls):
    """The page's own node an element is (its tab, address and DOM node), or None when it has none to tell."""

    backend = (element.get("handle") or {}).get("backend_id")
    doc = _document(element, urls)
    if doc is None or backend is None or isinstance(backend, bool):
        return None
    return doc + (backend,)


def _urls(observation_or_groups):
    groups = observation_or_groups.get("groups") if isinstance(observation_or_groups, dict) else observation_or_groups
    return {g.get("id"): g.get("url") or "" for g in groups or [] if isinstance(g, dict)}


def assign_refs(elements, previous, groups=None):
    """
    Give each element a ref, reusing the previous observation's ref only for the same control: one look's ref holds in
    the next only where that look shows the same thing under it. `groups` are this look's windows and tabs (their
    addresses).

    Numbering elements 1..N afresh on every observation would renumber
    everything after a single row is inserted, and an engine that had just
    read "e12 button Save" would click something else. So a ref follows its control - and only its control:
    - in a page, the same DOM node in the same tab at the same address; another address is another document, whose
      controls get refs of their own;
    - elsewhere (or a node drawn anew), the same path in the same document, with the same role and name, and - where
      the path is one of several alike ("[2]") - only while there are as many of them as before: when one goes, the
      next takes its place in the numbering, and the ref would silently name another control;
    - never a control whose role or name changed under it (a button that read "Edit" and reads "Save").
    A ref that was not carried is gone (core._gone), and the engine is told to look again (find, _same_named).
    """

    next_ref = 1
    urls_now = _urls(groups or [])
    by_node, by_path, nodes_now = {}, {}, set()
    count_then, count_now = {}, {}

    if previous:
        next_ref = previous.get("next_ref", 1)
        urls_then = _urls(previous)
        for old in previous.get("elements", []):
            if not old.get("ref"):
                continue
            node = _node(old, urls_then)
            if node is not None:
                by_node.setdefault(node, old)
            doc = _document(old, urls_then)
            by_path.setdefault((doc, old.get("path")), (old, node))
            key = (doc, _base(old.get("path")))
            count_then[key] = count_then.get(key, 0) + 1

    rows_then, rows_now = {}, {}
    if previous:
        for old in previous.get("elements", []):
            if old.get("row"):
                rows_then.setdefault(_document(old, urls_then), set()).add(old["row"])
    for element in elements:
        node = _node(element, urls_now)
        if node is not None:
            nodes_now.add(node)
        key = (_document(element, urls_now), _base(element.get("path")))
        count_now[key] = count_now.get(key, 0) + 1
        if element.get("row"):
            rows_now.setdefault(key[0], set()).add(element["row"])
    # The documents a numbered row went from (cdp._number_rows): the rows after it are numbered anew.
    rows_lost = {doc for doc, rows in rows_then.items() if not rows <= rows_now.get(doc, set())}

    def same(old, element):
        # Roles as observed (normalised once already), compared as they are: normalised again, static "text" and an
        # editable "textbox" were the same role, and a label could take a field's ref (see _same_named).
        return ((old.get("role") or "") == (element.get("role") or "")
                and (old.get("name") or "") == (element.get("name") or ""))

    used = set()

    for element in elements:
        ref = None
        node = _node(element, urls_now)
        doc = _document(element, urls_now)
        old = by_node.get(node) if node is not None else None
        if old is not None:
            ref = old["ref"] if same(old, element) else None
        else:
            held = by_path.get((doc, element.get("path")))
            if held is not None:
                old, old_node = held
                key = (doc, _base(element.get("path")))
                redrawn = old_node is not None or node is not None
                if same(old, element) and count_then.get(key) == count_now.get(key) \
                        and (old_node is None or old_node not in nodes_now) \
                        and not (redrawn and ((old.get("value") or "") != (element.get("value") or "")
                                              or (element.get("row") and doc in rows_lost))):
                    # A node that is still on the page elsewhere keeps its own ref (its old path is not this one's).
                    # A page's control drawn anew is the same one only holding the same value, and a numbered row's
                    # only while no row went: rows renumber, and "row 2" would be the next line's.
                    ref = old["ref"]

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


def find(elements, ref=None, role=None, name=None, path=None, active_group=None, gone=None, exact=False):
    """
    Resolve a target against a fresh observation, or raise a ComputerError
    that tells the engine what to do instead. `gone`: where refs no longer on
    the screen were last seen (the observation's "gone", core.observe).
    `exact`: a name is matched only as itself (or as a name quoted cut short),
    never as words inside a longer name - what a check of the screen needs
    ("Submitted" is not "Not Submitted", review 2026-11-03 D-14).
    """

    if ref:
        for element in elements:
            if element["ref"] == ref:
                return element
        # The ref is gone but the element it named may be on the screen under a new ref: the same role and name, in
        # the same window and document, once. Found live (2026-10-26): the review walked to another page before the
        # person's yes, and the approved click came back as "no longer on the screen" and a "could not finish" to the
        # person.
        same = _same_named(elements, ref, gone)
        if same is not None:
            return same
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
        # The name itself; then a name that holds it as whole words, at its start first ("Save" for "Save & New");
        # never inside another word ("Post" is not "Repost", "Rent" is not "Current"). A name quoted cut short
        # ("Leased Line, Ser…", as _quote shows a long one) is matched by what it begins with.
        whole = re.compile(r"(?<!\w)" + re.escape(wanted) + r"(?!\w)") if wanted else None
        cut = re.sub(r"(?:…|\.\.\.)$", "", wanted).rstrip() if re.search(r"(?:…|\.\.\.)$", wanted) else ""

        def shown(e):
            return " ".join((e.get("name") or "").split()).lower()

        tiers = [
            [e for e in candidates if shown(e) == wanted],
            [e for e in candidates if cut and shown(e).startswith(cut)],
        ]
        if not exact:
            tiers += [
                [e for e in candidates if whole and whole.match(shown(e))],
                [e for e in candidates if whole and whole.search(shown(e))],
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

# A table row's text is its cells: cut at the 80 characters other names get, a summary's row lost its last columns
# (found live, 2026-10-25: "Sales | 4 | 259,272.00 | 219,722.46 | ..." showed neither tax column, to the worker or
# the check of its figures).
ROW_ROLES = ("row",)
ROW_CHARS = 200


def element_line(element, coords=False):
    line = f"{element['ref']} {element['role']}"

    if element.get("name"):
        line += " " + _quote(element["name"], ROW_CHARS if element.get("role") in ROW_ROLES else 80)

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


def where_seen(element, url=""):
    """What a gone ref is re-bound by (_same_named): its role and name, and the window and document it was in."""

    return {"role": element.get("role") or "", "name": element.get("name") or "", "group": element.get("group") or "",
            "window_title": element.get("window_title") or "", "url": url or "",
            "within": _within(element.get("path"))}


def _within(path):
    """What a control sits in, by its path: every container before its own segment (its form, its table row)."""

    return _base(path).rpartition(" > ")[0]


def _same_named(elements, ref, gone=None):
    """
    The one element on the screen that is a gone ref's control drawn anew, else None: the same role and name, in the
    same window (its group and title) and, on a web page, the same document (the page's address now, gone["urls"]).
    A ref whose window was never kept is not re-bound; neither is one that two elements could be. `gone` is
    {"refs": {ref: where_seen}, "urls": {group: the address it shows now}} (core.observe). Found in review
    (2026-11-02): re-bound by role and name anywhere, an approved "Submit" landed on another document's Submit.
    """

    prior = ((gone or {}).get("refs") or {}).get(ref)
    if not prior or not prior.get("name") or not (prior.get("group") or prior.get("window_title")):
        return None
    # Roles as observed, already normalised once - compared as they are. normalize_role is not idempotent: a page's
    # static text is kept as "text", and "text" again is AT-SPI's editable field ("textbox"). Normalised twice, a gone
    # textbox was re-bound to the label left beside it and typed into (found live, 2026-11-15).
    role = prior.get("role") or ""
    urls = (gone or {}).get("urls") or {}
    same = [e for e in elements
            if (e.get("role") or "") == role and (e.get("name") or "") == prior["name"]
            and (e.get("group") or "") == (prior.get("group") or "")
            and (e.get("window_title") or "") == (prior.get("window_title") or "")
            and (not prior.get("url") or urls.get(e.get("group") or "") == prior["url"])]
    if len(same) != 1:
        return None
    # ...and in the same container (its form, its row): the one "Delete" left on a list after its row went is the next
    # row's, not the gone ref's.
    if "within" in prior and _within(same[0].get("path")) != prior["within"]:
        return None
    return same[0]
