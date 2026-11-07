"""
The worker's launch helper (broker mode; broker.py, docs/gate-boundary.md "Next: the input broker").

    PYTHONPATH=/opt python3 -m decurix_agent.computer.launch_helper      (start_agent_terminal.sh, as the worker)

In broker mode the screen broker runs as the screen user, and a program it started would be that user's: a terminal
opened by `gui open` would type as the screen side, where no gate looks. So the broker starts only the browser itself
(session.launch); every other program a command opens - session.launch from core.open, the launchers, an app's icon -
is started here, as the worker, with the worker's own environment (this process's, plus what session.app_env adds for
accessibility). The request is the program and its arguments, nothing else: no environment, no folder, no
descriptors of the screen side's.

The socket is in a folder of the worker's own that others may only pass through (/tmp/decurix-launch-<uid>, 0711; the
socket 0666), and a request is taken only from the screen user's uid (SO_PEERCRED): the broker is its only peer. The
broker in turn checks that the folder and the process answering are the worker's (request), so it never hands a
program to anyone else to start.

Protocol: one line of JSON each way. {"argv": ["mousepad", "/home/headless/notes.txt"]} -> {"pid": 1234} or
{"error": "why"}.
"""

import json
import os
import socket
import subprocess
import sys
import threading
import time

from decurix_agent.computer import broker, gui_client

MAX_REQUEST = 64 * 1024
MAX_ARGS = 256
REQUEST_SECONDS = 10.0

# Exit code of a helper that cannot serve here (its start loop stops on it), as the gui daemon's.
EXIT_DO_NOT_RESTART = 3


def log(line):
    """One line for the desktop's startup log: what the helper did, never which program or its arguments."""

    try:
        sys.stderr.write(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] launch helper: {line}\n")
        sys.stderr.flush()
    except (OSError, ValueError):
        pass


def socket_path(directory=None):
    """The helper's socket."""

    return os.path.join(directory or broker.launch_dir(), "launch.sock")


def valid_argv(argv):
    """`argv` when it is a program and its arguments (non-empty strings, no NUL); ValueError otherwise."""

    if not isinstance(argv, list) or not argv or len(argv) > MAX_ARGS:
        raise ValueError("a request names a program and its arguments")
    for arg in argv:
        if not isinstance(arg, str) or "\0" in arg:
            raise ValueError("every argument is a string with no NUL")
    if not argv[0].strip():
        raise ValueError("no program named")
    return argv


