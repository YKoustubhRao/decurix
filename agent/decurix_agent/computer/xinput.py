import re
import subprocess
import time

from decurix_agent.computer import session
from decurix_agent.computer.errors import ComputerError


# ============================================================
# Real input events, through xdotool (XTEST)
#
# This is the fallback path, and the one every app understands: a click at a
# point, keys on whatever has focus. Elements are acted on through their own
# accessibility interface first (atspi.py, cdp.py); this is what those fall
# back to, and what coordinates and key presses always use.
# ============================================================

# The spellings a model actually writes, mapped to X keysym names.
KEY_ALIASES = {
    "enter": "Return",
    "return": "Return",
    "esc": "Escape",
    "escape": "Escape",
    "tab": "Tab",
    "space": "space",
    "backspace": "BackSpace",
    "delete": "Delete",
    "del": "Delete",
    "up": "Up",
    "down": "Down",
    "left": "Left",
    "right": "Right",
    "home": "Home",
    "end": "End",
    "pageup": "Prior",
    "pagedown": "Next",
    "pgup": "Prior",
    "pgdn": "Next",
    "insert": "Insert",
    "ctrl": "ctrl",
    "control": "ctrl",
    "alt": "alt",
    "shift": "shift",
    "super": "super",
    "win": "super",
    "cmd": "ctrl",
    "meta": "super",
}


def _run(args, timeout=30):
    session.ensure()

    try:
        process = subprocess.run(
            ["xdotool"] + args,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except FileNotFoundError:
        raise ComputerError(
            "input is not installed on this desktop (xdotool); rebuild the desktop images"
        )
    except subprocess.TimeoutExpired:
        raise ComputerError(f"xdotool {args[0]} did not finish in {timeout}s")

    if process.returncode != 0:
        raise ComputerError(
            f"xdotool {args[0]} failed: {(process.stderr or process.stdout).strip()}"
        )

    return process.stdout


def normalize_combo(combo):
    """
    "Ctrl+S" -> "ctrl+s", "enter" -> "Return", "ctrl+shift+Tab" kept whole.
    """

    parts = [p for p in re.split(r"\s*\+\s*", combo.strip()) if p]

    if not parts:
        raise ComputerError("no key given")

    out = []
    for part in parts:
        alias = KEY_ALIASES.get(part.lower())
        if alias:
            out.append(alias)
        elif len(part) == 1:
            out.append(part.lower() if len(parts) > 1 else part)
        elif re.fullmatch(r"[fF]\d{1,2}", part):
            out.append(part.upper())
        else:
            out.append(part)

    return "+".join(out)


def click(x, y, button=1, repeat=1):
    _run(["mousemove", "--sync", str(int(x)), str(int(y))])
    _run(["click", "--repeat", str(repeat), "--delay", "80", str(button)])


def move(x, y):
    _run(["mousemove", "--sync", str(int(x)), str(int(y))])


def type_text(text):
    # --delay keeps up with apps that process each key on the main loop
    # (LibreOffice drops characters typed at xdotool's default speed).
    lines = text.split("\n")

    for index, line in enumerate(lines):
        if line:
            _run(["type", "--delay", "12", "--", line], timeout=max(30, len(line) // 10))
        if index < len(lines) - 1:
            _run(["key", "Return"])


def key(combo):
    _run(["key", "--delay", "40", normalize_combo(combo)])


def scroll(x, y, direction, amount=3):
    button = {"up": 4, "down": 5, "left": 6, "right": 7}.get(direction)

    if button is None:
        raise ComputerError(f"cannot scroll {direction!r}; use up, down, left or right")

    move(x, y)
    _run(["click", "--repeat", str(max(1, int(amount))), "--delay", "30", str(button)])


def drag(x1, y1, x2, y2, button=1):
    move(x1, y1)
    _run(["mousedown", str(button)])

    try:
        # The pointer travels rather than jumps: a drop target that follows
        # motion (a list being reordered, a slider) never sees a jump.
        for step in (1, 2, 3, 4):
            move(x1 + (x2 - x1) * step / 4, y1 + (y2 - y1) * step / 4)
            time.sleep(0.05)
    finally:
        _run(["mouseup", str(button)])


def activate(window_id):
    """Raise and focus a top-level window, waiting until it really has focus."""

    try:
        _run(["windowactivate", "--sync", str(int(window_id))], timeout=5)
    except ComputerError:
        # Some windows refuse _NET_ACTIVE_WINDOW (xfdesktop, override-redirect
        # popups); focusing is still worth attempting, and the action after it
        # reports honestly if the keys went nowhere.
        try:
            _run(["windowfocus", str(int(window_id))], timeout=5)
        except ComputerError:
            pass

    time.sleep(0.15)
