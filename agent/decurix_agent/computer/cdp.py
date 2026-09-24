import base64
import json
import os
import re
import socket
import struct
import time
import urllib.error
import urllib.parse
import urllib.request

from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


# ============================================================
# Chromium, through the Chrome DevTools Protocol
#
# A page is read from Chromium's own accessibility tree
# (Accessibility.getFullAXTree) - the same roles and names a screen reader
# gets - with every node's position from one DOMSnapshot call rather than one
# round trip per node. Elements are acted on through DevTools too: a click is
# a mouse event dispatched at the element's centre after scrolling it into
# view, and text goes in with Input.insertText. Neither depends on the browser
# window being on top, or on mapping page pixels onto the screen exactly.
#
# The browser listens on 127.0.0.1 only (agent/chromium-browser.init adds
# --remote-debugging-port), inside a container with no published ports. The
# websocket client is the standard library, because nothing else is
# guaranteed to be installed in a desktop image.
# ============================================================

PORT = int(os.environ.get("DECURIX_CDP_PORT", "9222"))
BASE = f"http://127.0.0.1:{PORT}"
CALL_TIMEOUT = 20

# How long observe() waits for a loading page before reading it anyway.
LOAD_WAIT_SECONDS = 8

INTERACTIVE = {
    "button", "link", "textbox", "searchbox", "checkbox", "radio", "combobox",
    "listbox", "option", "menuitem", "menuitemcheckbox", "menuitemradio", "tab",
    "switch", "slider", "spinbutton", "treeitem", "popupbutton",
}

READABLE = {"heading", "paragraph", "status", "alert", "dialog", "alertdialog", "img", "row", "listitem", "caption", "legend"}

# Roles whose own name already carries the text inside them.
NAMED_BY_CONTENT = INTERACTIVE | READABLE | {
    "cell", "gridcell", "columnheader", "rowheader", "labeltext", "label", "legend", "caption",
}

# Named containers that become part of an element's path.
CONTAINERS = {"form", "dialog", "alertdialog", "navigation", "main", "region", "table", "row", "list", "group", "tabpanel", "menu", "toolbar", "search", "banner", "complementary"}


class _Socket:
    """A minimal RFC 6455 client: text frames out, whole messages in."""

    def __init__(self, url, timeout=CALL_TIMEOUT):
        parsed = urllib.parse.urlparse(url)
        self.sock = socket.create_connection((parsed.hostname, parsed.port), timeout=timeout)
        key = base64.b64encode(os.urandom(16)).decode()
        path = parsed.path + (("?" + parsed.query) if parsed.query else "")

        # No Origin header: Chromium only enforces --remote-allow-origins on
        # connections that send one.
        self.sock.sendall((
            f"GET {path} HTTP/1.1\r\nHost: {parsed.netloc}\r\n"
            "Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
        ).encode())

        buffer = b""
        while b"\r\n\r\n" not in buffer:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ComputerError("Chromium closed the DevTools connection")
            buffer += chunk

        head, self.buffer = buffer.split(b"\r\n\r\n", 1)
        if b" 101 " not in head.split(b"\r\n", 1)[0]:
            raise ComputerError("Chromium refused the DevTools connection")

    def send(self, text):
        data = text.encode("utf-8")
        mask = os.urandom(4)
        header = bytes([0x81])
        length = len(data)

        if length < 126:
            header += bytes([0x80 | length])
        elif length < 65536:
            header += bytes([0x80 | 126]) + struct.pack(">H", length)
        else:
            header += bytes([0x80 | 127]) + struct.pack(">Q", length)

        masked = bytearray(data)
        for i in range(length):
            masked[i] ^= mask[i % 4]

        self.sock.sendall(header + mask + bytes(masked))

    def _need(self, n):
        while len(self.buffer) < n:
            chunk = self.sock.recv(1 << 20)
            if not chunk:
                raise ComputerError("Chromium closed the DevTools connection")
            self.buffer += chunk
        out, self.buffer = self.buffer[:n], self.buffer[n:]
        return out

    def receive(self):
        message = b""
        while True:
            b0, b1 = self._need(2)
            length = b1 & 0x7F
            if length == 126:
                length = struct.unpack(">H", self._need(2))[0]
            elif length == 127:
                length = struct.unpack(">Q", self._need(8))[0]
            payload = self._need(length)
            opcode = b0 & 0x0F

            if opcode == 0x8:
                raise ComputerError("Chromium closed the DevTools connection")
            if opcode == 0x9:  # ping
                continue
            if opcode in (0x0, 0x1, 0x2):
                message += payload
                if b0 & 0x80:
                    return message.decode("utf-8", "replace")

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


