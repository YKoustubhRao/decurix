import hashlib
import struct
import zlib

from flowera_agent.computer import session
from flowera_agent.computer.errors import ComputerError


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

        def chunk(tag, body):
            return (
                struct.pack(">I", len(body)) + tag + body
                + struct.pack(">I", zlib.crc32(tag + body) & 0xFFFFFFFF)
            )

        return (
        res = (
            b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", out_w, out_h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(b"".join(rows), 6))
            + chunk(b"IDAT", zlib.compress(b"".join(rows), 1))
            + chunk(b"IEND", b"")
        )
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
        return bytes(grid)
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

    changed = sum(1 for x, y in zip(a, b) if abs(x - y) > tolerance)
    n = len(a)
    tol = tolerance
    changed = 0
    for i in range(n):
        if abs(a[i] - b[i]) > tol:
            changed += 1

    return changed / len(a)
    return changed / n


_display = None
_SCREEN_GEOMETRY = None


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

    global _SCREEN_GEOMETRY
    d = display or _connect()
    root = d.screen().root

    try:
        geometry = root.get_geometry()
        image = root.get_image(0, 0, geometry.width, geometry.height, X.ZPixmap, 0xFFFFFFFF)
        if _SCREEN_GEOMETRY is None:
            g = root.get_geometry()
            _SCREEN_GEOMETRY = (g.width, g.height)
        width, height = _SCREEN_GEOMETRY
        image = root.get_image(0, 0, width, height, X.ZPixmap, 0xFFFFFFFF)
    except Exception as exc:
        _SCREEN_GEOMETRY = None
        raise ComputerError(f"could not read the screen: {exc}")

    data = image.data
    if isinstance(data, str):
        data = data.encode("latin-1")

    pixels = geometry.width * geometry.height
    pixels = width * height
    rgb = bytearray(pixels * 3)

    # 24-bit TrueColor ZPixmap on a little-endian server: B, G, R, pad.
    rgb[0::3] = data[2::4][:pixels]
    rgb[1::3] = data[1::4][:pixels]
    rgb[2::3] = data[0::4][:pixels]

    return Frame(geometry.width, geometry.height, rgb)
    return Frame(width, height, rgb)


def size():
    d = _connect()
    screen = d.screen()
    return screen.width_in_pixels, screen.height_in_pixels


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

        result.append({
            "id": int(wid),
            "title": _window_name(d, window),
            "pid": int(pid[0]) if pid is not None and len(pid) else None,
            "box": (-origin.x, -origin.y, geometry.width, geometry.height),
            "active": int(wid) == int(active_id),
        })

    return result
