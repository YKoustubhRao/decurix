"""
The screen side's window list in broker mode (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "The panel's window
list"): a row of buttons on the bottom bar, one per window, that a person clicks to bring a window forward.

Why it exists: in broker mode the panel is the worker's, an untrusted X client, and the SECURITY extension gives an
untrusted client none of the root window's events, so the panel's own window list never filled and a person could not
switch windows from it. This runs as the screen user (decurixscreen, started by agent/decurix-screen-session.sh) and
reads the window manager's list, which only a trusted client may. The worker's panel no longer holds a window list
in this mode (agent/decurix-panel-broker.xml: its launchers at the left, its tray and clock at the right); this list
sits in the space between them, where the window list was.

Why nothing here can start a program, as the screen user, past the gate:
- **What a click does.** The only input it acts on is a left-button release on one of its buttons, and the only thing
  that does is send the window manager one of two messages about a window already in the window manager's own list:
  _NET_ACTIVE_WINDOW (bring it forward) or WM_CHANGE_STATE IconicState (minimize the active one). That is window
  management, which the window manager does for any window, not input into an app, so it needs no gate. No other
  button does anything, it never takes the keyboard (WM_HINTS input False, no key events selected), and it has no
  menu, no dialog, no drop target and no tooltip.
- **It starts nothing.** It imports no way to start a process (no subprocess, os.exec*, os.spawn*, os.system,
  posix_spawn, xdg-open or Gio); agent/tests/test_windowlist.py reads this file for each of them.
- **No toolkit.** It draws its own pixels (PIL) and speaks X with python-xlib. A GTK program would read the XSETTINGS
  manager, which in this mode is the worker's xfsettingsd: its Gtk/Modules setting names libraries for every GTK
  client to load (a path is loaded as it is), and GTK brings menus, the inspector and link labels besides.
- **No configuration.** It reads no file but its fonts (under /usr/share/fonts, root's) and has no settings anyone
  can change while it runs; its look and its place are this file's.
- **What it reads of the worker's windows** - their titles, icons and the worker's bars' places - is data drawn as
  pixels, read with bounded sizes (a title of at most 4 kB, icons of at most 1 MB, sides of at most 512).

Why not a second xfce4-panel run as the screen user (the first design): it cannot be locked enough. With its panels
locked (xfconf's locked properties; 4.18 no longer reads kioskrc for this) its right-click menu still has Log Out
(runs xfce4-session-logout), Help (opens a browser) and About (links that open one), and the tasklist plugin's own
button menu has "Launch New Instance", which runs /proc/<pid>/exe of the pid in the window's _NET_WM_PID - a
property each window's own client sets, so a window of the worker's could name any process of the screen user's.

Run by agent/decurix-screen-session.sh: `python3 -m decurix_agent.computer.windowlist`, with the screen user's
trusted cookie. Exit code 3: it cannot run here (no PIL or python-xlib, no display), and the session does not start
it again.
"""

import os
import select
import struct
import sys
import unicodedata

# The bar: the worker's bars' height when they are found, else this; never outside these bounds.
HEIGHT_DEFAULT, HEIGHT_MIN, HEIGHT_MAX = 44, 24, 96
# A button: as wide as BUTTON_MAX, narrower to fit, never narrower than BUTTON_MIN (an icon alone below LABEL_MIN).
BUTTON_MAX, BUTTON_MIN, LABEL_MIN = 200, 44, 96
ICON = 22
# Where the list goes when the worker's bars are not where agent/decurix-panel-broker.xml puts them (a bar across the
# middle of the bottom edge): past a left group of four icons and before a tray and a clock.
LEFT_RESERVE, RIGHT_RESERVE = 200, 300
# Bounds on what is read of a window's client (the worker's, for its windows).
MAX_TITLE_LONGS = 1024          # 4 kB
MAX_ICON_LONGS = 1 << 18        # 1 MB
MAX_ICON_SIDE = 512
MAX_TITLE_CHARS = 200
MAX_CLIENTS = 256
# A full look at the window manager's list this often even with no event (a bar moved without telling anyone).
SCAN_SECONDS = 2.0

# The look: the worker's panel's (Yaru-red-dark): a dark bar, a lighter active button with a red line under it.
BACKGROUND = (24, 24, 24)
ACTIVE = (58, 58, 58)
ACCENT = (233, 84, 32)
TEXT = (238, 238, 238)
TEXT_HIDDEN = (150, 150, 150)
FONT_SIZE = 13
FONT_FILES = ("Inter-Regular.otf", "Inter-Regular.ttf", "InterVariable.ttf", "Inter.ttc", "DejaVuSans.ttf")
FONT_ROOT = "/usr/share/fonts"

