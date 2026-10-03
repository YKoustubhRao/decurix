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

from decurix_agent.computer import browser
from decurix_agent.logging_utils import log
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

# "complete" once the page has loaded and is not catching up with an input.
_READY = (
    "document.readyState === 'complete' && "
    "!document.documentElement.hasAttribute('data-decurix-busy') ? 'complete' : 'busy'"
)

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
        # Events that arrived while waiting for an answer, kept for wait_event:
        # only the domains a caller enabled send any.
        self.events = []

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
                if "method" in message and "id" not in message:
                    self.events.append(message)
                continue

            if "error" in message:
                raise ComputerError(f"{method}: {message['error'].get('message')}")

            return message.get("result", {})

    def wait_event(self, method, timeout):
        """The params of the first `method` event, waiting up to `timeout` seconds; None if none came."""

        deadline = time.monotonic() + timeout

        while True:
            for message in self.events:
                if message.get("method") == method:
                    self.events.remove(message)
                    return message.get("params") or {}

            left = deadline - time.monotonic()
            if left <= 0:
                return None

            self.ws.sock.settimeout(left)
            try:
                message = json.loads(self.ws.receive())
            except (socket.timeout, OSError, ValueError):
                return None
            if "method" in message and "id" not in message:
                self.events.append(message)

    def evaluate(self, expression):
        result = self.call("Runtime.evaluate", {"expression": expression, "returnByValue": True})
        return result.get("result", {}).get("value")

    def close(self):
        self.ws.close()


def _get(path, method="GET", timeout=None):
    # A busy browser answers late (BUSY_ANSWER_SECONDS); a missing one refuses at once.
    timeout = BUSY_ANSWER_SECONDS if timeout is None else timeout
    request = urllib.request.Request(BASE + path, method=method)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


# How long a browser whose DevTools port is open may take to answer. It was
# half a second, and a Chrome busy rendering an ERP page (Odoo's quotation form,
# with another desktop loading beside it) answered later: the layer reported
# "open without its debugging port" and showed the agent an empty screen.
BUSY_ANSWER_SECONDS = float(os.environ.get("DECURIX_CDP_ANSWER_SECONDS", "5"))


def _nothing_listens():
    """
    Whether the DevTools port is free to bind, i.e. no browser has it.

    Only Windows needs asking this way: there a connection to a port nothing
    listens on is not refused at once but after about two seconds (the system
    tries three times), and every look at a screen with no browser open paid
    them - measured, 2.06 s against a few milliseconds on Linux. Binding the
    port says the same thing immediately.
    """

    parsed = urllib.parse.urlparse(BASE)
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        probe.bind((parsed.hostname or "127.0.0.1", parsed.port or 80))
        return True
    except OSError:
        return False
    finally:
        probe.close()


def available():
    """
    Whether the browser's DevTools port answers. A refused connection (nothing
    listening) is known at once; a slow answer is a busy browser, not a missing
    port, and is waited for.
    """

    if os.name == "nt" and _nothing_listens():
        return False

    try:
        _get("/json/version", timeout=BUSY_ANSWER_SECONDS)
        return True
    except ConnectionRefusedError:
        return False
    except urllib.error.URLError as exc:
        if isinstance(exc.reason, ConnectionRefusedError):
            return False
        log(f"cdp: the DevTools port did not answer: {exc}")
        return False
    except (OSError, ValueError) as exc:
        log(f"cdp: the DevTools port did not answer: {exc}")
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


def _name_by_column(found, columns, metrics, origin_x, origin_y, dpr):
    """Give an unnamed field in a table cell its column's header as its name (_COLUMN_NAMES)."""

    boxes = []
    # Only well-formed entries: the page's own scripts can redefine what the
    # evaluation returns, and a name is a hint - never a reason to fail a look.
    for entry in columns if isinstance(columns, list) else []:
        if (not isinstance(entry, list) or len(entry) != 5 or not isinstance(entry[4], str)
                or not all(isinstance(v, (int, float)) for v in entry[:4])):
            continue
        x, y, w, h, header = entry
        sx, sy = origin_x + (x - metrics["scx"]) * dpr, origin_y + (y - metrics["scy"]) * dpr
        boxes.append((sx + w * dpr / 2, sy + h * dpr / 2, header[:60]))

    for element in found:
        if element["name"] or element["role"] not in _NAMEABLE_FIELDS or not element.get("box"):
            continue
        bx, by, bw, bh = element["box"]
        cx, cy = bx + bw / 2, by + bh / 2
        match = next((header for hx, hy, header in boxes if abs(hx - cx) <= 3 and abs(hy - cy) <= 3), None)
        if match:
            element["name"] = match
            element["named_by"] = "column"
            head, _, _ = element["path"].rpartition(" > ")
            element["path"] = (head + " > " if head else "") + f"{element['role']} {el._quote(match, 40)}"


