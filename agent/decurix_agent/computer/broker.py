"""
Broker mode: the screen (X server, window manager, browser) is a user of its own, `decurixscreen`, and the worker's
user drives it only through the screen broker - the gui daemon (daemon.py) running as that user, which asks the
approval gate per action as `gui` always did (docs/gate-boundary.md, "Next: the input broker").

Off unless the desktop was created with DECURIX_SCREEN_USER=1 (the control plane's DECURIX_SCREEN_BROKER=1). This
module holds what the screen side's code needs to know about it, read per call (never at import, so the daemon's
import-time comparison is unchanged):

- whether this process is the screen side (the switch, and a uid that is not the worker's);
- the DevTools relay's socket (cdp_relay.py), through which cdp.py reaches the browser - it has no port;
- the worker's launch helper's socket (launch_helper.py), which starts every program but the browser as the worker;
- deliver_to_caller: a file a command writes for its caller (a screenshot) goes back over the command's own
  connection and is written by the caller's client, as the caller (gui_client.save_output), never by the screen
  user into the worker's folders.
- the worker's state (docs/gate-boundary.md, "the screen side's own state"): what a command must know of it comes
  with the request and is put where the command's code reads it, in the screen user's own folders (take_context);
  what a command writes of it - its recording, its evidence notes, its record ledger - is written here as always
  and also handed back over the connection, for the caller's client to write in the worker's own folder (mirror,
  gui_client.save_state). The screen side never opens a path of the worker's: a file a command reads from the
  worker comes as a descriptor the caller opened (read_input, local_copy).
"""

import os
import re
import sys
import threading
import time

from decurix_agent.computer import gui_client

# The module a broker's child registers its caller's connection under (daemon.Child). Not under decurix_agent: a
# command whose import-time settings differ forgets and imports every decurix_agent module again, and the connection
# must outlive that.
CALLER_MODULE = "decurix_gui_caller"


def requested():
    """Whether this desktop drives its screen through the broker (DECURIX_SCREEN_USER=1)."""

    return gui_client.broker_requested()


def worker_uid():
    """The worker's uid (DECURIX_SCREEN_WORKER_UID, else the desktop user's, 1001)."""

    return gui_client.worker_uid()


def screen_side():
    """Whether this process is the screen side of a broker-mode desktop: the switch is on and it is not the worker."""

    return requested() and os.getuid() != worker_uid()


def screen_dir():
    """The broker's shared folder (/run/decurix-screen)."""

    return gui_client.screen_dir()


def relay_socket():
    """The DevTools relay's socket when this desktop is in broker mode, else None (the browser has its port then).
    Only the screen user may open it: it is in a folder of that user's alone (cdp_relay.main makes it)."""

    if not requested():
        return None
    return os.environ.get("DECURIX_CDP_RELAY_SOCKET") or os.path.join(screen_dir(), "cdp", "cdp.sock")


def launch_dir():
    """The worker's launch helper's folder: the worker's own, which others may only pass through."""

    return os.environ.get("DECURIX_LAUNCH_DIR") or "/tmp/decurix-launch-%d" % worker_uid()


# The worker's accessibility bus, as its session publishes it in its launch folder (agent/decurix-worker-session.sh):
# the bus the worker's apps (LibreOffice, GTK) show their widgets on. Only a Unix socket address: D-Bus also has
# transports that run a program (unixexec), and this address is the worker's to write.
A11Y_BUS_FILE = "a11y-bus"
A11Y_ADDRESS = re.compile(r"unix:(?:path|abstract)=[A-Za-z0-9_./-]{1,200}(?:,guid=[0-9a-f]{32})?")


def worker_a11y_bus():
    """The worker's accessibility bus address, or None: read from its launch folder (which must be the worker's own,
    nobody else's to write in), from a regular file of the worker's, and only when it is a plain Unix socket's."""

    directory = launch_dir()
    if not gui_client.shared_dir(directory, worker_uid()):
        return None
    try:
        fd = os.open(os.path.join(directory, A11Y_BUS_FILE), os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
                     | getattr(os, "O_NONBLOCK", 0))
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if (st.st_mode & 0o170000) != 0o100000 or st.st_uid != worker_uid() or st.st_size > 1024:
            return None
        address = os.read(fd, 1024).decode("ascii", "replace").strip()
    except OSError:
        return None
    finally:
        os.close(fd)
    return address if A11Y_ADDRESS.fullmatch(address) else None


def caller_connection():
    """The connection of the command this broker's child runs, or None (not a broker's child)."""

    holder = sys.modules.get(CALLER_MODULE)
    return getattr(holder, "conn", None)


