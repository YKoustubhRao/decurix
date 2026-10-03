import base64
import json
import os
import re
import shutil
import time
from contextlib import contextmanager
from pathlib import Path

from decurix_agent import config, filelock, progress
from decurix_agent.computer import approval, atspi, audit, browser, cdp, privacy, screen, session, shortcuts, volatility, xinput
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


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
            notes.append(f"could not read the browser tab: {exc}")
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

        observation = {
            "time": time.time(),
            "size": [width, height],
            "active_title": active_title,
            "windows": [{"id": w["id"], "title": w["title"], "box": w["box"], "active": w["active"]} for w in windows],
            "groups": groups,
            "elements": found,
            "notes": notes,
            "page_url": page_group.get("url") if page_group else "",
            "frame": frame,
        }

        if remember:
            _save_state(observation, next_ref)

    return observation


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


def _window_id_for(element, observation=None):
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


def _goto_sheet_cell(element):
    """
    Move a spreadsheet's cell cursor to an element's cell, by coordinate.

    Through LibreOffice's Name Box (Ctrl+Shift+F5, the cell reference,
    Enter): the Name Box itself is not exposed over AT-SPI, and a click at a
    cell's reported position lands a row off (see atspi.place_caret).
    """

    _focus_window(element)
    xinput.key("Escape")
    xinput.key("ctrl+shift+F5")
    time.sleep(0.2)
    xinput.type_text(element["name"])
    xinput.key("Return")
    time.sleep(0.2)


def _perform(kind, element, point, text, keys, direction, amount, option, append, end=None, files=None):
    """Carry out one action; return which backend did it."""

    if element is not None and element.get("handle", {}).get("sheet_cell"):
        if kind == "type":
            if text is None:
                raise ComputerError("nothing to type")
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

        if element is not None and element["source"] == "cdp":
            opened = cdp.click(element, button=button_cdp, count=count)
            return "cdp:file_chooser" if opened.get("file_chooser") else "cdp"

        # A list or table cell's own action is "activate" - open the file,
        # not select it - so a cell is clicked where it is drawn instead.
        if element is not None and kind == "click" and element["role"] not in ("cell", "listitem", "treeitem", "icon"):
            done = atspi.default_action(element)
            if done:
                return f"atspi:{done}"

        target = point or (el.center(element) if element else None)
        if target is None:
            raise ComputerError(f"{el.describe(element)} has no position on screen to click")

        if element is not None:
            _focus_window(element)

        xinput.click(target[0], target[1], button=button_x, repeat=count)
        return "xdotool"

    if kind == "type":
        if text is None:
            raise ComputerError("nothing to type")

        if element is not None and element["source"] == "cdp":
            cdp.type_text(element, text, replace=not append)
            return "cdp"

        if element is not None:
            # A control that takes its text whole (a native Windows worker's,
            # of a toolkit that tells its app - atspi.set_text) is given it;
            # text that ends in Enter is typed, since the Enter is a key.
            if not append and "\n" not in text and atspi.set_text(element, text):
                return "value"
            _focus_window(element)
            center = el.center(element)
            # Inside a document (a Writer table cell, a paragraph) focus is
            # not the caret: the caret is placed through the Text interface,
            # and a click at the element's centre is only the fallback.
            in_document = element["role"] in ("cell", "paragraph", "heading", "document")
            if in_document:
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

        xinput.type_text(text)
        return "xdotool"

    if kind == "key":
        if not keys:
            raise ComputerError("no key given")
        if element is not None:
            _focus_window(element)
            if element["source"] != "cdp":
                atspi.grab_focus(element)
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
        if element is not None:
            _focus_window(element)
        xinput.scroll(target[0], target[1], direction, amount or 3)
        return "xdotool"

    if kind == "select":
        if element is None or not option:
            raise ComputerError("select needs a dropdown and the option to pick")
        if element["source"] == "cdp":
            try:
                cdp.select_option(element, option)
                return "cdp"
            except ComputerError as exc:
                if "is not a dropdown" not in str(exc):
                    raise
            _select_custom(element, option)
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
CUSTOM_OPTIONS_SECONDS = 4.0
CUSTOM_OPEN_SECONDS = 1.5

