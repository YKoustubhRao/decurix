"""
The `gui` command's thin client (agent/gui runs it as `python3 -I -S` this file): hand the command to the desktop's gui
daemon (daemon.py) and be the command it ran - its output, its exit code. With no daemon, a daemon that does not answer
at once, or one that refuses, the command runs in this process's place exactly as it always did (`python3 -m
decurix_agent.computer`), so nothing can break because of the daemon.

What it hands over is the command itself, not a copy of it: its stdin, stdout and stderr (the file descriptors, over
the Unix socket), its working folder (a descriptor of it), its arguments, its environment, umask and niceness. The
daemon forks a child of an already-imported interpreter that takes those on and runs `python3 -m
decurix_agent.computer` in it, so what the command prints goes straight to where it would have gone.

With no daemon at all, the command also starts one (start_daemon): detached from the command - its own session, not
a child of it, none of its descriptors - so the next command finds it. The command that started it runs here, as it
would have, never waiting for it. The daemon exits by itself after a while with no command (daemon.IDLE_ENV).

Only the interpreter's own modules: this starts once per command, and a Python with site and a socket library costs
more than the daemon saves on a small command. Nothing from decurix_agent is imported here.

Frames (2 bytes each, kind + one byte):
  client -> daemon: the request (with the descriptors), then G (go: from here the daemon's run is the only run),
                    I (Ctrl-C: interrupt the command)
  daemon -> client: A (accepted) or R+why, S (the command started), C+code (its exit code, from the child),
                    E+code / K+signal (how the child ended, from the daemon)
  and one frame of its own length, from the child, in broker mode only:
                    O + 4-byte length + fields (flags, path, bytes): a file the command writes for its caller
                    (a screenshot), which this client writes as the caller (deliver_to_caller in broker.py), or
                    (OUTPUT_STATE) a write of the worker's own state - a recording, an evidence note - at a path
                    under the caller's state folder (broker.mirror, save_state)

Broker mode (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "Next: the input broker"): the screen is another user's,
`decurixscreen`, and its daemon - the screen broker - listens in a shared folder (/run/decurix-screen, the screen
user's, 0711). This client then trusts the socket only when the folder is the screen user's and nobody else may write
in it, and the process that answers is the screen user's (SO_PEERCRED). It never runs a screen command in its own process:
the worker's user cannot reach the screen, so a command the broker did not take is an error, said as such.

What is the worker's and what is the screen's, in broker mode (docs/gate-boundary.md, "the screen side's own
state"): the worker's state (~/.decurix, the task file in /tmp) stays the worker's, written only by the worker. The
screen side never opens a path of the worker's: what it must know of that state comes with each request
(caller_context: the task, its secrets, its evidence so far), the files a command was given to read come as
descriptors (input_files: an upload, a local page), and what it writes of the worker's state comes back over the
connection for this client to write (save_state). The commands that read or write only the worker's own stores
(WORKER_COMMANDS: its skills, recordings, records and guides) run here, as the worker, and reach the screen only
through the broker (screen_call.py), like the agent's own calls.
"""

import os
import re
import sys

PROTOCOL = b"DXG1"

# Broker mode: the switch the control plane sets on a desktop (DECURIX_SCREEN_BROKER on the control plane), the
# screen's user, and where its broker listens. The worker's uid is the desktop user's (decurix-entrypoint.sh).
SCREEN_ENV = "DECURIX_SCREEN_USER"
SCREEN_USER = "decurixscreen"
DEFAULT_SCREEN_DIR = "/run/decurix-screen"
DEFAULT_WORKER_UID = 1001

# Exit code of a command the screen could not be reached for: the CLI's own "could not do it" (cli.EXIT_ERROR).
UNREACHABLE_EXIT = 1

# The largest file a command hands its caller to write (a screenshot is a few MB).
MAX_OUTPUT = 64 * 1024 * 1024

# Flags of an O frame. STATE: the path is relative to the caller's state folder (save_state), APPEND: add to it.
OUTPUT_REQUIRED, OUTPUT_ATOMIC, OUTPUT_STATE, OUTPUT_APPEND = 1, 2, 4, 8

