"""
The gui daemon: one long-lived process per desktop, in the worker's user, that keeps the screen stack imported so a
`gui` command does not pay for starting Python and importing it again (docs/computer-use.md, "The gui daemon").

    PYTHONPATH=/opt python3 -m decurix_agent.computer.daemon      (start_agent_terminal.sh, in a restart loop)

It is a fork server. It imports what `gui` imports (and the screen libraries `gui` imports later), then waits on a
Unix socket in a folder of the worker's own (mode 700). For each command the thin client (gui_client.py) hands it the
command's stdin, stdout, stderr and working folder as descriptors, with its arguments, environment, umask and
niceness. The daemon forks; the child takes all of that on and runs `python3 -m decurix_agent.computer` in itself. So:

- Nothing one command leaves in memory reaches another: every command is a fresh child of a process that has only
  imported, exactly the state a new `gui` process has after its imports. Connections (the browser's DevTools socket,
  the accessibility bus, the X display) are opened by the child as they always were, never shared.
- The output is the command's own: the child writes to the caller's own descriptors, with the buffering a new
  process would give them, and its exit code (or the signal that ended it) is the client's.
- Commands run side by side as separate `gui` processes always could (TheBot runs up to three steps at once), up
  to MAX_CALLS at a time; one more is refused and runs in its client's process.
- It makes no decision of its own and holds no yes: a consequential action asks the approval gate through the gate's
  own socket from the child, exactly as a `gui` process does (approval.py). The daemon never imports or calls the
  gate, and it holds nothing secret: it drops secret-looking variables from its own environment at start, a
  command's environment is handed to that command's child only, and it logs no arguments, environment or output.

What could tell a child from a new process, and how each is handled:

- Variables read when a module is imported (module-level os.environ reads, found in the source at start). When a
  command's value differs from the daemon's, the child forgets every decurix_agent module and imports them again
  under the command's environment - right, only slower.
- What the interpreter reads as it starts (PYTHON*, LANG, LC_*, HOME): a command whose values differ is refused and
  runs in its own process.
- The code itself: a module file changed on disk since the daemon started (a dev loop's docker cp) refuses every
  command from then on, and the daemon exits when its last command ends, to be restarted with the new code.
- Process-wide state a command sets (working folder, umask, signal handlers, sys.argv, stdio, atexit hooks): set by
  each child for itself, from the command's own, and gone with it.
"""

import argparse
import ast
import io
import os
import selectors
import signal
import socket
import struct
import sys
import time
import traceback

from decurix_agent.computer import gui_client

PROTOCOL = gui_client.PROTOCOL
HANDSHAKE_SECONDS = gui_client.HANDSHAKE_SECONDS

# Commands running at once; one more is refused (and runs in its client's process, as it would have).
MAX_CALLS = 8

# The largest request: argv and environment together are bounded by the kernel's ARG_MAX (2 MB on Linux).
MAX_REQUEST = 8 * 1024 * 1024

# Exit code of a daemon that cannot serve here and must not be restarted (the start loop stops on it).
EXIT_DO_NOT_RESTART = 3

REFUSE_STALE, REFUSE_STARTUP, REFUSE_BUSY, REFUSE_PROTOCOL, REFUSE_USER = 1, 2, 3, 4, 5

# What the interpreter reads as it starts: a command that differs here would not be the same interpreter.
STARTUP_EXACT = (b"HOME", b"LANG", b"LC_ALL", b"LC_CTYPE")
STARTUP_PREFIX = (b"PYTHON",)

# Read at import outside the decurix_agent sources (tempfile, Path.home): compared like the ones found in them.
IMPORT_ALWAYS = (b"TMPDIR", b"TEMP", b"TMP")

# The modules imported before the first command. Everything `gui` imports at start, and the screen libraries it
# imports on its first look; importing them is all - no connection is made.
PRELOAD = ("decurix_agent.computer.cli",)
PRELOAD_SCREEN = ("Xlib.display", "Xlib.X", "Xlib.XK", "Xlib.protocol.rq", "Xlib.ext.xinput", "Xlib.ext.ge")