class Page:
    """One DevTools session on one tab."""

    def __init__(self, target):
        self.target = target
        self.ws = _Socket(target["webSocketDebuggerUrl"])
        self.seq = 0

    def call(self, method, params=None, timeout=CALL_TIMEOUT):
        self.seq += 1
        mine = self.seq
        self.ws.sock.settimeout(timeout)
        self.ws.send(json.dumps({"id": mine, "method": method, "params": params or {}}))

        while True:
            try:
                message = json.loads(self.ws.receive())
            except socket.timeout:
                raise ComputerError(f"Chromium did not answer {method} within {timeout}s")

            if message.get("id") != mine:
                continue

            if "error" in message:
                raise ComputerError(f"{method}: {message['error'].get('message')}")

            return message.get("result", {})

    def evaluate(self, expression):
        result = self.call("Runtime.evaluate", {"expression": expression, "returnByValue": True})
        return result.get("result", {}).get("value")

    def close(self):
        self.ws.close()


def _get(path, method="GET", timeout=2):
    request = urllib.request.Request(BASE + path, method=method)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def available():
    try:
        _get("/json/version", timeout=0.5)
        return True
    except (OSError, ValueError):
        return False


def _page_targets():
    try:
        targets = _get("/json/list")
    except (OSError, ValueError) as exc:
        raise ComputerError(f"could not list Chromium tabs: {exc}")

    return [
        t for t in targets
        if t.get("type") == "page" and t.get("webSocketDebuggerUrl")
        and not t.get("url", "").startswith(("devtools://", "chrome-extension://"))
    ]


def active_page():
    """
    Open a session on the tab a person would be looking at: the first page
    target (DevTools lists them most recently used first) that is visible.
    """

    targets = _page_targets()

    if not targets:
        raise ComputerError("Chromium has no open tab")

    fallback = None

    for target in targets:
        try:
            page = Page(target)
        except (OSError, ComputerError):
            continue

        try:
            if page.evaluate("document.visibilityState") == "visible":
                if fallback is not None:
                    fallback.close()
                return page
        except ComputerError:
            pass

        if fallback is None:
            fallback = page
        else:
            page.close()

    if fallback is None:
        raise ComputerError("could not attach to any Chromium tab")

    return fallback


_METRICS = """JSON.stringify({
  sx: window.screenX, sy: window.screenY,
  ow: window.outerWidth, oh: window.outerHeight,
  iw: window.innerWidth, ih: window.innerHeight,
  scx: window.scrollX, scy: window.scrollY,
  dpr: window.devicePixelRatio || 1,
  title: document.title, url: location.href
})"""


def _prop(node, key):
    for prop in node.get("properties", []):
        if prop.get("name") == key:
            return prop.get("value", {}).get("value")
    return None