# Broker mode: the commands that read and write only the worker's own stores - its skills, recordings, record ledger,
# memory and app guides - and reach the screen, where they do at all (replay, do), through the broker's screen calls
# (screen_call.py). They run in this process, as the worker, which is whose stores those are: run by the screen user
# they would read none of them (the screen user may not open the worker's files, and must not: a file the worker
# planted there could point at the screen user's own).
WORKER_COMMANDS = frozenset({"recordings", "recording", "records", "record", "skills", "recall", "skill", "do",
                             "replay", "guide"})

# What of the worker's state the screen side is told with every command (caller_context), as the worker holds it:
# the task file's parts (task_context.py, privacy's secret values beside it), state files a screen command reads,
# and the current task's evidence so far and its recording's meta. Each is read here, as the worker, and written by
# the broker's child into the screen user's own folders (broker.take_context); nothing of it is a path the screen
# side opens. A name is "t" + a task-file suffix or "s" + a path under the state folder (broker.CONTEXT_NAME).
TASK_SUFFIXES = ("", ".stage", ".request", ".files", ".since", ".continues", ".secrets", ".secrets.resume")
STATE_FILES = ("withheld-secrets", "active-task.json", "filled-documents.json", "opened-files.json",
               "computer/worker_mode.json", "computer/guided-run.json")
# The task's run context (task_context: the org's date order, the reply's judgement, and the questions a save waits on -
# a date's order, a field's unit) goes both ways: the save path reads it and keeps its question there, which the agent
# asks the person from (found live 2026-11-08: in broker mode the unit question stayed in the screen user's copy and
# the person was never asked it).
TASK_STATE = ("computer/evidence/%s.jsonl", "computer/evidence/%s.records.json", "computer/evidence/%s.files",
              "computer/evidence/%s.outputs", "computer/evidence/%s.said", "recordings/task-%s/meta.json",
              "runs/%s/context.json")
MAX_CONTEXT_FILE = 8 * 1024 * 1024
# The worker's launchers (its desktop's icons and its menu's apps, launchers.PLACES), sent the same way: "l" + the
# place + "/" + the file's name. The screen side lists and opens them by name (`gui open "Excel (Host)"`, a replay
# bringing a window forward) and starts what one runs through the worker's launch helper, as the worker.
LAUNCHER_PLACES = (("Desktop", "Desktop"), ("applications", ".local/share/applications"))
MAX_LAUNCHERS, MAX_LAUNCHER_FILE = 200, 64 * 1024
# The files the task's request brought (the task file's .files), handed over as descriptors with every command: the
# screen side reads them to check a chosen record against them (core._in_documents). At most this many.
MAX_TASK_DOCUMENTS = 4

# The parts of the caller's state folder the screen side writes into (save_state): recordings and evidence, and one
# file of a task's run folder, its context (TASK_STATE).
MIRRORED_ROOTS = ((b"recordings",), (b"computer", b"evidence"))
MIRRORED_FILES = re.compile(rb"runs/t[0-9a-f]{16}/context\.json")


def mirrored(rel):
    """Whether `rel` (bytes, "/"-separated, relative to the state folder) is a part the screen side writes back."""

    parts = tuple(rel.split(b"/"))
    return (any(parts[:len(root)] == root and len(parts) > len(root) for root in MIRRORED_ROOTS)
            or MIRRORED_FILES.fullmatch(rel) is not None)

# At most this many files a command names are handed over as descriptors (input_files).
MAX_INPUTS = 16

# How long the daemon has to accept a command before it is run here instead.
HANDSHAKE_SECONDS = 2.0

# DECURIX_GUI_DAEMON: "off" runs every command in this process; "require" never does - a command the daemon does not
# take exits with REFUSED_EXIT and says why (the latency bench uses it to know it measured the daemon).
MODE_ENV = "DECURIX_GUI_DAEMON"
REFUSED_EXIT = 125

# DECURIX_GUI_DAEMON_AUTOSTART=0: a command that finds no daemon does not start one (a test that drives a daemon of its
# own). Otherwise it does (start_daemon).
AUTOSTART_ENV = "DECURIX_GUI_DAEMON_AUTOSTART"

# The lock a daemon holds in its folder while it serves or starts (daemon.claim): held, no other one is started.
DAEMON_LOCK = "daemon.lock"

# What a started daemon writes its own events to (the desktop's startup log, as when the desktop's start script
# started it): only a file of this user's that is already there; otherwise nowhere.
LOG_ENV = "DECURIX_GUI_DAEMON_LOG"
DEFAULT_LOG = "/tmp/decurix-startup.log"