# What a dropdown's list shows: its options, and in many kits (Odoo) plain links.
_SUGGESTION_ROLES = ("option", "menuitem", "listitem", "link")

# Entries a list offers besides the choices: they make or search, never pick.
_NOT_A_CHOICE = re.compile(r"^\s*(create|add|search more|new)\b", re.I)


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


def _searches(option):
    """
    What to type into a searching dropdown, most specific first: the value,
    what comes before its first bracket or comma, its first two words, its
    first word. A list searches for the typed text as one piece, so a variant
    written in another order ("Desk (White, Aluminium)" for Odoo's "Desk
    (Aluminium, White)") finds nothing until the search is shorter; the pick is
    still judged against the whole value (_pick).
    """

    words = option.split()
    tries = [option.strip(), re.split(r"[(,]", option)[0].strip(), " ".join(words[:2]), words[0] if words else ""]
    out = []
    for t in tries:
        if len(t) >= 3 and t not in out:
            out.append(t)
    return out


def _select_custom(element, option):
    """
    Pick `option` in a dropdown a page drew itself (no <select>: OrangeHRM's
    "-- Select --", Odoo's customer and product fields, most component kits)
    the way a person does: open it - typing the value, then shorter searches,
    when it only suggests once typed into - then click the matching entry
    among what appeared. Only what appeared counts, so a link elsewhere on the
    page with the same words is never taken; "Create ..." and "Search
    More..." are never choices; two entries that fit equally are not guessed
    between. Live, typing a product and clicking the next field left Odoo's
    line with no product at all.
    """

    def expanded():
        now = observe()["elements"]
        me = next((e for e in now if e.get("path") == element.get("path")), None)
        return now, bool(me and "expanded" in (me.get("states") or []))

    # A list already open (an earlier try left it) would count as on the page
    # before the click, so none of its entries would count as appearing.
    now, is_open = expanded()
    if is_open:
        cdp.close_list(element)
        time.sleep(0.3)
        now, _ = expanded()
    known = {e["path"] for e in now}
    cdp.click(element)
    searches = _searches(option) if element["role"] in ("combobox", "textbox", "searchbox") else []
    deadline = time.monotonic() + CUSTOM_OPEN_SECONDS
    seen = []

    while True:
        now = observe()["elements"]
        candidates = [e for e in now if e["path"] not in known and e["role"] in _SUGGESTION_ROLES
                      and e.get("source") == "cdp" and e.get("box")
                      and not _NOT_A_CHOICE.match(e.get("name") or "")]
        seen = [e.get("name") or "" for e in candidates] or seen
        hit = _pick(candidates, option)
        if hit is not None:
            cdp.click(hit)
            return

        if time.monotonic() > deadline:
            if not searches:
                break
            cdp.type_text(element, searches.pop(0))
            deadline = time.monotonic() + CUSTOM_OPTIONS_SECONDS
        time.sleep(0.3)

    # Leave the page as it was found: an open list covers what is under it.
    if expanded()[1]:
        cdp.close_list(element)
    if seen:
        message = f"{el.describe(element)} has no option {option!r}. It offers: " + ", ".join(seen[:30])
        # A value written with its variant ("Stool (Navy)") against an entry
        # that is only its general item ("Stool", shown with its code as
        # "StoolD_0045"): say so, and what selecting it leads to.
        head = re.split(r"[(,]", option)[0].strip()
        general = [n for n in seen if head and head != option.strip() and n.lower().startswith(head.lower())]
        if len(general) == 1:
            message += (f". {general[0]!r} starts with {head!r}: it is likely the general item that {option!r} is a "
                        f"variant of - select {head!r} (or that entry), then choose the variant in what the page asks next")
        raise ComputerError(message)
    raise ComputerError(f"{el.describe(element)} showed no options for {option!r}")