def _name_by_group(found, labels, metrics, origin_x, origin_y, dpr):
    """
    Name a field whose only name is its placeholder (or none) by its group's
    label (_GROUP_LABELS). A custom dropdown - a focusable block with no role,
    read by the accessibility tree as its text ("-- Select --") - becomes a
    combobox named by its label, holding that text as its value.
    """

    fields, customs = [], []
    for entry in labels if isinstance(labels, list) else []:
        if (not isinstance(entry, list) or len(entry) != 7 or not all(isinstance(v, str) for v in entry[4:6])
                or not (entry[6] is None or isinstance(entry[6], str))
                or not all(isinstance(v, (int, float)) for v in entry[:4])):
            continue
        x, y, w, h, label, placeholder, shown = entry
        sx, sy = origin_x + (x - metrics["scx"]) * dpr, origin_y + (y - metrics["scy"]) * dpr
        box = (sx, sy, w * dpr, h * dpr)
        if shown is None:
            fields.append((sx + w * dpr / 2, sy + h * dpr / 2, label[:60], placeholder[:40]))
        else:
            customs.append((box, label[:60], shown[:60]))

    for element in found:
        if element.get("named_by") or not element.get("box"):
            continue
        bx, by, bw, bh = element["box"]
        cx, cy = bx + bw / 2, by + bh / 2

        if element["role"] in _NAMEABLE_FIELDS:
            match = next(((label, ph) for hx, hy, label, ph in fields if abs(hx - cx) <= 3 and abs(hy - cy) <= 3), None)
            if not match:
                continue
            label, placeholder = match
            # Only a name that is nothing or the placeholder is replaced: a field
            # the page labelled properly keeps its own name.
            if element["name"] and element["name"] != placeholder:
                continue
            name = f"{label} ({placeholder})" if placeholder and _FORMAT_PLACEHOLDER.match(placeholder) else label
            role = element["role"]
        elif element["role"] == "text":
            match = next(((label, shown) for (x, y, w, h), label, shown in customs
                          if x - 2 <= cx <= x + w + 2 and y - 2 <= cy <= y + h + 2
                          and (element.get("name") or "").strip() == shown), None)
            if not match:
                continue
            name, shown = match
            role = "combobox"
            element["role"] = role
            element["value"] = shown
        else:
            continue

        element["name"] = name
        element["named_by"] = "label"
        head, _, _ = element["path"].rpartition(" > ")
        element["path"] = (head + " > " if head else "") + f"{role} {el._quote(name, 40)}"


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


def _viewport(metrics):
    """
    (origin_x, origin_y, dpr): where the page's top-left corner is on the
    screen, in screen pixels, and how many screen pixels one CSS pixel is.

    On a Linux desktop the browser has no borders of its own and a CSS pixel
    is a screen pixel, so the window's position plus the height of its bars is
    the answer. On Windows neither holds - measured in Edge at 125% scaling: a
    box at CSS (200, 100) was drawn at screen (277, 238), where that sum, even
    scaled, said (291, 251): the window's invisible borders are on three sides
    and the page reports rounded CSS pixels. The browser's own content window
    says exactly where the page is, so that is asked instead (win.py); the
    arithmetic is only the fallback, with the border taken off the bottom too.
    """

    dpr = metrics["dpr"] or 1
    side = metrics["ow"] - metrics["iw"]
    bars = metrics["oh"] - metrics["ih"]

    if os.name == "nt":
        from decurix_agent.computer import win

        origin = win.browser_viewport()
        if origin is not None:
            return origin[0], origin[1], dpr
        return (metrics["sx"] + side / 2) * dpr, (metrics["sy"] + bars - side / 2) * dpr, dpr

    return metrics["sx"] + side, metrics["sy"] + bars, dpr


_METRICS = """JSON.stringify({
  sx: window.screenX, sy: window.screenY,
  ow: window.outerWidth, oh: window.outerHeight,
  iw: window.innerWidth, ih: window.innerHeight,
  scx: window.scrollX, scy: window.scrollY,
  dpr: window.devicePixelRatio || 1,
  title: document.title, url: location.href
})"""


# A page's inner frames are part of what a person sees, and a look at the page
# reads them too (see _frames). The two scripts below name fields from the
# page's own layout, so they go through every frame document the page may read
# (the same site's), each at its place in the page: `each(document, ox, oy)`
# is given a document and where its top-left corner is in the top document.
_INNER_FRAMES = """if (depth < 3) for (const f of document.querySelectorAll('iframe, frame')) {
    let inner = null;
    try { inner = f.contentDocument; } catch (e) {}
    if (!inner) continue;
    const fr = f.getBoundingClientRect();
    if (fr.width < 2 || fr.height < 2) continue;
    each(inner, ox + fr.left + f.clientLeft, oy + fr.top + f.clientTop, depth + 1);
  }"""