# How long a command run with DECURIX_GUI_DAEMON=require waits for a daemon it started to serve.
REQUIRE_START_SECONDS = 60.0

# hand_over's detail for a command that cannot be handed over at all (a terminal on stdin): no daemon is waited for.
NOT_HANDED = "not-handed"

REFUSALS = {
    1: "its code changed on disk since it started",
    2: "this command's interpreter settings (PYTHON*, LANG, LC_*, HOME) differ from the daemon's",
    3: "it is running as many commands as it may",
    4: "the request was not understood",
    5: "it serves another user",
    6: "it runs at a lower priority than this command asks",
}

UNSEEN = ("gui: the screen daemon stopped while this ran - what it did was not seen; observe before doing it "
          "again\n")


def socket_dir():
    """The daemon's folder: the worker's own, mode 700 (daemon.py makes it; both sides check it)."""

    return os.environ.get("DECURIX_GUI_DAEMON_DIR") or "/tmp/decurix-gui-%d" % os.getuid()


def socket_path():
    """The daemon's socket."""

    return os.path.join(socket_dir(), "gui.sock")


def broker_requested():
    """Whether this desktop drives its screen through the screen broker (DECURIX_SCREEN_USER=1, fixed at creation)."""

    return os.environ.get(SCREEN_ENV, "").strip() == "1"


def screen_dir():
    """The broker's shared folder (the entrypoint makes it, as root, for the screen user)."""

    return os.environ.get("DECURIX_SCREEN_DIR") or DEFAULT_SCREEN_DIR


def broker_socket_path():
    """The broker's socket."""

    return os.path.join(screen_dir(), "gui.sock")


def _uid_setting(name):
    value = os.environ.get(name, "").strip()
    return int(value) if value.isdigit() else None


def screen_uid():
    """The screen user's uid (DECURIX_SCREEN_UID, else the `decurixscreen` account), or None when there is none."""

    uid = _uid_setting("DECURIX_SCREEN_UID")
    if uid is not None:
        return uid
    try:
        import pwd
        return pwd.getpwnam(SCREEN_USER).pw_uid
    except (ImportError, KeyError):
        return None


def worker_uid():
    """The worker's uid: whose commands the broker takes (DECURIX_SCREEN_WORKER_UID, else the desktop user's)."""

    uid = _uid_setting("DECURIX_SCREEN_WORKER_UID")
    return DEFAULT_WORKER_UID if uid is None else uid


def shared_dir(path, owner):
    """Whether `path` is a real folder of `owner` that nobody else may write in (others may only pass through it):
    the shape of a folder that holds a socket two users meet at."""

    try:
        st = os.lstat(path)
    except OSError:
        return False
    return (st.st_mode & 0o170000) == 0o040000 and st.st_uid == owner and (st.st_mode & 0o022) == 0


def peer_uid(conn):
    """The user at the other end of a connected Unix socket (SO_PEERCRED: pid, uid, gid), or None where the system
    cannot say."""

    try:
        import _socket
    except ImportError:
        return None
    option = getattr(_socket, "SO_PEERCRED", None)
    if option is None:
        return None
    try:
        data = conn.getsockopt(_socket.SOL_SOCKET, option, 12)
    except OSError:
        return None
    return int.from_bytes(data[4:8], sys.byteorder, signed=True)


def owned_dir(path):
    """Whether `path` is a real folder of this user that nobody else may enter: otherwise a socket in it could be
    anybody's, and so could the answers it prints."""

    try:
        st = os.lstat(path)
    except OSError:
        return False
    return (st.st_mode & 0o170000) == 0o040000 and st.st_uid == os.getuid() and (st.st_mode & 0o077) == 0


def _pack(fields):
    return b"".join(len(f).to_bytes(4, "big") + f for f in fields)


def pack(fields):
    """Length-prefixed fields, as a request and an O frame carry them."""

    return _pack(fields)


def unpack(body):
    """The fields of pack(); ValueError when they are cut short."""

    fields, i = [], 0
    while i < len(body):
        if i + 4 > len(body):
            raise ValueError("truncated field")
        n = int.from_bytes(body[i:i + 4], "big")
        i += 4
        if i + n > len(body):
            raise ValueError("truncated field")
        fields.append(body[i:i + n])
        i += n
    return fields


def state_dir():
    """The worker's state folder (config.DECURIX_DIR)."""

    return os.environ.get("DECURIX_DIR") or os.path.join(os.path.expanduser("~"), ".decurix")


