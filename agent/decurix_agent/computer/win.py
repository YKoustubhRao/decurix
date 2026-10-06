"""
The computer-use layer on a native Windows worker.

A Linux desktop is read with Xlib (screen.py), AT-SPI (atspi.py) and worked
with xdotool (xinput.py). A native worker's screen is a private Win32 desktop,
where the counterparts are one helper program, decurix-ui.exe
(agent/windows/decurix-ui.cs): it prints every window into a picture, reads
their controls through UI Automation, and delivers input as posted window
messages - the only kind that reaches a desktop no person is looking at.

This module is that helper's client, shaped as the three modules it stands in
for: screen.py, atspi.py, xinput.py and session.py each hand over to it when
the agent runs on Windows, so everything above them - observe/act/verify, the
approval gate, the audit trail, replay - is the same code on both systems.
"""

import json
import os
import re
import subprocess
import threading
from pathlib import Path

from decurix_agent.computer import browser
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError

CREATE_NO_WINDOW = 0x08000000
CREATE_NEW_PROCESS_GROUP = 0x00000200

_lock = threading.Lock()
_process = None


def helper_path():
    """
    decurix-ui.exe: where the control plane says it is (DECURIX_UI_HELPER),
    else beside the agent folder, where the Windows app ships it.
    """

    named = os.environ.get("DECURIX_UI_HELPER", "").strip()
    if named:
        return Path(named)

    return Path(__file__).resolve().parents[3] / "decurix-ui.exe"


