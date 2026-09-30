"""
Which windows are the desktop's web browser, whatever build it is.

The browser is Google Chrome behind /usr/bin/chromium-browser
(agent/install-browser.sh); desktops made before that ran Chromium 112. Their
windows differ only in the title's last segment - "Invoices - Odoo - Google
Chrome" against "Invoices - Odoo - Chromium" - and every recording, skill and
result check made so far says "- Chromium". So screen.windows() reports a
browser window's title with that segment as CANONICAL (canonical_title), and
the DevTools layer names its page windows the same way: a recording made on
either build replays on either, and nothing above this module compares
product names. The raw title is kept beside it for anything shown to a person.
"""

import re

# The name every title, recording and check uses for the browser.
CANONICAL = "Chromium"

# Builds the desktop may run, longest first (so "Google Chrome for Testing"
# is not read as "Google Chrome" followed by junk).
PRODUCTS = ("Google Chrome for Testing", "Google Chrome", "Chromium")

# What a person or an engine may call the browser when asking to open it.
APP_NAMES = frozenset({
    "chromium", "chromium-browser", "chrome", "google chrome", "google-chrome",
    "google-chrome-stable", "browser", "web browser",
})

# The command that starts it: the launcher, with the profile and the DevTools port.
COMMAND = "chromium-browser"

_PRODUCT = "|".join(re.escape(p) for p in PRODUCTS)
_SUFFIX = re.compile(r"\s+[-–—]\s+(?:" + _PRODUCT + r")\s*$")
_WHOLE = re.compile(r"^\s*(?:" + _PRODUCT + r")\s*$")


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