def log(line):
    """One line for the desktop's startup log: what the daemon did, never what a command was or said."""

    try:
        sys.stderr.write(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] gui daemon: {line}\n")
        sys.stderr.flush()
    except (OSError, ValueError):
        pass


def secret_looking(name):
    """A variable the daemon does not keep: soul's rule for what its commands are not given (hostshell.ChildEnv),
    and the obvious other spellings of a key or password."""

    upper = name.upper()
    return (upper.endswith("_TOKEN") or upper.endswith("_KEY") or upper.endswith("_API_KEY")
            or "PASSWORD" in upper or "SECRET" in upper or upper in ("VNC_PW", "VNC_PASSWORD"))


# ------------------------------------------------------------
# What a command's environment must match
# ------------------------------------------------------------

def _env_reads(node):
    """The variable names `node` reads from the environment: os.environ.get/[]/in, os.getenv."""

    names = set()
    for sub in ast.walk(node):
        if isinstance(sub, ast.Call) and sub.args and isinstance(sub.args[0], ast.Constant) \
                and isinstance(sub.args[0].value, str):
            func = sub.func
            if isinstance(func, ast.Attribute) and func.attr in ("get", "getenv", "pop", "setdefault"):
                base = func.value
                if func.attr == "getenv" or (isinstance(base, ast.Attribute) and base.attr == "environ") \
                        or (isinstance(base, ast.Name) and base.id == "environ"):
                    names.add(sub.args[0].value)
        elif isinstance(sub, ast.Subscript) and isinstance(sub.value, ast.Attribute) and sub.value.attr == "environ":
            key = sub.slice
            if isinstance(key, ast.Constant) and isinstance(key.value, str):
                names.add(key.value)
        elif isinstance(sub, ast.Compare) and isinstance(sub.left, ast.Constant) and isinstance(sub.left.value, str):
            for comp in sub.comparators:
                if isinstance(comp, ast.Attribute) and comp.attr == "environ":
                    names.add(sub.left.value)
    return names


def import_time_reads(source):
    """The environment variables a module's source reads when it is imported: in its top-level statements and class
    bodies, decorators and default values - not inside the functions, which run per call."""

    names = set()
    pending = list(ast.parse(source).body)
    while pending:
        node = pending.pop()
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            for part in node.decorator_list + node.args.defaults + [d for d in node.args.kw_defaults if d]:
                names |= _env_reads(part)
            continue
        if isinstance(node, ast.ClassDef):
            for part in node.decorator_list + node.bases:
                names |= _env_reads(part)
            pending.extend(node.body)
            continue
        if isinstance(node, (ast.If, ast.Try, ast.With, ast.For, ast.While)):
            # A guarded import or a fallback: its test and its body both run at import.
            for field in ("test", "iter", "items"):
                part = getattr(node, field, None)
                if isinstance(part, list):
                    for item in part:
                        names |= _env_reads(item)
                elif part is not None:
                    names |= _env_reads(part)
            for field in ("body", "orelse", "finalbody"):
                pending.extend(getattr(node, field, []) or [])
            for handler in getattr(node, "handlers", []) or []:
                pending.extend(handler.body)
            continue
        names |= _env_reads(node)
    return names


def own_modules():
    """The decurix_agent modules this process has imported, with their source files."""

    out = {}
    for name, module in list(sys.modules.items()):
        path = getattr(module, "__file__", None)
        if (name == "decurix_agent" or name.startswith("decurix_agent.")) and path and path.endswith(".py"):
            out[name] = path
    return out


def import_keys(modules):
    """Every variable the imported modules read at import (bytes), plus IMPORT_ALWAYS."""

    keys = set(IMPORT_ALWAYS)
    for path in modules.values():
        try:
            with open(path, "rb") as f:
                keys |= {os.fsencode(k) for k in import_time_reads(f.read())}
        except (OSError, SyntaxError, ValueError):
            continue
    return keys