def start_program(argv):
    """Start `argv` as this process's user, detached, with this process's own environment (session.app_env), in its
    home folder. Returns the pid. Raises OSError when it cannot be started."""

    from decurix_agent.computer import session

    process = subprocess.Popen(
        argv,
        env=session.app_env(),
        cwd=os.path.expanduser("~"),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    # Reaped when it ends, so a long-lived helper leaves no zombies.
    threading.Thread(target=process.wait, daemon=True).start()
    return process.pid


def _read_line(conn, limit=MAX_REQUEST):
    data = b""
    while b"\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            break
        data += chunk
        if len(data) > limit:
            raise ValueError("request too large")
    return data.split(b"\n", 1)[0]


def _reply(conn, answer):
    try:
        conn.sendall((json.dumps(answer) + "\n").encode("utf-8"))
    except OSError:
        pass


def handle(conn, allowed_uid, peer=gui_client.peer_uid, start=start_program):
    """Serve one connection: refuse any caller but `allowed_uid` (the screen user), else start what it asks. Returns
    whether a program was started. Closes nothing: the caller's to close."""

    uid = peer(conn)
    if uid is None or uid != allowed_uid:
        _reply(conn, {"error": "this helper starts programs for the screen broker only"})
        return False
    try:
        argv = valid_argv(json.loads(_read_line(conn).decode("utf-8")).get("argv"))
    except (ValueError, AttributeError, UnicodeDecodeError, OSError) as exc:
        _reply(conn, {"error": f"not a request: {exc}"})
        return False
    try:
        pid = start(argv)
    except OSError as exc:
        _reply(conn, {"error": f"could not start {os.path.basename(argv[0])}: {exc.strerror or exc}"})
        return False
    _reply(conn, {"pid": pid})
    return True


def bind(directory, uid=None):
    """Make the folder (the worker's, 0711) and listen in it: the listening socket, or None when the folder is not
    safely this user's or another helper already serves it."""

    uid = os.getuid() if uid is None else uid
    try:
        os.mkdir(directory, 0o711)
    except FileExistsError:
        pass
    if not gui_client.shared_dir(directory, uid):
        log(f"{directory} is not this user's folder, or others may write in it; not serving")
        return None
    path = socket_path(directory)
    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    probe.settimeout(1.0)
    try:
        probe.connect(path)
        log("another launch helper already serves this desktop")
        return None
    except OSError:
        pass
    finally:
        probe.close()
    temp = f"{path}.{os.getpid()}"
    try:
        os.unlink(temp)
    except FileNotFoundError:
        pass
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(temp)
    # Open to every user of the desktop: who may use it is decided per connection (handle).
    os.chmod(temp, 0o666)
    listener.listen(16)
    os.rename(temp, path)
    return listener


def serve(listener, allowed_uid):
    """Accept connections until the listener is closed."""

    while True:
        try:
            conn, _ = listener.accept()
        except OSError:
            return
        with conn:
            conn.settimeout(REQUEST_SECONDS)
            try:
                handle(conn, allowed_uid)
            except Exception as exc:  # noqa: BLE001 - one bad request never ends the helper
                log(f"a request failed: {type(exc).__name__}")


def request(argv, timeout=REQUEST_SECONDS):
    """
    The screen side's call (session.launch in broker mode): have the worker's helper start `argv`. Returns its pid.
    Raises OSError when the helper is not there, is not the worker's, or could not start it - never starts the
    program here instead.
    """

    argv = valid_argv([str(a) for a in argv])
    directory = broker.launch_dir()
    worker = broker.worker_uid()
    if not gui_client.shared_dir(directory, worker):
        raise OSError(f"the worker's launch helper is not running ({directory} is not the worker's folder)")
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        conn.settimeout(timeout)
        try:
            conn.connect(socket_path(directory))
        except OSError as exc:
            raise OSError(f"the worker's launch helper is not running: {exc.strerror or exc}") from exc
        if gui_client.peer_uid(conn) != worker:
            raise OSError("the process at the launch helper's socket is not the worker's")
        conn.sendall((json.dumps({"argv": argv}) + "\n").encode("utf-8"))
        try:
            answer = json.loads(_read_line(conn).decode("utf-8") or "{}")
        except (ValueError, UnicodeDecodeError) as exc:
            raise OSError(f"the launch helper's answer was not understood: {exc}") from exc
    finally:
        conn.close()
    pid = answer.get("pid") if isinstance(answer, dict) else None
    if not isinstance(pid, int) or isinstance(pid, bool):
        why = answer.get("error") if isinstance(answer, dict) else None
        raise OSError(str(why) if why else "the launch helper did not start it")
    return pid


def main():
    """Serve as the worker's launch helper. Exit EXIT_DO_NOT_RESTART when it cannot serve here."""

    if not hasattr(socket, "AF_UNIX") or getattr(socket, "SO_PEERCRED", None) is None:
        log("this system cannot tell a caller's user; not serving")
        return EXIT_DO_NOT_RESTART
    if not broker.requested():
        log("this desktop does not use the screen broker (DECURIX_SCREEN_USER); not serving")
        return EXIT_DO_NOT_RESTART
    if os.getuid() != broker.worker_uid():
        log("not the worker's user; not serving")
        return EXIT_DO_NOT_RESTART
    screen = gui_client.screen_uid()
    if screen is None:
        log(f"this desktop has no screen user ({gui_client.SCREEN_USER}); not serving")
        return EXIT_DO_NOT_RESTART
    listener = bind(broker.launch_dir())
    if listener is None:
        return EXIT_DO_NOT_RESTART
    log(f"serving (pid {os.getpid()}) for uid {screen}")
    serve(listener, screen)
    return 0


if __name__ == "__main__":
    sys.exit(main())
