import base64
import json
import re
import shutil
import time
from pathlib import Path

from flowera_agent import progress
from flowera_agent.computer import approval, atspi, audit, cdp, privacy, screen, session, volatility, xinput
from flowera_agent.computer import elements as el
from flowera_agent.computer.errors import ComputerError


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

STATE_DIR = Path.home() / ".flowera" / "computer"
STATE_FILE = STATE_DIR / "observation.json"

CHROMIUM_APPS = {"chromium", "chromium-browser", "google chrome", "chrome"}

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


def observe(remember=True):
    """
    Look at the screen: a screenshot, the open windows, and every accessible
    element in them. Saves the result as the reference for the next action -
    unless `remember` is False, for the demonstration recorder, which looks
    at the screen without moving an engine's refs out from under it.
    """

    session.ensure()

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
    elif any(re.search(r"chromium", w["title"], re.I) for w in windows):
        notes.append(
            "Chromium is open without its debugging port, so its page cannot be read. "
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

    previous = _load_state()
    next_ref = el.assign_refs(found, previous)

    width, height = frame.width, frame.height

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
    time.sleep(0.25)
    last = screen.grab()
    stable_since = time.monotonic()

    while time.monotonic() < deadline:
        time.sleep(0.2)
        current = screen.grab()
        if screen.changed_fraction(last, current) > 0.002:
            last = current
            stable_since = time.monotonic()
        elif time.monotonic() - stable_since >= 0.6:
            break

    return last


def _window_id_for(element, observation=None):
    title = element.get("window_title") or ""
    for w in screen.windows():
        if w["title"] == title:
            return w["id"]
    for w in screen.windows():
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


def _perform(kind, element, point, text, keys, direction, amount, option, append, end=None):
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
            cdp.click(element, button=button_cdp, count=count)
            return "cdp"

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
            if element["source"] == "atspi":
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
            cdp.select_option(element, option)
            return "cdp"
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

    raise ComputerError(f"unknown action {kind!r}")


def act(kind, ref=None, role=None, name=None, path=None, point=None,
        text=None, keys=None, direction=None, amount=None, option=None, append=False, end=None):
    """
    Do one thing on the screen and report what changed. `end` is the point a
    drag drops at.

    Returns a dict: status ("done" | "needs_approval"), text (one
    paragraph for the engine), element, change, observation (the "after"
    observation, or the "before" one when nothing ran).
    """

    before = observe()

    element = None
    if ref or role or name or path:
        element = el.find(before["elements"], ref=ref, role=role, name=name, path=path,
                          active_group=active_group(before))
    elif point is not None:
        element = el.at_point(before["elements"], point[0], point[1])

    keys = [k for k in (keys or []) if k and k.strip()]
    target = el.describe(element) if element else (f"{point[0]},{point[1]}" if point else "")

    # Typing with no target goes to whatever has focus, and that field is the
    # one whose secrecy decides whether the text may be kept.
    field = element
    if kind == "type" and field is None:
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
        backend = _perform(kind, element, point, text, keys, direction, amount, option, append, end)
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
        "select": "Selected in", "drag": "Dragged",
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
    if decision.approved:
        summary += " (approved by the human)"

    return {
        "status": "done",
        "text": f"{summary}.\nWhat changed: {change_text}",
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

    notable = [e for e in change["appeared"] if e["role"] in el.INTERACTIVE_ROLES
               or e["role"] in ("dialog", "alert", "status", "heading")]
    if change["appeared"]:
        shown = ", ".join(el.describe(e) for e in (notable or change["appeared"])[:limit])
        more = len(change["appeared"]) - min(len(notable or change["appeared"]), limit)
        parts.append(f"{len(change['appeared'])} element(s) appeared: {shown}" + (f" (+{more} more)" if more > 0 else ""))

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
    "chromium": ["chromium-browser"],
    "browser": ["chromium-browser"],
}

OFFICE_EXTENSIONS = (".odt", ".ods", ".odp", ".docx", ".doc", ".xlsx", ".xls", ".pptx", ".csv", ".rtf")


def _chromium_argv(url):
    return [
        "chromium-browser", "--remote-debugging-port=9222", "--no-first-run",
        "--no-default-browser-check", url,
    ]


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


def _open(target, new_tab):
    target = (target or "").strip()
    if not target:
        raise ComputerError("say what to open: a URL, a file, or an app name")

    before_titles = {w["title"] for w in screen.windows()}

    if URL_RE.match(target):
        url = target if "://" in target else "http://" + target

        if cdp.available():
            title = cdp.navigate(url, new_tab=new_tab)
            _raise_browser()
            return f"Opened {url} in Chromium (tab title {el._quote(title, 60)})."

        if any(re.search(r"chromium", t, re.I) for t in before_titles):
            raise ComputerError(
                "Chromium is already open without its debugging port. Close it first "
                "(key ctrl+shift+q in its window), then open the page again."
            )

        session.launch(_chromium_argv(url))
        _wait_until(cdp.available, 30, "Chromium did not start")
        title = cdp.navigate(url)
        _raise_browser()
        return f"Started Chromium on {url} (tab title {el._quote(title, 60)})."

    path = Path(target).expanduser()
    lower = target.lower()

    if path.exists():
        if path.suffix.lower() in OFFICE_EXTENSIONS:
            argv = ["soffice", "--nologo", str(path)]
        elif path.is_dir():
            argv = ["thunar", str(path)]
        elif path.suffix.lower() in (".html", ".htm", ".pdf"):
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

    session.launch(argv)

    def new_window():
        return [w for w in screen.windows() if w["title"] not in before_titles and w["title"]]

    opened = _wait_until(new_window, 45, None)
    if not opened:
        return f"Started {' '.join(argv)}, but no new window appeared within 45 seconds - observe to check."

    time.sleep(1.0)
    return f"Opened {el._quote(opened[-1]['title'], 80)}."


def _raise_browser():
    for w in screen.windows():
        if re.search(r"chromium", w["title"], re.I):
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
