"""
Which windows are the desktop's web browser, whatever build it is.

On a Linux desktop the browser is Google Chrome behind /usr/bin/chromium-browser
(agent/install-browser.sh); desktops made before that ran Chromium 112. A
native Windows worker uses the browser picked for it (Chrome, Brave or Edge),
and with none picked whichever the computer has (windows_browser).
Their windows differ only in the title's last segment - "Invoices - Odoo -
Google Chrome" against "Invoices - Odoo - Chromium" - and every recording,
skill and result check made so far says "- Chromium". So screen.windows()
reports a browser window's title with that segment as CANONICAL
(canonical_title), and the DevTools layer names its page windows the same way:
a recording made on any build replays on any, and nothing above this module
compares product names. The raw title is kept beside it for anything shown to
a person.
"""

import json
import os
import re
from pathlib import Path

from decurix_agent.computer.errors import ComputerError

# The name every title, recording and check uses for the browser.
CANONICAL = "Chromium"

# Builds the desktop may run, longest first (so "Google Chrome for Testing"
# is not read as "Google Chrome" followed by junk). Edge writes its name with
# a zero-width space in it ("Microsoft​ Edge" - read off a real window).
PRODUCTS = ("Google Chrome for Testing", "Google Chrome", "Chromium", "Brave", "Microsoft​ Edge", "Microsoft Edge")

# What a person or an engine may call the browser when asking to open it.
APP_NAMES = frozenset({
    "chromium", "chromium-browser", "chrome", "google chrome", "google-chrome",
    "google-chrome-stable", "browser", "web browser", "edge", "microsoft edge", "msedge", "brave",
})

# The command that starts it on a Linux desktop: the launcher, with the
# profile and the DevTools port. launch_argv is what to run on either system.
COMMAND = "chromium-browser"

# Edge puts the profile's name between the page's title and its own:
# "Probe page - Profile 1 - Microsoft Edge" (read off a real window; a profile
# someone signed in to is "Personal" or "Work").
_EDGE_PROFILES = r"(?:Profile \d+|Personal|Work)"
_DASH = r"\s+[-–—]\s+"

_PRODUCT = "|".join(re.escape(p) for p in PRODUCTS if "Edge" not in p)
_EDGE = "|".join(re.escape(p) for p in PRODUCTS if "Edge" in p)

# The profile segment is Edge's alone: "Quarterly - Work - Chromium" is a page
# called "Quarterly - Work".
_SUFFIX = re.compile(
    r"(?:" + _DASH + r"(?:" + _PRODUCT + r")|(?:" + _DASH + _EDGE_PROFILES + r")?" + _DASH + r"(?:" + _EDGE + r"))\s*$"
)
_WHOLE = re.compile(r"^\s*(?:" + _PRODUCT + r"|(?:" + _EDGE_PROFILES + _DASH + r")?(?:" + _EDGE + r"))\s*$")


def is_browser_title(title):
    """Whether a window title is a browser window's ("... - Google Chrome", or just "Chromium")."""

    return bool(title) and bool(_SUFFIX.search(title) or _WHOLE.match(title))


def canonical_title(title):
    """A browser window's title with its product segment as CANONICAL; any other title unchanged."""

    if not title:
        return title

    if _WHOLE.match(title):
        return CANONICAL

    return _SUFFIX.sub(" - " + CANONICAL, title)


def page_window_title(page_title):
    """The title a page's window has, as screen.windows() reports it."""

    return f"{page_title} - {CANONICAL}" if page_title else CANONICAL


# The browsers a native Windows worker can run, by the kind the control plane
# names (desktop.browserKind): what a person calls it, and where it installs.
_WINDOWS_KINDS = {
    "chrome": ("Google Chrome", (
        r"%ProgramFiles%\Google\Chrome\Application\chrome.exe",
        r"%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe",
        r"%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe",
    )),
    "brave": ("Brave", (
        r"%ProgramFiles%\BraveSoftware\Brave-Browser\Application\brave.exe",
        r"%ProgramFiles(x86)%\BraveSoftware\Brave-Browser\Application\brave.exe",
        r"%LOCALAPPDATA%\BraveSoftware\Brave-Browser\Application\brave.exe",
    )),
    "msedge": ("Microsoft Edge", (
        r"%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe",
        r"%ProgramFiles%\Microsoft\Edge\Application\msedge.exe",
        r"%LOCALAPPDATA%\Microsoft\Edge\Application\msedge.exe",
    )),
}

# With no browser picked for the worker: whichever the computer has, Edge
# first because every Windows has it.
_FALLBACK_KINDS = ("msedge", "chrome", "brave")

