"""
Documents a run opened in apps of the person's computer: closed when they are done with, never the person's own.

Found live (2026-10-20, P1): after a planned run ended, PowerPoint on the hidden desktop still had the deck open (its
~$ owner file in the person's folder), so the next stage and the person met a file held open by an app nobody was
looking at. The decision (round 5):

1. A stage that ends DONE closes, through the app, every document it opened. If the app asks to save, the stage's
   file check has already confirmed the saved file (it is done), so "Don't Save" is chosen.
2. A stage that ends NOT done, or parks a question, leaves its documents open (the person or the re-run may need
   them) and they are listed in the report.
3. When the whole run ends or is stopped, every document the run opened is closed, never saving on its own: one the
   app says has unsaved changes stays open and is listed in the final report.
4. A document the run did not open (the person's own) is never closed.

An app of the person's computer is shown in the desktop's browser by a viewer page (/host-gui/view) whose title is the
app window's own ("MIS_Oct2026.pptx - PowerPoint"). What the run opened is told by comparing the viewers before and
after: a viewer that was not there, or one whose title (its document) changed.
"""

import re
import time
import urllib.request

from decurix_agent.logging_utils import log

VIEWER = "/host-gui/view"
# The app's own question about unsaved changes, by its buttons.
DONT_SAVE = re.compile(r"^\s*(?:don'?t save|do not save|discard(?: changes)?|no)\s*$", re.I)
CANCEL = re.compile(r"^\s*cancel\s*$", re.I)
CLOSE_KEYS = "ctrl+w"
SETTLE_SECONDS = 2.0
# How long "no browser is listening" is believed before the port is tried again.
NO_BROWSER_SECONDS = 20.0
_no_browser_until = [0.0]


# ------------------------------------------------------------ seams (stand-ins in tests)

def _viewers():
    """{target id: title} of every viewer page the desktop's browser shows."""

    import socket

    from decurix_agent.computer import cdp
    if time.time() < _no_browser_until[0]:
        return {}
    try:
        # No browser listening is the common case on a worker that shows no host app: known at once, not after the
        # browser's own wait for a busy answer - and remembered for a little while.
        socket.create_connection(("127.0.0.1", int(cdp.PORT)), timeout=0.3).close()
    except OSError:
        _no_browser_until[0] = time.time() + NO_BROWSER_SECONDS
        return {}
    try:
        return {t["id"]: (t.get("title") or "") for t in cdp._page_targets() if VIEWER in (t.get("url") or "")}
    except Exception as exc:  # noqa: BLE001 - no browser: no viewers
        log(f"host_documents: could not list the viewers: {exc}")
        return {}


def _activate(target_id):
    from decurix_agent.computer import cdp
    try:
        with urllib.request.urlopen(urllib.request.Request(cdp.BASE + f"/json/activate/{target_id}"), timeout=5):
            pass
        return True
    except Exception as exc:  # noqa: BLE001
        log(f"host_documents: could not bring a viewer forward: {exc}")
        return False


def _press_keys(keys):
    from decurix_agent.computer import core
    core.act("key", keys=[keys])


def _buttons():
    """[(ref, name)] of the buttons the front window shows now."""

    from decurix_agent.computer import core
    observation = core.observe()
    return [(e.get("ref"), e.get("name") or "") for e in observation.get("elements", [])
            if e.get("role") in ("button", "push button") and e.get("ref")]


def _press(ref):
    from decurix_agent.computer import core
    core.act("click", ref=ref)


# ------------------------------------------------------------ what was opened

def document(title):
    """The document a viewer shows, from its title ("MIS_Oct2026.pptx - PowerPoint" -> "MIS_Oct2026.pptx")."""

    title = re.sub(r"\s+-\s+Chromium$", "", title or "").strip()
    return title.rsplit(" - ", 1)[0].strip() if " - " in title else title


def snapshot():
    """The viewers open now: {target id: title} - kept at the start of a run and of each stage."""

    return _viewers()


def opened_since(before, now=None):
    """{target id: title} of the viewers whose document was opened since `before`: new ones, or ones showing another
    document than they did then. A viewer showing what it showed then is the person's (or an earlier one's): left."""

    now = _viewers() if now is None else now
    return {tid: title for tid, title in now.items()
            if tid not in (before or {}) or document(title) != document((before or {}).get(tid, ""))}


# ------------------------------------------------------------ closing

def close(target_id, title, discard):
    """
    Close one document through its app: bring its viewer forward and press Ctrl+W. If the app asks about unsaved
    changes, `discard` (the stage's file check confirmed the saved file) chooses Don't Save; otherwise Cancel, and it
    stays open. Returns "closed", "unsaved" (left open) or "failed".
    """

    if not _activate(target_id):
        return "failed"
    try:
        _press_keys(CLOSE_KEYS)
        time.sleep(SETTLE_SECONDS)
        buttons = _buttons()
        dont = next((ref for ref, name in buttons if DONT_SAVE.match(name)), None)
        if dont:
            if discard:
                _press(dont)
            else:
                cancel = next((ref for ref, name in buttons if CANCEL.match(name)), None)
                if cancel:
                    _press(cancel)
                return "unsaved"
        time.sleep(SETTLE_SECONDS)
        still = _viewers().get(target_id)
        return "closed" if still is None or document(still) != document(title) else "failed"
    except Exception as exc:  # noqa: BLE001 - closing is tidying: it never fails the work
        log(f"host_documents: could not close {document(title)}: {exc}")
        return "failed"


def close_opened(before, discard):
    """Close every document opened since `before`. Returns {"closed": [...], "left_open": [...]} by document name."""

    out = {"closed": [], "left_open": []}
    for tid, title in opened_since(before).items():
        result = close(tid, title, discard)
        (out["closed"] if result == "closed" else out["left_open"]).append(
            document(title) + ("" if result != "unsaved" else " (it has changes that were not saved)"))
    return out


def still_open(before):
    """The documents opened since `before` that are open now, by name and app ("MIS_Oct2026.pptx in PowerPoint")."""

    out = []
    for title in opened_since(before).values():
        t = re.sub(r"\s+-\s+Chromium$", "", title or "").strip()
        app = t.rsplit(" - ", 1)[1].strip() if " - " in t else ""
        out.append(document(title) + (f" in {app}" if app else ""))
    return out