def observe():
    """Return (elements, group) for the active Chromium tab."""

    page = active_page()

    try:
        # A page still loading has an accessibility tree with nothing in it
        # yet; reading it then reports an empty page the engine would act on.
        deadline = time.monotonic() + LOAD_WAIT_SECONDS
        while time.monotonic() < deadline and page.evaluate("document.readyState") != "complete":
            time.sleep(0.2)

        metrics = json.loads(page.evaluate(_METRICS))
        nodes = page.call("Accessibility.getFullAXTree").get("nodes", [])
        snapshot = page.call("DOMSnapshot.captureSnapshot", {"computedStyles": []})
    finally:
        page.close()

    bounds = {}
    # What a field says about itself in its HTML - type="password",
    # autocomplete="cc-number" - which the accessibility tree does not carry
    # and privacy.py needs to keep a secret out of the recording.
    declared = {}
    strings = snapshot.get("strings", [])
    for document in snapshot.get("documents", [])[:1]:
        backend_ids = document.get("nodes", {}).get("backendNodeId", [])
        layout = document.get("layout", {})
        for node_index, rect in zip(layout.get("nodeIndex", []), layout.get("bounds", [])):
            if node_index < len(backend_ids):
                bounds[backend_ids[node_index]] = rect
        for node_index, attributes in enumerate(document.get("nodes", {}).get("attributes", [])):
            pairs = {}
            for name_index, value_index in zip(attributes[0::2], attributes[1::2]):
                if 0 <= name_index < len(strings) and 0 <= value_index < len(strings):
                    pairs[strings[name_index].lower()] = strings[value_index]
            if node_index < len(backend_ids) and ("type" in pairs or "autocomplete" in pairs):
                declared[backend_ids[node_index]] = pairs

    origin_x = metrics["sx"] + (metrics["ow"] - metrics["iw"])
    origin_y = metrics["sy"] + (metrics["oh"] - metrics["ih"])
    dpr = metrics["dpr"] or 1

    def screen_box(backend_id):
        rect = bounds.get(backend_id)
        if not rect or rect[2] <= 0 or rect[3] <= 0:
            return None, False
        x = rect[0] - metrics["scx"]
        y = rect[1] - metrics["scy"]
        on_screen = (x + rect[2] > 0 and y + rect[3] > 0 and x < metrics["iw"] and y < metrics["ih"])
        box = [int(origin_x + x * dpr), int(origin_y + y * dpr), int(rect[2] * dpr), int(rect[3] * dpr)]
        return box, on_screen

    by_id = {n["nodeId"]: n for n in nodes}
    group_id = f"cdp:{page.target.get('id')}"
    title = metrics.get("title") or page.target.get("title") or ""
    url = metrics.get("url") or ""

    group = {
        "id": group_id,
        "title": f"Chromium tab {el._quote(title, 60)} {url[:100]}",
        "window_title": f"{title} - Chromium",
        "url": url,
        "source": "cdp",
    }

    def text_of(node, depth=0):
        """
        The plain text beneath a node, for rows and paragraphs - leaving out
        links, buttons and fields, which are listed as elements of their own.
        """
        if depth > 8:
            return ""
        role = ((node.get("role") or {}).get("value", "") or "").lower()
        if depth > 0 and role in INTERACTIVE:
            return ""
        name = (node.get("name") or {}).get("value", "") or ""
        if role in ("statictext", "inlinetextbox"):
            return " ".join(name.split())
        parts = []
        for child_id in node.get("childIds", []):
            child = by_id.get(child_id)
            if child:
                text = text_of(child, depth + 1)
                if text:
                    parts.append(text)
        return " ".join(parts)

    def cell_texts(row):
        texts = []
        for child_id in row.get("childIds", []):
            child = by_id.get(child_id)
            if child is None:
                continue
            text = text_of(child, 1) if ((child.get("role") or {}).get("value", "").lower() not in INTERACTIVE) else ""
            if text:
                texts.append(text)
        return texts

    found = []
    root = nodes[0] if nodes else None

    # Paths are rooted at the site, not the page title: the "Users" link in a
    # site's navigation is the same element on every page of it, and should
    # keep its ref when the page changes around it.
    parsed = urllib.parse.urlparse(url)
    site = parsed.netloc or parsed.scheme or "page"
    stack = [(root, [f"site {site}"], False, None)] if root else []
    visits = 0

    while stack:
        node, segments, inside_row, bar = stack.pop()
        visits += 1
        if visits > 6000:
            break

        role = (node.get("role") or {}).get("value", "")
        lower = role.lower()
        name = " ".join(((node.get("name") or {}).get("value", "") or "").split())
        backend_id = node.get("backendDOMNodeId")
        ignored = node.get("ignored", False)

        include = False
        display_name = name

        if not ignored and backend_id is not None:
            if lower in INTERACTIVE:
                include = True
            elif lower == "row":
                display_name = " | ".join(cell_texts(node))
                include = bool(display_name)
            elif lower in ("paragraph", "status", "alert", "listitem", "caption", "legend") and not inside_row:
                display_name = text_of(node)
                include = bool(display_name)
            elif lower in READABLE and not inside_row:
                display_name = name or text_of(node)
                include = bool(display_name)
            elif lower == "statictext" and re.search(r"\w", name) and not inside_row:
                # Loose text a person reads ("Status: Draft" in a <div>), but
                # not the text inside something already listed by its name.
                parent = by_id.get(node.get("parentId"))
                parent_role = ((parent or {}).get("role") or {}).get("value", "").lower()
                include = parent_role not in NAMED_BY_CONTENT

        if include:
            box, on_screen = screen_box(backend_id)
            if box is not None:
                states = []
                if _prop(node, "focused"):
                    states.append("focused")
                checked = _prop(node, "checked")
                if checked is not None and lower in ("checkbox", "radio", "switch", "menuitemcheckbox", "menuitemradio"):
                    states.append("checked" if checked in (True, "true", "mixed") else "unchecked")
                if _prop(node, "disabled"):
                    states.append("disabled")
                expanded = _prop(node, "expanded")
                if expanded is not None:
                    states.append("expanded" if expanded else "collapsed")
                if _prop(node, "selected") and lower in ("option", "tab", "treeitem", "listitem"):
                    states.append("selected")
                if not on_screen:
                    states.append("offscreen")

                value = (node.get("value") or {}).get("value", "")
                norm = el.normalize_role(lower)
                if norm == "text":
                    norm = "text"

                found.append({
                    "role": norm,
                    "name": display_name[:200],
                    "value": str(value) if value not in (None, "") else "",
                    "states": states,
                    "box": box,
                    "path": " > ".join(segments + [f"{norm} {el._quote(display_name, 40)}"]),
                    "source": "cdp",
                    "group": group_id,
                    "window_title": group["window_title"],
                    "app": "chromium",
                    "bar": bar,
                    "handle": {
                        "backend_id": backend_id,
                        "target_id": page.target.get("id"),
                        "dom_role": lower,
                        "input_type": declared.get(backend_id, {}).get("type", ""),
                        "autocomplete": declared.get(backend_id, {}).get("autocomplete", ""),
                    },
                })

        child_segments = segments
        if not ignored and lower in CONTAINERS and (name or lower == "row"):
            label = display_name if lower == "row" else name
            child_segments = segments + [f"{lower} {el._quote(label, 40)}"]

        next_inside_row = inside_row or (lower == "row" and not ignored)

        child_bar = bar
        if not ignored and lower in ("navigation", "toolbar", "menubar", "tablist"):
            child_bar = name or lower

        for child_id in reversed(node.get("childIds", [])):
            child = by_id.get(child_id)
            if child is not None:
                stack.append((child, child_segments, next_inside_row, child_bar))

    el.dedupe_paths(found)

    return found, group