# Where the control plane says which browser was picked for this worker
# (desktop.nativeHost.noteBrowser): {"kind": "chrome", "exe": "C:\\...\\chrome.exe"},
# and, when the shortcut picked says so, "profile_directory" (which of the
# browser's profiles it opens) and "user_data_dir" (the profile folder this
# worker's browser already has, from when its icon started it).
# Read at every launch, so a browser added to a running worker counts at once.
CHOICE_FILE = "worker-browser.json"


def _decurix_dir():
    return Path(os.environ.get("DECURIX_DIR") or (Path.home() / ".decurix"))


def _choice():
    """What the control plane wrote about this worker's browser, or {} when none was picked."""

    try:
        choice = json.loads((_decurix_dir() / CHOICE_FILE).read_text(encoding="utf-8"))
        kind = choice.get("kind")
    except (OSError, ValueError, AttributeError):
        return {}

    if not isinstance(kind, str) or kind not in _WINDOWS_KINDS:
        return {}

    return {key: str(value) for key, value in choice.items() if value}


def chosen():
    """(kind, exe) of the browser picked for this worker, or None when none was. exe may be ""."""

    choice = _choice()

    return (choice["kind"], choice.get("exe", "")) if choice else None


def _installed(kind):
    for candidate in _WINDOWS_KINDS[kind][1]:
        path = os.path.expandvars(candidate)
        if os.path.isfile(path):
            return path

    return ""


def windows_browser():
    """
    (kind, path) of the browser a native Windows worker runs.

    The browser picked for the worker is the only one it uses: found live, a
    person copied Chrome to a worker, used it, and the agent opened its pages
    in Edge beside it. When that browser cannot be found the worker says so
    and does not start another. Only a worker with none picked takes
    whichever the computer has.
    """

    choice = chosen()
    if choice:
        kind, exe = choice
        path = exe if exe and os.path.isfile(exe) else _installed(kind)
        if not path:
            raise ComputerError(f"this worker's browser is {_WINDOWS_KINDS[kind][0]}, and it was not found on this computer")
        return kind, path

    for kind in _FALLBACK_KINDS:
        path = _installed(kind)
        if path:
            return kind, path

    raise ComputerError("no browser was found on this computer (Google Chrome, Microsoft Edge or Brave)")


def windows_browser_name():
    """What a person calls the worker's browser ("Google Chrome"), or "" when the computer has none."""

    try:
        return _WINDOWS_KINDS[windows_browser()[0]][0]
    except ComputerError:
        choice = chosen()
        return _WINDOWS_KINDS[choice[0]][0] if choice else ""


def launch_argv(url):
    """
    The command that opens `url` in the worker's browser.

    On a Linux desktop the launcher adds everything. A native Windows worker
    starts the browser itself, so what the launcher adds is given here: a
    DevTools port on loopback (how the page is read and worked), a profile of
    the worker's own (a browser asked to use the person's profile hands the
    page to the copy they have open, on their screen), and no first-run or
    restore prompts, which nobody is there to answer.
    """

    if os.name != "nt":
        return [COMMAND, url]

    return windows_argv(url)


def windows_argv(url):
    """launch_argv for a native Windows worker."""

    kind, path = windows_browser()

    # One browser's profile is not another's: each kind has a folder of its
    # own ("browser" is Edge's, as it was when Edge was the only one).
    profile = _decurix_dir() / ("browser" if kind == "msedge" else f"browser-{kind}")

    # The profile this worker's browser already has is the one to go on with:
    # found live, a person signed in to a site in the worker's Chrome as the
    # agent had asked, and the agent then opened the site in a profile of its
    # own, where nobody had signed in.
    choice = _choice()
    kept = choice.get("user_data_dir", "")
    if kept and os.path.isdir(kept):
        profile = kept

    # A worker's profile starts as a copy of the person's own, so what they
    # are signed in to (Google above all) is signed in there too.
    from decurix_agent.computer import profile_seed  # Windows only: msvcrt
    from decurix_agent.logging_utils import log

    note = profile_seed.seed(kind, profile, choice.get("profile_directory", ""))
    if note:
        log(f"browser profile: {note}")

    argv = [
        path,
        f"--remote-debugging-port={os.environ.get('DECURIX_CDP_PORT', '9222')}",
        f"--user-data-dir={profile}",
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-session-crashed-bubble",
        "--hide-crash-restore-bubble",
    ]

    # Which of the browser's profiles the picked shortcut opens.
    if choice.get("profile_directory"):
        argv.append(f"--profile-directory={choice['profile_directory']}")

    return argv + [url]
