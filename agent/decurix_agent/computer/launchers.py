import configparser
import os
import re
import shlex
import time
from pathlib import Path

from decurix_agent.computer import browser, screen, session, xinput
from decurix_agent.logging_utils import log


# ============================================================
# The desktop's launchers
#
# A person opens an app by double-clicking its icon on the XFCE desktop, and a
# recording keeps that as a double click at a point - with a picture of what
# was around it. That picture is the least reliable thing a replay can go by
# on the desktop: found live, the recorded picture of a "Koustubh - Chrome
# (Host)" icon was mostly the tooltip xfdesktop shows on hover, and at replay
# time the icon was drawn selected. Neither is the icon.
#
# The recording also says what the double click was for: the window that
# opened. The launcher that opens that window is on the desktop as a .desktop
# file, and opening it is what the double click did - so that is what a
# replay does. The same lookup brings a window forward for a step recorded in
# it when something else is in front.
# ============================================================

# A native Windows worker: its launchers are the shortcuts the control plane
# gave it (shortcuts.py), not .desktop files, and it opens them by name.
NATIVE = os.name == "nt"

HOME = Path(os.environ.get("HOME", "/home/headless"))
PLACES = [HOME / "Desktop", HOME / ".local" / "share" / "applications"]

# How long an app may take to show its window: a Chromium --app window cold
# on a 1-vCPU desktop took 11-15 s, a host app's viewer a few more.
LAUNCH_SECONDS = 60

FIELD_CODE = re.compile(r"%[fFuUdDnNickvm]")
BRACKETS = re.compile(r"\s*[\(\[][^)\]]*[\)\]]\s*")


def _norm(text):
    """A name or title as compared: lower case, no "(Host)"-style tags, single spaces."""

    return " ".join(BRACKETS.sub(" ", text or "").lower().split())


def entries(places=None):
    """Every launcher on the desktop and in the menu: [{path, name, exec}], one per name and command."""

    # A native Windows worker's launchers are its shortcuts (shortcuts.py):
    # the icons on its wallpaper, which are not files it can read.
    if NATIVE and places is None:
        from decurix_agent.computer import shortcuts

        return [{"path": e["id"], "name": e["name"], "exec": "", "wm_class": "", "kind": e["kind"], "shortcut": e}
                for e in shortcuts.entries()]

    found, seen = [], set()

    for place in places or PLACES:
        try:
            files = sorted(Path(place).glob("*.desktop"))
        except OSError:
            continue

        for path in files:
            parser = configparser.ConfigParser(interpolation=None, strict=False)
            try:
                parser.read(path, encoding="utf-8")
                entry = parser["Desktop Entry"]
            except (configparser.Error, KeyError, OSError, UnicodeDecodeError):
                continue

            if entry.get("Type", "Application") != "Application" or not entry.get("Exec"):
                continue

            name, command = entry.get("Name", "").strip(), entry.get("Exec", "").strip()
            if not name or (name, command) in seen:
                continue
            seen.add((name, command))
            found.append({"path": str(path), "name": name, "exec": command,
                          "wm_class": entry.get("StartupWMClass", "").strip()})

    return found


def _opens_the_browser(entry):
    """A launcher that starts the browser itself - not a web app's --app window, which is titled by its page."""

    if entry.get("kind"):
        return entry["kind"] == "browser"

    try:
        words = shlex.split(entry["exec"])
    except ValueError:
        return False
    if not words:
        return False
    program = os.path.basename(words[0])
    return (program == browser.COMMAND or program in browser.APP_NAMES) and \
        not any(w.startswith("--app") for w in words[1:])


def for_window(title, places=None):
    """
    The one launcher that opens a window titled `title`, or None - never a
    guess between two. A launcher named "Koustubh - Chrome (Host)" opens
    "Koustubh - Chrome"; "Mousepad" opens "Untitled 1 - Mousepad".
    """

    wanted = _norm(title)
    if not wanted:
        return None

    scored = []
    for entry in entries(places):
        name = _norm(entry["name"])
        if not name:
            continue
        if name == wanted:
            score = 3
        elif wanted.startswith(name + " ") or wanted.endswith(" " + name) or f" {name} " in f" {wanted} ":
            score = 2
        elif browser.is_browser_title(title) and _opens_the_browser(entry):
            # A page's window is titled "<page> - Chromium" whatever the icon
            # that started the browser is called ("Google Chrome" since the
            # desktops run Chrome) - a recorded double click on that icon must
            # still be done by it.
            score = 2
        elif entry["wm_class"] and _norm(entry["wm_class"]) in wanted.split():
            score = 1
        else:
            continue
        scored.append((score, len(name), entry))

    if not scored:
        return None

    scored.sort(key=lambda s: (s[0], s[1]), reverse=True)
    if len(scored) > 1 and scored[0][:2] == scored[1][:2]:
        return None
    return scored[0][2]