def element_at_screen_point(x, y, known_elements=()):
    """
    Find the named browser control under a real screen point.

    The DOM snapshot used by observe() is deliberately cheap and normally
    gives every control a screen box. A browser can nevertheless report a
    box that is a few pixels out of sync with X11 while it is being resized,
    scaled, or composited by VNC. Recording must not turn that brief mapping
    problem into a coordinate-only step when Chromium itself can still tell
    us which DOM control received the click.

    known_elements is the observation that was used for the recording. If the
    hit is a descendant (for example, the SVG inside a Submit button), the
    accessibility tree is walked to its named interactive ancestor and the
    already-observed element is returned. That keeps the recording's normal
    path, role, name, and handle intact.
    """

    page = None

    try:
        page = active_page()
        metrics = json.loads(page.evaluate(_METRICS))
        dpr = metrics["dpr"] or 1
        origin_x = metrics["sx"] + (metrics["ow"] - metrics["iw"])
        origin_y = metrics["sy"] + (metrics["oh"] - metrics["ih"])
        viewport_x = (x - origin_x) / dpr + metrics["scx"]
        viewport_y = (y - origin_y) / dpr + metrics["scy"]

        if not (0 <= viewport_x < metrics["iw"] and 0 <= viewport_y < metrics["ih"]):
            return None

        hit = page.call("DOM.getNodeForLocation", {
            "x": max(0, int(viewport_x)),
            "y": max(0, int(viewport_y)),
            "includeUserAgentShadowDOM": True,
        })
        backend_id = hit.get("backendNodeId")
        if backend_id is None:
            return None

        accessibility = page.call("Accessibility.getPartialAXTree", {
            "backendNodeId": backend_id,
            "fetchRelatives": True,
        }).get("nodes", [])

        by_node_id = {node.get("nodeId"): node for node in accessibility}
        current = next(
            (node for node in accessibility if node.get("backendDOMNodeId") == backend_id),
            None,
        )
        known_by_backend = {
            element.get("handle", {}).get("backend_id"): element
            for element in known_elements
            if element.get("source") == "cdp"
            and element.get("handle", {}).get("backend_id") is not None
        }

        while current is not None:
            role = ((current.get("role") or {}).get("value") or "").lower()
            if role in INTERACTIVE:
                current_backend_id = current.get("backendDOMNodeId")
                known = known_by_backend.get(current_backend_id)
                if known is not None and known.get("name"):
                    return known

                name = " ".join(((current.get("name") or {}).get("value") or "").split())
                if name:
                    norm = el.normalize_role(role)
                    return {
                        "role": norm,
                        "name": name[:200],
                        "value": str((current.get("value") or {}).get("value") or ""),
                        "states": [],
                        "box": None,
                        "path": f"site browser > {norm} {el._quote(name, 40)}",
                        "source": "cdp",
                        "group": f"cdp:{page.target.get('id')}",
                        "window_title": f"{metrics.get('title') or page.target.get('title') or ''} - Chromium",
                        "app": "chromium",
                        "bar": None,
                        "handle": {
                            "backend_id": current_backend_id,
                            "target_id": page.target.get("id"),
                            "dom_role": role,
                            "input_type": "",
                            "autocomplete": "",
                        },
                    }

            parent_id = current.get("parentId")
            current = by_node_id.get(parent_id)

        return None
    except (ComputerError, KeyError, TypeError, ValueError):
        return None
    finally:
        if page is not None:
            page.close()


