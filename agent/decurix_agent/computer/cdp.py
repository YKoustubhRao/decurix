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
from decurix_agent.computer import datefields
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
# ...and for a viewer of an app running elsewhere to catch up with the last input: an app read from its picture is read
# again by a vision model, which takes longer than a page takes to load (at most one read every 5 s, plus the call).
RELAYED_WAIT_SECONDS = 20

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

# Pages that stand in for an app running elsewhere (core.RELAYED_VIEWERS): their elements are drawn clear on purpose.
RELAYED_VIEWER_PATHS = ("/host-gui/view", "/phone-app/view")

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
# half a second, and a Chrome busy rendering an application page (the web app's quotation form,
# with another desktop loading beside it) answered later: the layer reported
# "open without its debugging port" and showed the agent an empty screen.
BUSY_ANSWER_SECONDS = float(os.environ.get("DECURIX_CDP_ANSWER_SECONDS", "5"))


def available():
    """
    Whether the browser's DevTools port answers. A refused connection (nothing
    listening) is known at once; a slow answer is a busy browser, not a missing
    port, and is waited for.
    """

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


def _number_rows(found, rows, metrics, origin_x, origin_y, dpr):
    """Say which row of a table of entries a field is in (_ROW_NUMBERS): `row` on the element, and in its path."""

    boxes = []
    for entry in rows if isinstance(rows, list) else []:
        if (not isinstance(entry, list) or len(entry) != 5 or isinstance(entry[4], bool) or not isinstance(entry[4], int)
                or not all(isinstance(v, (int, float)) for v in entry[:4]) or not 0 < entry[4] < 100000):
            continue
        x, y, w, h, n = entry
        sx, sy = origin_x + (x - metrics["scx"]) * dpr, origin_y + (y - metrics["scy"]) * dpr
        boxes.append((sx + w * dpr / 2, sy + h * dpr / 2, n))

    for element in found:
        if element["role"] not in _NAMEABLE_FIELDS | {"checkbox"} or not element.get("box"):
            continue
        bx, by, bw, bh = element["box"]
        cx, cy = bx + bw / 2, by + bh / 2
        n = next((n for hx, hy, n in boxes if abs(hx - cx) <= 3 and abs(hy - cy) <= 3), None)
        if n:
            element["row"] = n
            head, _, last = element["path"].rpartition(" > ")
            element["path"] = (head + " > " if head else "") + f"row {n} > {last}"


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
            _cancel_pending_leave_dialog(page)
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

    On a desktop the browser has no borders of its own, so the window's
    position plus the height of its bars is the answer. The window's outer
    size and position are screen pixels, but its inner size is CSS pixels,
    which a person's browser zoom changes: at 75% the page is 1707 CSS pixels
    wide in a 1280 pixel window. So the inner size is turned into screen
    pixels (times dpr, which carries the zoom) before the bars are worked out.
    Found 2026-10-21: after ctrl+minus three times every box was reported 427
    pixels left and 200 up of where it was drawn (some off the screen).
    """

    dpr = metrics["dpr"] or 1
    side = max(0, round(metrics["ow"] - metrics["iw"] * dpr))
    bars = max(0, round(metrics["oh"] - metrics["ih"] * dpr))

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
# reads them too (see _inner_frames). The two scripts below name fields from the
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
# column's header, the way a screen reader announces it. Found in the web app's order
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

# Which row of a table of entries a field is in. A table a person fills line
# by line shows the same fields in every row - or, more often, shows them only
# in the one row being edited and moves them when Tab leaves its last cell.
# Either way the fields of two rows had one path, so one ref. Found live: an
# engine filled the first row of a table, pressed Tab, and was told the three
# values it had just entered had each changed to "" - they were the
# next row's fields, under the first row's refs. It added a third row, lost its
# place and asked a person. So a field's row is part of what it is.
#
# A row is found by shape, not by markup (most such tables are <div>s with no
# roles): the nearest ancestor whose children sit side by side (the cells),
# raised to the level where it has siblings of its own kind stacked under one
# another (the rows). Sibling rows that show fields must show the same ones - a
# form laid out in rows of columns has different fields in each, and is not a
# table. Returns [x, y, w, h, row number] for each field that is in one.
_ROW_NUMBERS = """JSON.stringify((() => {
  const out = [];
  const each = (document, ox, oy, depth) => {
  const selector = 'input:not([type=hidden]), select, textarea, [contenteditable="true"], ' +
    '[role=combobox], [role=textbox], [role=spinbutton], [role=searchbox]';
  const shown = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const kind = el => el.tagName + '.' + String(el.className && el.className.baseVal === undefined ? el.className : '').trim().split(/\\s+/)[0];
  const sideBySide = el => {
    const boxes = [...el.children].filter(shown).map(c => c.getBoundingClientRect());
    if (boxes.length < 3) return false;
    let run = 1;
    for (let i = 1; i < boxes.length; i++) {
      run = (Math.abs(boxes[i].top - boxes[i - 1].top) <= 8 && boxes[i].left >= boxes[i - 1].right - 2) ? run + 1 : 1;
      if (run >= 3) return true;
    }
    return false;
  };
  const key = f => [f.tagName, f.getAttribute('type') || '', f.getAttribute('role') || '',
    (f.getAttribute('aria-label') || f.getAttribute('placeholder') || '').replace(/\\d+/g, ''),
    (f.getAttribute('name') || f.getAttribute('data-fieldname') || f.id || '').replace(/\\d+/g, '')].join('|');
  const fieldsOf = row => [...row.querySelectorAll(selector)].filter(shown).map(key);
  // The row being edited shows more fields than the others (which may show only a tick box): one's fields
  // must all be among the other's.
  const alike = (a, b) => { const [few, many] = a.length <= b.length ? [a, b] : [b, a]; return few.every(k => many.includes(k)); };
  const cellsOf = row => {
    let at = row;
    for (let d = 0; at && d < 4; d++) {
      if (sideBySide(at)) return [...at.children].filter(shown).length;
      const kids = [...at.children].filter(shown);
      if (kids.length !== 1) return 0;
      at = kids[0];
    }
    return 0;
  };
  const rowOf = new Map();
  for (const el of document.querySelectorAll(selector)) {
    if (!shown(el)) continue;
    let cells = el.parentElement;
    for (let d = 0; cells && d < 6 && !sideBySide(cells); d++) cells = cells.parentElement;
    if (!cells || cells === document.body || !sideBySide(cells)) continue;
    let row = cells, peers = [];
    for (let d = 0; row && row.parentElement && d < 3; d++, row = row.parentElement) {
      peers = [...row.parentElement.children].filter(c => kind(c) === kind(row) && shown(c));
      if (peers.length > 1) break;
    }
    if (!row || peers.length < 2 || !row.parentElement) continue;
    const stacked = peers.every((p, i) => i === 0 ||
      p.getBoundingClientRect().top >= peers[i - 1].getBoundingClientRect().bottom - 2);
    if (!stacked) continue;
    if (!rowOf.has(row)) {
      const mine = fieldsOf(row);
      // Rows of one table have the same number of cells. (Found live: a form's sections, stacked the same way,
      // passed for rows when the others showed no fields at all, and a lone tick box was called "row 1".)
      const cells = cellsOf(row);
      const shaped = cells >= 3 && peers.filter(p => cellsOf(p) === cells).length >= Math.ceil(peers.length * 0.8);
      const same = shaped && peers.every(p => p === row || alike(fieldsOf(p), mine));
      let n = 0;
      if (same) {
        const stated = parseInt(row.getAttribute('aria-rowindex') || (row.dataset && row.dataset.idx) || '', 10);
        const heads = peers.filter(p => p.querySelector('th') && !p.querySelector('td')).length;
        n = stated > 0 ? stated : peers.indexOf(row) + 1 - (row.querySelector('th') && !row.querySelector('td') ? 0 : heads);
      }
      rowOf.set(row, n);
    }
    const n = rowOf.get(row);
    if (!(n > 0)) continue;
    const r = el.getBoundingClientRect();
    out.push([r.left + ox, r.top + oy, r.width, r.height, n]);
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
# How many accessibility nodes of one page are walked. It was 6000, and a form of some forty fields - most of
# them not shown - has more: found live, the walk ended before it reached a message the application had put up
# over the form, which is the last thing in the page; the screen showed the message, the look did not, and
# nothing said the look was cut short. A walk costs about a millisecond per hundred nodes.
MAX_VISITS = 60000

_NAMEABLE_FIELDS = {"textbox", "combobox", "spinbutton", "searchbox", "listbox", "checkbox"}


# How many inner frames of one page a look reads.
MAX_FRAMES = 12


def _loader(tree):
    """Which load of a page a frame tree is of: it changes when the tab goes to another document."""

    return ((tree or {}).get("frame") or {}).get("loaderId") or ""


def _frame_tree(page):
    try:
        return page.call("Page.getFrameTree").get("frameTree") or {}
    except ComputerError:
        return {}


def _inner_frames(page, tree):
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


# Chrome's roles for a native date or time input (<input type=date|time|datetime-local|month|week>). The
# accessibility tree names the field by its label and splits it into Month/Day/Year spin buttons; listed as
# none of those, a form's dates were missing from every look (found 2026-10-13 on an order form). Each is
# listed once, as a field, and typed into through its value (type_text).
DATE_ROLES = {"date", "datetime", "inputtime", "datetimelocal", "month", "week"}


def _ax_role(node):
    role = ((node.get("role") or {}).get("value") or "").lower()
    if role in DATE_ROLES:
        return "textbox"
    # Quill and other contenteditable editors expose a focusable generic
    # rich-text node, rather than a textbox. Its descendants also inherit
    # editable, so only the focusable editor itself becomes a field.
    if role == "generic" and _prop(node, "editable") == "richtext" and _prop(node, "focusable"):
        return "textbox"
    return role


def observe():
    """Return (elements, group) for the active Chromium tab."""

    page = active_page()

    try:
        # Keyboard reloads and address-bar navigation can leave Chrome's own
        # unsaved-work dialog open, blocking every Runtime/AX read. Keep the
        # page before reading it, just as navigate() does for gui open.
        _cancel_pending_leave_dialog(page)
        # A page still loading has an accessibility tree with nothing in it
        # yet; reading it then reports an empty page the engine would act on.
        # A page that mirrors something else's controls (a phone app's
        # viewer) marks itself data-decurix-busy until its copy has caught up
        # with the last input - the approval gate must judge what is there
        # now, not the button that was there before the tap.
        relayed_page = any(viewer in (page.target.get("url") or "") for viewer in RELAYED_VIEWER_PATHS)
        deadline = time.monotonic() + (RELAYED_WAIT_SECONDS if relayed_page else LOAD_WAIT_SECONDS)
        while time.monotonic() < deadline and page.evaluate(_READY) != "complete":
            time.sleep(0.2)
        # Still catching up when the wait ran out: what is read is the copy from before the last input. Found live
        # (2026-10-16): a picture-only app's controls are read by a vision model in a few seconds, and a look taken
        # sooner showed the cursor where it had been - a worker was told its typing went into a field it had left.
        behind = page.evaluate("document.documentElement.hasAttribute('data-decurix-busy') ? 1 : 0") == 1

        metrics = json.loads(page.evaluate(_METRICS))
        nodes = page.call("Accessibility.getFullAXTree").get("nodes", [])
        tree = _frame_tree(page)
        loader = _loader(tree)
        frames = _inner_frames(page, tree)
        _splice_frames(page, nodes, frames)
        snapshot = page.call("DOMSnapshot.captureSnapshot", {"computedStyles": ["opacity"]})
        named = []
        for script in (_COLUMN_NAMES, _GROUP_LABELS, _ROW_NUMBERS):
            try:
                named.append(json.loads(page.evaluate(script) or "[]"))
            except (ComputerError, ValueError):
                named.append([])  # a name from a column is help, never a reason to fail a look
        columns, labels, rows = named
    finally:
        page.close()

    bounds = {}
    # What a field says about itself in its HTML - type="password",
    # autocomplete="cc-number" - which the accessibility tree does not carry
    # and privacy.py needs to keep a secret out of the recording.
    declared = {}
    transparent = set()
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
        # Text drawn fully transparent (itself, or anything it is inside) is not on the screen. Found live: a
        # form kit keeps a field's internal name in a tooltip of opacity 0 beside every field, and each was
        # listed as text - a fifth of the page's lines, while the fields being filled fell past the cut.
        parents = document.get("nodes", {}).get("parentIndex", [])
        clear = set()
        for node_index, style in zip(layout.get("nodeIndex", []), layout.get("styles", [])):
            if style and isinstance(style[0], int) and 0 <= style[0] < len(strings) and strings[style[0]] in ("0", "0.0"):
                clear.add(node_index)
        if clear:
            for node_index in range(len(backend_ids)):
                at, hops = node_index, 0
                while isinstance(at, int) and 0 <= at < len(parents) and hops < 60:
                    if at in clear:
                        transparent.add(backend_ids[node_index])
                        break
                    at, hops = parents[at], hops + 1
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
        "behind": behind,
        "id": group_id,
        "title": f"Chromium tab {el._quote(title, 60)} {url[:100]}",
        "window_title": browser.page_window_title(title),
        "url": url,
        "source": "cdp",
    }

    def text_of(node, depth=0, links=False):
        """
        The plain text beneath a node, for rows and paragraphs - leaving out
        links, buttons and fields, which are listed as elements of their own.
        With `links`, a link's text is kept: a table's row reads as all its
        cells, the one that is a link included (found live 2026-10-26: a
        ledger's row read without its name, and the group was taken for it).
        """
        if depth > 8:
            return ""
        role = ((node.get("role") or {}).get("value", "") or "").lower()
        if depth > 0 and role in INTERACTIVE:
            if links and role == "link":
                return " ".join(((node.get("name") or {}).get("value", "") or "").split())
            return ""
        name = (node.get("name") or {}).get("value", "") or ""
        if role in ("statictext", "inlinetextbox"):
            return " ".join(name.split())
        parts = []
        for child_id in node.get("childIds", []):
            child = by_id.get(child_id)
            if child:
                text = text_of(child, depth + 1, links)
                if text:
                    parts.append(text)
        return " ".join(parts)

    def cell_texts(row):
        texts = []
        for child_id in row.get("childIds", []):
            child = by_id.get(child_id)
            if child is None:
                continue
            child_role = (child.get("role") or {}).get("value", "").lower()
            if child_role == "link":
                text = text_of(child, 1, links=True)
            else:
                text = text_of(child, 1, links=True) if child_role not in INTERACTIVE else ""
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
    relayed = any(viewer in url for viewer in RELAYED_VIEWER_PATHS)

    while stack:
        node, segments, inside_row, bar = stack.pop()
        visits += 1
        if visits > MAX_VISITS:
            # Whatever is left is what the page put last - which is where a dialog on top of everything is.
            log(f"cdp: a page of more than {MAX_VISITS} accessibility nodes was not read to its end")
            break

        role = (node.get("role") or {}).get("value", "")
        lower = _ax_role(node)
        name = " ".join(((node.get("name") or {}).get("value", "") or "").split())
        backend_id = node.get("backendDOMNodeId")
        ignored = node.get("ignored", False)

        include = False
        display_name = name

        if not ignored and backend_id is not None:
            if lower in INTERACTIVE:
                include = True
            elif backend_id in transparent and not relayed:
                # Words nobody can see (a control drawn clear over a styled one still counts). A viewer of an app that
                # runs elsewhere draws all of its stand-ins clear over the app's picture, on purpose: found live
                # (2026-10-16), a spreadsheet's cells were dropped by this rule and the worker could not read one.
                include = False
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
                pressed = _prop(node, "pressed")
                if lower == "button" and pressed in (True, "true", "mixed"):
                    states.append("pressed")  # a toggle button that is down: Bold, Italic, Freeze Panes
                if _prop(node, "disabled"):
                    states.append("disabled")
                expanded = _prop(node, "expanded")
                if expanded is not None:
                    states.append("expanded" if expanded else "collapsed")
                if _prop(node, "selected") and lower in ("option", "tab", "treeitem", "listitem"):
                    states.append("selected")
                if not on_screen:
                    states.append("offscreen")
                if role.lower() in DATE_ROLES:
                    states.append("date" if role.lower() == "date" else "date and time" if "datetime" in role.lower()
                                  else role.lower().replace("input", ""))

                value = (node.get("value") or {}).get("value", "")
                norm = el.normalize_role(lower)

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
                        # A native drop-down (<select>) is a combobox nothing can be typed into: keys only move its choice.
                        "editable": bool(_prop(node, "editable")),
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

        if lower == "statictext":
            continue  # its children are its own text again, line by line: nothing to list, a third of a page to walk
        if role.lower() in DATE_ROLES:
            continue  # its Month/Day/Year spin buttons and picker button are parts of the one field just listed
        for child_id in reversed(node.get("childIds", [])):
            child = by_id.get(child_id)
            if child is not None:
                stack.append((child, child_segments, next_inside_row, child_bar))

    _name_by_column(found, columns, metrics, origin_x, origin_y, dpr)
    _name_by_group(found, labels, metrics, origin_x, origin_y, dpr)
    _number_rows(found, rows, metrics, origin_x, origin_y, dpr)
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
        role = _ax_role(current)
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

    object_id = _object(page, backend_id)
    if not _unobscured(page, object_id):
        page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "functionDeclaration": "function() { const node = this.nodeType === 3 ? this.parentElement : this; node.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'}); }",
        })
        quads = page.call("DOM.getContentQuads", {"backendNodeId": backend_id}).get("quads") or []
        if not quads or not _unobscured(page, object_id):
            raise ComputerError("that element is covered by another control - close the overlay or scroll it into view")

    quad = quads[0]
    xs, ys = quad[0::2], quad[1::2]

    return sum(xs) / 4, sum(ys) / 4


def _unobscured(page, object_id):
    return bool(page.call("Runtime.callFunctionOn", {
        "objectId": object_id, "returnByValue": True,
        "functionDeclaration": """function() {
            const node = this.nodeType === 3 ? this.parentElement : this;
            const rect = node.getBoundingClientRect();
            const doc = node.ownerDocument;
            const hit = doc.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
            return !!hit && (node === hit || node.contains(hit) ||
                (hit !== doc.body && hit !== doc.documentElement && hit.contains(node)));
        }""",
    }).get("result", {}).get("value"))


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
    that clicked the web app's "Upload Bill" spent a minute double-clicking inside
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


def _call_in_app(element, declaration, arguments, timeout):
    """What a function of a viewer of an app running elsewhere answers, called on `element` ("" for nothing)."""

    page = _page_for(element)
    try:
        params = {"objectId": _object(page, element["handle"]["backend_id"]), "returnByValue": True,
                  "awaitPromise": True, "functionDeclaration": declaration}
        if arguments is not None:
            params["arguments"] = arguments
        return page.call("Runtime.callFunctionOn", params, timeout=timeout).get("result", {}).get("value") or ""
    finally:
        page.close()


def press_in_app(element):
    """Press a named control in a viewer of an app running elsewhere (hostgui.html's decurixPress); raises with why not."""

    answer = _call_in_app(element, "function() { return window.decurixPress ? window.decurixPress(this) "
                                   ": 'ERR this viewer cannot press through the app (an older page)'; }", None, 20)
    if not answer or answer.startswith("ERR"):
        raise ComputerError(f"{el.describe(element)}: {answer[3:].strip() or 'no answer'}")
    return answer


def choose_in_app(element, option):
    """
    Pick an entry of a drop-down in a viewer of an app running elsewhere (hostgui.html's decurixChoose: the app's own
    accessibility selects it). Returns the entry's name; raises with the app's reason (what it offers) when none was.
    """

    answer = _call_in_app(element, "function(text) { return window.decurixChoose ? window.decurixChoose(this, text) "
                                   ": 'ERR this viewer cannot choose from a list (an older page: open the app again)'; }",
                          [{"value": option}], 30)
    if answer.startswith("ERR"):
        raise ComputerError(f"{el.describe(element)}: {answer[3:].strip()}")
    return answer


def set_in_app(element, text, append=False):
    """
    Put one line of text into a field of an app running elsewhere (hostgui.html's decurixSetValue: the app's own
    accessibility sets the field's value and gives it the keyboard). Returns what the field holds afterwards; raises with
    the app's reason when it could not be set that way - the caller then types the text.
    """

    answer = _call_in_app(element, "function(text, add) { return window.decurixSetValue ? window.decurixSetValue(this, "
                                   "text, add) : 'ERR this viewer cannot set a value (an older page)'; }",
                          [{"value": text}, {"value": bool(append)}], 20)
    if not answer.startswith("SET"):
        raise ComputerError(f"{el.describe(element)}: {answer[3:].strip() or 'no answer'}")
    return answer[4:]


# A line longer than this is pasted (insertText) rather than typed key by key: nothing a form field takes is that long,
# and a key event per character would take a while.
KEYS_UP_TO = 500


def _type_line(page, object_id, line):
    """
    Type one line into the focused single-line field the way a keyboard does: a key down and up per character. A field
    a page builds itself often listens for keys, not for text arriving: the web app's date field stayed empty when the line
    was inserted with Input.insertText (measured: value '' after insertText, '04-10-2026' and the form's date set after
    key events), so a worker typed a bill's date four times, never saw it held, and asked a person what to do. If the
    keys left the field without the line (a field that swallows key events), it is inserted as before.
    """

    if len(line) > KEYS_UP_TO:
        page.call("Input.insertText", {"text": line})
        return

    before = _field_value(page, object_id)
    for char in line:
        down = {"type": "keyDown", "key": char, "text": char, "unmodifiedText": char}
        up = {"type": "keyUp", "key": char}
        # The key's code too, as a keyboard sends it: a page that reads keyCode/which (the web app's date field) treated a
        # key with none as no key at all, and cleared what had been typed a moment later.
        code = _key_code(char)
        if code:
            for event in (down, up):
                event["code"], event["windowsVirtualKeyCode"], event["nativeVirtualKeyCode"] = code
        page.call("Input.dispatchKeyEvent", down)
        page.call("Input.dispatchKeyEvent", up)
    # A viewer of an app running elsewhere (a host app, a phone app) passes each key on to that app and its stand-in
    # field does not change: found live (2026-10-15), every line typed into real Excel was inserted a second time here
    # ("TotalTotal"), corrupting the sheet. Its keys arrived; nothing is inserted after them.
    if any(v in ((getattr(page, "target", None) or {}).get("url") or "") for v in RELAYED_VIEWER_PATHS):
        return
    after = _field_value(page, object_id)
    if after is not None and line not in after and after == (before or ""):
        page.call("Input.insertText", {"text": line})


_PUNCTUATION = {" ": ("Space", 32), "-": ("Minus", 189), "/": ("Slash", 191), ".": ("Period", 190), ",": ("Comma", 188),
                ":": ("Semicolon", 186), "@": ("Digit2", 50), "_": ("Minus", 189), "+": ("Equal", 187), "=": ("Equal", 187)}


def _key_code(char):
    """(code, keyCode, keyCode) of the US-keyboard key that types char, or None for one with no plain key."""

    if "0" <= char <= "9":
        return ("Digit" + char, ord(char), ord(char))
    if char.isascii() and char.isalpha():
        return ("Key" + char.upper(), ord(char.upper()), ord(char.upper()))
    if char in _PUNCTUATION:
        code, vk = _PUNCTUATION[char]
        return (code, vk, vk)
    return None


def _field_value(page, object_id):
    """The field's value now, or None when it has none to read (not an input)."""

    try:
        value = page.call("Runtime.callFunctionOn", {
            "objectId": object_id, "returnByValue": True,
            "functionDeclaration": "function() { return typeof this.value === 'string' ? this.value : null; }",
        }).get("result", {}).get("value")
    except ComputerError:
        return None
    return value


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
                return {multiline: multiline, input: this.tagName === 'INPUT' ? (this.type || '') : ''};
            }""",
            "arguments": [{"value": bool(replace)}],
        }).get("result", {}).get("value") or {}

        if info.get("input") in datefields.FORMATS:
            datefields.put(page, object_id, info["input"], text)
            return
        if info.get("input") == "number" and "\n" not in text and "\t" not in text and replace:
            held = set_number(page, object_id, text.strip())
            if held is not None:
                if not same_number(held, text.strip()):
                    raise ComputerError(f"the number field holds {held!r} after setting it to {text.strip()!r} - "
                                        "it takes a plain number in its own format; read it and set it again")
                return
        if info.get("multiline"):
            page.call("Input.insertText", {"text": text})
        else:
            lines = text.split("\n")
            held = enter_would_submit(page, object_id) if len(lines) > 1 else None
            for index, line in enumerate(lines):
                if line:
                    _type_line(page, object_id, line)
                elif replace and index == 0 and len(lines) == 1:
                    # Replacing with nothing: delete the selection.
                    press_key(page, "Backspace")
                if index < len(lines) - 1 and not held:
                    press_key(page, "Enter")
            if held:
                raise EnterWouldSubmit(held_enter_message(held, typed="".join(lines)))
    finally:
        page.close()


# ============================================================
# Number fields: set whole, never stepped by accident
#
# Found live (2026-10-20, P2 on a portal): typing "24" into an order line's Quantity (an <input type=number>) opened
# the browser's list of earlier entries under it; the worker clicked "option 24" by its picture, at a point that was
# the field's up/down arrows, and each click stepped the quantity (24 -> 25 ...). Confused, it typed another line's
# quantity into the field and saved the order with 60 where 24 was meant.
# So: a number field is set as a whole value (no keys, so no list of earlier entries opens) and read back; a click at
# a point on a number field's arrows, an Up/Down key in a number field and a scroll over one are refused - they change
# its value; and before a form with several filled fields is saved, what it holds is listed and confirmed.
# ============================================================

SET_NUMBER = """function(text) {
  if (this.tagName !== 'INPUT' || (this.type || '').toLowerCase() !== 'number') return null;
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
  this.focus();
  setter.call(this, text);
  this.dispatchEvent(new Event('input', {bubbles: true}));
  this.dispatchEvent(new Event('change', {bubbles: true}));
  return {value: this.value};
}"""


def set_number(page, object_id, text):
    """Set a number field to text as a whole value; its value afterwards, or None when it is not a number field."""

    result = page.call("Runtime.callFunctionOn", {
        "objectId": object_id, "returnByValue": True, "functionDeclaration": SET_NUMBER,
        "arguments": [{"value": text}],
    }).get("result", {}).get("value")
    return None if result is None else str(result.get("value") or "")


def same_number(a, b):
    """Whether two written numbers are the same number ("24" and "24.0"); text compared as text."""

    try:
        return float(str(a).replace(",", "")) == float(str(b).replace(",", ""))
    except ValueError:
        return str(a).strip() == str(b).strip()


SPINNER_AT = """function(x, y) {
  const el = document.elementFromPoint(x, y);
  if (!el || el.tagName !== 'INPUT' || (el.type || '').toLowerCase() !== 'number') return null;
  const r = el.getBoundingClientRect();
  const label = el.getAttribute('aria-label') || el.name || el.id || 'number field';
  return {label: label, value: el.value, arrows: x >= r.right - 24};
}"""


def number_field_at(x, y):
    """{"label", "value", "arrows"} when the screen point is on a number field (arrows: on its up/down arrows), else None."""

    page = None
    try:
        page = active_page()
        metrics = json.loads(page.evaluate(_METRICS))
        origin_x, origin_y, dpr = _viewport(metrics)
        vx, vy = (x - origin_x) / dpr, (y - origin_y) / dpr
        if not (0 <= vx < metrics["iw"] and 0 <= vy < metrics["ih"]):
            return None
        return page.call("Runtime.evaluate", {
            "expression": f"({SPINNER_AT})({vx}, {vy})", "returnByValue": True,
        }).get("result", {}).get("value")
    except (ComputerError, KeyError, TypeError, ValueError, OSError):
        return None
    finally:
        if page is not None:
            page.close()


FOCUSED_NUMBER = """(() => { const a = document.activeElement;
  return a && document.hasFocus() && a.tagName === 'INPUT' && (a.type || '').toLowerCase() === 'number'
    ? (a.getAttribute('aria-label') || a.name || a.id || 'number field') : null; })()"""


def focused_number_field():
    """The label of the number field that has the keyboard, or None."""

    page = None
    try:
        page = active_page()
        return page.call("Runtime.evaluate", {"expression": FOCUSED_NUMBER, "returnByValue": True}).get(
            "result", {}).get("value")
    except (ComputerError, KeyError, TypeError, ValueError, OSError):
        return None
    finally:
        if page is not None:
            page.close()


FORM_FIELDS = """function() {
  const b = this;
  const form = b.form || (b.closest && b.closest('form'));
  if (!form) return null;
  const t = (b.type || '').toLowerCase();
  const submits = (b.tagName === 'BUTTON' && (t === '' || t === 'submit')) || (b.tagName === 'INPUT' && ['submit', 'image'].includes(t));
  if (!submits) return null;
  const labelOf = (f) => {
    if (f.getAttribute('aria-label')) return f.getAttribute('aria-label');
    if (f.id) { const l = form.ownerDocument.querySelector('label[for="' + f.id + '"]'); if (l) return l.textContent.trim(); }
    const l = f.closest('label'); if (l) return l.textContent.trim();
    return f.name || f.id || f.type;
  };
  const out = [];
  for (const f of form.elements) {
    if (f.disabled) continue;
    const ft = (f.type || '').toLowerCase();
    if (['hidden', 'submit', 'button', 'reset', 'image', 'file', 'password'].includes(ft)) continue;
    let v = '';
    if (f.tagName === 'SELECT') { const o = f.options[f.selectedIndex]; v = o && o.value !== '' ? o.label : ''; }
    else if (ft === 'checkbox' || ft === 'radio') { if (!f.checked) continue; v = 'checked'; }
    else v = String(f.value || '');
    if (v.trim() === '') continue;
    out.push([labelOf(f).slice(0, 60), v.slice(0, 80)]);
  }
  const name = form.getAttribute('aria-label') || form.getAttribute('name') || form.id ||
    ((form.querySelector('legend, h1, h2, h3') || {}).textContent || '').trim() || 'the form';
  return {form: name.slice(0, 80), fields: out};
}"""

# A form with fewer filled fields than this (a search box and its button, a sign-in) is sent without the check.
FORM_CHECK_MIN_FIELDS = 3


def submit_form_fields(element, min_fields=None):
    """{"form", "fields": [[label, value]]} when clicking element submits a form with several filled fields (at least
    `min_fields`, FORM_CHECK_MIN_FIELDS by default), else None."""

    page = None
    try:
        page = _page_for(element)
        object_id = _object(page, element["handle"]["backend_id"])
        found = page.call("Runtime.callFunctionOn", {
            "objectId": object_id, "returnByValue": True, "functionDeclaration": FORM_FIELDS,
        }).get("result", {}).get("value")
        if not found or len(found.get("fields") or []) < (FORM_CHECK_MIN_FIELDS if min_fields is None else min_fields):
            return None
        return found
    except (ComputerError, KeyError, TypeError, ValueError, OSError):
        return None
    finally:
        if page is not None:
            page.close()


# ============================================================
# Enter in a form's line field submits the form
#
# Found live (2026-10-20, P2 on a portal): a unit price typed into one line of an order form "and Enter" submitted the
# whole form at once - an order saved with one line of several, which the portal cannot edit. A browser submits a
# <form> when Enter is pressed in one of its single-line inputs (implicit submission). So Enter is held back there
# while the form still has other empty fields: the text is typed, and the operator is told to move on with Tab or a
# click, and to press the form's own submit button when everything is filled. A textarea, a search box, a field
# outside a form, a form with no submit control, or a form whose fields are all filled is left alone.
# ============================================================

ENTER_SUBMITS = """function() {
  const el = this;
  if (!el || el.tagName !== 'INPUT') return null;
  const t = (el.type || 'text').toLowerCase();
  if (['search', 'checkbox', 'radio', 'button', 'submit', 'reset', 'file', 'image', 'hidden', 'range', 'color'].includes(t)) return null;
  if ((el.getAttribute('role') || '').toLowerCase() === 'searchbox') return null;
  const form = el.form;
  if (!form) return null;
  const submit = form.querySelector('button:not([type]), button[type=submit], input[type=submit], input[type=image]');
  if (!submit) return null;
  const visible = (f) => { const r = f.getBoundingClientRect(); const s = getComputedStyle(f);
    return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'; };
  let empty = 0;
  for (const f of form.elements) {
    if (f === el || f.disabled || f.readOnly) continue;
    const ft = (f.type || '').toLowerCase();
    if (f.tagName === 'INPUT' && ['hidden', 'submit', 'button', 'reset', 'image', 'checkbox', 'radio', 'file'].includes(ft)) continue;
    if (!['INPUT', 'TEXTAREA', 'SELECT'].includes(f.tagName) || !visible(f)) continue;
    if (String(f.value || '').trim() === '') empty++;
  }
  if (!empty) return null;
  const name = form.getAttribute('aria-label') || form.getAttribute('name') || form.id ||
    ((form.querySelector('legend, h1, h2, h3') || {}).textContent || '').trim() || 'on this page';
  return {form: name.slice(0, 80), empty: empty};
}"""


class EnterWouldSubmit(ComputerError):
    """Enter was held back: it would have submitted a form with fields still empty."""


def enter_would_submit(page, object_id):
    """{"form", "empty"} when Enter in this element would submit its form with other fields still empty, else None."""

    try:
        return page.call("Runtime.callFunctionOn", {
            "objectId": object_id, "returnByValue": True, "functionDeclaration": ENTER_SUBMITS,
        }).get("result", {}).get("value")
    except ComputerError:
        return None


def held_enter_message(found, typed=None):
    """What the operator is told when Enter was held back."""

    form = found.get("form") or "on this page"
    form = form if form == "on this page" else f"'{form}'"
    did = f"Typed {typed!r} without the Enter: " if typed is not None else "Enter was not pressed: "
    return (did + f"Enter would submit the form {form} now, with {found.get('empty', 1)} other field(s) still empty. "
            "Move on with Tab or click the next field; press the form's submit button when every field is filled.")


def focused_enter_would_submit():
    """Enter pressed now: would it submit a form with fields still empty? ({"form", "empty"} or None.)"""

    page = None
    try:
        page = active_page()
        result = page.call("Runtime.evaluate", {"expression": _FOCUSED}).get("result", {})
        if not result.get("objectId"):
            return None
        return enter_would_submit(page, result["objectId"])
    except (ComputerError, KeyError, TypeError, ValueError, OSError):
        return None
    finally:
        if page is not None:
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


def submit_query(element):
    """
    Press Enter in a field whose form only asks the page something - a form sent by GET (a search, a filter), or a
    search box outside any form - and return True. A field of a form that would change something (sent by POST) is
    never submitted this way: False, nothing pressed. False too when the field cannot be read.
    """

    try:
        page = _page_for(element)
    except (ComputerError, KeyError, TypeError):
        return False
    try:
        object_id = _object(page, element["handle"]["backend_id"])
        asks = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                const f = this.form;
                if (!f) { return (this.type || '').toLowerCase() === 'search'; }
                return (f.getAttribute('method') || 'get').toLowerCase() === 'get';
            }""",
        }).get("result", {}).get("value")
        if asks is not True:
            return False
        page.call("DOM.focus", {"backendNodeId": element["handle"]["backend_id"]})
        press_key(page, "Enter")
        return True
    except (ComputerError, KeyError, TypeError):
        return False
    finally:
        page.close()