# A form control inside a table cell with no label of its own is named by its
# column's header, the way a screen reader announces it. Found in Odoo's order
# lines: after "Add Line" the row's cells were five unnamed textboxes and
# comboboxes, and nothing said which took the product and which the quantity.
_COLUMN_NAMES = """JSON.stringify((() => {
  const out = [];
  const each = (document, ox, oy, depth) => {
  const fields = document.querySelectorAll('input:not([type=hidden]), select, textarea, [contenteditable="true"], ' +
    '[role=combobox], [role=textbox], [role=spinbutton], [role=searchbox]');
  const labelled = el => el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') ||
    (el.labels && el.labels.length) || el.getAttribute('title') || el.getAttribute('placeholder');
  const start = cell => [...cell.parentElement.children].slice(0, [...cell.parentElement.children].indexOf(cell))
    .reduce((n, c) => n + (c.colSpan || 1), 0);
  for (const el of fields) {
    if (labelled(el)) continue;
    const cell = el.closest('td, th, [role=gridcell], [role=cell]');
    const table = cell && cell.closest('table, [role=grid], [role=table], [role=treegrid]');
    if (!cell || !table || !cell.parentElement) continue;
    const head = table.querySelector('thead tr') || table.querySelector('[role=row]');
    if (!head || head === cell.parentElement) continue;
    const column = start(cell);
    let n = 0, header = '';
    for (const h of head.children) {
      const span = h.colSpan || 1;
      if (column >= n && column < n + span) { header = (h.getAttribute('aria-label') || h.innerText || '').trim(); break; }
      n += span;
    }
    const r = el.getBoundingClientRect();
    if (header && r.width > 0 && r.height > 0) {
      out.push([r.left + ox, r.top + oy, r.width, r.height, header.replace(/\\s+/g, ' ').slice(0, 60)]);
    }
  }
  __INNER_FRAMES__
  };
  each(document, scrollX, scrollY, 0);
  return out;
})())""".replace("__INNER_FRAMES__", _INNER_FRAMES)

# A field with no label of its own - named, at best, by its placeholder - in
# a form whose label is a sibling element instead of a <label for> (OrangeHRM,
# many Vue/React kits): every date field there read `textbox "yyyy-dd-mm"`, and
# live the agent typed a date of birth into the driving licence's expiry. A
# screen reader has the same problem; a person reads the text beside the box.
# So the field is named by the text before it inside the smallest enclosing
# block that holds no other field (its own label group), and a placeholder that
# is a format ("yyyy-dd-mm") is kept after it. Returns [x, y, w, h, name,
# placeholder, shown text of a custom dropdown or null].
_GROUP_LABELS = """JSON.stringify((() => {
  const out = [];
  const each = (document, ox, oy, depth) => {
  const selector = 'input:not([type=hidden]):not([type=file]), select, textarea, [contenteditable="true"], ' +
    '[role=combobox], [role=textbox], [role=spinbutton], [role=searchbox]';
  // A custom dropdown is often a focusable <div> with no role at all
  // (OrangeHRM's "-- Select --"): a field too, reported with its shown text.
  const custom = '[tabindex]:not([tabindex="-1"]):not(a):not(button):not(input):not(select):not(textarea):not([role]):not(body)';
  const all = selector + ', ' + custom;
  const fields = document.querySelectorAll(all);
  const own = el => el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') ||
    (el.labels && el.labels.length) || el.getAttribute('title');
  for (const el of fields) {
    const isCustom = el.matches(custom);
    if (isCustom && (el.children.length > 2 || (el.innerText || '').trim().length > 60)) continue;
    if (own(el) || el.closest('td, th, [role=gridcell], [role=cell]')) continue;
    let group = el.parentElement, label = '';
    for (let depth = 0; group && depth < 5 && !label; depth++, group = group.parentElement) {
      if ([...group.querySelectorAll(all)].filter(f => !f.contains(el) && !el.contains(f)).length) break;
      const walker = document.createTreeWalker(group, NodeFilter.SHOW_TEXT);
      const words = [];
      for (let t = walker.nextNode(); t; t = walker.nextNode()) {
        if (el.contains(t) || !(t.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING)) continue;
        const text = t.textContent.replace(/\\s+/g, ' ').trim();
        if (text) words.push(text);
      }
      label = words.join(' ').replace(/\\s*\\*\\s*$/, '').trim();
    }
    const r = el.getBoundingClientRect();
    if (label && label.length <= 60 && r.width > 0 && r.height > 0) {
      out.push([r.left + ox, r.top + oy, r.width, r.height, label,
                isCustom ? '' : (el.getAttribute('placeholder') || ''),
                isCustom ? (el.innerText || '').replace(/\\s+/g, ' ').trim() : null]);
    }
  }
  __INNER_FRAMES__
  };
  each(document, scrollX, scrollY, 0);
  return out;
})())""".replace("__INNER_FRAMES__", _INNER_FRAMES)

# A placeholder that says how to write the value, worth keeping in the name.
_FORMAT_PLACEHOLDER = re.compile(r"^[ymdhsa]{1,4}([-/.: ][ymdhsa]{1,4})+$", re.I)


# The roles a column header may name.
_NAMEABLE_FIELDS = {"textbox", "combobox", "spinbutton", "searchbox", "listbox", "checkbox"}


# How many inner frames of one page a look reads.
MAX_FRAMES = 12


def _frames(page):
    """
    The page's inner frames a person can see, outermost first: [{"id", "owner"
    (the frame element's backend node id), "box" (where its content is drawn
    in the page's view, CSS pixels)}].

    A page is not one document. Found live on LinkedIn (2026-10-03): the whole
    Messaging page is drawn in a frame of the same site, and the page's own
    accessibility tree stops at the frame - a look listed 2 elements where a
    person saw a search box, the conversations, the message box and Send. The
    worker clicked at guessed coordinates and typed into nothing.

    Only frames this DevTools session holds are listed (the page's own
    process: the same site). A frame of another site is a target of its own
    and is not read. A frame with no size is not shown to anyone and is left
    out, with whatever is inside it.
    """

    try:
        tree = page.call("Page.getFrameTree").get("frameTree") or {}
    except ComputerError:
        return []

    return _inner_frames(page, tree)