def task_file():
    """The worker's task file (task_context.CURRENT_FILE)."""

    return os.environ.get("DECURIX_TASK_FILE") or "/tmp/decurix-current-task"


def _read_own(path, limit=MAX_CONTEXT_FILE):
    """A regular file's bytes, read as this user, or None (absent, not a regular file, larger than `limit`)."""

    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if (st.st_mode & 0o170000) != 0o100000 or st.st_size > limit:
            return None
        chunks, left = [], limit + 1
        while left > 0:
            chunk = os.read(fd, min(1 << 20, left))
            if not chunk:
                break
            chunks.append(chunk)
            left -= len(chunk)
        data = b"".join(chunks)
        return data if len(data) <= limit else None
    except OSError:
        return None
    finally:
        os.close(fd)


def _valid_task(raw):
    task = (raw or b"").strip()
    return task if len(task) == 17 and task[:1] == b"t" and all(c in b"0123456789abcdef" for c in task[1:]) else b""


def caller_context():
    r"""The request's context fields (broker mode): b"\0c" + name + b"\0" + bytes for each part of the worker's state
    the screen side is told (TASK_SUFFIXES, STATE_FILES, TASK_STATE of the current task), b"\0x" + name for one that
    is not there (the screen side's copy is removed)."""

    fields, base, root = [], task_file(), state_dir()
    items = [(b"t" + suffix.encode(), base + suffix) for suffix in TASK_SUFFIXES]
    items += [(b"s" + rel.encode(), os.path.join(root, rel)) for rel in STATE_FILES]
    task = _valid_task(_read_own(base, 64))
    if task:
        items += [(b"s" + (rel % task.decode()).encode(), os.path.join(root, rel % task.decode())) for rel in TASK_STATE]
    for name, path in items:
        data = _read_own(path)
        fields.append(b"\0x" + name if data is None else b"\0c" + name + b"\0" + data)
    fields.append(b"\0cl\0")  # (the whole set of launchers follows: one the screen side has and is not in it goes)
    home, sent = os.path.expanduser("~"), 0
    for place, folder in LAUNCHER_PLACES:
        try:
            names = sorted(n for n in os.listdir(os.path.join(home, folder)) if n.endswith(".desktop"))
        except OSError:
            continue
        for file_name in names:
            if sent >= MAX_LAUNCHERS or "/" in file_name or file_name.startswith("."):
                continue
            data = _read_own(os.path.join(home, folder, file_name), MAX_LAUNCHER_FILE)
            if data is not None:
                fields.append(b"\0cl" + os.fsencode(place + "/" + file_name) + b"\0" + data)
                sent += 1
    return fields


def task_documents():
    """The files the current task's request brought, as the task file lists them (at most MAX_TASK_DOCUMENTS)."""

    listed = _read_own(task_file() + ".files", 1 << 20) or b""
    return [os.fsdecode(line.strip()) for line in listed.splitlines() if line.strip()][:MAX_TASK_DOCUMENTS]


def _file_argument(arg):
    """The local path an argument names (a path, or a file:// URL's), or None."""

    if arg.startswith("file://"):
        arg = arg[len("file://"):]
        if "%" in arg:
            import urllib.parse
            arg = urllib.parse.unquote(arg)
    if not arg or "\0" in arg or len(arg) > 4096:
        return None
    return arg


def input_files(argv):
    """The files a command names that this user can read (broker mode): [(given, absolute path, descriptor)], read
    only and at most MAX_INPUTS. The screen side reads them only from these descriptors - an upload, a local page the
    browser opens - never by their paths, which the screen user may not open."""

    found, seen = [], set()
    for arg in list(argv[1:]):
        for candidate in (arg, arg.split("=", 1)[1] if arg.startswith("--") and "=" in arg else None):
            path = _file_argument(candidate) if candidate else None
            if path is None or path in seen or len(found) >= MAX_INPUTS:
                continue
            seen.add(path)
            try:
                fd = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0))
            except OSError:
                continue
            try:
                regular = (os.fstat(fd).st_mode & 0o170000) == 0o100000
            except OSError:
                regular = False
            if not regular:
                os.close(fd)
                continue
            found.append((os.fsencode(candidate), os.fsencode(os.path.abspath(path)), fd))
    return found