def native_options(element):
    """The options of a native drop-down (<select>) as its labels, or None when the element is not one - a list that
    opens in the page (its entries then become elements of their own) is not."""
    page = _page_for(element)
    try:
        object_id = _object(page, element["handle"]["backend_id"])
        result = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                if (this.tagName !== 'SELECT' || !this.options) { return null; }
                return Array.from(this.options).map(o => o.label.trim()).filter(Boolean);
            }""",
        }).get("result", {}).get("value")
    finally:
        page.close()
    return result if isinstance(result, list) else None


def datalist_options(element):
    """The entries of a field's <datalist> (an <input list=...>: a list the browser draws itself, outside the page's
    tree), as their values, or None when the field has none."""
    page = _page_for(element)
    try:
        object_id = _object(page, element["handle"]["backend_id"])
        result = page.call("Runtime.callFunctionOn", {
            "objectId": object_id,
            "returnByValue": True,
            "functionDeclaration": """function() {
                const l = this.list;
                if (!l || !l.options) { return null; }
                return Array.from(l.options).map(o => (o.value || o.label || o.textContent || '').trim()).filter(Boolean);
            }""",
        }).get("result", {}).get("value")
    finally:
        page.close()
    return result if isinstance(result, list) else None


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
                // A document says more than a list's entry: "IT department" for the option "IT". The one option
                // whose whole name lies within the text given, as words, is that option - never one of two.
                const words = s => ' ' + s.toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim() + ' ';
                const within = () => {
                    const given = words(label);
                    const fits = options.filter(o => o.value !== '' && words(o.label).trim() && given.includes(words(o.label)));
                    return fits.length === 1 ? fits[0] : null;
                };
                // The one option holding every word given, in any order and singular or plural: "navy baseball
                // cap" is "CAP-NV - Baseball cap, navy". Found live (2026-10-14): word order alone sent a worker to
                // pick a product by position, and it took a notebook.
                const stem = w => w.replace(/(?:es|s)$/, '');
                const allWords = () => {
                    const given = words(label).trim().split(' ').filter(Boolean).map(stem);
                    if (given.length < 2) { return null; }
                    const fits = options.filter(o => {
                        const have = new Set(words(o.label).trim().split(' ').map(stem));
                        return o.value !== '' && given.every(w => have.has(w));
                    });
                    return fits.length === 1 ? fits[0] : null;
                };
                const hit = options.find(o => o.label.trim().toLowerCase() === want)
                    || options.find(o => o.label.trim().toLowerCase().startsWith(want))
                    || options.find(o => o.label.toLowerCase().includes(want))
                    || within()
                    || allWords();
                if (!hit) { return {ok: false, options: options.map(o => o.label)}; }
                // A list a click opened keeps its own highlighted entry: set underneath it, the value is undone by
                // the Return that closes it - found live (2026-10-22), "29-Karnataka" went back to
                // "Select" at the taught Return. Losing focus closes the list; focus comes back after.
                const focused = document.activeElement === this;
                if (focused) { this.blur(); }
                this.value = hit.value;
                this.dispatchEvent(new Event('input', {bubbles: true}));
                this.dispatchEvent(new Event('change', {bubbles: true}));
                if (focused) { this.focus(); }
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
    when it is clicked - the web app's "Upload Bill") is clicked with the page's
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


def _cancel_pending_leave_dialog(page):
    """Cancel only beforeunload dialogs; leave other dialogs for the normal workflow."""
    page.call("Page.enable")
    showing = [m for m in page.events if m.get("method") == "Page.javascriptDialogOpening"]
    pending = [m for m in showing if (m.get("params") or {}).get("type") == "beforeunload"]
    if not pending:
        return False
    page.call("Page.handleJavaScriptDialog", {"accept": False})
    page.events = [m for m in page.events if m not in pending]
    return True


def _navigate_unless_asked_to_stay(page, url):
    """
    Page.navigate's answer, or None when the page asked before leaving ("Leave site? Changes you made may not be
    saved") - which this answers Cancel, so nothing is lost. Found live: a form left unsaved in the web app, a `gui open`
    on its tab, and Chrome's own dialog held every DevTools call after it ("did not answer Page.navigate within 20s")
    with the browser frozen behind it. A person reading that dialog keeps their work; so does this.
    """

    page.call("Page.enable")
    # A dialog already showing is announced when the domain is enabled: the same answer, before anything else.
    showing = [m for m in page.events if m.get("method") == "Page.javascriptDialogOpening"]
    page.events = [m for m in page.events if m not in showing]
    if any((m.get("params") or {}).get("type") == "beforeunload" for m in showing):
        page.call("Page.handleJavaScriptDialog", {"accept": False})
        return None
    page.seq += 1
    mine = page.seq
    page.ws.send(json.dumps({"id": mine, "method": "Page.navigate", "params": {"url": url}}))
    deadline = time.monotonic() + CALL_TIMEOUT
    stayed = False

    while time.monotonic() < deadline:
        page.ws.sock.settimeout(max(0.1, deadline - time.monotonic()))
        try:
            message = json.loads(page.ws.receive())
        except socket.timeout:
            break
        if message.get("method") == "Page.javascriptDialogOpening" and \
                (message.get("params") or {}).get("type") == "beforeunload":
            page.seq += 1
            page.ws.send(json.dumps({"id": page.seq, "method": "Page.handleJavaScriptDialog",
                                     "params": {"accept": False}}))
            stayed = True
            continue
        if message.get("id") == mine:
            if stayed:
                return None
            if "error" in message:
                raise ComputerError(f"Page.navigate: {message['error'].get('message')}")
            return message.get("result", {})

    if stayed:
        return None
    raise ComputerError(f"Chromium did not answer Page.navigate within {CALL_TIMEOUT}s")


NEW_TAB_WAIT_SECONDS = 30


def _new_tab_after(before, seconds):
    """A page target that was not among `before`, waited for up to `seconds`, or None."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            for t in _page_targets():
                if t.get("id") not in before:
                    return t
        except (OSError, ValueError, ComputerError):
            pass
        time.sleep(0.5)
    return None


