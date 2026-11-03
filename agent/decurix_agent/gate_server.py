"""
The approval gate as a process of its own (docs/gate-boundary.md).

Run by the container's entrypoint as the `decurixgate` user, with state in a directory only that user can read
(DECURIX_DIR=/var/lib/decurix-gate). It holds what makes a yes: the pending approval, the one-shot grants and the
person's latest reply. Two doors:

  - TCP :8083, POST /reply: the control plane's signed copy of a person's reply (internal/desktop/gatereply.go). The
    signature is an HMAC with the desktop's gate key, which only this process and the control plane hold (the
    entrypoint removes it from every other process's environment). A reply that verifies is noted and settles the
    pending approval, exactly as the agent's command watcher used to; anything else is refused. This is the only way
    a grant comes to exist.
  - A unix socket for the agent and the `gui` command (computer/approval.py): check an action (a grant that exists is
    consumed), see what is pending, ask what was done with a reply, withdraw grants. None of these makes a grant, so
    the worker, who can reach the socket, can ask but not approve.

Everything here is in the standard library plus approval.py's own logic, run in its local mode (DECURIX_GATE_LOCAL).
"""

import hashlib
import hmac
import json
import os
import socketserver
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

os.environ["DECURIX_GATE_LOCAL"] = "1"

from decurix_agent.computer import approval  # noqa: E402

MAX_BODY = 64 * 1024
# A signed reply older than this (or from the future) is refused; the control plane signs it as it sends it.
SKEW_SECONDS = 120
# How long a settled reply waits for the agent to ask what became of it.
OUTCOME_KEEP_SECONDS = 600

_lock = threading.Condition()
_nonces = {}   # nonce -> expiry: one use each
_outcomes = []  # [{"text", "outcome", "at", "taken"}]


def log(message):
    sys.stderr.write(time.strftime("%H:%M:%S ") + message + "\n")
    sys.stderr.flush()


def _norm(text):
    return " ".join(str(text or "").split())


def sign(key, task_id, ts, nonce, text, asked, kind):
    """The signature the control plane puts on a reply (desktop.SignGateReply): the same parts, NUL-terminated."""

    mac = hmac.new(key.encode("utf-8"), digestmod=hashlib.sha256)
    for part in ("reply", task_id, ts, nonce, hashlib.sha256(text.encode("utf-8")).hexdigest(),
                 hashlib.sha256(asked.encode("utf-8")).hexdigest(), kind):
        mac.update(part.encode("utf-8"))
        mac.update(b"\x00")
    return mac.hexdigest()


def verify(key, data, now=None):
    """Whether `data` (a parsed /reply body) is a reply the control plane signed, fresh and not seen before. Returns
    a reason it is refused, or "" when it is genuine (and its nonce is used up)."""

    if not key:
        return "this gate has no key"
    try:
        task_id, ts, nonce = str(data["task_id"]), str(data["ts"]), str(data["nonce"])
        text, asked, kind, sig = str(data["text"]), str(data.get("asked", "")), str(data.get("kind", "")), str(data["sig"])
    except (KeyError, TypeError):
        return "incomplete"
    now = time.time() if now is None else now
    if not hmac.compare_digest(sign(key, task_id, ts, nonce, text, asked, kind).encode("utf-8"), sig.encode("utf-8")):
        return "bad signature"
    try:
        sent = float(ts)
    except ValueError:
        return "bad time"
    if abs(now - sent) > SKEW_SECONDS:
        return "stale"
    with _lock:
        for n in [n for n, until in _nonces.items() if until < now]:
            del _nonces[n]
        if nonce in _nonces:
            return "replayed"
        _nonces[nonce] = now + 2 * SKEW_SECONDS
    return ""


def settle(text, asked, kind):
    """A genuine reply: the latest reply for the next gated action, and the answer to the pending approval."""

    outcome = None
    try:
        approval.note_reply(text, asked=asked)
        outcome = approval.resolve_reply(text, asked=asked if kind == "approval" else "")
    except Exception as exc:  # noqa: BLE001 - a reply that could not be settled grants nothing
        log(f"could not settle a reply: {type(exc).__name__}: {exc}")
    # TheBot keeps its own approvals (a shell command it paused on), which this gate's store knows nothing of: it
    # asks for the reading of the person's reply here instead of accepting one in a request, which anything holding
    # the agent token could write.
    reading = None
    if kind == "approval":
        try:
            reading = approval.last_reading(text) or approval.reading(text, asked)
        except Exception as exc:  # noqa: BLE001 - no reading is no yes
            log(f"could not read a reply: {type(exc).__name__}: {exc}")
    with _lock:
        now = time.time()
        _outcomes[:] = [o for o in _outcomes if now - o["at"] < OUTCOME_KEEP_SECONDS]
        _outcomes.append({"text": _norm(text), "outcome": outcome, "at": now, "taken": False,
                          "reading": reading, "read": False})
        _lock.notify_all()
    log(f"a signed reply was settled: {outcome or 'nothing pending'}")