def caller_paths(argv):
    """The paths a command was given (bytes) - its arguments, the value of each `--option=value`, and
    DECURIX_GUI_SCREENSHOT. A file the broker hands back is written only at one of these: the screen side fills in
    what the caller asked for, it never chooses where the worker's files go."""

    paths = set()
    shot = os.environ.get("DECURIX_GUI_SCREENSHOT", "").strip()
    if shot:
        paths.add(os.fsencode(shot))
    for arg in argv:
        paths.add(os.fsencode(arg))
        if arg.startswith("--") and "=" in arg:
            paths.add(os.fsencode(arg.split("=", 1)[1]))
    paths.discard(b"")
    return paths


def state_target(rel):
    """Where a state write the screen side handed back goes (bytes), or None: a relative path of plain parts, in one
    of MIRRORED_ROOTS, under this user's state folder."""

    parts = rel.split(b"/")
    if not rel or rel.startswith(b"/") or any(p in (b"", b".", b"..") or b"\0" in p for p in parts):
        return None
    if not mirrored(rel):
        return None
    return os.path.join(os.fsencode(state_dir()), rel)


def save_state(flags, rel, data):
    """Write a state write the screen side handed back (OUTPUT_STATE), as this user: added to the file (APPEND) or
    put in its place whole. Returns whether it was written; one that was not is said on stderr, the command's own
    result stands (a lost note is a lost note, as in-process)."""

    target = state_target(rel)
    shown = os.fsdecode(rel)
    if target is None:
        os.write(2, ("gui: the screen broker sent a state file for %r, which is not one it may write; not written\n"
                     % shown).encode("utf-8", "replace"))
        return False
    try:
        # Nobody else's to write, whatever this command's umask (a `docker exec` has 0000): 0755 and 0644 at most.
        mask = os.umask(0o022)
        try:
            os.makedirs(os.path.dirname(target), exist_ok=True)
        finally:
            os.umask(mask)
        # A run's context is the worker's alone, as task_context writes it (0600).
        mode = 0o600 if MIRRORED_FILES.fullmatch(rel) else 0o644
        if flags & OUTPUT_APPEND:
            fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_CLOEXEC", 0)
                         | getattr(os, "O_BINARY", 0), mode)
            part = None
        else:
            part = target + b".%d.part" % os.getpid()
            fd = os.open(part, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_CLOEXEC", 0)
                         | getattr(os, "O_BINARY", 0), mode)
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
        finally:
            os.close(fd)
        if part is not None:
            os.replace(part, target)
    except OSError as exc:
        os.write(2, ("gui: could not keep %s: %s\n" % (shown, exc.strerror or exc)).encode("utf-8", "replace"))
        return False
    return True


def save_output(payload, allowed):
    """Write one O frame's file as this process's user, in its working folder: (written, required). A path the
    caller did not give is not written. A state write (OUTPUT_STATE) goes to save_state."""

    try:
        flags, path, data = unpack(payload)
        flags = int(flags)
    except ValueError:
        return False, True
    if flags & OUTPUT_STATE:
        return save_state(flags, path, data), False
    required = bool(flags & OUTPUT_REQUIRED)
    shown = os.fsdecode(path)
    if path not in allowed:
        os.write(2, ("gui: the screen broker sent a file for %r, which this command did not name; not written\n"
                     % shown).encode("utf-8", "replace"))
        return False, required
    target = path + b".part" if flags & OUTPUT_ATOMIC else path
    try:
        flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_BINARY", 0)
        fd = os.open(target, flags, 0o666)
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
        finally:
            os.close(fd)
        if target != path:
            os.replace(target, path)
    except OSError as exc:
        os.write(2, ("gui: could not save %s: %s\n" % (shown, exc.strerror or exc)).encode("utf-8", "replace"))
        return False, required
    return True, required


def request(argv, fd_mask, extra=()):
    """The request's bytes: protocol, length, then umask, niceness, which of 0-2 are sent, argc, argv, environment,
    and (broker mode) the `extra` fields: the caller's context and the files handed over (each starts with a NUL,
    which no environment entry can)."""

    mask = os.umask(0)
    os.umask(mask)
    try:
        nice = os.getpriority(os.PRIO_PROCESS, 0)
    except (AttributeError, OSError):
        nice = 0
    fields = [b"%d" % mask, b"%d" % nice, b"%d" % fd_mask, b"%d" % len(argv)]
    fields += [os.fsencode(a) for a in argv]
    fields += [k + b"=" + v for k, v in os.environb.items()]
    fields += list(extra)
    body = _pack(fields)
    return PROTOCOL + len(body).to_bytes(4, "big") + body