def deliver_to_caller(path, data, required=False, atomic=False):
    """
    Write `data` at `path` for the command's caller. In a broker's child the bytes go back over the command's own
    connection and the caller's client writes them, as the caller, at that path in its own working folder - only if
    the caller named that path (gui_client.caller_paths). Anywhere else this process writes them itself, as it
    always did: straight to the path, or (`atomic`) to path.part and renamed over it.

    `required`: the command's result depends on the file (observe --screenshot); the caller's client makes a
    command that exited 0 exit 1 when it could not write it. Raises OSError as a write here would.
    """

    conn = caller_connection()
    if conn is None:
        target = path + ".part" if atomic else path
        with open(target, "wb") as f:
            f.write(data)
        if atomic:
            os.replace(target, path)
        return
    flags = (gui_client.OUTPUT_REQUIRED if required else 0) | (gui_client.OUTPUT_ATOMIC if atomic else 0)
    payload = gui_client.pack([b"%d" % flags, os.fsencode(path), bytes(data)])
    if len(payload) > gui_client.MAX_OUTPUT:
        raise OSError(f"{path}: {len(data)} bytes is more than a command may hand back")
    _send(conn, b"O" + len(payload).to_bytes(4, "big") + payload)


# One frame at a time on a command's connection: a recording's frame and an evidence note may be written from two
# threads of one command.
_send_lock = threading.Lock()


def _send(conn, frame):
    with _send_lock:
        conn.sendall(frame)


# ------------------------------------------------------------
# The worker's state, told to the screen side (take_context)
# ------------------------------------------------------------

_TASK_ID = "t[0-9a-f]{16}"
# The state files the caller may send (gui_client.STATE_FILES, TASK_STATE): their paths under DECURIX_DIR.
_CONTEXT_STATE = re.compile("|".join([re.escape(name) for name in gui_client.STATE_FILES]
                                     + [_TASK_ID.join(re.escape(part) for part in rel.split("%s"))
                                        for rel in gui_client.TASK_STATE]))


def _private_dir(path):
    """Whether `path` is a real folder of this user's that nobody else may enter."""

    try:
        st = os.lstat(path)
    except OSError:
        return False
    return (st.st_mode & 0o170000) == 0o040000 and st.st_uid == os.getuid() and (st.st_mode & 0o077) == 0


