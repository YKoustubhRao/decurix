"""
The `gui` command's thin client (agent/gui runs it as `python3 -I -S` this file): hand the command to the desktop's gui
daemon (daemon.py) and be the command it ran - its output, its exit code. With no daemon, a daemon that does not answer
at once, or one that refuses, the command runs in this process's place exactly as it always did (`python3 -m
decurix_agent.computer`), so nothing can break because of the daemon.

What it hands over is the command itself, not a copy of it: its stdin, stdout and stderr (the file descriptors, over
the Unix socket), its working folder (a descriptor of it), its arguments, its environment, umask and niceness. The
daemon forks a child of an already-imported interpreter that takes those on and runs `python3 -m
decurix_agent.computer` in it, so what the command prints goes straight to where it would have gone.

Only the interpreter's own modules: this starts once per command, and a Python with site and a socket library costs
more than the daemon saves on a small command. Nothing from decurix_agent is imported here.

Frames (2 bytes each, kind + one byte):
  client -> daemon: the request (with the descriptors), then G (go: from here the daemon's run is the only run),
                    I (Ctrl-C: interrupt the command)
  daemon -> client: A (accepted) or R+why, S (the command started), C+code (its exit code, from the child),
                    E+code / K+signal (how the child ended, from the daemon)
"""

import os
import sys

PROTOCOL = b"DXG1"

# How long the daemon has to accept a command before it is run here instead.
HANDSHAKE_SECONDS = 2.0

# DECURIX_GUI_DAEMON: "off" runs every command in this process; "require" never does - a command the daemon does not
# take exits with REFUSED_EXIT and says why (the latency bench uses it to know it measured the daemon).
MODE_ENV = "DECURIX_GUI_DAEMON"
REFUSED_EXIT = 125

REFUSALS = {
    1: "its code changed on disk since it started",
    2: "this command's interpreter settings (PYTHON*, LANG, LC_*, HOME) differ from the daemon's",
    3: "it is running as many commands as it may",
    4: "the request was not understood",
    5: "it serves another user",
}

UNSEEN = ("gui: the screen daemon stopped while this ran - what it did was not seen; observe before doing it "
          "again\n")


def socket_dir():
    """The daemon's folder: the worker's own, mode 700 (daemon.py makes it; both sides check it)."""

    return os.environ.get("DECURIX_GUI_DAEMON_DIR") or "/tmp/decurix-gui-%d" % os.getuid()


def socket_path():
    """The daemon's socket."""

    return os.path.join(socket_dir(), "gui.sock")


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


def request(argv, fd_mask):
    """The request's bytes: protocol, length, then umask, niceness, which of 0-2 are sent, argc, argv, environment."""

    mask = os.umask(0)
    os.umask(mask)
    try:
        nice = os.getpriority(os.PRIO_PROCESS, 0)
    except (AttributeError, OSError):
        nice = 0
    fields = [b"%d" % mask, b"%d" % nice, b"%d" % fd_mask, b"%d" % len(argv)]
    fields += [os.fsencode(a) for a in argv]
    fields += [k + b"=" + v for k, v in os.environb.items()]
    body = _pack(fields)
    return PROTOCOL + len(body).to_bytes(4, "big") + body


def in_process(argv):
    """Run the command here, as `gui` always did. Never returns."""

    os.execvp("python3", ["python3", "-m", "decurix_agent.computer"] + list(argv))


def _recv_exact(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            return data
        data += chunk
    return data


def _descriptors():
    """The descriptors to hand over: those of 0-2 that are open, and the working folder. None when this command
    cannot be handed over (a terminal on stdin: a forked child is not in the terminal's foreground group and would
    be stopped reading it; a working folder that cannot be opened)."""

    fds, mask = [], 0
    for fd in (0, 1, 2):
        try:
            os.fstat(fd)
        except OSError:
            continue
        if fd == 0 and os.isatty(0):
            return None
        fds.append(fd)
        mask |= 1 << fd
    try:
        cwd = os.open(".", getattr(os, "O_PATH", os.O_RDONLY) | getattr(os, "O_DIRECTORY", 0))
    except OSError:
        return None
    return fds + [cwd], mask, cwd


def hand_over(argv):
    """
    Hand the command to the daemon. Returns ("ran", outcome) once the daemon took it - outcome ("exit", code),
    ("signal", number) or ("unseen", None) - or ("refused", why) / ("absent", None) when it did not, in which case
    nothing of the command ran.
    """

    try:
        import _socket
    except ImportError:
        return "absent", None
    if not hasattr(_socket, "AF_UNIX") or not hasattr(_socket.socket, "sendmsg"):
        return "absent", None
    if not owned_dir(socket_dir()):
        return "absent", None
    handed = _descriptors()
    if handed is None:
        return "absent", None
    fds, mask, cwd = handed

    conn = _socket.socket(_socket.AF_UNIX, _socket.SOCK_STREAM)
    try:
        conn.settimeout(HANDSHAKE_SECONDS)
        try:
            conn.connect(socket_path())
            data = request(argv, mask)
            rights = [(_socket.SOL_SOCKET, _socket.SCM_RIGHTS, b"".join(fd.to_bytes(4, sys.byteorder, signed=True)
                                                                         for fd in fds))]
            sent = conn.sendmsg([data], rights)
            if sent < len(data):
                conn.sendall(data[sent:])
            reply = _recv_exact(conn, 2)
        except OSError:
            return "absent", None
        finally:
            os.close(cwd)
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
        return "ran", _outcome(conn)
    finally:
        conn.close()


def _outcome(conn):
    buf, started, code, final = b"", False, None, None
    while True:
        try:
            chunk = conn.recv(64)
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
            kind, value, buf = buf[:1], buf[1], buf[2:]
            if kind == b"S":
                started = True
            elif kind == b"C":
                code = value
            elif kind == b"E":
                final = ("exit", value)
            elif kind == b"K":
                final = ("signal", value)
    if final is not None:
        return final
    if code is not None:
        return ("exit", code)
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


def main(argv=None):
    """The `gui` command: through the daemon when it takes the command, here otherwise."""

    argv = sys.argv[1:] if argv is None else argv
    mode = os.environ.get(MODE_ENV, "").strip().lower()
    if mode == "off":
        in_process(argv)
    status, detail = hand_over(argv)
    if status == "ran" and detail[0] != "absent":
        _end(detail)
    if mode == "require":
        why = REFUSALS.get(detail, "it is not running") if status == "refused" else "it is not running"
        os.write(2, ("gui: the screen daemon did not take this command: %s\n" % why).encode())
        os._exit(REFUSED_EXIT)
    in_process(argv)


if __name__ == "__main__":
    main()
