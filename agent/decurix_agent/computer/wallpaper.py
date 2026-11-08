"""
The desktop's wallpaper in broker mode (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "the worker's apps as untrusted
clients"), drawn by the screen user on the root window.

In the default mode xfdesktop draws it, as the desktop user. In broker mode that user's X clients are untrusted, and
the SECURITY extension gives an untrusted client no write to the root window and none of its events: xfdesktop exits
at its first root property (BadAccess). Run as the screen user instead, xfdesktop would be a way round the gate - its
menus open a terminal, "Open With" runs any command, all as the screen user - so it is not run at all in broker mode,
and this draws the picture only: no icons, no menu, nothing it can start. The worker's apps are reached from the panel
(the worker's, untrusted) and `gui open`.

Run by agent/decurix-screen-session.sh: `python3 -m decurix_agent.computer.wallpaper PICTURE`. It draws the picture
filling the screen (cropped to fit, xfdesktop's "zoom") and again whenever the screen's size changes (a viewer resized
it), until the display goes.
"""

import sys

# Rows put per request: a few hundred kB, under the core protocol's request size whatever the server.
ROWS_PER_REQUEST = 32


def scaled(path, width, height):
    """The picture as BGRX bytes (the root's 24-bit ZPixmap order on a little-endian server), filling width x height:
    scaled to cover it and cut to it, centred."""

    import gi
    gi.require_version("GdkPixbuf", "2.0")
    from gi.repository import GdkPixbuf

    picture = GdkPixbuf.Pixbuf.new_from_file(path)
    scale = max(width / picture.get_width(), height / picture.get_height())
    w, h = max(width, round(picture.get_width() * scale)), max(height, round(picture.get_height() * scale))
    picture = picture.scale_simple(w, h, GdkPixbuf.InterpType.BILINEAR)
    picture = GdkPixbuf.Pixbuf.new_subpixbuf(picture, (w - width) // 2, (h - height) // 2, width, height).copy()
    picture = picture.add_alpha(False, 0, 0, 0)
    stride, rgba = picture.get_rowstride(), picture.get_pixels()
    rows = bytearray()
    for y in range(height):
        rows += rgba[y * stride:y * stride + width * 4]
    out = bytearray(rows)
    out[0::4], out[2::4] = rows[2::4], rows[0::4]
    return bytes(out)


def draw(display, path):
    """Set the picture as the root window's background, at the screen's size now."""

    from Xlib import X

    screen = display.screen()
    root = screen.root
    geometry = root.get_geometry()
    width, height = geometry.width, geometry.height
    data = scaled(path, width, height)
    pixmap = root.create_pixmap(width, height, screen.root_depth)
    gc = pixmap.create_gc()
    row = width * 4
    for y in range(0, height, ROWS_PER_REQUEST):
        rows = min(ROWS_PER_REQUEST, height - y)
        pixmap.put_image(gc, 0, y, width, rows, X.ZPixmap, screen.root_depth, 0, data[y * row:(y + rows) * row])
    root.change_attributes(background_pixmap=pixmap)
    root.clear_area(0, 0, 0, 0)
    gc.free()
    pixmap.free()
    display.sync()


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 1:
        sys.stderr.write("usage: python3 -m decurix_agent.computer.wallpaper PICTURE\n")
        return 2
    from Xlib import display as xdisplay
    from Xlib.ext import randr

    display = xdisplay.Display()
    try:
        draw(display, argv[0])
    except Exception as exc:  # noqa: BLE001 - the root stays its plain colour; the desktop works without a picture
        sys.stderr.write(f"wallpaper: could not draw {argv[0]}: {exc}\n")
        return 1
    try:
        display.screen().root.xrandr_select_input(randr.RRScreenChangeNotifyMask)
    except Exception:  # noqa: BLE001 - no RandR: the picture stays at the size it was drawn
        return 0
    while True:
        event = display.next_event()
        if event.type == display.extension_event.ScreenChangeNotify:
            try:
                draw(display, argv[0])
            except Exception as exc:  # noqa: BLE001
                sys.stderr.write(f"wallpaper: could not draw {argv[0]} again: {exc}\n")


if __name__ == "__main__":
    sys.exit(main())
