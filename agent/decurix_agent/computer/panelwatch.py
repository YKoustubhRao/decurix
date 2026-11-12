"""
Keeps the worker's panel on the screen's edges when the screen changes size, in broker mode (DECURIX_SCREEN_USER=1).

Why it exists: in broker mode the worker's panel (xfce4-panel) is an untrusted X client, and an untrusted client of
Xtigervnc sees no RandR at all, so it is never told the screen changed size. A viewer's noVNC fits the screen to its own
window (1280x720 at start, then, say, 1536x793), and the panel stayed where the first size put it: its bars floated 73
pixels above the new bottom edge and stopped short of the right one, and the screen user's window list, finding no
bar on the bottom edge, drew an empty bar the whole width under them (found live 2026-11-12). A new X connection is
told the screen's current size in its setup reply, untrusted or not, so this opens one every few seconds, reads the
size, closes it, and restarts the panel (`xfce4-panel --restart`, which keeps its layout and lays it out for the
screen it finds) once a new size has held for two looks in a row (a window being dragged larger resizes many times).

Started by agent/decurix-worker-session.sh as the worker, after the panel. It starts one program, the panel, by
absolute path, and only on a change of size; it reads nothing else.
"""

import subprocess
import sys
import time

POLL_SECONDS = 2.0
PANEL = "/usr/bin/xfce4-panel"


def screen_size():
    """(width, height) of the display's first screen, from a connection opened for this look only; None when the
    display cannot be opened."""

    from Xlib import display as xdisplay

    try:
        d = xdisplay.Display()
    except Exception:  # noqa: BLE001 - no display yet, or gone
        return None
    try:
        s = d.screen()
        return int(s.width_in_pixels), int(s.height_in_pixels)
    finally:
        d.close()


class Watch:
    """Decides, look by look, when the panel must be laid out again: a size that differs from the one the panel was
    laid out for, seen twice running. The first size seen is the one the panel started with."""

    def __init__(self):
        self.laid_out = None
        self.candidate = None

    def look(self, size):
        """True when the panel should be restarted now for size (None: no answer this time, nothing changes)."""
        if size is None:
            return False
        if self.laid_out is None:
            self.laid_out = size
            return False
        if size == self.laid_out:
            self.candidate = None
            return False
        if size != self.candidate:
            self.candidate = size
            return False
        self.laid_out, self.candidate = size, None
        return True


def restart_panel():
    """Restart the panel; never raises (the panel is decoration for the worker, a person's convenience)."""
    try:
        subprocess.run([PANEL, "--restart"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=30, check=False)
    except Exception as exc:  # noqa: BLE001
        print(f"panelwatch: restarting the panel: {exc}", file=sys.stderr)


def main():
    try:
        import Xlib.display  # noqa: F401
    except ImportError:
        print("panelwatch: no python-xlib; the panel is not laid out again after a resize", file=sys.stderr)
        return 3
    watch = Watch()
    while True:
        size = screen_size()
        if watch.look(size):
            print(f"panelwatch: the screen is now {size[0]}x{size[1]}; laying the panel out again", file=sys.stderr)
            restart_panel()
        time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    sys.exit(main())