# ============================================================
# Acting
# ============================================================

def _page_for(element):
    for target in _page_targets():
        if target.get("id") == element["handle"]["target_id"]:
            return Page(target)
    raise ComputerError(f"the tab {el.describe(element)} was on has been closed. Run observe again.")


def _center(page, backend_id):
    page.call("DOM.scrollIntoViewIfNeeded", {"backendNodeId": backend_id})
    quads = page.call("DOM.getContentQuads", {"backendNodeId": backend_id}).get("quads") or []

    if not quads:
        raise ComputerError("that element has no size on the page (it may be hidden)")

    quad = quads[0]
    xs, ys = quad[0::2], quad[1::2]

    return sum(xs) / 4, sum(ys) / 4


def click(element, button="left", count=1):
    page = _page_for(element)

    try:
        x, y = _center(page, element["handle"]["backend_id"])
        page.call("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": x, "y": y})

        for n in range(1, count + 1):
            for kind in ("mousePressed", "mouseReleased"):
                page.call("Input.dispatchMouseEvent", {
                    "type": kind, "x": x, "y": y, "button": button, "clickCount": n,
                })
    finally:
        page.close()


def _object(page, backend_id):
    resolved = page.call("DOM.resolveNode", {"backendNodeId": backend_id})
    return resolved["object"]["objectId"]


