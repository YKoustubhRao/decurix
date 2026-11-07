import hashlib
import os
import threading
import time

from decurix_agent.computer import browser, session, visual
from decurix_agent.computer.errors import ComputerError


# ============================================================
# Pixels and windows, straight off the X server
#
# python3-xlib, not a screenshot binary: one GetImage request returns the
# whole framebuffer in ~60 ms, and the PNG is encoded here with zlib from the
# standard library. The BGRX -> RGB conversion and every resize are done with
# extended slice assignment, which runs in C - a per-pixel Python loop over a
# 1280x720 screen would take seconds.
# ============================================================


class Frame:
    """One screenshot: width, height and packed 8-bit RGB rows."""

    def __init__(self, width, height, rgb):
        self.width = width
        self.height = height
        self.rgb = rgb
        self._png_cache = {}
        self._sample = None

    def png(self, scale=1):
        """
        Encode as PNG, optionally shrunk by an integer factor.

        Shrinking takes every `scale`-th pixel of every `scale`-th row. That is
        nearest-neighbour, which is exactly right for what the smaller copies
        are for: a thumbnail in the chat's audit trail.
        """

        if not hasattr(self, "_png_cache"):
            self._png_cache = {}
        if scale in self._png_cache:
            return self._png_cache[scale]

        width, height, rgb = self.width, self.height, self.rgb
        stride = width * 3

        rows = []
        for y in range(0, height, scale):
            row = rgb[y * stride:(y + 1) * stride]
            if scale > 1:
                shrunk = bytearray((width + scale - 1) // scale * 3)
                shrunk[0::3] = row[0::3 * scale]
                shrunk[1::3] = row[1::3 * scale]
                shrunk[2::3] = row[2::3 * scale]
                row = shrunk
            rows.append(b"\x00" + bytes(row))

        out_w = (width + scale - 1) // scale
        out_h = len(rows)
        res = visual.png_of_rows(out_w, out_h, b"".join(rows), 1)
        self._png_cache[scale] = res
        return res

    def sample(self, columns=64, rows=36):
        """A coarse grid of pixels, for cheap before/after comparisons."""

        if columns == 64 and rows == 36 and getattr(self, "_sample", None) is not None:
            return self._sample

        step_x = max(1, self.width // columns)
        step_y = max(1, self.height // rows)
        stride = self.width * 3

        grid = bytearray()
        for y in range(0, self.height, step_y):
            row = self.rgb[y * stride:(y + 1) * stride]
            grid += row[0::3 * step_x] + row[1::3 * step_x] + row[2::3 * step_x]
        sampled = bytes(grid)
        if columns == 64 and rows == 36:
            self._sample = sampled
        return sampled

    def digest(self):
        return hashlib.sha1(self.sample()).hexdigest()

    def masked(self, boxes):
        """
        A copy with every [x, y, w, h] box painted over - how a secret field
        leaves a saved frame (privacy.py). The live frame an engine reads is
        never masked.
        """

        if not boxes:
            return self

        rgb = bytearray(self.rgb)
        stride = self.width * 3

        for box in boxes:
            x, y, w, h = (int(v) for v in box)
            left, top = max(0, x), max(0, y)
            right, bottom = min(self.width, x + w), min(self.height, y + h)
            if right <= left or bottom <= top:
                continue
            fill = bytes((right - left) * 3)
            for row in range(top, bottom):
                start = row * stride + left * 3
                rgb[start:start + len(fill)] = fill

        return Frame(self.width, self.height, rgb)


def changed_fraction(before, after, tolerance=24):
    """
    Roughly what share of the screen changed between two frames, 0.0 to 1.0.

    Compared on the coarse sample grid: a blinking caret or a clock ticking in
    the panel must not read as "the screen changed", and a dialog opening
    over a third of it must.
    """

    if before is None or after is None:
        return 1.0

    a = before.sample()
    b = after.sample()

    if len(a) != len(b) or not a:
        return 1.0

    return sum(1 for x, y in zip(a, b) if abs(x - y) > tolerance) / len(a)


def changed_in(before, after, box, tolerance=24, step=2):
    """
    What share of the pixels inside one [x, y, w, h] box differ between two
    frames, 0.0 to 1.0 (1.0 when they cannot be compared). Every `step`-th
    pixel of every `step`-th row: a control's own pixels, not the screen's.
    """

    if before is None or after is None or (before.width, before.height) != (after.width, after.height):
        return 1.0
    x, y, w, h = (int(v) for v in box)
    left, top = max(0, x), max(0, y)
    right, bottom = min(before.width, x + w), min(before.height, y + h)
    if right <= left or bottom <= top:
        return 1.0
    stride = before.width * 3
    seen = changed = 0
    for row in range(top, bottom, step):
        a = before.rgb[row * stride + left * 3:row * stride + right * 3]
        b = after.rgb[row * stride + left * 3:row * stride + right * 3]
        for i in range(0, len(a), 3 * step):
            seen += 1
            if abs(a[i] - b[i]) > tolerance or abs(a[i + 1] - b[i + 1]) > tolerance or abs(a[i + 2] - b[i + 2]) > tolerance:
                changed += 1
    return changed / seen if seen else 1.0


def _grid(width, height, columns=64, rows=36):
    """(step_x, step_y, columns, rows) of Frame.sample's grid on a width x height screen."""

    step_x = max(1, width // columns)
    step_y = max(1, height // rows)
    return step_x, step_y, -(-width // step_x), -(-height // step_y)


def grid_size(width, height):
    """How many points Frame.sample's grid has on a width x height screen."""

    _, _, n, m = _grid(width, height)
    return n * m


def grid_points(width, height, rects):
    """
    The points of Frame.sample's grid (row * columns + column) that lie inside any of `rects` (x, y, w, h). A pixel
    drawn nowhere near a grid point cannot change changed_fraction, so these are every point a drawing can change.
    """

    step_x, step_y, n, m = _grid(width, height)
    points = set()
    for x, y, w, h in rects:
        c0, c1 = max(0, -(-x // step_x)), min(n, -(-(x + w) // step_x))
        r0, r1 = max(0, -(-y // step_y)), min(m, -(-(y + h) // step_y))
        for r in range(r0, r1):
            points.update(range(r * n + c0, r * n + c1))
    return points


def differing_points(before, after, tolerance=24):
    """The points of the sample grid at which two frames of one size differ (a channel by more than `tolerance`),
    as grid_points numbers them."""

    a, b = before.sample(), after.sample()
    _, _, n, _ = _grid(before.width, before.height)
    points = set()
    for k, (x, y) in enumerate(zip(a, b)):
        if abs(x - y) > tolerance:
            row, within = divmod(k, 3 * n)
            points.add(row * n + within % n)
    return points


# ============================================================
# What was drawn, without a picture: the X DAMAGE extension
#
# Every change to the screen's pixels is drawing, and with DAMAGE on the root
# window the X server says where each drawing went (one event per rectangle,
# drawing into any window included). Reading those events is a local read of
# what the server already sent: the settle loop (core._wait_for_settle) learns
# that nothing was drawn without taking and comparing 1280x720 screenshots.
# One connection of its own per process, so its events never mix with the
# screen connection's; one watch at a time (a second caller compares pictures).
# ============================================================

# A connection that could not be made (no X server, no DAMAGE, no python-xlib) is tried again after this long.
DAMAGE_RETRY_SECONDS = 60.0

_damage = {"display": None, "pid": None, "code": None, "failed_at": None}
_damage_lock = threading.Lock()


class DamageLost(ComputerError):
    """The drawing reports stopped (the connection broke, the server refused a request): compare pictures instead."""


def _forget_damage(failed):
    d = _damage["display"]
    if d is not None and _damage["pid"] == os.getpid():
        try:
            d.close()
        except Exception:  # noqa: BLE001 - a broken connection is dropped either way
            pass
    _damage.update(display=None, pid=None, code=None, failed_at=time.monotonic() if failed else None)


def _damage_connection():
    """(display, DamageNotify's event code) of this process's damage connection, or (None, None) when there is none."""

    if _damage["display"] is not None and _damage["pid"] == os.getpid():
        return _damage["display"], _damage["code"]
    if _damage["pid"] != os.getpid():
        # A forked child (the gui daemon's) never uses its parent's connection: the socket is shared.
        _damage.update(display=None, pid=None, code=None)
    failed_at = _damage["failed_at"]
    if failed_at is not None and time.monotonic() - failed_at < DAMAGE_RETRY_SECONDS:
        return None, None

    try:
        session.ensure()
        from Xlib import display as xdisplay
        d = xdisplay.Display()
    except Exception:  # noqa: BLE001 - ImportError (no python-xlib), no X server: pictures are compared
        _damage["failed_at"] = time.monotonic()
        return None, None
    try:
        if not d.has_extension("DAMAGE"):
            raise ComputerError("the X server has no DAMAGE extension")
        d.damage_query_version()  # the server takes no other DAMAGE request before it
        code = d.query_extension("DAMAGE").first_event
    except Exception:  # noqa: BLE001
        try:
            d.close()
        except Exception:  # noqa: BLE001
            pass
        _damage["failed_at"] = time.monotonic()
        return None, None
    _damage.update(display=d, pid=os.getpid(), code=code, failed_at=None)
    return d, code


class DamageWatch:
    """
    What was drawn on the screen since the watch began: take() returns the rectangles reported since its last call.
    Always closed (close()): it holds the process's one damage connection.
    """

    def __init__(self, display, code, damage_id, size, errors):
        self._display = display
        self._code = code
        self._id = damage_id
        self._errors = errors
        self.size = size
        self._closed = False

    def take(self):
        """
        ([(x, y, w, h), ...] drawn since the last call, the screen's size as the latest report gives it or None).
        Raises DamageLost when the reports cannot be read.
        """

        rects, size = [], None
        try:
            for _ in range(self._display.pending_events()):
                event = self._display.next_event()
                if (getattr(event, "type", 0) & 0x7F) != self._code or getattr(event, "damage", None) != self._id:
                    continue
                area = event.area
                rects.append((int(area.x), int(area.y), int(area.width), int(area.height)))
                geometry = event.drawable_geometry
                size = (int(geometry.width), int(geometry.height))
        except Exception as exc:  # noqa: BLE001
            raise DamageLost(f"the screen's drawing could not be read: {exc}") from exc
        if self._errors:
            raise DamageLost(f"the screen refused a drawing report: {self._errors[0]}")
        return rects, size

    def close(self):
        """End the watch: the server stops reporting, and what it had sent is read and dropped."""

        if self._closed:
            return
        self._closed = True
        try:
            self._display.damage_destroy(self._id)
            self._display.sync()
            while self._display.pending_events():
                self._display.next_event()
            if self._errors:
                raise DamageLost(str(self._errors[0]))
        except Exception:  # noqa: BLE001 - the next watch opens a new connection
            _forget_damage(failed=False)
        finally:
            _damage_lock.release()


def damage_watch():
    """
    A DamageWatch on the whole screen, or None when drawing cannot be reported here (no python-xlib DAMAGE module, no
    extension on the server, any error, another watch running): the caller then compares screenshots, as before.
    """

    if not _damage_lock.acquire(blocking=False):
        return None
    try:
        d, code = _damage_connection()
        if d is None:
            _damage_lock.release()
            return None
        from Xlib.ext import damage
        errors = []
        d.set_error_handler(lambda err, *rest: errors.append(err))
        root = d.screen().root
        geometry = root.get_geometry()
        damage_id = root.damage_create(damage.DamageReportRawRectangles)
        d.sync()
        # A new damage reports its whole window at once (the server's own start): that is no drawing.
        while d.pending_events():
            d.next_event()
        if errors:
            raise DamageLost(str(errors[0]))
        return DamageWatch(d, code, damage_id, (int(geometry.width), int(geometry.height)), errors)
    except Exception:  # noqa: BLE001
        _forget_damage(failed=True)
        _damage_lock.release()
        return None


_display = None


def _connect():
    global _display

    if _display is not None:
        return _display

    session.ensure()

    try:
        from Xlib import display as xdisplay
    except ImportError:
        raise ComputerError(
            "screen access is not installed on this desktop (python3-xlib); "
            "rebuild the desktop images"
        )

    try:
        _display = xdisplay.Display()
    except Exception as exc:
        raise ComputerError(f"could not open the desktop's screen: {exc}")

    return _display


def grab(display=None):
    """
    Capture the whole screen as a Frame. `display` is an Xlib connection of
    the caller's own, for a thread that must not share this module's.
    """

    from Xlib import X

    d = display or _connect()
    root = d.screen().root

    # The size is read every time, never kept: a person's noVNC client resizes
    # the screen to fit their browser (resize=remote), and a kept size went on
    # capturing - and reporting - the old one for the life of the process.
    try:
        g = root.get_geometry()
        width, height = g.width, g.height
        image = root.get_image(0, 0, width, height, X.ZPixmap, 0xFFFFFFFF)
    except Exception as exc:
        raise ComputerError(f"could not read the screen: {exc}")

    data = image.data
    if isinstance(data, str):
        data = data.encode("latin-1")

    pixels = width * height
    rgb = bytearray(pixels * 3)

    # 24-bit TrueColor ZPixmap on a little-endian server: B, G, R, pad.
    rgb[0::3] = data[2::4][:pixels]
    rgb[1::3] = data[1::4][:pixels]
    rgb[2::3] = data[0::4][:pixels]

    return Frame(width, height, rgb)


def size():
    # Not d.screen().width_in_pixels: that is the size when this process
    # connected, and the screen is resized under it (see grab).
    g = _connect().screen().root.get_geometry()
    return g.width, g.height


def set_size(width, height):
    """
    Make the screen `width` x `height`, the way a noVNC client's remote resize
    does (Xvnc accepts any size): add a mode of exactly that size and switch
    the output to it. Returns whether the screen is that size afterwards -
    False on a display that will not resize, never an error.
    """

    import subprocess

    width, height = int(width), int(height)
    if not (32 <= width <= 8192 and 32 <= height <= 8192):
        return False

    try:
        if tuple(size()) == (width, height):
            return True
    except Exception:
        pass

    session.ensure()

    def xrandr(*args):
        return subprocess.run(["xrandr", *args], capture_output=True, text=True, timeout=10)

    try:
        listing = xrandr().stdout
    except (OSError, subprocess.SubprocessError):
        return False

    output = next((line.split()[0] for line in listing.splitlines() if " connected" in line), None)
    if output is None:
        return False

    # The plain "WxH" name can be taken by a mode Xvnc made itself for an
    # earlier remote resize and then dropped from the output: xrandr no longer
    # lists it, so --addmode cannot find it, and --newmode is refused with
    # BadName because the name exists. Found live: a 1536x864 demonstration met
    # a 1536x729 screen and every resize back silently failed. A name of our
    # own is tried next, and a fresh one after that.
    base = f"{width}x{height}"
    for mode in (base, f"decurix-{base}", f"decurix-{base}-{os.getpid()}-{int(time.time())}"):
        try:
            # A mode that already exists is fine; any real failure shows in the size.
            xrandr("--newmode", mode, "60", str(width), str(width + 1), str(width + 2), str(width + 3),
                   str(height), str(height + 1), str(height + 2), str(height + 3))
            xrandr("--addmode", output, mode)
            xrandr("--output", output, "--mode", mode)
        except (OSError, subprocess.SubprocessError):
            return False

        try:
            if tuple(size()) == (width, height):
                return True
        except Exception:
            return False

    return False


def close_window(window_id):
    """
    Ask a window to close, the way its title bar's close button does: the
    window manager sends it WM_DELETE_WINDOW, so the app closes itself (and
    can still ask about unsaved work) rather than being destroyed.
    """

    from Xlib import X, protocol

    d = _connect()
    root = d.screen().root
    event = protocol.event.ClientMessage(
        window=d.create_resource_object("window", int(window_id)),
        client_type=d.intern_atom("_NET_CLOSE_WINDOW"),
        data=(32, [int(time.time()) & 0xFFFFFFFF, 2, 0, 0, 0]),
    )
    root.send_event(event, event_mask=X.SubstructureRedirectMask | X.SubstructureNotifyMask)
    d.flush()


def _prop(d, window, name, kind=None):
    atom = d.intern_atom(name)
    try:
        value = window.get_full_property(atom, kind if kind is not None else 0)
    except Exception:
        return None
    return value.value if value is not None else None


def _window_name(d, window):
    utf8 = d.intern_atom("UTF8_STRING")
    name = _prop(d, window, "_NET_WM_NAME", utf8)

    if name is None:
        try:
            name = window.get_wm_name()
        except Exception:
            name = None

    if isinstance(name, bytes):
        name = name.decode("utf-8", "replace")

    return name or ""


def docks():
    """The boxes (x, y, w, h) of the desktop's own bars - the taskbar - which windows() leaves out."""

    from Xlib import X

    d = _connect()
    root = d.screen().root
    dock = d.intern_atom("_NET_WM_WINDOW_TYPE_DOCK")
    boxes = []
    for wid in _prop(d, root, "_NET_CLIENT_LIST_STACKING", X.AnyPropertyType) or []:
        window = d.create_resource_object("window", wid)
        try:
            if window.get_attributes().map_state != X.IsViewable:
                continue
            if dock not in (_prop(d, window, "_NET_WM_WINDOW_TYPE", X.AnyPropertyType) or []):
                continue
            geometry = window.get_geometry()
            origin = window.translate_coords(root, 0, 0)
        except Exception:
            continue
        boxes.append((-origin.x, -origin.y, geometry.width, geometry.height))
    return boxes


def windows():
    """
    Top-level application windows, as the window manager lists them:
    [{"id", "title", "pid", "box": (x, y, w, h), "active"}], topmost last.
    """

    from Xlib import X

    d = _connect()
    root = d.screen().root

    active_ids = _prop(d, root, "_NET_ACTIVE_WINDOW", X.AnyPropertyType) or []
    active_id = active_ids[0] if len(active_ids) else 0

    stacking = _prop(d, root, "_NET_CLIENT_LIST_STACKING", X.AnyPropertyType) or []
    result = []

    for wid in stacking:
        window = d.create_resource_object("window", wid)

        try:
            attrs = window.get_attributes()
            if attrs.map_state != X.IsViewable:
                continue

            types = _prop(d, window, "_NET_WM_WINDOW_TYPE", X.AnyPropertyType) or []
            skip = {d.intern_atom(n) for n in (
                "_NET_WM_WINDOW_TYPE_DOCK", "_NET_WM_WINDOW_TYPE_DESKTOP",
            )}
            if any(t in skip for t in types):
                continue

            geometry = window.get_geometry()
            origin = window.translate_coords(root, 0, 0)
            pid = _prop(d, window, "_NET_WM_PID", X.AnyPropertyType)
        except Exception:
            continue

        raw_title = _window_name(d, window)

        result.append({
            "id": int(wid),
            # A browser window's product segment is reported as one name
            # (browser.py), so a recording made on Chromium replays on Chrome.
            "title": browser.canonical_title(raw_title),
            "raw_title": raw_title,
            "pid": int(pid[0]) if pid is not None and len(pid) else None,
            "box": (-origin.x, -origin.y, geometry.width, geometry.height),
            "active": int(wid) == int(active_id),
        })

    return result


def popups():
    """
    The boxes (x, y, w, h) of the menus and drop-down lists open on the screen now: mapped override-redirect windows,
    which the window manager does not list (they are drawn over every window). Small ones (a tooltip's shadow) and
    ones as large as the screen are left out.
    """

    try:
        from Xlib import X
        d = _connect()
        root = d.screen().root
        width, height = d.screen().width_in_pixels, d.screen().height_in_pixels
        children = root.query_tree().children
    except Exception:
        return []

    boxes = []
    for window in children:
        try:
            attrs = window.get_attributes()
            if attrs.map_state != X.IsViewable or not attrs.override_redirect:
                continue
            geometry = window.get_geometry()
        except Exception:
            continue
        if geometry.width < 40 or geometry.height < 20 or (geometry.width >= width and geometry.height >= height):
            continue
        boxes.append((geometry.x, geometry.y, geometry.width, geometry.height))
    return boxes
