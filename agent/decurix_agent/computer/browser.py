"""
Which windows are the desktop's web browser, whatever build it is.

On a Linux desktop the browser is Google Chrome behind /usr/bin/chromium-browser
(agent/install-browser.sh); desktops made before that ran Chromium 112. A
native Windows worker uses Microsoft Edge, the Chromium every Windows has.
Their windows differ only in the title's last segment - "Invoices - Odoo -
Google Chrome" against "Invoices - Odoo - Chromium" - and every recording,
skill and result check made so far says "- Chromium". So screen.windows()
reports a browser window's title with that segment as CANONICAL
(canonical_title), and the DevTools layer names its page windows the same way:
a recording made on any build replays on any, and nothing above this module
compares product names. The raw title is kept beside it for anything shown to
a person.
"""

import os
import re
from pathlib import Path

from decurix_agent.computer.errors import ComputerError

# The name every title, recording and check uses for the browser.
CANONICAL = "Chromium"

# Builds the desktop may run, longest first (so "Google Chrome for Testing"
# is not read as "Google Chrome" followed by junk). Edge writes its name with
# a zero-width space in it ("Microsoft​ Edge" - read off a real window).
PRODUCTS = ("Google Chrome for Testing", "Google Chrome", "Chromium", "Microsoft​ Edge", "Microsoft Edge")

# What a person or an engine may call the browser when asking to open it.
APP_NAMES = frozenset({
    "chromium", "chromium-browser", "chrome", "google chrome", "google-chrome",
    "google-chrome-stable", "browser", "web browser", "edge", "microsoft edge", "msedge",
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


# Where Edge is installed, and Chrome for a computer that somehow has no Edge.
_WINDOWS_BROWSERS = (
    r"%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe",
    r"%ProgramFiles%\Microsoft\Edge\Application\msedge.exe",
    r"%LOCALAPPDATA%\Microsoft\Edge\Application\msedge.exe",
    r"%ProgramFiles%\Google\Chrome\Application\chrome.exe",
    r"%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe",
    r"%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe",
)


def windows_browser():
    """The browser a native Windows worker runs, or a ComputerError when the computer has none."""

    for candidate in _WINDOWS_BROWSERS:
        path = os.path.expandvars(candidate)
        if os.path.isfile(path):
            return path

    raise ComputerError("no browser was found on this computer (Microsoft Edge or Google Chrome)")


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

    profile = Path(os.environ.get("DECURIX_DIR") or (Path.home() / ".decurix")) / "browser"

    return [
        windows_browser(),
        f"--remote-debugging-port={os.environ.get('DECURIX_CDP_PORT', '9222')}",
        f"--user-data-dir={profile}",
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-session-crashed-bubble",
        "--hide-crash-restore-bubble",
        url,
    ]