def _keep_copy(target, data):
    """`data` at `target`, whole, 0600 - unless it holds that already (its time then stays, and the caches keyed by
    it)."""

    try:
        st = os.lstat(target)
        if (st.st_mode & 0o170000) == 0o100000 and st.st_size == len(data):
            with open(target, "rb") as f:
                if f.read() == data:
                    return
    except OSError:
        pass
    os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
    part = "%s.%d.ctx" % (target, os.getpid())
    fd = os.open(part, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    finally:
        os.close(fd)
    os.replace(part, target)


def context_target(name):
    """Where the screen side keeps a part of the caller's state it was sent (gui_client.caller_context), or None for
    a name it does not take: a task-file part beside this side's own task file, which must be in a folder of this
    user's alone (never /tmp, where the worker's are); a state file under this side's DECURIX_DIR."""

    name = os.fsdecode(name)
    if name[:1] == "t":
        base = os.environ.get("DECURIX_TASK_FILE", "")
        if name[1:] not in gui_client.TASK_SUFFIXES or not base or not _private_dir(os.path.dirname(base)):
            return None
        return base + name[1:]
    if name[:1] == "s":
        state = os.environ.get("DECURIX_DIR", "")
        if not state or not _CONTEXT_STATE.fullmatch(name[1:]):
            return None
        return os.path.join(state, name[1:])
    return None


# Where the screen side keeps the worker's launchers it was sent (launchers.PLACES on the screen side): under its home.
LAUNCHERS_DIR = "caller-launchers"
_LAUNCHER_NAME = re.compile(r"(Desktop|applications)/[^/\0]{1,200}\.desktop")


def launcher_places(home=None):
    """The folders the screen side lists launchers in: its copies of the worker's desktop and menu."""

    base = os.path.join(home or os.environ.get("HOME", ""), LAUNCHERS_DIR)
    return [os.path.join(base, "Desktop"), os.path.join(base, "applications")]


def _take_launchers(context):
    """Keep the worker's launchers as it sent them (the whole set: one it no longer has goes). Never run here: what
    one starts is started by the worker's launch helper (session.launch)."""

    home = os.environ.get("HOME", "")
    if b"l" not in context or not home or not _private_dir(home):
        return
    wanted = {}
    for raw, data in context.items():
        name = os.fsdecode(raw)
        if name[:1] == "l" and data is not None and _LAUNCHER_NAME.fullmatch(name[1:]) \
                and not os.path.basename(name).startswith("."):
            wanted[name[1:]] = data
    base = os.path.join(home, LAUNCHERS_DIR)
    for place in ("Desktop", "applications"):
        folder = os.path.join(base, place)
        try:
            present = os.listdir(folder)
        except OSError:
            present = []
        for file_name in present:
            if place + "/" + file_name not in wanted:
                try:
                    os.unlink(os.path.join(folder, file_name))
                except OSError:
                    pass
    for rel, data in wanted.items():
        try:
            _keep_copy(os.path.join(base, *rel.split("/")), data)
        except OSError:
            continue


def take_context(context):
    """Put what the caller sent of its state where this command's code reads it (a part it does not have is removed
    here too). Only in a broker's child; never raises (a part not kept is a part this command does not see)."""

    if not context or caller_connection() is None:
        return
    _take_launchers(context)
    for name, data in context.items():
        target = context_target(name)
        if target is None:
            continue
        try:
            if data is None:
                os.unlink(target)
            else:
                _keep_copy(target, data)
        except FileNotFoundError:
            pass
        except OSError as exc:
            sys.stderr.write(f"gui: the screen side could not keep the caller's {os.fsdecode(name)[1:]}: {exc}\n")


# ------------------------------------------------------------
# The worker's state, written back (mirror)
# ------------------------------------------------------------

def state_relative(path):
    """`path` relative to this side's DECURIX_DIR, as bytes with "/", when it is in a part the caller keeps too
    (gui_client.mirrored); else None."""

    state = os.environ.get("DECURIX_DIR", "")
    if not state:
        return None
    full = os.path.abspath(os.fspath(path))
    root = os.path.abspath(state).rstrip(os.sep)
    if not full.startswith(root + os.sep):
        return None
    rel = os.fsencode(full[len(root) + 1:]).replace(os.fsencode(os.sep), b"/")
    if not gui_client.mirrored(rel):
        return None
    return rel


def mirror(path, data, append=False):
    """
    A write of the worker's state this side just made (`data` added to `path`, or `path` now holding `data`), handed
    back to the command's caller to make in the worker's own folder (gui_client.save_state). Only in a broker's child,
    and only for a path in a part the caller keeps (state_relative). What is handed back was written here first, so
    it has been through privacy.write_scrubbed (or is a recording's pixels). Never raises: losing the caller's copy
    is said on stderr, and the command's own result stands.
    """

    conn = caller_connection()
    if conn is None:
        return
    try:
        rel = state_relative(path)
        if rel is None:
            return
        flags = gui_client.OUTPUT_STATE | (gui_client.OUTPUT_APPEND if append else 0)
        payload = gui_client.pack([b"%d" % flags, rel, bytes(data)])
        if len(payload) > gui_client.MAX_OUTPUT:
            sys.stderr.write(f"gui: {os.fsdecode(rel)} is too large to hand back to the caller; its copy is not kept\n")
            return
        _send(conn, b"O" + len(payload).to_bytes(4, "big") + payload)
    except (OSError, ValueError) as exc:
        try:
            sys.stderr.write(f"gui: could not hand {path} back to the caller: {exc}\n")
        except Exception:  # noqa: BLE001
            pass


def mirror_tree(directory):
    """Hand back every file of a folder of the worker's state (a demonstration's recording, written by a recorder
    process that has no caller of its own): at its stop, the caller's copy becomes the whole of it."""

    if caller_connection() is None:
        return
    for folder, dirs, files in os.walk(os.fspath(directory)):
        dirs[:] = [d for d in dirs if not d.startswith(".")]
        for name in files:
            if name.startswith(".") or name.endswith((".tmp", ".part", ".ctx")):
                continue
            path = os.path.join(folder, name)
            try:
                with open(path, "rb") as f:
                    data = f.read()
            except OSError:
                continue
            mirror(path, data)


# ------------------------------------------------------------
# Files the caller handed over (read_input, local_copy)
# ------------------------------------------------------------

# Copies of handed files that a program of this side reads by path (the browser: an upload, a local page): a day.
COPIES_DIR = "inputs"
COPIES_KEEP_SECONDS = 24 * 3600


def _handed_fd(path):
    holder = sys.modules.get(CALLER_MODULE)
    inputs = getattr(holder, "inputs", None) or {}
    path = os.fsdecode(path)
    if path in inputs:
        return inputs[path]
    return inputs.get(os.path.abspath(os.path.expanduser(path)))


def handed(path):
    """Whether the command's caller handed over the file `path` names (gui_client.input_files)."""

    return _handed_fd(path) is not None


def read_input(path):
    """The bytes of a file the caller handed over, read from its descriptor; None when it handed no such file."""

    fd = _handed_fd(path)
    if fd is None:
        return None
    chunks, offset = [], 0
    while True:
        chunk = os.pread(fd, 1 << 20, offset)
        if not chunk:
            return b"".join(chunks)
        chunks.append(chunk)
        offset += len(chunk)


class NotHanded(OSError):
    """A command in the broker's child named a file of the caller's that the caller did not hand over."""


def local_copy(path):
    """
    A path this side's programs can read for a file the caller handed over: a copy, under the same name, in a folder
    of the screen user's alone (the browser runs as that user and may not open the worker's files). Outside a broker's
    child, `path` itself. In one, a file the caller did not hand over is never opened by its name (NotHanded): the
    name is the worker's to point anywhere - at the screen user's own files - and the browser would read it as the
    screen user.
    """

    if caller_connection() is None or _own_copy(path):
        return path
    data = read_input(path)
    if data is None:
        raise NotHanded(f"{path} was not handed over by the command that named it (a file is read only as the worker "
                        "opened it: name it to the command itself, a regular file you can read)")
    home = os.environ.get("HOME", "")
    if not home or not _private_dir(home):
        raise OSError(f"no private folder to copy {path} into")
    base = os.path.join(home, COPIES_DIR)
    os.makedirs(base, mode=0o700, exist_ok=True)
    _prune_copies(base)
    import uuid
    folder = os.path.join(base, uuid.uuid4().hex[:12])
    os.mkdir(folder, 0o700)
    target = os.path.join(folder, os.path.basename(os.fsdecode(path).rstrip("/")) or "file")
    _keep_copy(target, data)
    original = os.fsdecode(path)
    if os.path.isabs(original) and "\n" not in original:
        _keep_copy(os.path.join(folder, ORIGIN_FILE), original.encode("utf-8", "surrogateescape"))
    return target


# Beside each copy, the worker's path it was made from (local_copy): what shown_url gives back for the copy.
ORIGIN_FILE = ".origin"


def shown_url(url):
    """
    A page's address as the worker knows it. The browser shows a local page the worker opened as this side's copy
    (local_copy: file://<home>/inputs/<id>/<name>); every reading of the page that goes back to the worker (the
    observation's page_url and groups, a recording's context, a replay's page check) names the worker's own file it
    was made from instead. Found live (2026-11-07): an imported recording's check "the page file:///home/headless/
    work/form.html" never matched the copy's address, and a recorded address held the copy's random folder. Any other
    address, and any outside broker mode, is itself.
    """

    if not url or not url.startswith("file://") or not requested():
        return url
    home = os.environ.get("HOME", "")
    if not home:
        return url
    import urllib.parse
    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return url
    path = urllib.parse.unquote(parts.path)
    base = os.path.join(home, COPIES_DIR)
    folder, name = os.path.split(path)
    if os.path.dirname(folder) != base or not name or name == ORIGIN_FILE or not _private_dir(folder):
        return url
    try:
        with open(os.path.join(folder, ORIGIN_FILE), "rb") as f:
            original = f.read(8192).decode("utf-8", "surrogateescape")
    except OSError:
        return url
    if not os.path.isabs(original) or "\n" in original:
        return url
    shown = "file://" + urllib.parse.quote(original)
    if parts.query:
        shown += "?" + parts.query
    if parts.fragment:
        shown += "#" + parts.fragment
    return shown


def _own_copy(path):
    """Whether `path` is one of this side's own copies already (local_copy made it)."""

    home = os.environ.get("HOME", "")
    if not home:
        return False
    base = os.path.realpath(os.path.join(home, COPIES_DIR))
    return os.path.realpath(os.fsdecode(path)).startswith(base + os.sep)


def _prune_copies(base):
    import shutil
    now = time.time()
    try:
        entries = list(os.scandir(base))
    except OSError:
        return
    for entry in entries:
        try:
            old = now - entry.stat(follow_symlinks=False).st_mtime > COPIES_KEEP_SECONDS
            if old and entry.is_dir(follow_symlinks=False):
                shutil.rmtree(entry.path, ignore_errors=True)
        except OSError:
            continue
