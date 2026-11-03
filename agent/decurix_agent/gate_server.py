"""
The approval gate as a process of its own (docs/gate-boundary.md).

Run by the container's entrypoint as the `decurixgate` user, with state in a directory only that user can read
(DECURIX_DIR=/var/lib/decurix-gate) and its key in a file there (`key`, 0400). It holds what makes a yes: the pending
approval, the one-shot grants. Two doors:

  - TCP :8083, POST /reply: the control plane's signed copy of a person's reply (internal/desktop/gatereply.go). The
    signature is an HMAC with the desktop's gate key, which only this process and the control plane hold. This is the
    only way a grant comes to exist.
  - A unix socket for the agent, `gui` and TheBot (computer/approval.py, cmd/soul/gate.go): check an action (a grant
    that exists is consumed), see what is pending, ask what was done with a reply, withdraw grants. None of these makes
    a grant, so the worker, who can reach the socket, can ask but not approve.

A yes is bound to one action, not to wording (round-3 recheck):
  - The gate makes an action's fingerprint and its words from the same description of the action (`material`), so a
    caller cannot have one action's grant asked for in another's words. A pending action keeps the words it was first
    asked in.
  - The gate words the question itself and signs it (`sign_question`). The control plane shows a person those words and
    signs their reply with that request's id; the reply settles that request only, read against the gate's words, never
    against text the worker wrote.
  - What a reply settled is taken once, by whoever asks first (the agent's watcher or TheBot), never twice.
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

from decurix_agent.computer import approval

MAX_BODY = 64 * 1024
# A check describes the action with the page's controls beside it (approval._for_gate cuts them short).
MAX_CALL = 512 * 1024
# A signed reply older than this (or from the future) is refused; the control plane signs it as it sends it.
SKEW_SECONDS = 120
# How long a settled reply waits for the agent to ask what became of it.
OUTCOME_KEEP_SECONDS = 600
# At most this many calls at once on each door: a flood waits, it does not grow threads without end.
MAX_CALLS = 32
KEY_FILE = "key"
# The reason in the gate's question when it finds none of its own in the action (never the caller's words).
GATE_REASON = "it needs your approval"

_lock = threading.Condition()
_nonces = {}   # nonce -> expiry: one use each
_settled = []  # [{"text", "request_id", "outcome", "reading", "at", "taken"}]
_key = {"value": ""}


def log(message):
    sys.stderr.write(time.strftime("%H:%M:%S ") + message + "\n")
    sys.stderr.flush()


def _norm(text):
    return " ".join(str(text or "").split())


def _sha(text):
    return hashlib.sha256(str(text or "").encode("utf-8")).hexdigest()


def _mac(key, parts):
    mac = hmac.new(key.encode("utf-8"), digestmod=hashlib.sha256)
    for part in parts:
        mac.update(str(part).encode("utf-8"))
        mac.update(b"\x00")
    return mac.hexdigest()


def sign(key, task_id, ts, nonce, text, asked, kind, request_id=""):
    """The signature the control plane puts on a reply (desktop.SignGateReply): the same parts, NUL-terminated."""

    return _mac(key, ("reply", task_id, ts, nonce, _sha(text), _sha(asked), kind, request_id))


def sign_question(key, request_id, question):
    """The gate's signature on its own question (desktop.VerifyGateQuestion): the control plane shows a person these
    words, and signs their reply with this request id, only when this verifies."""

    return _mac(key, ("question", request_id, _sha(_norm(question))))


def verify(key, data, now=None):
    """Whether `data` (a parsed /reply body) is a reply the control plane signed, fresh and not seen before. Returns
    a reason it is refused, or "" when it is genuine (and its nonce is used up)."""

    if not key:
        return "this gate has no key"
    try:
        task_id, ts, nonce = str(data["task_id"]), str(data["ts"]), str(data["nonce"])
        text, asked, kind, sig = str(data["text"]), str(data.get("asked", "")), str(data.get("kind", "")), str(data["sig"])
        request_id = str(data.get("request_id", ""))
    except (KeyError, TypeError):
        return "incomplete"
    now = time.time() if now is None else now
    if not hmac.compare_digest(sign(key, task_id, ts, nonce, text, asked, kind, request_id).encode("utf-8"),
                               sig.encode("utf-8")):
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


def _record(text, request_id, outcome, reading):
    with _lock:
        now = time.time()
        _settled[:] = [o for o in _settled if now - o["at"] < OUTCOME_KEEP_SECONDS]
        _settled.append({"text": _norm(text), "request_id": request_id, "outcome": outcome, "reading": reading,
                         "at": now, "taken": False})
        _lock.notify_all()


def settle(text, asked, kind, request_id=""):
    """
    A genuine reply. It settles the pending approval only when it answers that very request (the id the control plane
    signed) as the gate worded it; anything else is a reply to a question, and grants nothing.
    """

    outcome, reading = None, None
    try:
        pending = approval._local_pending()
        if kind != "approval" or not request_id:
            why = "not an answer to a question of the gate's"
        elif not pending or pending.get("id") != request_id:
            why = "the request it answers is no longer pending"
        elif _norm(asked) != _norm(approval.request_text(pending)):
            why = "the person was not shown the gate's own words"
        else:
            why = ""
            outcome = approval._local_resolve_reply(text, asked=approval.request_text(pending))
            reading = approval._local_last_reading(text)
        if why:
            log(f"a signed reply settled nothing: {why}")
    except Exception as exc:  # noqa: BLE001 - a reply that could not be settled grants nothing
        log(f"could not settle a reply: {type(exc).__name__}: {exc}")
        outcome, reading = None, None
    _record(text, request_id, outcome, reading)
    log(f"a signed reply was settled: {outcome or 'nothing pending'}")


def take(wait, text=None, request_id=None):
    """
    What one signed reply settled - (seen, outcome, reading) - taken once: by request id (TheBot keeps the id of the
    request it paused on) or by the reply's text (the agent's watcher, which received the same text). Waits up to `wait`
    seconds for the control plane's copy to arrive and be settled. (False, None, None) when no signed copy came: that
    reply settled nothing.
    """

    text = _norm(text) if text is not None else None
    deadline = time.monotonic() + max(0, min(float(wait or 0), 120))
    with _lock:
        while True:
            # Newest first: the control plane signs a reply just before it delivers it, so an older "yes" (to another
            # question, never asked about) is not this one.
            for o in reversed(_settled):
                if o["taken"] or time.time() - o["at"] >= OUTCOME_KEEP_SECONDS:
                    continue
                if (request_id and o["request_id"] == request_id) or (not request_id and text and o["text"] == text):
                    o["taken"] = True
                    return True, o["outcome"], o["reading"]
            left = deadline - time.monotonic()
            if left <= 0:
                return False, None, None
            _lock.wait(left)


def with_question(request):
    """A pending request as the gate gives it out: with its own words and its signature on them."""

    if not request:
        return request
    out = dict(request)
    out["question"] = approval.request_text(request)
    out["sig"] = sign_question(_key["value"], out.get("id", ""), out["question"])
    return out


def decision_json(decision):
    return {"allowed": bool(decision.allowed), "approved": bool(decision.approved),
            "request": with_question(decision.request), "by_reply": decision.by_reply or ""}


def handle(call):
    """One call on the unix socket. Nothing here makes a grant."""

    op = call.get("op")
    if op == "check":
        material = call.get("material")
        if not isinstance(material, dict):
            return {"error": "no material"}
        fingerprint, description, element, about = approval.from_material(material)
        # The gate's own reason, or a fixed one: the caller's reason is the worker's words, and these words are the
        # gate's signed question (round-4 recheck: "it only saves a draft" could stand in the gate's voice). The
        # worker's text still reaches the person, as the worker's note.
        reason = approval.reason_from_material(material) or GATE_REASON
        return decision_json(approval._decide_local(fingerprint, description, reason, element, about))
    if op == "pending":
        return {"pending": with_question(approval._local_pending())}
    if op == "has_grant":
        return {"grant": approval._local_has_grant()}
    if op == "reset":
        approval._local_reset()
        return {}
    if op == "outcome":
        seen, outcome, _ = take(call.get("wait", 0), text=call.get("reply"))
        return {"seen": seen, "outcome": outcome}
    if op == "settled":
        # TheBot's paused command: the outcome and reading of the one reply to its own request. A yes is consumed here,
        # with the grant it made: TheBot runs the command itself, so the grant must not stand for anyone else.
        request_id = str(call.get("request_id") or "")
        if not request_id:
            return {"seen": False, "outcome": None, "reading": None}
        seen, outcome, reading = take(call.get("wait", 0), request_id=request_id)
        if outcome == "granted":
            approval._local_take_grant(str(call.get("fingerprint") or ""))
        return {"seen": seen, "outcome": outcome, "reading": reading}
    if op == "last_reading":
        return {"reading": approval._local_last_reading(call.get("reply"))}
    return {"error": "unknown call"}


class _Bounded:
    """At most MAX_CALLS requests in flight on a server; a request over that waits for a slot."""

    _slots = None

    def process_request(self, request, client_address):
        self._slots.acquire()
        try:
            super().process_request(request, client_address)
        except Exception:
            self._slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._slots.release()


class SocketHandler(socketserver.StreamRequestHandler):
    def handle(self):
        try:
            self.request.settimeout(150)
            line = self.rfile.readline(MAX_CALL)
            reply = handle(json.loads(line.decode("utf-8")))
        except Exception as exc:  # noqa: BLE001 - the caller fails closed on an error
            reply = {"error": type(exc).__name__}
        try:
            self.wfile.write((json.dumps(reply) + "\n").encode("utf-8"))
        except OSError:
            pass


if hasattr(socketserver, "UnixStreamServer"):  # (not on Windows, where the gate never runs but its logic is tested)
    class ThreadedUnix(_Bounded, socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True
        _slots = threading.BoundedSemaphore(MAX_CALLS)


class ThreadedHTTP(_Bounded, ThreadingHTTPServer):
    daemon_threads = True
    _slots = threading.BoundedSemaphore(MAX_CALLS)


class ReplyHandler(BaseHTTPRequestHandler):
    key = ""
    timeout = 10

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
        threading.Thread(target=settle, args=(str(data["text"]), str(data.get("asked", "")), str(data.get("kind", "")),
                                              str(data.get("request_id", ""))), daemon=True).start()
        return self._send(204)

    def do_GET(self):
        self._send(200 if self.path == "/health" else 404)


def read_key(directory):
    """The gate's key, from the file the entrypoint put in its own directory; "" when there is none."""

    try:
        with open(os.path.join(directory, KEY_FILE), encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


def main():
    approval.IN_GATE = True  # this process is the gate: its decisions are its own store's, never another gate's
    os.environ.pop("DECURIX_GATE_KEY", None)
    # The gate reads replies with the model (approval.read_choice): its calls reach the proxy as the gate's (C1).
    os.environ["DECURIX_CALLER"] = "gate"
    key = read_key(os.environ.get("DECURIX_DIR") or "/var/lib/decurix-gate")
    if not key:
        log("no key in the gate's directory: this desktop has no approval gate")
        return 1
    ReplyHandler.key = key
    _key["value"] = key
    sock_path = approval.GATE_SOCKET
    try:
        os.unlink(sock_path)
    except FileNotFoundError:
        pass
    unix = ThreadedUnix(sock_path, SocketHandler)
    os.chmod(sock_path, 0o666)  # the worker may ask; the calls above make no grant
    threading.Thread(target=unix.serve_forever, daemon=True).start()
    http = ThreadedHTTP(("0.0.0.0", 8083), ReplyHandler)
    log("approval gate up")
    http.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