def navigate(url, new_tab=False):
    """Load a URL in the active tab (or a new one) and wait for it to load."""

    # A pre-warmed Chromium (--no-startup-window, start_agent_terminal.sh) is
    # running with no tab at all: open the page in a new one rather than
    # refusing - "Chromium has no open tab" sent an agent on a long detour.
    if not new_tab and not _page_targets():
        new_tab = True

    if new_tab:
        try:
            before = {t.get("id") for t in _page_targets()}
        except ComputerError:
            before = set()
        try:
            # The whole URL is escaped, % included: DevTools decodes this query
            # once, and a URL's own escapes left as they were came out decoded -
            # found live, `chatgpt.com/?q=what%20is%207%2B8` opened as
            # `?q=what%20is%207+8`, and ChatGPT was asked "what is 7 8?".
            target = _get("/json/new?" + urllib.parse.quote(url, safe=""), method="PUT", timeout=10)
        except (OSError, ValueError) as exc:
            # A busy browser (a 1-vCPU desktop just restarted, a heavy web app in other tabs) answers after the
            # tab is made: found live, "timed out" here, the tab there a moment later, and every retry opened
            # another one. Take the tab when it comes rather than ask for a second.
            target = _new_tab_after(before, NEW_TAB_WAIT_SECONDS)
            if target is None:
                raise ComputerError(f"could not open a new tab: {exc}")
        page = Page(target)
    else:
        page = active_page()

    try:
        if not new_tab:
            result = _navigate_unless_asked_to_stay(page, url)
            if result is None:
                # The page has unsaved work and asked before leaving: keep it, open the address beside it.
                page.close()
                return navigate(url, new_tab=True)
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