def stamps(modules):
    """Each imported module file's size and change time: a file that differs is code this daemon does not run."""

    out = {}
    for path in modules.values():
        try:
            st = os.stat(path)
            out[path] = (st.st_size, st.st_mtime_ns)
        except OSError:
            out[path] = None
    return out


def startup_key(name):
    """Whether the interpreter reads `name` (bytes) as it starts."""

    return name in STARTUP_EXACT or name.startswith(STARTUP_PREFIX) or name.startswith(b"LC_")


def differing(env, own, keys):
    """The names among `keys` whose value in `env` is not this daemon's (a missing variable is its own value)."""

    return sorted(k for k in keys if env.get(k) != own.get(k))


def startup_differs(env, own):
    """The startup variables (startup_key) that differ between a command's environment and the daemon's."""

    names = {k for k in set(env) | set(own) if startup_key(k)}
    return differing(env, own, names)


# ------------------------------------------------------------
# The request
# ------------------------------------------------------------

class Request:
    """One command as its client handed it over."""

    def __init__(self, umask, nice, fd_mask, argv, env, fds, cwd_fd):
        self.umask, self.nice, self.fd_mask, self.argv, self.env = umask, nice, fd_mask, argv, env
        self.fds, self.cwd_fd = fds, cwd_fd


def _unpack(body):
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


def parse_request(data, fds):
    """The request's fields; `fds` the descriptors that came with it (0-2 per the mask, then the working folder)."""

    if data[:4] != PROTOCOL:
        raise ValueError("not a gui request")
    fields = _unpack(data[8:])
    umask, nice, fd_mask, argc = (int(f) for f in fields[:4])
    argv = fields[4:4 + argc]
    if len(argv) != argc:
        raise ValueError("argv cut short")
    env = {}
    for item in fields[4 + argc:]:
        key, sep, value = item.partition(b"=")
        if sep:
            env[key] = value
    standard = [fd for fd in (0, 1, 2) if fd_mask & (1 << fd)]
    if len(fds) != len(standard) + 1:
        raise ValueError("descriptors do not match the request")
    return Request(umask, nice, fd_mask, argv, env, dict(zip(standard, fds[:-1])), fds[-1])


def read_request(conn):
    """Read one request and its descriptors from `conn`. Returns (bytes, [fds])."""

    space = socket.CMSG_SPACE(4 * struct.calcsize("i"))
    data, ancdata, flags, _ = conn.recvmsg(65536, space)
    fds = []
    for level, kind, payload in ancdata:
        if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
            usable = len(payload) - len(payload) % struct.calcsize("i")
            fds.extend(struct.unpack(f"{usable // struct.calcsize('i')}i", payload[:usable]))
    try:
        if flags & getattr(socket, "MSG_CTRUNC", 0):
            raise ValueError("descriptors cut short")
        while len(data) < 8:
            chunk = conn.recv(8 - len(data))
            if not chunk:
                raise ValueError("request cut short")
            data += chunk
        total = 8 + int.from_bytes(data[4:8], "big")
        if total > MAX_REQUEST:
            raise ValueError("request too large")
        while len(data) < total:
            chunk = conn.recv(min(1 << 20, total - len(data)))
            if not chunk:
                raise ValueError("request cut short")
            data += chunk
    except BaseException:
        _close_all(fds)
        raise
    return data, fds


def _close_all(fds):
    for fd in fds:
        try:
            os.close(fd)
        except OSError:
            pass