def type_text(element, text, replace=True):
    page = _page_for(element)

    try:
        backend_id = element["handle"]["backend_id"]
        page.call("DOM.scrollIntoViewIfNeeded", {"backendNodeId": backend_id})

        try:
            page.call("DOM.focus", {"backendNodeId": backend_id})
        except ComputerError:
            x, y = _center(page, backend_id)
            for kind in ("mousePressed", "mouseReleased"):
                page.call("Input.dispatchMouseEvent", {"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1})

        object_id = _object(page, backend_id)
        info = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function(replace) {
                const multiline = this.tagName === 'TEXTAREA' || this.isContentEditable;
                if (replace) {
                    if (typeof this.select === 'function') { this.select(); }
                    else if (this.isContentEditable) { document.execCommand('selectAll'); }
                } else if (typeof this.setSelectionRange === 'function' && this.value !== undefined) {
                    try { this.setSelectionRange(this.value.length, this.value.length); } catch (e) {}
                }
                return {multiline: multiline};
            }""",
            "arguments": [{"value": bool(replace)}],
        }).get("result", {}).get("value") or {}

        if info.get("multiline"):
            page.call("Input.insertText", {"text": text})
        else:
            lines = text.split("\n")
            for index, line in enumerate(lines):
                if line:
                    page.call("Input.insertText", {"text": line})
                elif replace and index == 0 and len(lines) == 1:
                    # Replacing with nothing: delete the selection.
                    press_key(page, "Backspace")
                if index < len(lines) - 1:
                    press_key(page, "Enter")
    finally:
        page.close()


_KEYS = {
    "Enter": ("Enter", 13, "\r"),
    "Backspace": ("Backspace", 8, None),
    "Tab": ("Tab", 9, None),
    "Escape": ("Escape", 27, None),
}


def press_key(page, name):
    key, code, text = _KEYS[name]
    down = {"type": "keyDown", "key": key, "code": key, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code}
    if text:
        down["text"] = text
    page.call("Input.dispatchKeyEvent", down)
    page.call("Input.dispatchKeyEvent", {"type": "keyUp", "key": key, "code": key, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code})


def select_option(element, option):
    page = _page_for(element)

    try:
        object_id = _object(page, element["handle"]["backend_id"])
        result = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function(label) {
                if (!this.options) { return {ok: false, error: 'not a dropdown'}; }
                const want = label.trim().toLowerCase();
                const options = Array.from(this.options);
                const hit = options.find(o => o.label.trim().toLowerCase() === want)
                    || options.find(o => o.label.trim().toLowerCase().startsWith(want))
                    || options.find(o => o.label.toLowerCase().includes(want));
                if (!hit) { return {ok: false, options: options.map(o => o.label)}; }
                this.value = hit.value;
                this.dispatchEvent(new Event('input', {bubbles: true}));
                this.dispatchEvent(new Event('change', {bubbles: true}));
                return {ok: true, label: hit.label};
            }""",
            "arguments": [{"value": option}],
        }).get("result", {}).get("value") or {}
    finally:
        page.close()

    if not result.get("ok"):
        if result.get("options"):
            raise ComputerError(
                f"{el.describe(element)} has no option {option!r}. Options: " + ", ".join(result["options"])
            )
        raise ComputerError(f"{el.describe(element)} is not a dropdown: {result.get('error')}")

    return result["label"]