ATOMS = (
    "_NET_CLIENT_LIST", "_NET_ACTIVE_WINDOW", "_NET_WM_WINDOW_TYPE", "_NET_WM_WINDOW_TYPE_NORMAL",
    "_NET_WM_WINDOW_TYPE_DIALOG", "_NET_WM_WINDOW_TYPE_DOCK", "_NET_WM_STATE", "_NET_WM_STATE_SKIP_TASKBAR",
    "_NET_WM_STATE_HIDDEN", "_NET_WM_STATE_STICKY", "_NET_WM_STATE_SKIP_PAGER", "_NET_WM_NAME", "_NET_WM_ICON",
    "_NET_WM_PID", "_NET_WM_DESKTOP", "_NET_WM_STRUT", "_NET_WM_STRUT_PARTIAL", "UTF8_STRING", "WM_CHANGE_STATE",
    "WM_NAME",
)
# A window's properties whose change changes its button.
WATCHED = ("_NET_WM_NAME", "WM_NAME", "_NET_WM_ICON", "_NET_WM_STATE", "_NET_WM_WINDOW_TYPE")
# Changes of the root window's that change the list.
ROOT_WATCHED = ("_NET_CLIENT_LIST", "_NET_ACTIVE_WINDOW")

ICONIC_STATE = 3
SOURCE_PAGER = 2


def listed(types, states, transient, atoms):
    """Whether a client window gets a button: a normal window, or a dialog with no parent window, unless it asks to be
    left out of a task bar. types and states are the window's _NET_WM_WINDOW_TYPE and _NET_WM_STATE atoms; the first
    type this knows decides, as EWMH says; no type is a normal window, or a dialog when it has a parent."""

    if atoms["_NET_WM_STATE_SKIP_TASKBAR"] in states:
        return False
    for kind in types:
        if kind == atoms["_NET_WM_WINDOW_TYPE_NORMAL"]:
            return True
        if kind == atoms["_NET_WM_WINDOW_TYPE_DIALOG"]:
            return not transient
        return False
    return not transient


def place(width, height, bars):
    """Where the list goes, (x, y, w, h) on the bottom edge of a width x height screen: between the worker's bars at
    the left and the right of that edge (bars: (x, y, w, h) of each dock window). A bar across the middle of the edge
    (not the broker layout) leaves the reserves either side instead."""

    bottom = [b for b in bars if b[2] > 0 and b[3] > 0 and b[1] + b[3] == height]
    tall = max((b[3] for b in bottom), default=HEIGHT_DEFAULT)
    tall = max(HEIGHT_MIN, min(HEIGHT_MAX, tall))
    middle = width // 2
    if any(x < middle < x + w for x, _, w, _ in bottom):
        left, right = LEFT_RESERVE, width - RIGHT_RESERVE
    else:
        left = max((x + w for x, _, w, _ in bottom if x + w <= middle), default=0)
        right = min((x for x, _, _, _ in bottom if x >= middle), default=width)
    left = max(0, min(left, width))
    right = max(left, min(right, width))
    return left, height - tall, right - left, tall