def _loader(tree):
    """Which load of a page a frame tree is of: it changes when the tab goes to another document."""

    return ((tree or {}).get("frame") or {}).get("loaderId") or ""


def _frame_tree(page):
    try:
        return page.call("Page.getFrameTree").get("frameTree") or {}
    except ComputerError:
        return {}


def _inner_frames(page, tree):
    frames = []
    queue = list(tree.get("childFrames") or [])

    while queue and len(frames) < MAX_FRAMES:
        entry = queue.pop(0)
        frame_id = (entry.get("frame") or {}).get("id")
        if not frame_id:
            continue
        try:
            owner = page.call("DOM.getFrameOwner", {"frameId": frame_id}).get("backendNodeId")
            if owner is None:
                continue
            quads = page.call("DOM.getContentQuads", {"backendNodeId": owner}).get("quads") or []
        except ComputerError:
            continue
        if not quads:
            continue
        xs, ys = quads[0][0::2], quads[0][1::2]
        box = (min(xs), min(ys), max(xs) - min(xs), max(ys) - min(ys))
        if box[2] < 2 or box[3] < 2:
            continue
        frames.append({"id": frame_id, "owner": owner, "box": box})
        queue.extend(entry.get("childFrames") or [])

    return frames


def _splice_frames(page, nodes, frames):
    """
    Put each inner frame's accessibility tree into `nodes`, under the frame
    element it is drawn in, so one walk reads the page as a person sees it.
    Node ids of a frame's tree are given a prefix of their own: they are only
    promised to be unique within one tree.
    """

    for index, frame in enumerate(frames):
        try:
            inner = page.call("Accessibility.getFullAXTree", {"frameId": frame["id"]}).get("nodes", [])
        except ComputerError:
            continue
        if not inner:
            continue

        prefix = f"f{index}:"
        for node in inner:
            node["nodeId"] = prefix + str(node.get("nodeId"))
            if node.get("parentId") is not None:
                node["parentId"] = prefix + str(node["parentId"])
            node["childIds"] = [prefix + str(child) for child in node.get("childIds") or []]

        owner = next((node for node in nodes if node.get("backendDOMNodeId") == frame["owner"]), None)
        if owner is None:
            owner = nodes[0] if nodes else None
        if owner is None:
            continue

        owner.setdefault("childIds", []).append(inner[0]["nodeId"])
        inner[0]["parentId"] = owner["nodeId"]
        nodes.extend(inner)


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
        # A page that mirrors something else's controls (a phone app's
        # viewer) marks itself data-decurix-busy until its copy has caught up
        # with the last input - the approval gate must judge what is there
        # now, not the button that was there before the tap.
        deadline = time.monotonic() + LOAD_WAIT_SECONDS
        while time.monotonic() < deadline and page.evaluate(_READY) != "complete":
            time.sleep(0.2)

        metrics = json.loads(page.evaluate(_METRICS))
        nodes = page.call("Accessibility.getFullAXTree").get("nodes", [])
        tree = _frame_tree(page)
        loader = _loader(tree)
        frames = _inner_frames(page, tree)
        _splice_frames(page, nodes, frames)
        snapshot = page.call("DOMSnapshot.captureSnapshot", {"computedStyles": []})
        try:
            columns = json.loads(page.evaluate(_COLUMN_NAMES) or "[]")
        except (ComputerError, ValueError):
            columns = []  # a name from a column is help, never a reason to fail a look
        try:
            labels = json.loads(page.evaluate(_GROUP_LABELS) or "[]")
        except (ComputerError, ValueError):
            labels = []
    finally:
        page.close()

    bounds = {}
    # What a field says about itself in its HTML - type="password",
    # autocomplete="cc-number" - which the accessibility tree does not carry
    # and privacy.py needs to keep a secret out of the recording.
    declared = {}
    strings = snapshot.get("strings", [])
    by_frame = {frame["id"]: frame for frame in frames}
    # Which frame's document a node is in (absent: the page's own).
    frame_of = {}
    for index, document in enumerate(snapshot.get("documents", [])):
        frame = None
        if index:
            frame_index = document.get("frameId")
            frame_id = strings[frame_index] if isinstance(frame_index, int) and 0 <= frame_index < len(strings) else None
            frame = by_frame.get(frame_id)
            if frame is None:
                continue  # a frame nobody sees (no size), or one inside it
            frame = dict(frame, scroll=(document.get("scrollOffsetX") or 0, document.get("scrollOffsetY") or 0))
        backend_ids = document.get("nodes", {}).get("backendNodeId", [])
        layout = document.get("layout", {})
        for node_index, rect in zip(layout.get("nodeIndex", []), layout.get("bounds", [])):
            if node_index < len(backend_ids):
                bounds[backend_ids[node_index]] = rect
                if frame is not None:
                    frame_of[backend_ids[node_index]] = frame
        for node_index, attributes in enumerate(document.get("nodes", {}).get("attributes", [])):
            pairs = {}
            for name_index, value_index in zip(attributes[0::2], attributes[1::2]):
                if 0 <= name_index < len(strings) and 0 <= value_index < len(strings):
                    pairs[strings[name_index].lower()] = strings[value_index]
            if node_index < len(backend_ids) and ("type" in pairs or "autocomplete" in pairs):
                declared[backend_ids[node_index]] = pairs

    origin_x, origin_y, dpr = _viewport(metrics)

    def screen_box(backend_id):
        rect = bounds.get(backend_id)
        if not rect or rect[2] <= 0 or rect[3] <= 0:
            return None, False
        # The snapshot's bounds are already in screen pixels (measured at 125%
        # scaling: a 120 px wide box came back 150 wide), from the top of the
        # document; the scroll position is the page's own, in CSS pixels.
        frame = frame_of.get(backend_id)
        if frame is None:
            x = rect[0] - metrics["scx"] * dpr
            y = rect[1] - metrics["scy"] * dpr
            inside = True
        else:
            # A frame's document has its own origin and its own scroll
            # position; the frame says where it is drawn in the page's view.
            # The snapshot gives that scroll position in the same pixels as
            # its bounds (measured at 125%: scrolled 60 CSS pixels, it said 75).
            fx, fy, fw, fh = frame["box"]
            x = fx * dpr - frame["scroll"][0] + rect[0]
            y = fy * dpr - frame["scroll"][1] + rect[1]
            inside = (x + rect[2] > fx * dpr and y + rect[3] > fy * dpr
                      and x < (fx + fw) * dpr and y < (fy + fh) * dpr)
        on_screen = inside and (x + rect[2] > 0 and y + rect[3] > 0
                                and x < metrics["iw"] * dpr and y < metrics["ih"] * dpr)
        box = [int(origin_x + x), int(origin_y + y), int(rect[2]), int(rect[3])]
        return box, on_screen

    by_id = {n["nodeId"]: n for n in nodes}
    group_id = f"cdp:{page.target.get('id')}"
    title = metrics.get("title") or page.target.get("title") or ""
    url = metrics.get("url") or ""

    group = {
        "id": group_id,
        "title": f"Chromium tab {el._quote(title, 60)} {url[:100]}",
        "window_title": browser.page_window_title(title),
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
                        "loader": loader,
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

    _name_by_column(found, columns, metrics, origin_x, origin_y, dpr)
    _name_by_group(found, labels, metrics, origin_x, origin_y, dpr)
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

    element, _ = hit(x, y, known_elements, controls_only=True)
    return element


# What a field says about itself in its HTML, which privacy.py needs to keep
# a secret out of a recording (observe reads the same two off its snapshot).
_DECLARED = ("type", "autocomplete")

# Fields are listed, and typed into, with or without a name.
_FIELD_ROLES = ("textbox", "combobox", "searchbox", "spinbutton")

# What a look at a page lists besides its controls: text a person reads.
_TEXT_ROLES = ("statictext", "paragraph", "listitem", "status", "alert", "heading", "cell", "row")


def _known_by_backend(known_elements, page):
    """
    The elements of an earlier look that are still this tab's, by node id.

    A node id only means something within the document it was read from. Found
    live (2026-10-03): a person typed "linkedin" into the new tab page's
    search box and clicked LinkedIn in Google's results; the result's node had
    the number the search box had had on the page before, and the click was
    recorded as a click on `combobox "Ask Google"` - a control of a page that
    was gone. So an element is only taken from a look at the same load of the
    tab (its `loader`); one from before the tab went elsewhere is forgotten.
    """

    target_id = page.target.get("id")
    loader = _loader(_frame_tree(page))

    def same_document(handle):
        return not loader or handle.get("loader") in (None, "", loader)

    return {
        element["handle"]["backend_id"]: element
        for element in known_elements or ()
        if element.get("source") == "cdp" and (element.get("handle") or {}).get("backend_id") is not None
        and (element.get("handle") or {}).get("target_id") == target_id
        and same_document(element["handle"])
    }


def _context(page, metrics):
    """Where a page is: the title of its window as recordings write it, and its address."""

    return {
        "title": browser.page_window_title(metrics.get("title") or page.target.get("title") or ""),
        "url": metrics.get("url") or "",
    }


def _describe(page, metrics, backend_id, known, controls_only=False):
    """
    The element a DOM node is, as observe would list it: the node itself or
    the nearest thing around it that a look at the page lists - the button an
    icon is inside, the row a cell is in. One a look already listed is
    returned as listed (its path, name and box), with the value it has now;
    one it did not (the page changed since) is described from the page's own
    accessibility tree. None when nothing around the node has a name.
    """

    nodes = page.call("Accessibility.getPartialAXTree", {"backendNodeId": backend_id, "fetchRelatives": True}).get("nodes", [])
    by_node_id = {node.get("nodeId"): node for node in nodes}
    # A node inside a control's own shadow tree (the arrow of a <select>, the
    # editing box of an <input>) names no parent: it is found as the child it
    # is listed as. Measured: a press on a drop-down hit such a node, and the
    # drop-down was not found from it.
    parent_of = {child: node for node in nodes for child in node.get("childIds") or []}
    current = next((node for node in nodes if node.get("backendDOMNodeId") == backend_id), None)

    text = None
    hops = 0

    # A block that only holds text (a <div> a page made into a button with a
    # click handler) is the text a look at the page lists inside it.
    if current is not None and not " ".join((((current.get("name") or {}).get("value")) or "").split()):
        for child_id in current.get("childIds") or []:
            child = by_node_id.get(child_id)
            name = " ".join((((child or {}).get("name") or {}).get("value") or "").split())
            if child is not None and name and ((child.get("role") or {}).get("value") or "").lower() == "statictext":
                listed = known.get(child.get("backendDOMNodeId"))
                if listed is not None and not controls_only:
                    text = ("listed", listed, "")
                else:
                    text = (child, name, "text")
                break

        # The page's accessibility tree, asked about one node, answers with
        # the node and what is above it - not always what is inside it. Found
        # live: a press on a line of a conversation list (a <span> holding
        # only its words) came back with no child at all, nothing above it
        # had a name, and the press was recorded as a bare point. The DOM
        # says what words the node holds.
        if text is None:
            text = _own_text(page, backend_id, known, controls_only)

    while current is not None and hops < 60:
        hops += 1
        role = ((current.get("role") or {}).get("value") or "").lower()
        name = " ".join(((current.get("name") or {}).get("value") or "").split())
        here = current.get("backendDOMNodeId")
        value = (current.get("value") or {}).get("value")

        listed = known.get(here)
        if listed is not None and (listed.get("name") or listed["role"] in _FIELD_ROLES) \
                and not (controls_only and role not in INTERACTIVE):
            found = dict(listed)
            if role in INTERACTIVE:
                found["value"] = str(value) if value not in (None, "") else ""
            found["handle"] = dict(listed["handle"], tag=_tag(page, here))
            return found

        if role in INTERACTIVE and (name or el.normalize_role(role) in _FIELD_ROLES):
            return _built(page, metrics, current, role, name, value)

        if text is None and name and role in _TEXT_ROLES:
            text = (current, name, "text" if role == "statictext" else role)

        current = by_node_id.get(current.get("parentId")) or parent_of.get(current.get("nodeId"))

    if text is not None and not controls_only:
        if text[0] == "listed":
            return dict(text[1])
        return _built(page, metrics, text[0], text[2], text[1], None)

    return None


def _own_text(page, backend_id, known, controls_only):
    """
    The words a DOM node holds directly, as _describe keeps a text it found:
    ("listed", element, "") when a look at the page listed them, else (a node
    to build the element from, the words, "text"); None when it holds none.
    """

    try:
        children = page.call("DOM.describeNode", {"backendNodeId": backend_id, "depth": 1}).get("node", {}).get("children") or []
    except ComputerError:
        return None

    for child in children:
        words = " ".join((child.get("nodeValue") or "").split())
        if child.get("nodeType") != 3 or not re.search(r"\w", words):
            continue
        listed = known.get(child.get("backendNodeId"))
        if listed is not None and not controls_only:
            return ("listed", listed, "")
        # Built from the node that was pressed: it is the one with a box.
        return ({"backendDOMNodeId": backend_id}, words, "text")

    return None


def _tag(page, backend_id):
    try:
        return (page.call("DOM.describeNode", {"backendNodeId": backend_id}).get("node", {}).get("nodeName") or "").lower()
    except ComputerError:
        return ""


def _built(page, metrics, node, role, name, value):
    """An element described from the page's accessibility tree alone, for one a look at the page has not listed."""

    backend_id = node.get("backendDOMNodeId")
    norm = "text" if role == "text" else el.normalize_role(role)

    declared, tag = {}, ""
    try:
        described = page.call("DOM.describeNode", {"backendNodeId": backend_id}).get("node", {})
        tag = (described.get("nodeName") or "").lower()
        attributes = described.get("attributes") or []
        declared = {k.lower(): v for k, v in zip(attributes[0::2], attributes[1::2]) if k.lower() in _DECLARED}
    except ComputerError:
        pass

    box = None
    try:
        quads = page.call("DOM.getContentQuads", {"backendNodeId": backend_id}).get("quads") or []
        if quads:
            origin_x, origin_y, dpr = _viewport(metrics)
            xs, ys = quads[0][0::2], quads[0][1::2]
            box = [int(origin_x + min(xs) * dpr), int(origin_y + min(ys) * dpr),
                   int((max(xs) - min(xs)) * dpr), int((max(ys) - min(ys)) * dpr)]
    except ComputerError:
        pass

    # Rooted at the site, as observe roots every path of a page.
    parsed = urllib.parse.urlparse(metrics.get("url") or "")
    site = parsed.netloc or parsed.scheme or "page"

    states = []
    if _prop(node, "focused"):
        states.append("focused")

    return {
        "role": norm,
        "name": name[:200],
        "value": str(value) if value not in (None, "") and role in INTERACTIVE else "",
        "states": states,
        "box": box,
        "path": f"site {site} > {norm} {el._quote(name, 40)}",
        "source": "cdp",
        "group": f"cdp:{page.target.get('id')}",
        "window_title": browser.page_window_title(metrics.get("title") or page.target.get("title") or ""),
        "app": "chromium",
        "bar": None,
        "handle": {
            "backend_id": backend_id,
            "target_id": page.target.get("id"),
            "dom_role": role,
            "input_type": declared.get("type", ""),
            "autocomplete": declared.get("autocomplete", ""),
            "tag": tag,
        },
    }


def hit(x, y, known_elements=(), controls_only=False):
    """
    What the page shows at a screen point, asked of the page itself:
    (element or None, {"title", "url"} or None). The browser says which node
    is there - the one on top, whatever a list of boxes would suggest - and
    that node is described as a look at the page describes it (_describe).
    Asked the moment a person presses, it is exactly what they pressed.
    """

    page = None

    try:
        page = active_page()
        metrics = json.loads(page.evaluate(_METRICS))
        context = _context(page, metrics)
        origin_x, origin_y, dpr = _viewport(metrics)
        viewport_x = (x - origin_x) / dpr
        viewport_y = (y - origin_y) / dpr

        if not (0 <= viewport_x < metrics["iw"] and 0 <= viewport_y < metrics["ih"]):
            return None, context

        # DOM.getNodeForLocation takes a point of the document, in CSS pixels:
        # measured on a page scrolled to its foot, the viewport's own point
        # answered "No node found at given location".
        found = page.call("DOM.getNodeForLocation", {
            "x": max(0, int(viewport_x + metrics["scx"])),
            "y": max(0, int(viewport_y + metrics["scy"])),
            "includeUserAgentShadowDOM": True,
        })
        backend_id = found.get("backendNodeId")
        if backend_id is None:
            return None, context

        return _describe(page, metrics, backend_id, _known_by_backend(known_elements, page), controls_only), context
    except (ComputerError, KeyError, TypeError, ValueError, OSError) as exc:
        log(f"Could not ask the page what is at {x},{y}: {exc!r}")
        return None, None
    finally:
        if page is not None:
            page.close()


_FOCUSED = """(() => {
  if (!document.hasFocus()) return null;
  let a = document.activeElement;
  // The keyboard is in the innermost thing that has it: inside a component's
  // shadow tree, and inside an inner frame, whose own document says which of
  // its fields. Found live on LinkedIn's Messaging page, which is a frame:
  // the page said "the frame" had the keyboard, so what a person typed into
  // its search box was recorded as typed into nothing.
  for (let i = 0; a && i < 20; i++) {
    if (a.shadowRoot && a.shadowRoot.activeElement) { a = a.shadowRoot.activeElement; continue; }
    let inner = null;
    if (a.tagName === 'IFRAME' || a.tagName === 'FRAME') { try { inner = a.contentDocument; } catch (e) {} }
    if (inner && inner.activeElement) { a = inner.activeElement; continue; }
    break;
  }
  return a && !['BODY', 'HTML', 'IFRAME', 'FRAME'].includes(a.tagName) ? a : null;
})()"""


def focused(known_elements=()):
    """
    The field of the page that has the keyboard right now, asked of the page:
    (element or None, {"title", "url"} or None). None when the keyboard is
    elsewhere - the address bar, or no field at all. Its value is what the
    field holds at this moment: asked as a person starts typing, it says
    whether they typed into an empty field.
    """

    page = None

    try:
        page = active_page()
        metrics = json.loads(page.evaluate(_METRICS))
        context = _context(page, metrics)

        result = page.call("Runtime.evaluate", {"expression": _FOCUSED}).get("result", {})
        if not result.get("objectId"):
            return None, context

        backend_id = page.call("DOM.describeNode", {"objectId": result["objectId"]}).get("node", {}).get("backendNodeId")
        if backend_id is None:
            return None, context

        return _describe(page, metrics, backend_id, _known_by_backend(known_elements, page), controls_only=True), context
    except (ComputerError, KeyError, TypeError, ValueError, OSError) as exc:
        log(f"Could not ask the page which field has the keyboard: {exc!r}")
        return None, None
    finally:
        if page is not None:
            page.close()


def chosen_option(element):
    """The label of the option a <select> shows as chosen right now, or None when it cannot be asked."""

    page = None

    try:
        page = _page_for(element)
        object_id = _object(page, element["handle"]["backend_id"])
        return page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                if (!this.options) { return null; }
                const o = this.options[this.selectedIndex];
                return o ? (o.label || o.text || '').trim() : '';
            }""",
        }).get("result", {}).get("value")
    except (ComputerError, KeyError, TypeError, ValueError, OSError):
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


# How long a click waits for a file chooser after its barrier (below). The
# page opens it from the click's own handler; measured on a desktop's Chrome,
# the announcement was already in before a round trip sent after the click
# came back, 20 of 20 times - the wait is a margin for a loaded desktop, and
# it is what every click that opens nothing pays.
CHOOSER_WAIT_SECONDS = 0.1


def _press(page, x, y, button, count):
    page.call("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": x, "y": y})

    for n in range(1, count + 1):
        for kind in ("mousePressed", "mouseReleased"):
            page.call("Input.dispatchMouseEvent", {
                "type": kind, "x": x, "y": y, "button": button, "clickCount": n,
            })


def _intercept_choosers(page):
    """Have the page announce a file chooser (Page.fileChooserOpened) instead of showing one."""

    try:
        page.call("Page.enable")
        page.call("Page.setInterceptFileChooserDialog", {"enabled": True})
        return True
    except ComputerError:
        return False


def click(element, button="left", count=1):
    """
    Click an element. Returns {"file_chooser": mode} when the click opened a
    file chooser, else {}.

    A native file chooser is a GTK dialog outside the page: live, an agent
    that clicked Odoo's "Upload Bill" spent a minute double-clicking inside
    "Open Files" and never chose the invoice. So file choosers are
    intercepted for the click - none is shown - and the caller is told, so
    the engine uses the upload action (set_file_input_files) instead.
    """

    page = _page_for(element)

    try:
        intercepting = button == "left" and _intercept_choosers(page)
        x, y = _center(page, element["handle"]["backend_id"])
        _press(page, x, y, button, count)

        if intercepting:
            try:
                page.evaluate("1")  # a barrier: the click's handler has run by its answer
            except ComputerError:
                pass
            opened = page.wait_event("Page.fileChooserOpened", CHOOSER_WAIT_SECONDS)
            if opened is not None:
                return {"file_chooser": opened.get("mode") or "selectSingle"}
        return {}
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


def close_list(element):
    """Press Escape in the tab `element` is on: closes an open suggestion list, keeping what was typed."""

    page = _page_for(element)
    try:
        press_key(page, "Escape")
    finally:
        page.close()


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
    Choose files for the upload control `element` is.

    The page decides which input that is, so it is asked: an element that is
    a file input, holds one or is the label of one is set directly; anything
    else (a styled "Upload" or "Browse" button, whose input may only be made
    when it is clicked - Odoo's "Upload Bill") is clicked with the page's
    file chooser intercepted, and the files go to the input the page opened
    it for. Only when clicking opens no chooser is a lone file input near the
    element used. DOM.setFileInputFiles fires the input's own input/change
    events; firing them again made some pages upload twice.
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

        direct = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "functionDeclaration": """function() {
                const isFile = el => el && el.tagName === 'INPUT' && (el.type || '').toLowerCase() === 'file';
                if (isFile(this)) return this;
                const inner = this.querySelectorAll ? this.querySelectorAll('input[type="file"]') : [];
                if (inner.length === 1) return inner[0];
                if (this.htmlFor) {
                    const el = document.getElementById(this.htmlFor);
                    if (isFile(el)) return el;
                }
                return null;
            }"""
        }).get("result", {}).get("objectId")

        if direct:
            _check_multiple(page, direct, abs_files)
            page.call("DOM.setFileInputFiles", {"files": abs_files, "objectId": direct})
            return _uploaded(abs_files)

        if _intercept_choosers(page):
            x, y = _center(page, backend_id)
            _press(page, x, y, "left", 1)
            opened = page.wait_event("Page.fileChooserOpened", CHOOSER_OPEN_SECONDS)
            if opened is not None and opened.get("backendNodeId"):
                if opened.get("mode") == "selectSingle" and len(abs_files) > 1:
                    raise ComputerError(f"{el.describe(element)} takes one file at a time - upload them one by one")
                page.call("DOM.setFileInputFiles", {"files": abs_files, "backendNodeId": opened["backendNodeId"]})
                return _uploaded(abs_files)

        # No chooser opened: a lone input in an enclosing block (a hidden
        # input beside a "Browse" button that listens for something else).
        nearby = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "functionDeclaration": """function() {
                let node = this.parentElement;
                for (let i = 0; node && i < 6; i++, node = node.parentElement) {
                    const inputs = node.querySelectorAll('input[type="file"]');
                    if (inputs.length === 1) return inputs[0];
                    if (inputs.length > 1) return null;
                }
                const all = document.querySelectorAll('input[type="file"]');
                return all.length === 1 ? all[0] : null;
            }"""
        }).get("result", {}).get("objectId")

        if not nearby:
            raise ComputerError(f"{el.describe(element)} opened no file chooser and has no file input near it - "
                                "target the control that opens the file dialog")

        _check_multiple(page, nearby, abs_files)
        page.call("DOM.setFileInputFiles", {"files": abs_files, "objectId": nearby})
        return _uploaded(abs_files)
    finally:
        page.close()


# How long an upload waits for the page to open its file chooser after the
# click: longer than a plain click's wait, since this click is for nothing else.
CHOOSER_OPEN_SECONDS = 3.0


def _check_multiple(page, object_id, files):
    if len(files) < 2:
        return
    multiple = page.call("Runtime.callFunctionOn", {
        "objectId": object_id, "returnByValue": True,
        "functionDeclaration": "function() { return !!this.multiple; }",
    }).get("result", {}).get("value")
    if not multiple:
        raise ComputerError("that file field takes one file at a time - upload them one by one")


def _uploaded(files):
    return f"Uploaded {len(files)} file(s): " + ", ".join(os.path.basename(f) for f in files)


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

    # A pre-warmed Chromium (--no-startup-window, start_agent_terminal.sh) is
    # running with no tab at all: open the page in a new one rather than
    # refusing - "Chromium has no open tab" sent an agent on a long detour.
    if not new_tab and not _page_targets():
        new_tab = True

    if new_tab:
        try:
            # The whole URL is escaped, % included: DevTools decodes this query
            # once, and a URL's own escapes left as they were came out decoded -
            # found live, `chatgpt.com/?q=what%20is%207%2B8` opened as
            # `?q=what%20is%207+8`, and ChatGPT was asked "what is 7 8?".
            target = _get("/json/new?" + urllib.parse.quote(url, safe=""), method="PUT", timeout=10)
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

