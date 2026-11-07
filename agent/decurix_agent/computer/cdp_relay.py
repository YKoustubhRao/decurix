"""
The DevTools relay (broker mode; broker.py, docs/gate-boundary.md "Next: the input broker").

    python3 -m decurix_agent.computer.cdp_relay [--socket PATH] -- chromium-browser [PAGES...]

In broker mode the browser has no DevTools port: a loopback TCP port is open to every user of the desktop, the
worker included, and cannot be limited to one uid without NET_ADMIN. The relay starts the browser itself with
--remote-debugging-pipe (agent/chromium-browser swaps the port for it when DECURIX_BROWSER_DEBUG=pipe) and holds both
ends of that pipe: the browser reads commands on its fd 3 and writes answers and events on its fd 4, one JSON message
per NUL. The relay serves on a Unix socket in a folder only the screen user may enter (/run/decurix-screen/cdp, 0700)
exactly what cdp.py uses, and nothing else:

    GET /json/version             Browser.getVersion
    GET /json/list (and /json)    Target.getTargets, pages with a webSocketDebuggerUrl
    PUT /json/new?URL             Target.createTarget
    GET /json/activate/ID, /json/close/ID
    GET /devtools/page/ID         a WebSocket on that page: Target.attachToTarget (flatten), and every message
                                  the client sends goes to that session only (its sessionId is set here, whatever
                                  the client wrote); every message of that session comes back without it

No browser-wide WebSocket is offered. The broker's children (the gui daemon's, as the screen user) are the only ones
who can open the socket; each connection's peer must be this user (SO_PEERCRED) as well.

The relay lives as long as the browser: it ends when the browser closes its end of the pipe, and takes its socket
away. Started a second time while one serves, it hands the pages it was given to the one serving and exits.
"""

import argparse
import json
import os
import re
import socket
import struct
import sys
import threading
import urllib.parse

from decurix_agent.computer import broker, gui_client

# What tells the desktop's browser launcher to give the browser a pipe and no port (agent/chromium-browser).
PIPE_ENV = ("DECURIX_BROWSER_DEBUG", "pipe")

# The browser's ends of the pipe, as --remote-debugging-pipe expects them.
BROWSER_READS, BROWSER_WRITES = 3, 4

CALL_SECONDS = 15.0
MAX_HEAD = 64 * 1024
# The largest message either way: cdp.MAX_FRAME_BYTES.
MAX_MESSAGE = 64 * 1024 * 1024

WS_GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
TARGET_ID = r"[A-Za-z0-9._-]{1,128}"
PAGE_PATH = re.compile(r"/devtools/page/(" + TARGET_ID + ")")
TARGET_PATH = re.compile(r"/json/(activate|close)/(" + TARGET_ID + ")")

REASONS = {101: "Switching Protocols", 200: "OK", 400: "Bad Request", 404: "Not Found", 405: "Method Not Allowed",
           500: "Internal Server Error", 503: "Service Unavailable"}


class RelayError(Exception):
    """The browser did not do what was asked: not running, no answer, or an error of its own."""


# ------------------------------------------------------------
# The browser's end: NUL-separated JSON over the pipe
# ------------------------------------------------------------

