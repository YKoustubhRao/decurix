"""
The shortcuts on a native Windows worker's screen, as its agent knows them.

A worker's desktop shows the apps a person picked for it: icons on its
wallpaper and rows in its Start menu, drawn by the control plane's helper (a
private desktop has no Explorer). The control plane writes the same list here
(desktop.nativeHost.noteApps), and it is what makes two things possible:

- A shortcut is opened by its name. A person's double click on an icon is
  handed to the agent as "open Character Map", so while they are recording it
  is a step of the demonstration - and a replay opens it again by the same
  name, by the same road. Found live: Character Map opened from the Start
  menu left no trace in the recording, and its replay began in a window
  nothing had opened.
- A window that is not there can be opened. Each recorded step says which
  program its window belonged to; the shortcut that starts that program is
  what puts the window back.

A program shortcut is started by the control plane's helper, never by the
agent itself: the helper resolves it as Windows would and gives a browser or
an Electron app a profile of its own, so it cannot hand the launch to the
copy open on the person's own screen. The agent only names the shortcut's id.
"""

import json
import ntpath
import os
import urllib.error
import urllib.request

from decurix_agent import config
from decurix_agent.computer import browser
from decurix_agent.computer.errors import ComputerError

APPS_FILE = "worker-apps.json"

# Only a native Windows worker has shortcuts of this kind.
NATIVE = os.name == "nt"

# How long the control plane may take to say a program was started: the
# helper resolves the shortcut first, and a browser's first start for a worker
# brings a profile over.
OPEN_SECONDS = 120


def _norm(text):
    return " ".join((text or "").lower().split())


def entries():
    """This worker's shortcuts: [{"id", "name", "kind": "browser"|"web"|"program", "target", "exe"}]. None elsewhere."""

    if not NATIVE:
        return []

    try:
        listed = json.loads((browser._decurix_dir() / APPS_FILE).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []

    return [e for e in listed if isinstance(e, dict) and e.get("id") and e.get("name") and e.get("kind")]


def named(name):
    """The shortcut a person calls `name`, or None - never a guess between two of the same name."""

    wanted = _norm(name)
    if not wanted:
        return None

    found = [e for e in entries() if _norm(e["name"]) == wanted]

    return found[0] if len(found) == 1 else None


def for_app(app):
    """
    The program shortcut that starts the program `app` names ({"name": the
    process, "exe": its path}), or None - never a guess between two. By the
    program's own path when the shortcut says it, else by its file name.
    """

    if not app:
        return None

    exe = ntpath.normcase(app.get("exe") or "")
    stem = _norm(app.get("name"))
    programs = [e for e in entries() if e["kind"] == "program"]

    found = [e for e in programs if exe and e.get("exe") and ntpath.normcase(e["exe"]) == exe]
    if not found and stem:
        found = [e for e in programs if e.get("exe") and _norm(_stem(e["exe"])) == stem]

    return found[0] if len(found) == 1 else None


def _stem(path):
    """A Windows program's name without its folder or ".exe", read the same on any system."""

    return ntpath.splitext(ntpath.basename(path or ""))[0]


def open_program(entry):
    """Have the control plane start a program shortcut on this worker's screen. Raises ComputerError saying why not."""

    body = json.dumps({"desktop_id": config.DESKTOP_ID, "app_id": entry["id"]}).encode("utf-8")
    request = urllib.request.Request(
        config.CONTROL_PLANE_URL.rstrip("/") + "/api/agent/open-app",
        data=body, headers={"Content-Type": "application/json"}, method="POST",
    )

    try:
        with urllib.request.urlopen(request, timeout=OPEN_SECONDS) as response:
            response.read()
    except urllib.error.HTTPError as exc:
        try:
            reason = json.loads(exc.read().decode("utf-8", "replace")).get("error") or ""
        except ValueError:
            reason = ""
        raise ComputerError(reason or f"{entry['name']} could not be opened (the control plane answered {exc.code})")
    except (OSError, ValueError) as exc:
        raise ComputerError(f"{entry['name']} could not be opened: the control plane could not be reached ({exc})")