def take_outcome(text, wait):
    """What was done with a reply the agent has received: the outcome of the oldest signed copy of it not yet
    asked about, waiting up to `wait` seconds for the control plane's copy to arrive and be settled. (False, None)
    when no signed copy came: that reply settled nothing."""

    text = _norm(text)
    deadline = time.monotonic() + max(0, min(float(wait or 0), 120))
    with _lock:
        while True:
            for o in _outcomes:
                if not o["taken"] and o["text"] == text and time.time() - o["at"] < OUTCOME_KEEP_SECONDS:
                    o["taken"] = True
                    return True, o["outcome"]
            left = deadline - time.monotonic()
            if left <= 0:
                return False, None
            _lock.wait(left)


def take_reading(text, wait):
    """The reading made of a signed approval reply (decision, condition, what they want first), once, waiting up to
    `wait` seconds for the control plane's copy to arrive and be read. (False, None) when no signed copy came."""

    text = _norm(text)
    deadline = time.monotonic() + max(0, min(float(wait or 0), 120))
    with _lock:
        while True:
            for o in _outcomes:
                fresh = time.time() - o["at"] < OUTCOME_KEEP_SECONDS
                if fresh and not o["read"] and o["reading"] is not None and o["text"] == text:
                    o["read"] = True
                    return True, o["reading"]
            left = deadline - time.monotonic()
            if left <= 0:
                return False, None
            _lock.wait(left)


def decision_json(decision):
    return {"allowed": bool(decision.allowed), "approved": bool(decision.approved), "request": decision.request,
            "by_reply": decision.by_reply or ""}


def handle(call):
    """One call on the unix socket. Nothing here makes a grant."""

    op = call.get("op")
    if op == "check":
        decision = approval._decide_local(
            str(call["fingerprint"]), str(call["description"]), str(call["reason"]),
            {"name": str(call.get("control") or "")}, str(call.get("about") or ""))
        return decision_json(decision)
    if op == "pending":
        return {"pending": approval.pending()}
    if op == "has_grant":
        return {"grant": approval.has_grant()}
    if op == "reset":
        approval.reset()
        return {}
    if op == "outcome":
        seen, outcome = take_outcome(call.get("reply"), call.get("wait", 0))
        return {"seen": seen, "outcome": outcome}
    if op == "reading":
        seen, reading = take_reading(call.get("reply"), call.get("wait", 0))
        return {"seen": seen, "reading": reading}
    if op == "last_reading":
        return {"reading": approval.last_reading(call.get("reply"))}
    return {"error": "unknown call"}


class SocketHandler(socketserver.StreamRequestHandler):
    def handle(self):
        try:
            line = self.rfile.readline(MAX_BODY)
            reply = handle(json.loads(line.decode("utf-8")))
        except Exception as exc:  # noqa: BLE001 - the caller fails closed on an error
            reply = {"error": type(exc).__name__}
        try:
            self.wfile.write((json.dumps(reply) + "\n").encode("utf-8"))
        except OSError:
            pass


if hasattr(socketserver, "UnixStreamServer"):  # (not on Windows, where the gate never runs but its logic is tested)
    class ThreadedUnix(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True


class ReplyHandler(BaseHTTPRequestHandler):
    key = ""

    def log_message(self, format, *args):  # noqa: A002
        pass

    def _send(self, status, body=b""):
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        # A web page cannot sign; a request that names an origin is not the control plane's.
        if self.path != "/reply" or self.headers.get("Origin") is not None:
            return self._send(404)
        try:
            length = int(self.headers.get("Content-Length") or "0")
            if not 0 < length <= MAX_BODY:
                return self._send(400)
            data = json.loads(self.rfile.read(length).decode("utf-8"))
        except (ValueError, OSError):
            return self._send(400)
        why = verify(self.key, data if isinstance(data, dict) else {})
        if why:
            log(f"a reply was refused: {why}")
            return self._send(403)
        threading.Thread(target=settle, args=(str(data["text"]), str(data.get("asked", "")), str(data.get("kind", ""))),
                         daemon=True).start()
        return self._send(204)

    def do_GET(self):
        self._send(200 if self.path == "/health" else 404)


def main():
    key = os.environ.pop("DECURIX_GATE_KEY", "")
    if not key:
        log("DECURIX_GATE_KEY is not set: this desktop has no approval gate")
        return 1
    ReplyHandler.key = key
    sock_path = approval.GATE_SOCKET
    try:
        os.unlink(sock_path)
    except FileNotFoundError:
        pass
    unix = ThreadedUnix(sock_path, SocketHandler)
    os.chmod(sock_path, 0o666)  # the worker may ask; the calls above make no grant
    threading.Thread(target=unix.serve_forever, daemon=True).start()
    http = ThreadingHTTPServer(("0.0.0.0", 8083), ReplyHandler)
    log("approval gate up")
    http.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
