"""
The input broker's paths, read-only, as the worker (bench/security/screen_probes.sh; docs/gate-boundary.md, "Next: the
input broker"). Run inside a desktop as the worker, `python3 -I - ROW [ARG]`, with this file on stdin. Prints one line:
"ok" when the path is closed, else what is open. Nothing here sends input to the screen or a request that changes
anything: it opens the display to list extensions, reads files and /proc, connects to sockets, and asks a DevTools
endpoint for /json/version.
"""

import glob
import os
import socket
import sys

HOME = "/home/headless"
SCREEN_HOME = "/var/lib/decurix-screen"
SCREEN_NAMES = ("Xvnc", "Xtigervnc", "xfwm4", "chrome")


def screen_side_pids():
    """{pid: comm} of the screen's own processes: the X server, the window manager, the browser, the broker."""

    found = {}
    for d in glob.glob("/proc/[0-9]*"):
        try:
            comm = open(d + "/comm").read().strip()
            cmdline = open(d + "/cmdline", "rb").read().replace(b"\0", b" ")
        except OSError:
            continue
        if comm in SCREEN_NAMES or b"decurix_agent.computer.daemon" in cmdline:
            found[int(d[6:])] = comm
    return found


def uid_of(pid):
    try:
        return os.stat(f"/proc/{pid}").st_uid
    except OSError:
        return None


def xtest():
    os.environ["DISPLAY"] = ":1"
    try:
        from Xlib import display
    except ImportError:
        return "python-xlib is missing"
    opened, with_xtest = [], []
    for cookie in (HOME + "/.Xauthority", SCREEN_HOME + "/Xauthority", "/run/decurix-screen/worker.xauth"):
        if not os.access(cookie, os.R_OK):
            continue
        os.environ["XAUTHORITY"] = cookie
        try:
            d = display.Display(":1")
        except Exception:  # noqa: BLE001 - this cookie does not open the display
            continue
        opened.append(cookie)
        if d.query_extension("XTEST") is not None:
            with_xtest.append(cookie)
        d.close()
    if not opened:
        return "the worker's X clients cannot open the display at all (its own apps would not run)"
    return "ok" if not with_xtest else "XTEST is offered to the worker's X client (cookie " + ", ".join(with_xtest) + ")"


def vnc_password(obfuscated_hex, plain):
    want = bytes.fromhex(obfuscated_hex)
    hits = []
    for top in (HOME, "/tmp", "/run", "/var/lib", "/dockerstartup", "/etc", "/opt"):
        for folder, dirs, files in os.walk(top):
            dirs[:] = [d for d in dirs if not d.startswith((".cache", "node_modules"))]
            for name in files:
                path = os.path.join(folder, name)
                try:
                    if os.path.islink(path) or os.path.getsize(path) > 64:
                        continue
                    with open(path, "rb") as f:
                        if want in f.read():
                            hits.append(path)
                except OSError:
                    continue
    for d in glob.glob("/proc/[0-9]*"):
        try:
            env = open(d + "/environ", "rb").read().split(b"\0")
        except OSError:
            continue
        if any(kv.partition(b"=")[2] == plain.encode() for kv in env):
            hits.append(f"the environment of {open(d + '/comm').read().strip()} ({d[6:]})")
    return "ok" if not hits else "readable: " + ", ".join(sorted(set(hits))[:6])


def _listeners():
    """[(port, uid, inode)] of listening TCP sockets."""

    out = []
    for table in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            lines = open(table).read().splitlines()[1:]
        except OSError:
            continue
        for line in lines:
            f = line.split()
            if len(f) > 9 and f[3] == "0A":
                out.append((int(f[1].rsplit(":", 1)[1], 16), int(f[7]), f[9]))
    return out


def _answers_devtools(port):
    try:
        s = socket.create_connection(("127.0.0.1", port), timeout=2)
    except OSError:
        return False
    try:
        s.sendall(b"GET /json/version HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n")
        data = b""
        while len(data) < 65536:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
    except OSError:
        pass
    finally:
        s.close()
    return b"webSocketDebuggerUrl" in data or b"Browser" in data


def devtools(vnc_port="5901"):
    browsers = [p for p, c in screen_side_pids().items() if c == "chrome"]
    if not browsers:
        return "inconclusive: no browser is running"
    open_ = []
    if _answers_devtools(9222):
        open_.append("127.0.0.1:9222 answers /json/version")
    # The browser's own listening sockets: by its descriptors where the worker may read them (its own browser), else
    # every listener of the browser's user but the VNC port.
    inodes = set()
    for pid in browsers:
        for fd in glob.glob(f"/proc/{pid}/fd/*"):
            try:
                link = os.readlink(fd)
            except OSError:
                continue
            if link.startswith("socket:["):
                inodes.add(link[8:-1])
    owners = {uid_of(p) for p in browsers}
    for port, uid, inode in _listeners():
        if inode in inodes or (uid in owners and uid != os.getuid() and str(port) != vnc_port):
            if _answers_devtools(port):
                open_.append(f"port {port} answers /json/version")
            else:
                open_.append(f"the browser's user listens on port {port}")
    for path in glob.glob("/run/decurix-screen/cdp/*") + ["/run/decurix-screen/cdp/cdp.sock"]:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.settimeout(2)
            s.connect(path)
            open_.append(f"the relay socket {path} accepts the worker")
        except OSError:
            pass
        finally:
            s.close()
    return "ok" if not open_ else "; ".join(sorted(set(open_)))


def cookies(paths):
    if not paths:
        return "inconclusive: no cookie store found"
    readable = []
    for path in paths:
        try:
            with open(path, "rb") as f:
                f.read(16)
            readable.append(path)
        except OSError:
            continue
    return "ok" if not readable else "readable: " + ", ".join(readable)


def environ():
    found = screen_side_pids()
    if not found:
        return "inconclusive: no screen-side process found"
    bad = []
    for pid, comm in sorted(found.items()):
        if uid_of(pid) == os.getuid():
            bad.append(f"{comm} ({pid}) is the worker's own")
            continue
        try:
            open(f"/proc/{pid}/environ", "rb").read(1)
            bad.append(f"the environment of {comm} ({pid}) is readable")
        except OSError:
            pass
    return "ok" if not bad else "; ".join(bad[:6])


def main(argv):
    row = argv[0] if argv else ""
    if row == "xtest":
        print(xtest())
    elif row == "vnc":
        print(vnc_password(argv[1], argv[2]))
    elif row == "devtools":
        print(devtools())
    elif row == "cookies":
        print(cookies(argv[1:]))
    elif row == "environ":
        print(environ())
    else:
        print(f"unknown row {row!r}")


if __name__ == "__main__":
    main(sys.argv[1:])