def act(kind, ref=None, role=None, name=None, path=None, point=None,
        text=None, keys=None, direction=None, amount=None, option=None, append=False, end=None, files=None):
    """
    Do one thing on the screen and report what changed. `end` is the point a
    drag drops at.

    Returns a dict: status ("done" | "needs_approval"), text (one
    paragraph for the engine), element, change, observation (the "after"
    observation, or the "before" one when nothing ran).
    """

    # One field per typing action. A Tab inside the text moves focus to the next
    # field, and what lands there is recorded - and withheld or not - as if it
    # were typed into this one: live, "Admin\tadmin123\n" put the password into
    # the username's step in plain text. Typing each field on its own lets the
    # password field be recognised as one.
    if kind == "type" and text and "\t" in text:
        raise ComputerError("type one field at a time: a Tab in the text would carry the rest into another field. "
                            "Type into this field, then type the next value into its own field (by its ref).")

    before = observe()

    element = None
    if ref or role or name or path:
        element = el.find(before["elements"], ref=ref, role=role, name=name, path=path,
                          active_group=active_group(before))
    elif point is not None:
        element = el.at_point(before["elements"], point[0], point[1])

    keys = [k for k in (keys or []) if k and k.strip()]
    target = el.describe(element) if element else (f"{point[0]},{point[1]}" if point else "")

    # Typing needs a field. Aimed at a page's row, button or link, the text goes
    # to whatever still has focus: live, a product name typed "into" an Odoo
    # order row at a coordinate landed in the previous line's Unit Price.
    # The same on a Windows worker's screen, where it was found live: a small
    # model sent `gui type e40 "Hello World"` with e40 a toolbar button of
    # Notepad, the keys went to the button, and nothing was typed. There the
    # refusal names the fields that are on the screen, because the one the
    # model wants is usually a line or two away in the list it just read.
    if kind == "type" and element is not None and element["role"] in NOT_A_FIELD \
            and element.get("source") in ("cdp", "uia"):
        message = (f"{el.describe(element)} is not a field to type into - click the cell or control that "
                   "opens a field, observe, then type into that field by its ref")
        if element.get("source") == "uia":
            fields = [el.describe(e) for e in before["elements"]
                      if e.get("role") in TYPING_ROLES and "disabled" not in (e.get("states") or [])][:4]
            if fields:
                message = (f"{el.describe(element)} is not a field to type into. The fields on the screen are: "
                           + "; ".join(fields) + " - type into one of those by its ref")
        raise ComputerError(message)

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

    decision = approval.check(kind, element, text=text, keys=keys, point=point, context=_context(before))

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

    try:
        backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end, files=files)
    except ComputerError as exc:
        entry.update({
            "outcome": "failed",
            "reason": str(exc),
            "summary": f"{described} - failed: {exc}",
        })
        audit.record(entry, before=before["frame"], masks=masks)
        raise

    settled = _wait_for_settle(before["frame"])
    after = observe()
    change = compare(before, after)
    change_text = summarize(change)

    entry.update({
        "outcome": "done",
        "backend": backend,
        "approved": decision.approved,
        "change": change_text,
        "expect": expectations(change),
        "summary": described + (" (approved)" if decision.approved else "") + f" - {change_text}",
    })

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
    elif kind == "drag":
        summary = f"Dragged {target} to {end[0]},{end[1]}"
    elif kind == "key":
        summary = f"Pressed {' '.join(keys)}" + (f" on {target}" if target else "")
    elif kind == "select":
        summary = f"Selected {el._quote(option or '')} in {target}"
    elif kind == "upload":
        files_str = ", ".join(os.path.basename(f) for f in (files or []))
        summary = f"Uploaded {files_str} to {target or 'the file input'}"
    if decision.approved:
        summary += " (approved by the human)"

    note = ""
    if backend == "cdp:file_chooser":
        # cdp.click intercepted it: no dialog is on the screen, nothing was chosen.
        note = (f"\n{target} opens a file chooser; none was shown and no file was chosen. "
                "To attach a file, use the upload action on this same control with the file path(s).")

    return {
        "status": "done",
        "text": f"{summary}.\nWhat changed: {change_text}{note}",
        "element": element,
        "change": change,
        "observation": after,
    }


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
                parts.append(f"{el.describe(element)} value {el._quote(old, 40)} -> {el._quote(new, 40)}")
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
    "libreoffice": ["soffice", "--nologo"],
    "writer": ["soffice", "--nologo", "--writer"],
    "calc": ["soffice", "--nologo", "--calc"],
    "impress": ["soffice", "--nologo", "--impress"],
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
}

OFFICE_EXTENSIONS = (".odt", ".ods", ".odp", ".docx", ".doc", ".xlsx", ".xls", ".pptx", ".csv", ".rtf")


