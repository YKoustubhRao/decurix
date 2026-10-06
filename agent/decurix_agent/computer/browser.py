"""
Which windows are the desktop's web browser, whatever build it is.

On a Linux desktop the browser is Google Chrome behind /usr/bin/chromium-browser
(agent/install-browser.sh); desktops made before that ran Chromium 112.
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
# profile and the DevTools port.
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


def launch_argv(url):
    """
    The command that opens `url` in the desktop's browser. The launcher
    (/usr/bin/chromium-browser) adds the DevTools port, the profile and the
    flags.
    """

    return [COMMAND, url]