def argv(entry):
    """The command a launcher runs, without its %U-style field codes."""

    return [word for word in shlex.split(FIELD_CODE.sub("", entry["exec"])) if word]


def find_window(title, matches):
    """The window on screen whose title `matches` the recorded one, or None."""

    for window in screen.windows():
        if window.get("title") and matches(title, window["title"]):
            return window
    return None


def app_open(app):
    """Whether a window of the program `app` names ({"name", "exe"}) is on the screen. False when it cannot be told."""

    name = ((app or {}).get("name") or "").lower()
    if not name:
        return False

    return any((w.get("app") or "") == name for w in screen.windows())


def _open_native(title, app, url, places):
    """
    bring_forward's opening half on a native Windows worker: the browser on
    the page the step was recorded on, or the shortcut that starts the step's
    program. None when the program is open already (in another state than the
    one recorded - not something to open a second copy over), or when nothing
    here starts it.
    """

    from decurix_agent.computer import core, shortcuts
    from decurix_agent.computer.errors import ComputerError

    is_browser = browser.is_browser_title(title) or ((app or {}).get("name") or "").lower() in BROWSER_PROGRAMS

    try:
        if is_browser:
            if any(browser.is_browser_title(w["title"]) for w in screen.windows()):
                return None
            if url and url.lower().startswith(("http://", "https://")):
                core._open(url, False)
                return f"opened the browser on {url}, where this was recorded"
            core._open("browser", False)
            return "opened the browser"

        if app_open(app):
            return None

        shortcut = shortcuts.for_app(app)
        entry = for_window(title, places) if shortcut is None else None
        name = (shortcut or (entry or {}).get("shortcut") or {}).get("name")

        if name:
            core._open(name, False)
            return f"opened {name!r} from its shortcut"

        # No shortcut for it: the program itself, when Windows knows it by name.
        if (app or {}).get("name"):
            core._open(app["name"], False)
            return f"opened {app['name']}"
    except ComputerError as exc:
        log(f"Could not open what {title!r} was recorded in: {exc}")

    return None


# The programs that are a browser, as a recording names a window's program.
BROWSER_PROGRAMS = ("chrome", "msedge", "brave", "chromium")


def bring_forward(title, matches, places=None, wait=None, app=None, url=None):
    """
    Put the window recorded as `title` in front: raise it when it is open,
    else open it from its launcher and wait for it. Returns a sentence saying
    what was done, or None when neither is possible (no such window, no
    launcher that opens it, or it never appeared). `app` and `url` are what
    the recording says of the step's window - its program and, for a page,
    its address - and are what a native Windows worker opens it by.
    """

    window = find_window(title, matches)
    if window is not None:
        xinput.activate(window["id"])
        time.sleep(0.5)
        return f"brought {title!r} to the front"

    if NATIVE:
        return _open_native(title, app, url, places)

    entry = for_window(title, places)
    if entry is None:
        return None

    try:
        session.launch(argv(entry))
    except (OSError, ValueError) as exc:
        log(f"Could not start {entry['path']}: {exc}")
        return None

    wait = LAUNCH_SECONDS if wait is None else wait
    deadline = time.monotonic() + wait
    while time.monotonic() < deadline:
        window = find_window(title, matches)
        if window is not None:
            xinput.activate(window["id"])
            time.sleep(1.0)
            return f"opened {entry['name']!r} from its launcher"
        time.sleep(0.5)

    log(f"Started {entry['path']}, but no window {title!r} appeared within {wait} s")
    return None


def close(title, matches, wait=15):
    """
    Close the window recorded as `title` and wait for it to go: True when it
    went (or was not open). Only ever on a person's word - `restart` - since
    whatever is open in it goes with it.
    """

    window = find_window(title, matches)
    if window is None:
        return True

    screen.close_window(window["id"])

    deadline = time.monotonic() + wait
    while time.monotonic() < deadline:
        if find_window(title, matches) is None:
            # A host app's viewer that closes ends its session on the host;
            # give it a moment before the launcher opens it again.
            time.sleep(2.0)
            return True
        time.sleep(0.5)
    return False