def set_file_input_files(element, files):
    """
    Set files on a file input element (<input type="file">), or an element wrapping/referencing one.
    Uses Chrome DevTools Protocol DOM.setFileInputFiles.
    """
    if isinstance(files, (str, bytes)):
        files = [files]
    abs_files = [str(os.path.abspath(os.path.expanduser(f))) for f in files]
    for f in abs_files:
        if not os.path.exists(f):
            raise ComputerError(f"file to upload does not exist: {f}")

    page = _page_for(element)
    try:
        backend_id = element["handle"]["backend_id"]
        object_id = _object(page, backend_id)
        target_info = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                if (this.tagName === 'INPUT' && (this.type || '').toLowerCase() === 'file') {
                    return {found: true, is_self: true};
                }
                const input = this.querySelector('input[type="file"]');
                if (input) return {found: true, is_child: true};
                if (this.htmlFor) {
                    const el = document.getElementById(this.htmlFor);
                    if (el && el.tagName === 'INPUT' && (el.type || '').toLowerCase() === 'file') {
                        return {found: true, is_for: true, id: this.htmlFor};
                    }
                }
                return {found: false, tag: this.tagName};
            }"""
        }).get("result", {}).get("value") or {}

        target_object_id = object_id
        if target_info.get("is_child"):
            resolved = page.call("Runtime.callFunctionOn", {
                "objectId": object_id,
                "functionDeclaration": "function() { return this.querySelector('input[type=\"file\"]'); }"
            })
            target_object_id = resolved.get("result", {}).get("objectId") or object_id
        elif target_info.get("is_for"):
            resolved = page.call("Runtime.callFunctionOn", {
                "objectId": object_id,
                "arguments": [{"value": target_info["id"]}],
                "functionDeclaration": "function(id) { return document.getElementById(id); }"
            })
            target_object_id = resolved.get("result", {}).get("objectId") or object_id

        try:
            page.call("DOM.setFileInputFiles", {
                "files": abs_files,
                "objectId": target_object_id,
            })
        except Exception:
            page.call("DOM.setFileInputFiles", {
                "files": abs_files,
                "backendNodeId": backend_id,
            })

        page.call("Runtime.callFunctionOn", {
            "objectId": target_object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                this.dispatchEvent(new Event('input', {bubbles: true}));
                this.dispatchEvent(new Event('change', {bubbles: true}));
                return true;
            }"""
        })
        return f"Uploaded {len(abs_files)} file(s): " + ", ".join(os.path.basename(f) for f in abs_files)
    finally:
        page.close()


def scroll(element, direction, amount=3):
    delta = {"up": (0, -120), "down": (0, 120), "left": (-120, 0), "right": (120, 0)}.get(direction)
    if delta is None:
        raise ComputerError(f"cannot scroll {direction!r}; use up, down, left or right")

    page = _page_for(element) if element else active_page()

    try:
        if element and element.get("handle", {}).get("backend_id") is not None:
            quads = page.call("DOM.getContentQuads", {"backendNodeId": element["handle"]["backend_id"]}).get("quads") or []
            x, y = (sum(quads[0][0::2]) / 4, sum(quads[0][1::2]) / 4) if quads else (100, 100)
        else:
            size = json.loads(page.evaluate("JSON.stringify([innerWidth, innerHeight])"))
            x, y = size[0] / 2, size[1] / 2

        page.call("Input.dispatchMouseEvent", {
            "type": "mouseWheel", "x": x, "y": y,
            "deltaX": delta[0] * amount, "deltaY": delta[1] * amount,
        })
    finally:
        page.close()


def navigate(url, new_tab=False):
    """Load a URL in the active tab (or a new one) and wait for it to load."""

    if new_tab:
        try:
            target = _get("/json/new?" + urllib.parse.quote(url, safe=":/?&=#%"), method="PUT", timeout=10)
        except (OSError, ValueError) as exc:
            raise ComputerError(f"could not open a new tab: {exc}")
        page = Page(target)
    else:
        page = active_page()

    try:
        if not new_tab:
            result = page.call("Page.navigate", {"url": url})
            if result.get("errorText"):
                raise ComputerError(f"could not load {url}: {result['errorText']}")

        page.call("Page.bringToFront")

        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            try:
                if page.evaluate("document.readyState") == "complete":
                    break
            except ComputerError:
                pass
            time.sleep(0.2)

        return page.evaluate("document.title") or url
    finally:
        page.close()


def wait_ready(timeout=15, default_title=""):
    """
    Wait for the active Chromium tab to reach interactive or complete state
    and return its title, without navigating again.
    """

    deadline = time.monotonic() + timeout
    page = None
    while time.monotonic() < deadline:
        try:
            page = active_page()
            break
        except ComputerError:
            time.sleep(0.2)

    if page is None:
        return default_title

    try:
        try:
            page.call("Page.bringToFront")
        except ComputerError:
            pass

        while time.monotonic() < deadline:
            try:
                state = page.evaluate("document.readyState")
                if state in ("interactive", "complete"):
                    break
            except ComputerError:
                pass
            time.sleep(0.2)

        try:
            return page.evaluate("document.title") or default_title
        except ComputerError:
            return default_title
    finally:
        page.close()

