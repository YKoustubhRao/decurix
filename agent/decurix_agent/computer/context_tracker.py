import subprocess
import time

from decurix_agent.computer import browser


# ============================================================
# Context Tracker: IrisGo AI "Watch" Sensing Layer
#
# Real-time sensory tracking across desktop, browser, and OS:
#   - Active window title and application
#   - Active browser URL and tab
#   - Focused control / interactive target
#   - Activity summary
# ============================================================

def get_active_window():
    """Retrieve the currently focused window title and process."""

    try:
        # Try xdotool to get active window id and name
        res = subprocess.run(
            ["xdotool", "getactivewindow", "getwindowname"],
            capture_output=True,
            text=True,
            timeout=2,
        )
        title = res.stdout.strip()
        if title:
            return {"title": title, "app": _infer_app_from_title(title)}
    except Exception:
        pass

    return {"title": "Desktop", "app": "system"}


def _infer_app_from_title(title):
    """Infer the application from the window title."""

    t = (title or "").lower()
    if browser.is_browser_title(title) or "chromium" in t or "chrome" in t:
        return "chromium"
    if "firefox" in t:
        return "firefox"
    if "terminal" in t or "bash" in t:
        return "terminal"
    if "calc" in t or "sheet" in t:
        return "spreadsheet"
    if "writer" in t or "document" in t:
        return "document_editor"
    if "thunderbird" in t or "mail" in t:
        return "email_client"
    if "slack" in t:
        return "slack"
    return "desktop_app"


def capture_context(browser_url=None, focused_element=None):
    """
    Produce a complete sensory context snapshot.
    """

    win = get_active_window()
    return {
        "timestamp": time.time(),
        "active_window": win.get("title", "Desktop"),
        "active_app": win.get("app", "system"),
        "url": browser_url or "",
        "focused_element": focused_element or {},
        "summary": f"{win.get('app', 'app')}: {win.get('title', 'Desktop')}" + (f" ({browser_url})" if browser_url else ""),
    }