class Browser:
    """The pipe to the browser: browser-wide calls answered by id, and each page session's messages routed to the
    WebSocket client that opened it."""

    def __init__(self, write_fd, read_fd):
        self.write_fd, self.read_fd = write_fd, read_fd
        self.write_lock = threading.Lock()
        self.lock = threading.Lock()
        self.pending = {}       # id -> [Event, reply]
        self.sessions = {}      # sessionId -> PageClient
        self.next_id = 0
        self.closed = threading.Event()

    def start(self):
        threading.Thread(target=self._read, daemon=True, name="cdp-relay-pipe").start()
        return self

    def send(self, message):
        """One message to the browser. Raises OSError when the pipe is gone."""

        data = json.dumps(message, separators=(",", ":")).encode("utf-8") + b"\0"
        with self.write_lock:
            view = memoryview(data)
            while view:
                view = view[os.write(self.write_fd, view):]

    def call(self, method, params=None, timeout=CALL_SECONDS):
        """A browser-wide command; its result. RelayError when it is not answered or answered with an error."""

        if self.closed.is_set():
            raise RelayError("the browser is not running")
        with self.lock:
            self.next_id += 1
            mine = self.next_id
            waiter = self.pending[mine] = [threading.Event(), None]
        try:
            try:
                self.send({"id": mine, "method": method, "params": params or {}})
            except OSError as exc:
                raise RelayError("the browser is not running") from exc
            if not waiter[0].wait(timeout):
                raise RelayError(f"the browser did not answer {method} within {timeout:g}s")
            reply = waiter[1]
            if reply is None:
                raise RelayError("the browser is not running")
            if "error" in reply:
                raise RelayError(f"{method}: {(reply.get('error') or {}).get('message')}")
            return reply.get("result") or {}
        finally:
            with self.lock:
                self.pending.pop(mine, None)

    def attach(self, session_id, client):
        with self.lock:
            self.sessions[session_id] = client

    def detach(self, session_id):
        with self.lock:
            return self.sessions.pop(session_id, None)

    def _read(self):
        buf, scanned = bytearray(), 0
        try:
            while True:
                chunk = os.read(self.read_fd, 1 << 20)
                if not chunk:
                    break
                buf += chunk
                while True:
                    end = buf.find(b"\0", scanned)
                    if end < 0:
                        scanned = len(buf)
                        break
                    raw = bytes(buf[:end])
                    del buf[:end + 1]
                    scanned = 0
                    self._dispatch(raw)
                if len(buf) > MAX_MESSAGE:
                    break
        except OSError:
            pass
        finally:
            self._closed()

    def _closed(self):
        self.closed.set()
        with self.lock:
            waiters = list(self.pending.values())
            clients = list(self.sessions.values())
            self.sessions.clear()
        for waiter in waiters:
            waiter[0].set()
        for client in clients:
            client.close()

    def _dispatch(self, raw):
        try:
            message = json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return
        if not isinstance(message, dict):
            return
        session_id = message.pop("sessionId", None)
        if session_id is not None:
            with self.lock:
                client = self.sessions.get(session_id)
            if client is not None:
                client.deliver(json.dumps(message, separators=(",", ":")))
            return
        if "id" in message:
            with self.lock:
                waiter = self.pending.get(message.get("id"))
            if waiter is not None:
                waiter[1] = message
                waiter[0].set()
            return
        if message.get("method") == "Target.detachedFromTarget":
            client = self.detach((message.get("params") or {}).get("sessionId"))
            if client is not None:
                client.close()


# ------------------------------------------------------------
# WebSocket frames (RFC 6455), the server's side
# ------------------------------------------------------------

def ws_frame(opcode, payload):
    """A whole, unmasked frame (a server's)."""

    n = len(payload)
    head = bytes([0x80 | opcode])
    if n < 126:
        head += bytes([n])
    elif n < 65536:
        head += bytes([126]) + struct.pack(">H", n)
    else:
        head += bytes([127]) + struct.pack(">Q", n)
    return head + payload