def _recv_exact(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            return data
        data += chunk
    return data


def peer_uid(conn):
    """The user at the other end of `conn` (SO_PEERCRED), or None where the system cannot say."""

    option = getattr(socket, "SO_PEERCRED", None)
    if option is None:
        return None
    _pid, uid, _gid = struct.unpack("3i", conn.getsockopt(socket.SOL_SOCKET, option, struct.calcsize("3i")))
    return uid


# ------------------------------------------------------------
# The child: one command
# ------------------------------------------------------------

def _stdio(fd, mode, encoding, errors, line_buffering):
    """A standard stream over `fd` as a new interpreter makes it (create_stdio): buffered, its line buffering by
    whether it is a terminal (stdout) or always (stderr, since 3.9)."""

    try:
        os.fstat(fd)
    except OSError:
        return None
    buffered = io.open(fd, mode, buffering=-1, closefd=False)
    try:
        buffered.raw.name = {0: "<stdin>", 1: "<stdout>", 2: "<stderr>"}[fd]
    except AttributeError:
        pass
    return io.TextIOWrapper(buffered, encoding=encoding, errors=errors, newline="\n",
                            line_buffering=line_buffering)


def exit_code(value):
    """What the interpreter makes of SystemExit(value): None is 0, an int is itself, anything else is printed to
    stderr and is 1."""

    if value is None:
        return 0
    if isinstance(value, int):
        return value & 0xFF
    try:
        sys.stderr.write(f"{value}\n")
    except Exception:  # noqa: BLE001 - as the interpreter: a message that cannot be written is not
        pass
    return 1


def forget_own_modules():
    """Drop every decurix_agent module, so the command imports them again under its own environment."""

    for name in [n for n in sys.modules if n == "decurix_agent" or n.startswith("decurix_agent.")]:
        del sys.modules[name]


class Child:
    """What the forked child does with one request: become that command's process, run it, and end as it would."""

    def __init__(self, server, conn, req):
        self.server, self.conn, self.req = server, conn, req

    def run(self):
        """Never returns."""

        code, interrupted = 1, False
        try:
            self._detach()
            try:
                self.conn.sendall(b"S\0")
            except OSError:
                os._exit(1)  # the client went away before anything ran
            self._become()
            code, interrupted = self._command()
            code = self._finish(code)
            if interrupted:
                # An uncaught Ctrl-C ends a Python process by the signal itself, so its caller sees that.
                signal.signal(signal.SIGINT, signal.SIG_DFL)
                os.kill(os.getpid(), signal.SIGINT)
        except BaseException:  # noqa: BLE001 - nothing may unwind into the daemon's own loop
            try:
                traceback.print_exc()
            except Exception:  # noqa: BLE001
                pass
        try:
            self.conn.sendall(b"C" + bytes([code & 0xFF]))
        except OSError:
            pass
        os._exit(code & 0xFF)

    def _detach(self):
        """Leave the daemon's world: its signal wiring, its sockets, its other commands' connections."""

        signal.set_wakeup_fd(-1)
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        # A daemon started in the background has Ctrl-C ignored; a `gui` process has Python's own handler.
        signal.signal(signal.SIGINT, signal.default_int_handler)
        signal.signal(signal.SIGQUIT, signal.SIG_DFL)
        self.server.close_in_child(self.conn)
        os.setpgid(0, 0)

    def _become(self):
        """Take on the command's descriptors, folder, umask, niceness, environment, sys.path, stdio and argv."""

        req = self.req
        for fd in (0, 1, 2):
            if fd in req.fds:
                os.dup2(req.fds[fd], fd)
            else:
                try:
                    os.close(fd)
                except OSError:
                    pass
        _close_all(list(req.fds.values()))
        os.fchdir(req.cwd_fd)
        os.close(req.cwd_fd)
        os.umask(req.umask)
        try:
            if req.nice > os.getpriority(os.PRIO_PROCESS, 0):
                os.setpriority(os.PRIO_PROCESS, 0, req.nice)
        except (AttributeError, OSError):
            pass

        os.environb.clear()
        os.environb.update(req.env)
        if differing(req.env, self.server.own_env, self.server.import_keys):
            forget_own_modules()

        # `python3 -m` puts the working folder first on sys.path.
        if self.server.path0_is_cwd and sys.path:
            sys.path[0] = os.getcwd()

        enc = self.server.stdio
        sys.stdin = sys.__stdin__ = _stdio(0, "rb", *enc[0], False)
        sys.stdout = sys.__stdout__ = _stdio(1, "wb", *enc[1], os.isatty(1) if 1 in req.fds else False)
        sys.stderr = sys.__stderr__ = _stdio(2, "wb", *enc[2], True)
        sys.argv = [self.server.argv0] + [os.fsdecode(a) for a in req.argv]

    def _command(self):
        """Run `python3 -m decurix_agent.computer` here. Returns (exit code, ended by an uncaught Ctrl-C)."""

        import runpy

        try:
            runpy.run_module("decurix_agent.computer", run_name="__main__", alter_sys=True)
            return 0, False
        except SystemExit as exc:
            return exit_code(exc.code), False
        except KeyboardInterrupt:
            traceback.print_exc()
            return 130, True
        except BaseException:  # noqa: BLE001 - as the interpreter: print it, exit 1
            traceback.print_exc()
            return 1, False

    def _finish(self, code):
        """The interpreter's own ending: wait for non-daemon threads, run atexit hooks, flush stdout and stderr (a
        flush that fails is 120), then close 0-2 so the caller's pipes end before the client does."""

        import atexit
        import threading

        shutdown = getattr(threading, "_shutdown", None)
        if shutdown is not None:
            try:
                shutdown()
            except Exception:  # noqa: BLE001
                pass
        try:
            atexit._run_exitfuncs()
        except Exception:  # noqa: BLE001
            pass
        for stream in (sys.stdout, sys.stderr):
            if stream is None:
                continue
            try:
                stream.flush()
            except Exception as exc:  # noqa: BLE001
                code = 120
                try:
                    if stream is sys.stdout and sys.stderr is not None:
                        sys.stderr.write(f"Exception ignored in: {stream!r}\n{type(exc).__name__}: {exc}\n")
                        sys.stderr.flush()
                except Exception:  # noqa: BLE001
                    pass
        for fd in (0, 1, 2):
            try:
                os.close(fd)
            except OSError:
                pass
        return code


# ------------------------------------------------------------
# The daemon
# ------------------------------------------------------------

class Server:
    """The listening daemon: accepts requests, forks a child for each, reports how each child ended."""

    def __init__(self, directory):
        self.directory = directory
        self.path = os.path.join(directory, "gui.sock")
        self.modules = own_modules()
        self.import_keys = import_keys(self.modules)
        self.stamps = stamps(self.modules)
        self.own_env = dict(os.environb)
        self.stdio = [(getattr(s, "encoding", None) or "utf-8", getattr(s, "errors", None) or default)
                      for s, default in ((sys.__stdin__, "strict"), (sys.__stdout__, "strict"),
                                         (sys.__stderr__, "backslashreplace"))]
        main = sys.modules.get("decurix_agent.computer.__main__")
        self.argv0 = os.path.join(os.path.dirname(gui_client.__file__), "__main__.py") if main is None else main.__file__
        self.path0_is_cwd = bool(sys.path) and sys.path[0] == os.getcwd()
        self.calls = {}          # pid -> conn
        self.stale = False
        self.listener = None
        self.selector = None
        self.wake = None

    # -- set up --

    def bind(self):
        """Make the folder (mode 700, the worker's own) and listen in it. Returns False when another daemon already
        serves this folder or the folder is not safely ours."""

        try:
            os.mkdir(self.directory, 0o700)
        except FileExistsError:
            pass
        if not gui_client.owned_dir(self.directory):
            log(f"{self.directory} is not a folder of this user only; not serving")
            return False
        probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        probe.settimeout(1.0)
        try:
            probe.connect(self.path)
            log("another gui daemon already serves this desktop")
            return False
        except OSError:
            pass
        finally:
            probe.close()
        # Bound under a name of its own, then renamed over the socket: a client never finds a half-made one.
        temp = f"{self.path}.{os.getpid()}"
        try:
            os.unlink(temp)
        except FileNotFoundError:
            pass
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(temp)
        os.chmod(temp, 0o600)
        self.listener.listen(64)
        os.rename(temp, self.path)
        return True

    def close_in_child(self, keep):
        """In a forked child: close every socket of the daemon's but `keep`, the child's own connection."""

        if self.selector is not None:
            self.selector.close()
        for sock in [self.listener, *(self.wake or ())] + [c for c in self.calls.values() if c is not keep]:
            if sock is not None:
                try:
                    sock.close()
                except OSError:
                    pass

    # -- serving --

    def serve(self):
        """The loop: never returns until the daemon's code went stale and its last command ended."""

        self.selector = selectors.DefaultSelector()
        wake_r, wake_w = socket.socketpair()
        wake_r.setblocking(False)
        wake_w.setblocking(False)
        self.wake = (wake_r, wake_w)
        signal.set_wakeup_fd(wake_w.fileno())
        signal.signal(signal.SIGCHLD, lambda *_: None)
        self.selector.register(self.listener, selectors.EVENT_READ, "accept")
        self.selector.register(wake_r, selectors.EVENT_READ, "wake")
        try:
            while not (self.stale and not self.calls):
                for key, _ in self.selector.select(timeout=5.0):
                    if key.data == "accept":
                        self.accept()
                    elif key.data == "wake":
                        try:
                            while wake_r.recv(256):
                                pass
                        except OSError:
                            pass
                    else:
                        self.client_event(key.fileobj, key.data)
                self.reap()
        finally:
            self.unlink()

    def unlink(self):
        """Take the socket away (only if it is still this daemon's)."""

        if self.listener is None:
            return
        try:
            if os.stat(self.path).st_ino == os.fstat(self.listener.fileno()).st_ino:
                os.unlink(self.path)
        except OSError:
            pass

    def code_changed(self):
        """Whether a module file this daemon imported changed on disk since it started."""

        return stamps(self.modules) != self.stamps

    def refusal(self, req):
        """Why this daemon will not run `req`, or 0."""

        if self.stale or self.code_changed():
            if not self.stale:
                log("its code changed on disk; refusing commands and exiting when the last one ends")
                self.stale = True
                self.unlink()
            return REFUSE_STALE
        startup = startup_differs(req.env, self.own_env)
        if startup:
            log("a command's interpreter settings differ (" + ", ".join(os.fsdecode(k) for k in startup)
                + "); it runs in its own process")
            return REFUSE_STARTUP
        if len(self.calls) >= MAX_CALLS:
            return REFUSE_BUSY
        return 0

    def accept(self):
        """Take one request: check it, agree, wait for the client's go, fork."""

        try:
            conn, _ = self.listener.accept()
        except OSError:
            return
        conn.settimeout(HANDSHAKE_SECONDS)
        fds = []
        forked = False
        try:
            uid = peer_uid(conn)
            if uid is not None and uid != os.getuid():
                conn.sendall(b"R" + bytes([REFUSE_USER]))
                return
            try:
                data, fds = read_request(conn)
                req = parse_request(data, fds)
            except (ValueError, OSError):
                try:
                    conn.sendall(b"R" + bytes([REFUSE_PROTOCOL]))
                except OSError:
                    pass
                return
            finally:
                data = None
            why = self.refusal(req)
            if why:
                conn.sendall(b"R" + bytes([why]))
                return
            conn.sendall(b"A\0")
            if _recv_exact(conn, 2) != b"G\0":
                return  # the client gave up waiting and runs the command itself
            conn.settimeout(None)
            sys.stderr.flush()
            pid = os.fork()
            if pid == 0:
                Child(self, conn, req).run()  # never returns
            forked = True
            try:
                # The child's own group, made from both sides so a kill sent at once cannot miss it.
                os.setpgid(pid, pid)
            except OSError:
                pass
            self.calls[pid] = conn
            self.selector.register(conn, selectors.EVENT_READ, pid)
        except OSError:
            pass
        finally:
            req = None
            _close_all(fds)
            if not forked:
                try:
                    conn.close()
                except OSError:
                    pass

    def client_event(self, conn, pid):
        """The client sent Ctrl-C, or went away (killed by its caller): the command gets the same."""

        try:
            data = conn.recv(64)
        except OSError:
            data = b""
        if pid not in self.calls:
            return
        try:
            if not data:
                # A `gui` process killed with its caller's process group dies at once; so does this command.
                os.killpg(pid, signal.SIGKILL)
                self.selector.unregister(conn)
            elif b"I" in data:
                os.killpg(pid, signal.SIGINT)
        except (OSError, KeyError, ValueError):
            pass

    def reap(self):
        """Report every child that ended to its client: its exit code or the signal that ended it."""

        while True:
            try:
                pid, status = os.waitpid(-1, os.WNOHANG)
            except ChildProcessError:
                return
            if pid == 0:
                return
            conn = self.calls.pop(pid, None)
            if conn is None:
                continue
            try:
                if os.WIFSIGNALED(status):
                    conn.sendall(b"K" + bytes([os.WTERMSIG(status) & 0xFF]))
                else:
                    conn.sendall(b"E" + bytes([os.WEXITSTATUS(status) & 0xFF]))
            except OSError:
                pass
            try:
                self.selector.unregister(conn)
            except (KeyError, ValueError):
                pass
            conn.close()


# ------------------------------------------------------------
# Start
# ------------------------------------------------------------

def native_threads():
    """How many threads this process has, the ones libraries started in C included (fork is only safe with one)."""

    try:
        return len(os.listdir("/proc/self/task"))
    except OSError:
        import threading
        return threading.active_count()


def preload(screen=True):
    """Import what `gui` imports. The accessibility library (gi) last, and only if asked: it is the one that could
    start a thread of its own."""

    import importlib

    for name in PRELOAD:
        importlib.import_module(name)
    for name in PRELOAD_SCREEN:
        try:
            importlib.import_module(name)
        except Exception:  # noqa: BLE001 - not installed (a test, a host): `gui` imports it later, or says so
            pass
    if screen:
        try:
            import gi
            gi.require_version("Atspi", "2.0")
            from gi.repository import Atspi  # noqa: F401
        except Exception:  # noqa: BLE001
            pass


def _reexec(args, env):
    argv = [sys.executable] + list(getattr(sys, "orig_argv", [sys.executable, "-m", __spec__.name])[1:])
    for flag in args:
        if flag not in argv:
            argv.append(flag)
    os.execve(sys.executable, argv, env)


def main(argv=None):
    """Start the daemon. Exit 0 to be restarted (the code changed), EXIT_DO_NOT_RESTART when it cannot serve here."""

    parser = argparse.ArgumentParser(prog="decurix_agent.computer.daemon")
    parser.add_argument("--no-gi", action="store_true", help="do not import the accessibility library ahead")
    args = parser.parse_args(sys.argv[1:] if argv is None else argv)

    if not hasattr(os, "fork") or not hasattr(socket, "AF_UNIX") or not hasattr(socket.socket, "recvmsg"):
        log("this system cannot fork or pass descriptors; gui runs in its own process")
        return EXIT_DO_NOT_RESTART

    # Nothing secret stays in this process's environment (/proc/PID/environ is the one it was started with, so it is
    # started again without them). A command's own environment is its child's alone.
    kept = {k: v for k, v in os.environb.items() if not secret_looking(os.fsdecode(k))}
    if len(kept) != len(os.environb):
        _reexec([], kept)

    preload(screen=not args.no_gi)
    if native_threads() > 1:
        if not args.no_gi:
            log("a library started a thread on import; starting again without importing the accessibility library")
            _reexec(["--no-gi"], dict(os.environb))
        log("a thread is running before the first fork; not serving")
        return EXIT_DO_NOT_RESTART

    server = Server(gui_client.socket_dir())
    if not server.bind():
        return EXIT_DO_NOT_RESTART
    log(f"serving (pid {os.getpid()}, {len(server.modules)} modules, {len(server.import_keys)} import-time variables)")
    server.serve()
    log("exiting to be restarted with the changed code")
    return 0


if __name__ == "__main__":
    sys.exit(main())
