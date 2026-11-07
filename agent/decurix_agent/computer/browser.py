"""
Which windows are the desktop's web browser, whatever build it is.

On a Linux desktop the browser is Google Chrome behind /usr/bin/chromium-browser
(agent/install-browser.sh); desktops made before that ran Chromium 112.
Their windows differ only in the title's last segment - "Requests - Portal -
Google Chrome" against "Requests - Portal - Chromium" - and every recording,
skill and result check made so far says "- Chromium". So screen.windows()
reports a browser window's title with that segment as CANONICAL
(canonical_title), and the DevTools layer names its page windows the same way:
a recording made on any build replays on any, and nothing above this module
compares product names. The raw title is kept beside it for anything shown to
a person.
"""

import os
import re
import signal
import threading
import time

from decurix_agent.computer import screen_call
from decurix_agent.logging_utils import log

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


# How often a browser warm() started is looked at, to close it once the work is over and it never showed a page.
UNUSED_CHECK_SECONDS = 60


def warm_soon():
    """warm() on a thread of its own: a busy browser takes seconds to answer, and the caller never waits on it."""

    threading.Thread(target=warm, daemon=True, name="browser-warm").start()


@screen_call.routed("browser.warm")
def warm():
    """
    Start the browser with no window when it is not running, so a page the
    request opens finds it warm: its cold start (3 s idle, over 30 s on a
    loaded host) overlaps the model's first call. It is not started at boot
    (start_agent_terminal.sh) - idle it was the largest part of a desktop's
    memory. A browser started here that no window ever came from is closed
    once no task is running. Never raises; nothing waits on it.
    """

    try:
        from decurix_agent.computer import cdp, session
        if cdp.available():
            return
        process = session.launch([COMMAND, "--no-startup-window"])
    except Exception as exc:
        log(f"Could not warm the browser: {exc}")
        return
    threading.Thread(target=_close_if_unused, args=(process,), daemon=True, name="browser-unused").start()


def _close_if_unused(process, check_seconds=None):
    """Close a browser warm() started once no task is running, unless a browser window came of it."""

    from decurix_agent import task_context
    from decurix_agent.computer import screen

    check_seconds = UNUSED_CHECK_SECONDS if check_seconds is None else check_seconds
    while process.poll() is None:
        time.sleep(check_seconds)
        try:
            if any(is_browser_title(w["title"]) for w in screen.windows()):
                return  # in use: it stays, as a browser anyone opened does
            if task_context.is_active():
                continue
            os.killpg(process.pid, signal.SIGTERM)
            log("Closed the browser warmed for a task that opened no page.")
        except Exception as exc:
            log(f"Could not check the warmed browser: {exc}")
        return