def unmask(payload, mask):
    n = len(payload)
    if not n:
        return b""
    key = (mask * (n // 4 + 1))[:n]
    return (int.from_bytes(payload, "big") ^ int.from_bytes(key, "big")).to_bytes(n, "big")


def accept_key(key):
    import base64
    import hashlib

    return base64.b64encode(hashlib.sha1(key.encode("ascii") + WS_GUID).digest()).decode("ascii")


class _Reader:
    def __init__(self, conn, buffered=b""):
        self.conn, self.buf = conn, bytearray(buffered)

    def need(self, n):
        while len(self.buf) < n:
            chunk = self.conn.recv(1 << 16)
            if not chunk:
                raise EOFError("closed")
            self.buf += chunk
        out = bytes(self.buf[:n])
        del self.buf[:n]
        return out


def read_message(reader):
    """The next whole message (opcode, payload), continuation frames joined; a control frame as it comes."""

    message, first = bytearray(), None
    while True:
        b0, b1 = reader.need(2)
        opcode, n = b0 & 0x0F, b1 & 0x7F
        if n == 126:
            n = struct.unpack(">H", reader.need(2))[0]
        elif n == 127:
            n = struct.unpack(">Q", reader.need(8))[0]
        if n > MAX_MESSAGE:
            raise ValueError("frame too large")
        mask = reader.need(4) if b1 & 0x80 else None
        payload = reader.need(n)
        if mask is not None:
            payload = unmask(payload, mask)
        if opcode >= 0x8:
            return opcode, payload
        if opcode:
            first = opcode
        message += payload
        if len(message) > MAX_MESSAGE:
            raise ValueError("message too large")
        if b0 & 0x80:
            return first or 0x1, bytes(message)


class PageClient:
    """One WebSocket client on one page session."""

    def __init__(self, conn):
        self.conn = conn
        self.lock = threading.Lock()
        self.open = True

    def _send(self, frame):
        with self.lock:
            if not self.open:
                return
            try:
                self.conn.sendall(frame)
            except OSError:
                self.open = False

    def deliver(self, text):
        self._send(ws_frame(0x1, text.encode("utf-8")))

    def pong(self, payload):
        self._send(ws_frame(0xA, payload[:125]))

    def close(self):
        with self.lock:
            if not self.open:
                return
            self.open = False
            try:
                self.conn.sendall(ws_frame(0x8, b""))
            except OSError:
                pass
            try:
                self.conn.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


# ------------------------------------------------------------
# HTTP
# ------------------------------------------------------------

def read_head(conn):
    """(method, target, headers, what came after the head) of one HTTP request."""

    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            raise EOFError("closed")
        data += chunk
        if len(data) > MAX_HEAD:
            raise ValueError("request head too large")
    head, rest = data.split(b"\r\n\r\n", 1)
    lines = head.decode("latin-1").split("\r\n")
    parts = lines[0].split(" ")
    if len(parts) != 3:
        raise ValueError("not an HTTP request")
    headers = {}
    for line in lines[1:]:
        name, sep, value = line.partition(":")
        if sep:
            headers[name.strip().lower()] = value.strip()
    return parts[0].upper(), parts[1], headers, rest


def respond(conn, status, body, kind="application/json; charset=UTF-8"):
    if not isinstance(body, bytes):
        body = json.dumps(body, indent=1).encode("utf-8")
        kind = "application/json; charset=UTF-8"
    conn.sendall((f"HTTP/1.1 {status} {REASONS.get(status, 'Error')}\r\nContent-Type: {kind}\r\n"
                  f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n").encode("latin-1") + body)


def describe(info):
    """A target as /json/list describes it (what cdp.py reads: id, type, title, url, webSocketDebuggerUrl). Only a
    page gets a WebSocket address: the relay offers no other."""

    target_id = info.get("targetId") or ""
    out = {"description": "", "id": target_id, "title": info.get("title") or "", "type": info.get("type") or "",
           "url": info.get("url") or ""}
    if out["type"] == "page" and re.fullmatch(TARGET_ID, target_id):
        out["webSocketDebuggerUrl"] = f"ws://localhost/devtools/page/{target_id}"
    return out


def same_user(conn):
    """Whether the peer of `conn` is this process's user (or the system cannot say: the folder is the guard then)."""

    uid = gui_client.peer_uid(conn)
    return uid is None or uid == os.getuid()


class Relay:
    """Serves connections for one browser."""

    def __init__(self, browser, peer_ok=same_user):
        self.browser, self.peer_ok = browser, peer_ok

    def handle(self, conn):
        """Serve one connection to its end, then close it."""

        try:
            if not self.peer_ok(conn):
                return
            try:
                method, target, headers, rest = read_head(conn)
            except (EOFError, ValueError):
                return
            path, _, query = target.partition("?")
            if headers.get("upgrade", "").lower() == "websocket":
                found = PAGE_PATH.fullmatch(path)
                if method != "GET" or not found or not headers.get("sec-websocket-key"):
                    respond(conn, 404, b"No such page\n", "text/plain")
                    return
                self.page(conn, found.group(1), headers["sec-websocket-key"], rest)
                return
            try:
                status, body = self.route(method, path, query)
            except RelayError as exc:
                status = 503 if self.browser.closed.is_set() else 500
                body = (str(exc) + "\n").encode("utf-8")
            respond(conn, status, body, "text/plain")
        except OSError:
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def route(self, method, path, query):
        """(status, body) of a /json request."""

        if self.browser.closed.is_set():
            return 503, b"the browser is not running\n"
        if path == "/json/version":
            if method != "GET":
                return 405, b"GET only\n"
            version = self.browser.call("Browser.getVersion")
            return 200, {"Browser": version.get("product", ""), "Protocol-Version": version.get("protocolVersion", ""),
                         "User-Agent": version.get("userAgent", ""), "V8-Version": version.get("jsVersion", ""),
                         "WebKit-Version": version.get("revision", "")}
        if path in ("/json", "/json/list"):
            if method != "GET":
                return 405, b"GET only\n"
            infos = self.browser.call("Target.getTargets").get("targetInfos") or []
            return 200, [describe(info) for info in infos if isinstance(info, dict)]
        if path == "/json/new":
            if method != "PUT":
                return 405, b"/json/new takes PUT only\n"
            url = urllib.parse.unquote(query) or "about:blank"
            target_id = self.browser.call("Target.createTarget", {"url": url}).get("targetId")
            info = self.browser.call("Target.getTargetInfo", {"targetId": target_id}).get("targetInfo") or {}
            return 200, describe(info)
        found = TARGET_PATH.fullmatch(path)
        if found:
            verb, target_id = found.groups()
            try:
                if verb == "activate":
                    self.browser.call("Target.activateTarget", {"targetId": target_id})
                    return 200, b"Target activated"
                self.browser.call("Target.closeTarget", {"targetId": target_id})
                return 200, b"Target is closing"
            except RelayError:
                if self.browser.closed.is_set():
                    raise
                return 404, ("No such target id: " + target_id + "\n").encode("utf-8")
        return 404, b"Not found\n"

    def page(self, conn, target_id, key, rest):
        """A WebSocket on one page: its own session, until either side closes."""

        try:
            info = self.browser.call("Target.getTargetInfo", {"targetId": target_id}).get("targetInfo") or {}
            if info.get("type") != "page":
                respond(conn, 404, b"No such page\n", "text/plain")
                return
            session_id = self.browser.call("Target.attachToTarget",
                                           {"targetId": target_id, "flatten": True}).get("sessionId")
        except RelayError as exc:
            respond(conn, 503 if self.browser.closed.is_set() else 404, (str(exc) + "\n").encode("utf-8"),
                    "text/plain")
            return
        if not session_id:
            respond(conn, 500, b"the browser opened no session\n", "text/plain")
            return
        client = PageClient(conn)
        try:
            # The session's messages reach the client only once it has its 101: it sends nothing before then, so
            # the browser has nothing of its to answer.
            conn.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                          f"Sec-WebSocket-Accept: {accept_key(key)}\r\n\r\n").encode("latin-1"))
            self.browser.attach(session_id, client)
            self._pump(_Reader(conn, rest), session_id, client)
        finally:
            self.browser.detach(session_id)
            client.close()
            if not self.browser.closed.is_set():
                try:
                    self.browser.call("Target.detachFromTarget", {"sessionId": session_id}, timeout=2)
                except RelayError:
                    pass

    def _pump(self, reader, session_id, client):
        while client.open:
            try:
                opcode, payload = read_message(reader)
            except (EOFError, OSError, ValueError):
                return
            if opcode == 0x8:
                return
            if opcode == 0x9:
                client.pong(payload)
                continue
            if opcode not in (0x1, 0x2):
                continue
            try:
                message = json.loads(payload.decode("utf-8"))
            except (ValueError, UnicodeDecodeError):
                continue
            if not isinstance(message, dict) or not isinstance(message.get("method"), str) or "id" not in message:
                continue
            # This page's session and no other, whatever the client wrote.
            message["sessionId"] = session_id
            try:
                self.browser.send(message)
            except OSError:
                return

    def serve(self, listener):
        """Accept until the listener is closed; each connection on a thread of its own."""

        while True:
            try:
                conn, _ = listener.accept()
            except OSError:
                return
            conn.settimeout(None)
            threading.Thread(target=self.handle, args=(conn,), daemon=True, name="cdp-relay-conn").start()


# ------------------------------------------------------------
# Start
# ------------------------------------------------------------

def _high(fd):
    """`fd` moved above the browser's 3 and 4, close-on-exec (only the browser's dup2'd copies are inherited)."""

    import fcntl

    moved = fcntl.fcntl(fd, fcntl.F_DUPFD_CLOEXEC, 10)
    os.close(fd)
    return moved


def spawn_browser(argv, env):
    """Start `argv` with the pipe on its fds 3 (it reads) and 4 (it writes). Returns (pid, write_fd, read_fd): the
    relay's ends."""

    to_browser_r, to_browser_w = (_high(fd) for fd in os.pipe())
    from_browser_r, from_browser_w = (_high(fd) for fd in os.pipe())
    try:
        pid = os.posix_spawnp(argv[0], argv, env, file_actions=[
            (os.POSIX_SPAWN_OPEN, 0, os.devnull, os.O_RDONLY, 0),
            (os.POSIX_SPAWN_DUP2, to_browser_r, BROWSER_READS),
            (os.POSIX_SPAWN_DUP2, from_browser_w, BROWSER_WRITES),
        ])
    except BaseException:
        for fd in (to_browser_r, to_browser_w, from_browser_r, from_browser_w):
            os.close(fd)
        raise
    os.close(to_browser_r)
    os.close(from_browser_w)
    return pid, to_browser_w, from_browser_r


def _hand_to_serving(path, pages):
    """Open each page in the browser a relay already serves (PUT /json/new). Whether one serves there."""

    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    probe.settimeout(2.0)
    try:
        probe.connect(path)
    except OSError:
        probe.close()
        return False
    probe.close()
    for page in pages:
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(CALL_SECONDS)
        try:
            conn.connect(path)
            conn.sendall(("PUT /json/new?" + urllib.parse.quote(page, safe="") + " HTTP/1.1\r\nHost: localhost\r\n"
                          "Content-Length: 0\r\n\r\n").encode("latin-1"))
            while conn.recv(65536):
                pass
        except OSError:
            pass
        finally:
            conn.close()
    return True


# The (st_dev, st_ino) of each socket file this process bound, by its path (bind, _unlink).
_BOUND = {}


def bind(path):
    """Listen at `path`, in a folder of this user's alone (made 0700). None when the folder is not that."""

    directory = os.path.dirname(path)
    try:
        os.mkdir(directory, 0o700)
    except FileExistsError:
        pass
    if not gui_client.owned_dir(directory):
        sys.stderr.write(f"cdp relay: {directory} is not a folder of this user only; not serving\n")
        return None
    temp = f"{path}.{os.getpid()}"
    try:
        os.unlink(temp)
    except FileNotFoundError:
        pass
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(temp)
    os.chmod(temp, 0o600)
    listener.listen(32)
    # The file's own identity (_unlink): fstat of the listener is its inode in sockfs on Linux, never the file's.
    bound = os.stat(temp)
    _BOUND[path] = (bound.st_dev, bound.st_ino)
    os.rename(temp, path)
    return listener


def main(argv=None):
    parser = argparse.ArgumentParser(prog="decurix_agent.computer.cdp_relay")
    parser.add_argument("--socket", default=None)
    parser.add_argument("browser", nargs=argparse.REMAINDER)
    args = parser.parse_args(sys.argv[1:] if argv is None else argv)
    command = args.browser[1:] if args.browser[:1] == ["--"] else args.browser
    if not command:
        parser.error("name the browser to start after --")
    path = args.socket or broker.relay_socket() or os.path.join(broker.screen_dir(), "cdp", "cdp.sock")

    pages = [a for a in command[1:] if not a.startswith("-")]
    if _hand_to_serving(path, pages):
        return 0
    listener = bind(path)
    if listener is None:
        return 1
    env = dict(os.environ)
    env[PIPE_ENV[0]] = PIPE_ENV[1]
    try:
        pid, write_fd, read_fd = spawn_browser(command, env)
    except OSError as exc:
        sys.stderr.write(f"cdp relay: could not start the browser: {exc}\n")
        _unlink(path, listener)
        listener.close()
        return 1
    browser = Browser(write_fd, read_fd).start()
    threading.Thread(target=Relay(browser).serve, args=(listener,), daemon=True, name="cdp-relay-accept").start()
    browser.closed.wait()
    _unlink(path, listener)
    listener.close()
    _, status = os.waitpid(pid, 0)
    return os.waitstatus_to_exitcode(status) if hasattr(os, "waitstatus_to_exitcode") else 0


def _unlink(path, listener):
    """Take the socket away, only if it is still this relay's."""

    try:
        there = os.stat(path)
        if (there.st_dev, there.st_ino) == _BOUND.get(path):
            os.unlink(path)
    except (OSError, ValueError):
        pass


if __name__ == "__main__":
    code = main()
    sys.exit(code if isinstance(code, int) and code >= 0 else 1)