def _start():
    exe = helper_path()

    if not exe.is_file():
        raise ComputerError(f"screen access is not installed for this worker ({exe} is missing)")

    try:
        return subprocess.Popen(
            [str(exe)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            creationflags=CREATE_NO_WINDOW,
        )
    except OSError as exc:
        raise ComputerError(f"could not start screen access: {exc}")


def call(op, **fields):
    """
    One request to the helper. Returns its reply; for "grab" the reply also
    carries the picture's bytes as reply["data"].
    """

    global _process

    with _lock:
        reply, _process = _exchange(_process, op, fields)

    return _checked(reply)


def _exchange(process, op, fields):
    """One request and its reply on a helper process, started (or replaced once) as needed: (reply, process)."""

    line = (json.dumps(dict(fields, op=op)) + "\n").encode("utf-8")

    for attempt in (1, 2):
        if process is None or process.poll() is not None:
            process = _start()

        try:
            process.stdin.write(line)
            process.stdin.flush()
            head = process.stdout.readline()
            if not head:
                raise OSError("the helper ended")
            reply = json.loads(head.decode("utf-8"))
            if reply.get("ok") and "bytes" in reply:
                reply["data"] = _read_exactly(process.stdout, reply["bytes"])
            return reply, process
        except (OSError, ValueError) as exc:
            # A helper that died is replaced once; one that dies again is reported.
            _end(process)
            process = None
            if attempt == 2:
                raise ComputerError(f"screen access stopped working: {exc}")


def _end(process):
    try:
        process.kill()
        process.wait(timeout=5)
    except (OSError, subprocess.TimeoutExpired):
        pass
    for pipe in (process.stdin, process.stdout):
        try:
            pipe.close()
        except OSError:
            pass


def _checked(reply):
    if not reply.get("ok"):
        raise ComputerError(_plain(reply.get("error") or "the screen could not be reached"))

    return reply


class Camera:
    """
    A helper of its own that only takes pictures of the screen - what a second
    X connection is to the demonstration recorder on Linux. The shared helper
    answers one request at a time, and reading a window's controls can take a
    second or two: the picture of the screen as it was *before* a person's
    click must not wait behind that, or it is a picture of after.
    """

    def __init__(self):
        self._lock = threading.Lock()
        self._process = None

    def grab(self):
        from decurix_agent.computer.screen import Frame

        with self._lock:
            reply, self._process = _exchange(self._process, "grab", {})

        reply = _checked(reply)
        return Frame(reply["width"], reply["height"], reply["data"])

    def ask(self, op, **fields):
        """One request on this helper, for what must not wait behind a reading of the whole screen (hit, front)."""

        with self._lock:
            reply, self._process = _exchange(self._process, op, fields)

        return _checked(reply)

    def close(self):
        with self._lock:
            if self._process is not None:
                _end(self._process)
                self._process = None


def _read_exactly(stream, count):
    data = bytearray()
    while len(data) < count:
        chunk = stream.read(count - len(data))
        if not chunk:
            raise OSError("the picture was cut short")
        data += chunk
    return data


def _plain(error):
    """The helper's error without the exception class name a person has no use for."""

    return re.sub(r"^\w+(Exception|Error): ", "", error)


# ============================================================
# screen.py
# ============================================================

def grab(display=None):
    from decurix_agent.computer.screen import Frame

    reply = call("grab")
    return Frame(reply["width"], reply["height"], reply["data"])


def size():
    width, height = call("ping")["screen"]
    return width, height


def set_size(width, height):
    """
    A private desktop is the size of the computer's own screen and cannot be
    given another: true only when it already is that size.
    """

    try:
        return tuple(size()) == (int(width), int(height))
    except ComputerError:
        return False


def close_window(window_id):
    call("close", hwnd=int(window_id))


def windows():
    """
    Top-level application windows, as screen.windows() lists them on Linux:
    [{"id", "title", "pid", "box": (x, y, w, h), "active"}], topmost last.
    """

    result = []
    for w in call("windows")["windows"]:
        result.append({
            "id": int(w["id"]),
            "title": browser.canonical_title(w["title"]),
            "raw_title": w["title"],
            "pid": w.get("pid"),
            # The program it belongs to ("chrome", "notepad"): what tells a
            # replay whether an app is open at all, whatever its title is now.
            "app": (w.get("app") or "").lower(),
            "exe": w.get("exe") or "",
            "box": tuple(w["box"]),
            "active": bool(w["active"]),
        })
    return result


def browser_viewport():
    """
    (x, y) of the top-left corner of the page the topmost browser window
    shows, in screen pixels, or None when no browser is open.
    """

    try:
        box = call("viewport").get("box")
    except ComputerError:
        return None

    return (box[0], box[1]) if box else None


# ============================================================
# xinput.py
# ============================================================

def click(x, y, button=1, repeat=1):
    call("click", x=int(x), y=int(y), button="right" if button == 3 else "left", count=int(repeat))


def move(x, y):
    call("move", x=int(x), y=int(y))


def type_text(text):
    call("type", text=text)


def key(combo):
    call("key", combo=combo)


def scroll(x, y, direction, amount=3):
    if direction not in ("up", "down", "left", "right"):
        raise ComputerError(f"cannot scroll {direction!r}; use up, down, left or right")
    call("scroll", x=int(x), y=int(y), direction=direction, amount=max(1, int(amount)))


def drag(x1, y1, x2, y2, button=1):
    call("drag", x=int(x1), y=int(y1), x2=int(x2), y2=int(y2))


def activate(window_id):
    try:
        call("activate", hwnd=int(window_id))
    except ComputerError:
        # Like a window that refuses focus on Linux: the action after it
        # reports honestly if the keys went nowhere.
        pass


# ============================================================
# session.py
# ============================================================

def ensure():
    """Nothing to find: the agent's own desktop is the worker's screen."""


def app_env():
    return dict(os.environ)


def launch(argv, cwd=None):
    """
    Start a program detached from the caller. It inherits this process's
    desktop, which is what puts its window on the worker's screen and not on
    the person's.
    """

    return subprocess.Popen(
        argv,
        env=app_env(),
        cwd=cwd or os.environ.get("DECURIX_WORK_DIR") or str(Path.home()),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        creationflags=CREATE_NEW_PROCESS_GROUP,
    )


# ============================================================
# Opening things
# ============================================================

# What a person or an engine may call the programs every Windows has. Only
# ones that open on a private desktop: Calculator, Photos and the other Store
# apps are started by the shell, on the screen a person is looking at.
APPS = {
    "notepad": "notepad.exe",
    "text editor": "notepad.exe",
    "editor": "notepad.exe",
    "paint": "mspaint.exe",
    "word": "winword.exe",
    "excel": "excel.exe",
    "spreadsheet": "excel.exe",
    "powerpoint": "powerpnt.exe",
    "outlook": "outlook.exe",
    "character map": "charmap.exe",
}

# Opened in the browser rather than by the program Windows names for them.
BROWSER_FILES = (".html", ".htm", ".pdf", ".svg")


def find_program(name):
    """
    The full path of a program named the way a person names one ("excel",
    "winword.exe"), or None: on the PATH, or registered under App Paths -
    which is where Office and most installed programs are, and the PATH is not.
    """

    import shutil
    import winreg

    exe = name if name.lower().endswith(".exe") else name + ".exe"

    found = shutil.which(exe)
    if found:
        return found

    for hive in (winreg.HKEY_CURRENT_USER, winreg.HKEY_LOCAL_MACHINE):
        try:
            with winreg.OpenKey(hive, r"Software\Microsoft\Windows\CurrentVersion\App Paths" + "\\" + exe) as key:
                path = os.path.expandvars(winreg.QueryValueEx(key, None)[0]).strip('"')
        except OSError:
            continue
        if os.path.isfile(path):
            return path

    return None


def associated_program(path):
    """The program Windows opens this kind of file with, or None when it is a Store app or nothing."""

    import ctypes
    from ctypes import wintypes

    size = wintypes.DWORD(1024)
    buffer = ctypes.create_unicode_buffer(1024)
    # ASSOCSTR_EXECUTABLE: the program itself, not the command line a shell would run.
    result = ctypes.windll.shlwapi.AssocQueryStringW(0, 2, Path(path).suffix.lower(), None, buffer, ctypes.byref(size))

    if result != 0 or not os.path.isfile(buffer.value):
        return None

    return buffer.value


def open_argv(target):
    """
    The command that opens a file or an app on the worker's desktop, or
    ("url", address) for what the browser shows. Raises ComputerError for
    what cannot be opened there.

    Never through the shell (os.startfile, `start`, explorer): the shell is the
    person's own, and opens things on their screen.
    """

    path = Path(target).expanduser()

    if path.exists():
        if path.is_dir():
            raise ComputerError(
                f"{target} is a folder, and a folder window cannot be shown on this worker's screen; "
                "list it with a command instead (Get-ChildItem)"
            )
        if path.suffix.lower() in BROWSER_FILES:
            return ("url", path.resolve().as_uri())
        program = associated_program(path)
        if program is None:
            raise ComputerError(
                f"nothing on this computer opens {path.suffix or 'that kind of'} files on this worker's screen; "
                "read it with a command, or open it with a program you name"
            )
        return [program, str(path.resolve())]

    words = target.split()
    name = APPS.get(target.strip().lower())
    program = find_program(name or words[0])

    if program is None:
        raise ComputerError(
            f"don't know how to open {target!r}: give a URL, an existing file path, a program's name "
            "(excel, winword, notepad...) or one of " + ", ".join(sorted(APPS))
        )

    return [program] + ([] if name else words[1:])


# ============================================================
# atspi.py: the controls of every window, through UI Automation
# ============================================================

# UI Automation's control types, as the roles every backend uses.
ROLES = {
    "Button": "button", "SplitButton": "button", "CheckBox": "checkbox", "RadioButton": "radio",
    "ComboBox": "combobox", "Edit": "textbox", "Document": "document", "Hyperlink": "link",
    "Image": "image", "List": "list", "ListItem": "listitem", "Menu": "menu", "MenuBar": "menubar",
    "MenuItem": "menuitem", "Tab": "tablist", "TabItem": "tab", "Text": "text", "ToolBar": "toolbar",
    "Tree": "tree", "TreeItem": "treeitem", "DataGrid": "table", "Table": "table", "DataItem": "listitem",
    "Header": "header", "HeaderItem": "header", "Slider": "slider", "Spinner": "spinbutton",
    "StatusBar": "status", "ProgressBar": "progressbar", "Window": "dialog", "Group": "group",
    "Pane": "pane", "Custom": "custom", "Calendar": "calendar", "ToolTip": "tooltip",
}

# Structure a person does not act on, and controls that only repeat another.
SKIPPED = {"ScrollBar", "Thumb", "Separator", "TitleBar", "Pane", "Group", "Custom", "Window", "ToolTip",
           "MenuBar", "ToolBar", "Tab", "Header"}

# Listed for reading even though they cannot be acted on.
READABLE = {"text", "status", "dialog", "progressbar"}

# Still worth listing without a name.
UNNAMED_OK = {"textbox", "document", "combobox", "spinbutton", "slider"}

# How many documents are asked for their text in one look: each is a call of its own.
MAX_DOCUMENT_READS = 6

BROWSER_APPS = ("msedge", "chrome", "chromium", "brave")

# Toolkits whose controls are operated by window messages rather than by a
# provider inside the app (see default_action).
CLASSIC_FRAMEWORKS = ("Win32", "WinForm", "")


def _container(entry):
    role, _, name = entry.partition("\t")
    return f"{ROLES.get(role, role.lower())} {el._quote(name, 40)}"


def observe(skip_apps=()):
    """
    (elements, groups) for every window on the worker's desktop, in the shape
    atspi.observe returns. `skip_apps` names the browser when its page is read
    over DevTools: its UI Automation tree is the whole page again, slowly.
    """

    skip = list(BROWSER_APPS) if skip_apps else []
    listed = call("elements", skip_apps=skip)["windows"]

    found = []
    groups = []
    reads = 0

    # An open menu or drop-down is a window of its own with no title, above
    # the window it belongs to: its entries are listed as part of that
    # window, which is what a person takes them for.
    owner = {}
    for window in listed:
        if not window.get("popup"):
            title = browser.canonical_title(window.get("title") or "")
            owner.setdefault(window.get("pid"), (f"uia:{window.get('app') or 'app'}:{title}", title))

    for window in listed:
        title = browser.canonical_title(window.get("title") or "")
        app = window.get("app") or "app"
        popup = bool(window.get("popup"))

        if popup and window.get("pid") in owner:
            group_id, window_title = owner[window["pid"]]
        else:
            group_id = f"uia:{app}:{title}"
            window_title = title
            groups.append({
                "id": group_id,
                "title": f"{app} window {el._quote(title, 80)}",
                "window_title": title,
                "pid": window.get("pid"),
                "source": "uia",
            })

        elements = window_elements(window, group_id, window_title)

        for element in elements:
            if element["role"] == "document" and not element["value"] and reads < MAX_DOCUMENT_READS \
                    and "text" in element["handle"]["patterns"]:
                reads += 1
                try:
                    element["value"] = call("text", hwnd=window["hwnd"], rid=element["handle"]["rid"],
                                            limit=200)["text"].strip()
                except ComputerError:
                    pass

        found.extend(elements)

    return found, groups


def window_elements(window, group_id, window_title):
    """
    One window's controls, as the helper listed them, in the shape every
    backend uses. Shared by a look at the whole screen (observe) and by the
    question "what is under this point" (hit), so a control a person pressed
    is described exactly as a later look at the screen describes it.
    """

    app = window.get("app") or "app"

    # An editor marks unsaved changes with a leading "*"; it is still the
    # same window, and its controls must keep their refs.
    stable = window_title.lstrip("*").strip()
    root = f"{app} window {el._quote(stable, 60)}"

    found = []
    for node in window.get("nodes") or []:
        element = _element(node, window, root, group_id, window_title, app)
        if element is not None:
            found.append(element)

    el.dedupe_paths(found)

    return found


def _hit_window(window):
    """
    What the helper said about one window (its "at" and "front"), ready to
    use: who it is - its title as recordings write one, its program, whether
    it is the browser - and its controls. A menu or drop-down is a window of
    its own; its entries are listed as part of the window it belongs to, as
    observe lists them.
    """

    if not window:
        return None

    owner = window.get("owner") or {}
    title = browser.canonical_title((owner.get("title") if window.get("popup") and owner else window.get("title")) or "")
    app = window.get("app") or ""

    found = {
        "hwnd": window.get("hwnd"),
        "title": title,
        "app": app,
        "exe": window.get("exe") or "",
        "popup": bool(window.get("popup")),
        "browser": bool(window.get("browser")),
        "on_page": bool(window.get("on_page")),
        "box": window.get("box"),
        "elements": [],
    }

    if not found["browser"]:
        found["elements"] = window_elements(window, f"uia:{app or 'app'}:{title}", title)

    return found


def hit(x, y, ask=None):
    """
    The window under a point of the screen right now - the topmost one there,
    never one behind it - as _hit_window describes it, or None for the bare
    desktop. `ask` is a helper to ask in place of the shared one (Camera.ask).
    """

    return _hit_window((ask or call)("at", x=int(x), y=int(y)).get("window"))


def front(ask=None):
    """The window keys go to right now, as hit() describes one, or None when nothing is open."""

    return _hit_window((ask or call)("front").get("window"))


def _element(node, window, root, group_id, window_title, app):
    raw = node.get("role") or ""
    role = ROLES.get(raw, raw.lower())
    patterns = node.get("patterns") or []
    name = (node.get("name") or "").strip()

    if not name and role in el.INTERACTIVE_ROLES | {"document"}:
        name = (node.get("label") or "").strip().rstrip(":").strip() or (node.get("help") or "").strip()

    box = node.get("box")
    if not box:
        return None

    acts = any(p in patterns for p in ("invoke", "toggle", "select", "expand"))
    interactive = role in el.INTERACTIVE_ROLES or role == "document" or (acts and raw not in SKIPPED)
    readable = role in READABLE and name

    if raw in SKIPPED and not readable:
        return None
    if not (interactive or readable):
        return None
    # A control with no name cannot be asked for; a field is still "the text box".
    if interactive and not name and role not in UNNAMED_OK:
        return None

    if name and browser.is_browser_title(name):
        name = browser.canonical_title(name)

    password = bool(node.get("password"))
    # A rich edit control reports an empty field as a lone line ending.
    value = "" if password else (node.get("value") or "").strip("\r\n")
    if role in ("text", "status"):
        value = ""

    segments = [root] + [_container(c) for c in node.get("containers") or []]

    return {
        "role": role,
        "name": name,
        "value": value[:200],
        "states": list(node.get("states") or []),
        "box": [int(v) for v in box],
        "path": " > ".join(segments + [f"{role} {el._quote(name, 40)}"]),
        "source": "uia",
        "group": group_id,
        "window_title": window_title,
        "app": app,
        "bar": None,
        "handle": {
            "hwnd": window["hwnd"],
            "rid": node.get("rid") or [],
            # privacy.py reads a password field off this, as it does AT-SPI's.
            "raw_role": "password text" if password else raw,
            "patterns": patterns,
            "framework": node.get("framework") or "",
            "single_line": raw == "Edit",
            "editable": "value" in patterns and not node.get("read_only"),
            "pid": window.get("pid"),
        },
    }


def _locator(element):
    handle = element.get("handle") or {}
    if not handle.get("hwnd") or not handle.get("rid"):
        return None
    return {"hwnd": handle["hwnd"], "rid": handle["rid"]}


def default_action(element):
    """
    Do what a click on the control does, through the control's own pattern.
    Returns how, or None when it offers nothing (the caller clicks its centre).
    """

    where = _locator(element)
    if where is None:
        return None

    # A classic control's "press" is a message its window only acts on while it
    # is the active window of a desktop that is on screen. Measured on a private
    # desktop: Toggle on a Win32 check box and Invoke on a Win32 button both
    # reported success and did nothing, while a posted click at the same
    # control worked. So those are clicked where they are drawn.
    if (element.get("handle") or {}).get("framework", "") in CLASSIC_FRAMEWORKS:
        return None

    try:
        return call("invoke", **where).get("did") or None
    except ComputerError:
        return None


def place_caret(element, at_end=True):
    """Not something UI Automation offers; the caller clicks into the document."""

    return False


def grab_focus(element):
    where = _locator(element)
    # A classic control takes focus from a click, like its press (default_action).
    if where is None or (element.get("handle") or {}).get("framework", "") in CLASSIC_FRAMEWORKS:
        return False

    try:
        call("focus", **where)
        return True
    except ComputerError:
        return False


def set_text(element, text):
    """
    Replace a field's contents through its own value pattern. False when the
    control has none or is read-only - the caller then types into it.
    """

    where = _locator(element)
    handle = element.get("handle") or {}
    if where is None or "value" not in handle.get("patterns", []):
        return False

    # A classic edit control given its text this way shows it and tells its
    # app nothing: measured, Character Map's Copy button stayed disabled over a
    # field that read "typed by UIA". Typing into it is what the app notices.
    if handle.get("framework", "") in CLASSIC_FRAMEWORKS:
        return False

    try:
        return bool(call("set_value", text=text, **where).get("set"))
    except ComputerError:
        return False


def select_option(element, option):
    where = _locator(element)
    if where is None:
        raise ComputerError(f"{el.describe(element)} is no longer on the screen")

    try:
        return call("select", option=option, **where)["chose"]
    except ComputerError as exc:
        raise ComputerError(f"{el.describe(element)}: {exc}")