def _chromium_argv(url):
    # On a Linux desktop the launcher adds the profile, the DevTools port and
    # every other flag; a native Windows worker's are given by browser.py.
    return browser.launch_argv(url)


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
    before_titles = {w["title"] for w in before}
    # A window that was not there is told by which window it is, not by its
    # title: a second window of an app that is open has the first one's title.
    before_ids = {w["id"] for w in before}

    # One of this worker's own shortcuts, by its name: what a person opens
    # from its icon, opened the same way (shortcuts.py).
    shortcut = shortcuts.named(target)
    if shortcut is not None and shortcut["kind"] == "browser":
        target = "browser"
    elif shortcut is not None and shortcut["kind"] == "web":
        target = shortcut.get("target") or target

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

        session.launch(_chromium_argv(url))
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
        session.launch(_chromium_argv("about:blank"))
        _wait_until(cdp.available, BROWSER_START_SECONDS, "Chromium did not start")
        _raise_browser()
        return "Started Chromium."

    if shortcut is not None and shortcut["kind"] == "program":
        # Started by the control plane's helper, which holds this desktop.
        shortcuts.open_program(shortcut)
        argv = [shortcut.get("exe") or shortcut["name"]]
    elif os.name == "nt":
        # What opens what is the computer's own business there (win.py): the
        # programs installed on it, never the shell.
        from decurix_agent.computer import win

        argv = win.open_argv(target)
        if argv[0] == "url":
            return _open(argv[1], new_tab)
    elif path.exists():
        if path.suffix.lower() in OFFICE_EXTENSIONS:
            argv = ["soffice", "--nologo", str(path)]
        elif path.is_dir():
            argv = ["thunar", str(path)]
        elif path.suffix.lower() in (".html", ".htm", ".pdf", ".eml"):
            return _open("file://" + str(path.resolve()), new_tab)
        else:
            argv = ["mousepad", str(path)]
    elif lower in APPS:
        argv = APPS[lower]
    else:
        parts = target.split()
        if not shutil.which(parts[0]):
            raise ComputerError(
                f"don't know how to open {target!r}: give a URL, an existing file path, or one of "
                + ", ".join(sorted(APPS))
            )
        argv = parts

    started = shortcut["name"] if shortcut is not None and shortcut["kind"] == "program" else " ".join(argv)
    if shortcut is None or shortcut["kind"] != "program":
        session.launch(argv)

    def new_window():
        return [w for w in screen.windows() if w["id"] not in before_ids and w["title"]]

    opened = _wait_until(new_window, 45, None)
    if not opened:
        return f"Started {started}, but no new window appeared within 45 seconds - observe to check."

    time.sleep(1.0)
    title = opened[-1]["title"]

    if os.name == "nt":
        # A window opened on a private desktop is given the keyboard by nobody
        # (win.py / decurix-ui.cs GiveFocus), and many name themselves only
        # once they have loaded ("Notepad", then "Untitled - Notepad").
        xinput.activate(opened[-1]["id"])
        title = next((w["title"] for w in screen.windows() if w["id"] == opened[-1]["id"] and w["title"]), title)

        # Windows 11's Notepad opens with the tabs it had when it was last
        # closed - and that memory is the person's own, so a worker's Notepad
        # came up showing their unsaved notes, and the worker typed its text
        # into one of them (found by bench/native). Asked for with no file, it
        # is given a new, empty tab to work in.
        if len(argv) == 1 and Path(argv[0]).stem.lower() == "notepad" and not title.lower().startswith("untitled"):
            xinput.key("ctrl+n")
            time.sleep(0.8)
            title = next((w["title"] for w in screen.windows() if w["id"] == opened[-1]["id"] and w["title"]), title)
            return f"Opened Notepad with a new, empty tab ({el._quote(title, 60)}); its other tabs were already there - leave them alone."

    return f"Opened {el._quote(title, 80)}."


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


def upload_files(target, files, ref=None):
    """
    Convenience helper to upload one or more files to a target element in the browser.
    `target` can be an element ref (e12), role and name ('button "Upload"'), or selector.
    """
    if ref:
        return act("upload", ref=ref, files=files)
    if isinstance(target, str) and el.REF_RE.match(target):
        return act("upload", ref=target, files=files)
    return act("upload", name=target, files=files)