def in_process(argv):
    """Run the command here, as `gui` always did. Never returns."""

    os.execvp("python3", ["python3", "-m", "decurix_agent.computer"] + list(argv))


def broker_ready():
    """Whether the screen broker serves now: its folder is the screen user's and the screen user answers at its socket
    (nothing is sent; the broker reads no request and lets the connection go)."""

    try:
        import _socket
    except ImportError:
        return False
    owner = screen_uid()
    if owner is None or not shared_dir(screen_dir(), owner):
        return False
    conn = _socket.socket(_socket.AF_UNIX, _socket.SOCK_STREAM)
    try:
        conn.settimeout(HANDSHAKE_SECONDS)
        conn.connect(broker_socket_path())
        return peer_uid(conn) == owner
    except OSError:
        return False
    finally:
        conn.close()


def _recv_exact(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            return data
        data += chunk
    return data


def _descriptors(broker=False):
    """The descriptors to hand over: those of 0-2 that are open, and the working folder. None when this command
    cannot be handed over (a terminal on stdin: a forked child is not in the terminal's foreground group and would
    be stopped reading it; a working folder that cannot be opened). In broker mode, where nothing runs here, a
    terminal on stdin is not handed over at all: the command runs with no stdin."""

    fds, mask = [], 0
    for fd in (0, 1, 2):
        try:
            os.fstat(fd)
        except OSError:
            continue
        if fd == 0 and os.isatty(0):
            if broker:
                continue
            return None
        fds.append(fd)
        mask |= 1 << fd
    try:
        cwd = os.open(".", getattr(os, "O_PATH", os.O_RDONLY) | getattr(os, "O_DIRECTORY", 0))
    except OSError:
        return None
    return fds + [cwd], mask, cwd


def hand_over(argv, broker=False):
    """
    Hand the command to the daemon (or, with `broker`, to the screen broker). Returns ("ran", outcome) once the
    daemon took it - outcome ("exit", code), ("signal", number) or ("unseen", None) - or ("refused", why) /
    ("absent", None) / ("untrusted", what) when it did not, in which case nothing of the command ran - ("absent",
    NOT_HANDED) when this command cannot be handed to any daemon (a terminal on stdin).
    """

    try:
        import _socket
    except ImportError:
        return "absent", None
    if not hasattr(_socket, "AF_UNIX") or not hasattr(_socket.socket, "sendmsg"):
        return "absent", None
    if broker:
        owner = screen_uid()
        if owner is None:
            return "untrusted", "this desktop has no screen user (%s)" % SCREEN_USER
        if not shared_dir(screen_dir(), owner):
            if not os.path.lexists(screen_dir()):
                return "absent", None
            return "untrusted", "%s is not the screen user's own folder" % screen_dir()
        path = broker_socket_path()
    else:
        if not owned_dir(socket_dir()):
            return "absent", None
        path = socket_path()
    handed = _descriptors(broker)
    if handed is None:
        return "absent", NOT_HANDED
    fds, mask, cwd = handed
    inputs, extra = [], []
    if broker:
        inputs = input_files(argv)
        named = {path for _, path, _ in inputs}
        inputs += [i for i in input_files([""] + task_documents()) if i[1] not in named][:MAX_TASK_DOCUMENTS]
        inputs = inputs[:MAX_INPUTS + MAX_TASK_DOCUMENTS]
        fds = fds + [fd for _, _, fd in inputs]
        extra = caller_context() + [b"\0f" + given + b"\0" + path for given, path, _ in inputs]

    conn = _socket.socket(_socket.AF_UNIX, _socket.SOCK_STREAM)
    try:
        conn.settimeout(HANDSHAKE_SECONDS)
        try:
            conn.connect(path)
            if broker and peer_uid(conn) != owner:
                # Not the screen user answering: nothing is handed to it, not even the command.
                return "untrusted", "the process at %s is not the screen user's" % path
            data = request(argv, mask, extra)
            rights = [(_socket.SOL_SOCKET, _socket.SCM_RIGHTS, b"".join(fd.to_bytes(4, sys.byteorder, signed=True)
                                                                         for fd in fds))]
            try:
                sent = conn.sendmsg([data], rights)
                if sent < len(data):
                    conn.sendall(data[sent:])
            except OSError:
                # A daemon that refuses a caller at once (another user, too many commands) sends its refusal and
                # closes; our send may then fail first. Its refusal is still waiting to be read: read it, so the
                # command says why (found on CI, 2026-11-07: "not running" instead of "serves another user").
                try:
                    early = _recv_exact(conn, 2)
                except OSError:
                    early = b""
                if early[:1] == b"R":
                    return "refused", early[1] if len(early) == 2 else 0
                return "absent", None
            reply = _recv_exact(conn, 2)
        except OSError:
            return "absent", None
        finally:
            os.close(cwd)
            for _, _, fd in inputs:
                os.close(fd)
        if reply[:1] == b"R":
            return "refused", reply[1] if len(reply) == 2 else 0
        if reply != b"A\0":
            return "absent", None
        try:
            conn.sendall(b"G\0")
        except OSError:
            return "absent", None
        # From here the daemon's run is the only run: it is never also run here, except when the daemon went away
        # before the command started (no S).
        conn.settimeout(None)
        return "ran", _outcome(conn, caller_paths(argv) if broker else set())
    finally:
        conn.close()


def _outcome(conn, allowed=frozenset()):
    """How the command ended, reading the daemon's frames to the end. A file the command handed back (O, broker
    mode) is written here as it comes; one it had to write and could not makes a command that exited 0 exit 1."""

    buf, started, code, final, lost = b"", False, None, None, False
    while True:
        try:
            chunk = conn.recv(1 << 16)
        except KeyboardInterrupt:
            # Ctrl-C reaches this process, not the command's (it is not in this terminal's group): pass it on, and
            # wait for the command to end as it would have here.
            try:
                conn.sendall(b"I\0")
            except OSError:
                pass
            continue
        except OSError:
            chunk = b""
        if not chunk:
            break
        buf += chunk
        while len(buf) >= 2:
            kind = buf[:1]
            if kind == b"O":
                if len(buf) < 5:
                    break
                n = int.from_bytes(buf[1:5], "big")
                if n > MAX_OUTPUT:
                    # Not a frame this client can follow: what the command did is not known from here.
                    return ("unseen", None)
                if len(buf) < 5 + n:
                    break
                written, required = save_output(buf[5:5 + n], allowed)
                lost = lost or (required and not written)
                buf = buf[5 + n:]
                continue
            value, buf = buf[1], buf[2:]
            if kind == b"S":
                started = True
            elif kind == b"C":
                code = value
            elif kind == b"E":
                final = ("exit", value)
            elif kind == b"K":
                final = ("signal", value)
    if final is None and code is not None:
        final = ("exit", code)
    if final is not None:
        if lost and final == ("exit", 0):
            return ("exit", 1)
        return final
    return ("unseen", None) if started else ("absent", None)


def _end(outcome):
    kind, value = outcome
    if kind == "signal":
        # Killed by a signal: end the same way, so the caller's shell sees what it would have.
        try:
            import _signal
            _signal.signal(value, _signal.SIG_DFL)
            os.kill(os.getpid(), value)
        except (ImportError, OSError, ValueError):
            pass
        os._exit(128 + value)
    if kind == "unseen":
        try:
            os.write(2, UNSEEN.encode())
        except OSError:
            pass
        os._exit(1)
    os._exit(value)


def may_start_daemon():
    """Whether a daemon may be started for this user now: no daemon holds the folder's lock (none serves, none is
    starting), and the folder is absent or this user's alone."""

    directory = socket_dir()
    if not os.path.lexists(directory):
        return True
    if not owned_dir(directory):
        return False
    try:
        import fcntl
    except ImportError:
        return False
    try:
        fd = os.open(os.path.join(directory, DAEMON_LOCK),
                     os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0), 0o600)
    except OSError:
        return False
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        return False
    finally:
        os.close(fd)   # the lock, when it was taken here, goes with it: the daemon takes it for itself
    return True