def layout(count, width):
    """The buttons' (x, w) inside a list width wide, left to right: BUTTON_MAX wide, narrower to fit, never under
    BUTTON_MIN; the ones that do not fit are left out."""

    if count <= 0 or width < BUTTON_MIN:
        return []
    fits = min(count, width // BUTTON_MIN)
    each = min(BUTTON_MAX, width // fits)
    return [(i * each, each) for i in range(fits)]


def pick_icon(data, want=ICON):
    """The icon nearest want px in a _NET_WM_ICON value (width, height, then width*height ARGB cardinals; repeated):
    (w, h, cardinals) of the smallest one at least want wide, else the largest; None when there is none. A size past
    MAX_ICON_SIDE or past the end of the data ends the walk: the window's client wrote it."""

    best = None
    i, n = 0, len(data)
    while i + 2 <= n:
        w, h = int(data[i]), int(data[i + 1])
        i += 2
        if not (0 < w <= MAX_ICON_SIDE and 0 < h <= MAX_ICON_SIDE) or i + w * h > n:
            break
        if best is None:
            best = (w, h, i)
        else:
            bw = best[0]
            if (bw < want and w > bw) or (want <= w < bw):
                best = (w, h, i)
        i += w * h
    if best is None:
        return None
    w, h, start = best
    return w, h, data[start:start + w * h]


def clean_title(raw):
    """A window's title as it is drawn: text only (no control or format characters), at most MAX_TITLE_CHARS."""

    if isinstance(raw, (bytes, bytearray)):
        raw = bytes(raw).decode("utf-8", "replace")
    text = "".join(" " if unicodedata.category(c)[0] in "CZ" else c for c in str(raw or "")[:MAX_TITLE_CHARS * 2])
    return " ".join(text.split())[:MAX_TITLE_CHARS]


def load_font(size=FONT_SIZE):
    """The first of FONT_FILES under FONT_ROOT, else PIL's own font."""

    from PIL import ImageFont

    found = {}
    for folder, _, files in os.walk(FONT_ROOT):
        for name in files:
            if name in FONT_FILES and name not in found:
                found[name] = os.path.join(folder, name)
    for name in FONT_FILES:
        if name in found:
            try:
                return ImageFont.truetype(found[name], size)
            except Exception:  # noqa: BLE001 - the next one
                continue
    try:
        return ImageFont.load_default(size)
    except TypeError:  # an older PIL: its fixed bitmap font
        return ImageFont.load_default()


def fit(text, font, room):
    """text cut to room pixels with an ellipsis, or "" when not even that fits."""

    if room <= 0 or not text:
        return ""
    if font.getlength(text) <= room:
        return text
    while text and font.getlength(text + "\u2026") > room:
        text = text[:-1]
    return text + "\u2026" if text else ""


def icon_image(icon, size=ICON):
    """A PIL RGBA image of an icon from pick_icon, scaled to size."""

    from PIL import Image

    w, h, cardinals = icon
    raw = struct.pack(f"<{w * h}I", *(int(v) & 0xFFFFFFFF for v in cardinals))
    image = Image.frombytes("RGBA", (w, h), raw, "raw", "BGRA")
    if (w, h) != (size, size):
        image = image.resize((size, size), getattr(Image, "Resampling", Image).LANCZOS)
    return image


def render(entries, width, height, font):
    """The list as BGRX bytes (the screen's 24-bit ZPixmap order on a little-endian server), width x height: one button
    per entry ({"title", "icon", "active", "hidden"}) as layout() places them."""

    from PIL import Image, ImageDraw

    image = Image.new("RGB", (max(1, width), max(1, height)), BACKGROUND)
    draw = ImageDraw.Draw(image)
    for (x, w), entry in zip(layout(len(entries), width), entries):
        if entry.get("active"):
            draw.rectangle([x + 2, 3, x + w - 3, height - 4], fill=ACTIVE)
            draw.rectangle([x + 8, height - 4, x + w - 9, height - 2], fill=ACCENT)
        labelled = w >= LABEL_MIN
        ix = x + 10 if labelled else x + (w - ICON) // 2
        iy = (height - ICON) // 2
        icon = entry.get("icon")
        if icon is not None:
            try:
                picture = icon_image(icon)
                image.paste(picture, (ix, iy), picture)
            except Exception:  # noqa: BLE001 - a broken icon: the letter instead
                icon = None
        colour = TEXT_HIDDEN if entry.get("hidden") else TEXT
        if icon is None:
            draw.rectangle([ix + 2, iy + 2, ix + ICON - 3, iy + ICON - 3], outline=colour)
            letter = (entry.get("title") or "?")[:1].upper()
            draw.text((ix + ICON // 2, iy + ICON // 2), letter, fill=colour, font=font, anchor="mm")
        if labelled:
            room = w - (ix - x) - ICON - 18
            draw.text((ix + ICON + 8, height // 2), fit(entry.get("title") or "", font, room), fill=colour,
                      font=font, anchor="lm")
    rgba = image.convert("RGBA").tobytes()
    out = bytearray(rgba)
    out[0::4], out[2::4] = rgba[2::4], rgba[0::4]
    return bytes(out)


def button_at(entries, width, x):
    """The entry whose button is at x in the list, or None."""

    for (bx, bw), entry in zip(layout(len(entries), width), entries):
        if bx <= x < bx + bw:
            return entry
    return None


class WindowList:
    """The list's X window and what it shows. Everything it asks of the X server is a read, its own window's
    properties, or the two window-manager messages of click()."""

    def __init__(self, display, font):
        from Xlib import X, Xatom, Xutil

        self.d = display
        self.font = font
        self.screen = display.screen()
        self.root = self.screen.root
        self.atom = {name: display.intern_atom(name) for name in ATOMS}
        self.icons = {}
        self.serial = 0
        self.watched = set()
        self.entries = []
        self.shown = None
        self.box = None
        self.pixmap = None
        r, g, b = BACKGROUND
        geometry = self.root.get_geometry()
        # Presses are selected only so the press's implicit grab is this window's and its release comes here (without
        # them the window manager's selection on the root took the grab, found live); handle() ignores them.
        self.window = self.root.create_window(
            0, geometry.height - HEIGHT_DEFAULT, 1, HEIGHT_DEFAULT, 0, self.screen.root_depth, X.InputOutput,
            X.CopyFromParent, background_pixel=(r << 16) | (g << 8) | b,
            event_mask=X.ButtonPressMask | X.ButtonReleaseMask | X.StructureNotifyMask)
        w = self.window
        w.set_wm_name("Windows")
        w.set_wm_class("decurix-windowlist", "Decurix-windowlist")
        w.set_wm_hints(flags=Xutil.InputHint, input=0)
        w.change_property(self.atom["_NET_WM_NAME"], self.atom["UTF8_STRING"], 8, b"Windows")
        w.change_property(self.atom["_NET_WM_WINDOW_TYPE"], Xatom.ATOM, 32, [self.atom["_NET_WM_WINDOW_TYPE_DOCK"]])
        w.change_property(self.atom["_NET_WM_STATE"], Xatom.ATOM, 32,
                          [self.atom[n] for n in ("_NET_WM_STATE_STICKY", "_NET_WM_STATE_SKIP_TASKBAR",
                                                  "_NET_WM_STATE_SKIP_PAGER")])
        w.change_property(self.atom["_NET_WM_DESKTOP"], Xatom.CARDINAL, 32, [0xFFFFFFFF])
        w.change_property(self.atom["_NET_WM_PID"], Xatom.CARDINAL, 32, [os.getpid()])
        self.root.change_attributes(event_mask=X.PropertyChangeMask | X.StructureNotifyMask)
        display.set_error_handler(lambda *_: None)   # a window that went away between the list and a request
        w.map()
        display.flush()

    def _get(self, window, name, kind, longs):
        from Xlib import X

        try:
            reply = window.get_property(self.atom[name] if isinstance(name, str) else name,
                                        kind if kind is not None else X.AnyPropertyType, 0, longs)
        except Exception:  # noqa: BLE001 - gone, or not readable
            return None
        return None if reply is None else reply.value

    def _title(self, window):
        value = self._get(window, "_NET_WM_NAME", self.atom["UTF8_STRING"], MAX_TITLE_LONGS)
        if not value:
            value = self._get(window, "WM_NAME", None, MAX_TITLE_LONGS)
        return clean_title(value or "")

    def _icon(self, wid, window):
        """(serial, icon): the serial changes whenever the icon is read again."""
        if wid not in self.icons:
            from Xlib import Xatom

            data = self._get(window, "_NET_WM_ICON", Xatom.CARDINAL, MAX_ICON_LONGS)
            self.serial += 1
            self.icons[wid] = (self.serial, pick_icon(list(data)) if data else None)
        return self.icons[wid]

    def _box(self, window):
        try:
            geometry = window.get_geometry()
            origin = window.translate_coords(self.root, 0, 0)
        except Exception:  # noqa: BLE001
            return None
        return -origin.x, -origin.y, geometry.width, geometry.height

    def _watch(self, wid, window, mask):
        if wid not in self.watched:
            window.change_attributes(event_mask=mask)
            self.watched.add(wid)

    def scan(self):
        """Read the window manager's list again and show it: move the list between the worker's bars, and draw it when
        what it shows changed."""

        from Xlib import X, Xatom

        clients = list(self._get(self.root, "_NET_CLIENT_LIST", Xatom.WINDOW, MAX_CLIENTS) or [])
        active = list(self._get(self.root, "_NET_ACTIVE_WINDOW", Xatom.WINDOW, 1) or [0])[:1] or [0]
        bars, entries = [], []
        for wid in clients:
            wid = int(wid)
            if wid == self.window.id:
                continue
            window = self.d.create_resource_object("window", wid)
            types = list(self._get(window, "_NET_WM_WINDOW_TYPE", Xatom.ATOM, 32) or [])
            if self.atom["_NET_WM_WINDOW_TYPE_DOCK"] in types:
                box = self._box(window)
                if box:
                    bars.append(box)
                self._watch(wid, window, X.StructureNotifyMask)
                continue
            states = list(self._get(window, "_NET_WM_STATE", Xatom.ATOM, 32) or [])
            transient = bool(self._get(window, Xatom.WM_TRANSIENT_FOR, Xatom.WINDOW, 1))
            if not listed(types, states, transient, self.atom):
                continue
            self._watch(wid, window, X.PropertyChangeMask | X.StructureNotifyMask)
            serial, icon = self._icon(wid, window)
            entries.append({"id": wid, "title": self._title(window), "icon": icon, "serial": serial,
                            "active": wid == int(active[0]), "hidden": self.atom["_NET_WM_STATE_HIDDEN"] in states})
        live = {e["id"] for e in entries}
        self.icons = {k: v for k, v in self.icons.items() if k in live}
        self.watched &= set(int(c) for c in clients)
        geometry = self.root.get_geometry()
        box = place(geometry.width, geometry.height, bars)
        if box != self.box:
            self._move(box)
        self.entries = entries
        shown = (box, tuple((e["id"], e["title"], e["serial"], e["active"], e["hidden"]) for e in entries))
        if shown != self.shown:
            self._draw(box, entries)
            self.shown = shown
        self.d.flush()

    def _move(self, box):
        from Xlib import Xatom

        x, y, w, h = box
        self.window.configure(x=x, y=y, width=max(1, w), height=h)
        self.window.change_property(self.atom["_NET_WM_STRUT"], Xatom.CARDINAL, 32, [0, 0, 0, h])
        self.window.change_property(self.atom["_NET_WM_STRUT_PARTIAL"], Xatom.CARDINAL, 32,
                                    [0, 0, 0, h, 0, 0, 0, 0, 0, 0, x, max(x, x + w - 1)])
        self.box = box

    def _draw(self, box, entries):
        from Xlib import X

        _, _, width, height = box
        width = max(1, width)
        data = render(entries, width, height, self.font)
        pixmap = self.window.create_pixmap(width, height, self.screen.root_depth)
        gc = pixmap.create_gc()
        row = width * 4
        step = max(1, 200000 // row)
        for y in range(0, height, step):
            rows = min(step, height - y)
            pixmap.put_image(gc, 0, y, width, rows, X.ZPixmap, self.screen.root_depth, 0,
                             data[y * row:(y + rows) * row])
        gc.free()
        self.window.change_attributes(background_pixmap=pixmap)
        self.window.clear_area(0, 0, 0, 0)
        if self.pixmap is not None:
            self.pixmap.free()
        self.pixmap = pixmap

    def click(self, x, when):
        """A left-button release at x in the list: bring that button's window forward, or minimize it when it is the
        active one already. The window is one of the window manager's own list (the last scan); the message goes to
        the window manager, as a pager's would (EWMH source 2)."""

        from Xlib import X, protocol

        if self.box is None:
            return
        entry = button_at(self.entries, self.box[2], x)
        if entry is None:
            return
        target = self.d.create_resource_object("window", entry["id"])
        if entry["active"] and not entry["hidden"]:
            kind, data = self.atom["WM_CHANGE_STATE"], [ICONIC_STATE, 0, 0, 0, 0]
        else:
            kind, data = self.atom["_NET_ACTIVE_WINDOW"], [SOURCE_PAGER, int(when) & 0xFFFFFFFF, self.window.id, 0, 0]
        event = protocol.event.ClientMessage(window=target, client_type=kind, data=(32, data))
        self.root.send_event(event, event_mask=X.SubstructureRedirectMask | X.SubstructureNotifyMask)
        self.d.flush()

    def handle(self, event):
        """One X event: True when the list must be read again."""

        from Xlib import X

        if event.type == X.ButtonRelease:
            if event.window.id == self.window.id and event.detail == 1:
                self.click(event.event_x, event.time)
            return False
        if event.type == X.PropertyNotify:
            if event.window.id == self.root.id:
                return event.atom in (self.atom[n] for n in ROOT_WATCHED)
            if event.atom == self.atom["_NET_WM_ICON"]:
                self.icons.pop(event.window.id, None)
            return event.atom in (self.atom[n] for n in WATCHED)
        return event.type in (X.ConfigureNotify, X.DestroyNotify, X.UnmapNotify, X.MapNotify)

    def run(self):
        fd = self.d.fileno()
        self.scan()
        while True:
            again = False
            while self.d.pending_events():
                again = self.handle(self.d.next_event()) or again
            if again:
                self.scan()
                continue
            ready, _, _ = select.select([fd], [], [], SCAN_SECONDS)
            if not ready:
                self.scan()


def main(argv=None):
    try:
        from Xlib import display as xdisplay
        font = load_font()
    except ImportError as exc:
        sys.stderr.write(f"windowlist: cannot run here: {exc}\n")
        return 3
    try:
        display = xdisplay.Display()
    except Exception as exc:  # noqa: BLE001
        sys.stderr.write(f"windowlist: no display: {exc}\n")
        return 3
    WindowList(display, font).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