def _daemon_log():
    """The descriptor a started daemon logs to: the startup log when it is a regular file of this user's, else None."""

    path = os.environ.get(LOG_ENV) or DEFAULT_LOG
    try:
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if st.st_uid == os.getuid() and (st.st_mode & 0o170000) == 0o100000:
            return fd
    except OSError:
        pass
    os.close(fd)
    return None


def start_daemon(argv=None):
    """
    Start this user's gui daemon, when none serves or is starting (may_start_daemon), as the desktop's start script
    used to: `python3 -m decurix_agent.computer.daemon` with this command's environment (the PYTHONPATH `gui` set;
    the daemon drops anything secret-looking from it at once), in /, its stdin /dev/null and its output to the
    startup log. Detached from this command: its own session (a kill of the caller's process group does not reach
    it), not a child of this process (a second fork), and none of this command's descriptors (the caller's pipes end
    when the command does). Returns whether one was started; nothing of it is waited for.
    """

    if not hasattr(os, "fork") or not hasattr(os, "setsid") or not may_start_daemon():
        return False
    argv = list(argv or ["python3", "-m", "decurix_agent.computer.daemon"])
    env = {k: v for k, v in os.environb.items() if k != os.fsencode(MODE_ENV)}
    try:
        pid = os.fork()
    except OSError:
        return False
    if pid == 0:
        try:
            os.setsid()
            if os.fork():
                os._exit(0)
            os.chdir("/")
            null = os.open(os.devnull, os.O_RDWR)
            out = _daemon_log()
            os.dup2(null, 0)
            os.dup2(null if out is None else out, 1)
            os.dup2(null if out is None else out, 2)
            try:
                limit = os.sysconf("SC_OPEN_MAX")
            except (AttributeError, OSError, ValueError):
                limit = 65536
            os.closerange(3, max(limit, 256))
            os.execvpe(argv[0], argv, env)
        except BaseException:  # noqa: BLE001 - nothing of a failed start may reach the command
            pass
        os._exit(127)
    try:
        os.waitpid(pid, 0)
    except OSError:
        pass
    return True


def autostart():
    """Whether a command that finds no daemon starts one (DECURIX_GUI_DAEMON_AUTOSTART is not 0)."""

    return os.environ.get(AUTOSTART_ENV, "").strip() != "0"


def unreachable(status, detail):
    """What a command the screen broker did not take says (broker mode: it is never run here instead)."""

    if status == "refused":
        why = REFUSALS.get(detail, "it refused the command")
    elif status == "untrusted":
        why = detail
    elif status == "ran":
        why = "it went away before the command started"
    else:
        why = "it is not running"
    return ("gui: cannot reach the screen: the screen broker did not take this command (%s). Nothing was done; "
            "the screen is not this user's to drive directly.\n" % why)


def main(argv=None):
    """The `gui` command: through the daemon when it takes the command, here otherwise. In broker mode only through
    the screen broker: a command it does not take is an error, never run here."""

    argv = sys.argv[1:] if argv is None else argv
    if broker_requested():
        if argv and argv[0] in WORKER_COMMANDS:
            # The worker's own stores, read and written as the worker; the screen only through the broker.
            in_process(argv)
        status, detail = hand_over(argv, broker=True)
        if status == "ran" and detail[0] != "absent":
            _end(detail)
        os.write(2, unreachable(status, detail).encode("utf-8", "replace"))
        os._exit(UNREACHABLE_EXIT)
    mode = os.environ.get(MODE_ENV, "").strip().lower()
    if mode == "off":
        in_process(argv)
    status, detail = hand_over(argv)
    if status == "ran" and detail[0] != "absent":
        _end(detail)
    # No daemon (or one gone before it ran this): start one for the next command. This one runs here, as it would
    # have, and does not wait for it.
    started = status in ("absent", "ran") and autostart() and start_daemon()
    if mode == "require" and status in ("absent", "ran") and detail != NOT_HANDED and autostart():
        # The latency bench: measured through a daemon only, so wait for the one started (by this or another
        # command) to serve.
        import time
        deadline = time.monotonic() + REQUIRE_START_SECONDS
        while status in ("absent", "ran") and time.monotonic() < deadline:
            time.sleep(0.1)
            status, detail = hand_over(argv)
            if status == "ran" and detail[0] != "absent":
                _end(detail)
            if status in ("absent", "ran") and not started:
                started = start_daemon()
    if mode == "require":
        why = REFUSALS.get(detail, "it is not running") if status == "refused" else "it is not running"
        os.write(2, ("gui: the screen daemon did not take this command: %s\n" % why).encode())
        os._exit(REFUSED_EXIT)
    in_process(argv)


if __name__ == "__main__":
    main()
