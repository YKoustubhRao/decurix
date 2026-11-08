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
  - What a reply settled is taken once by its request's id (TheBot, whose paused job resumes on it) and once by its
    text (the agent's watcher, which only notes it): neither take makes or moves a grant, and neither hides the reply
    from the other. Found live (2026-11-08): the watcher, which reads every reply first, took TheBot's own yes; TheBot
    waited out its 45 seconds, resumed on "unclear", and the person was asked again.
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

from decurix_agent.computer import approval, privacy

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
# A refusal's reason: fixed, so it carries none of the action's words.
REFUSED_REASON = ("the gate could not check this action's words for secrets, so it was refused without asking anyone "
                  "(no reply can allow it): do not try it again - do it another way, or report that it could not be "
                  "done")

_lock = threading.Condition()
_nonces = {}   # nonce -> expiry: one use each
_settled = []  # [{"text", "request_id", "outcome", "reading", "at", "taken_id", "taken_text"}]
_key = {"value": ""}


def log(message):
    sys.stderr.write(time.strftime("%H:%M:%S ") + message + "\n")
    sys.stderr.flush()


def _norm(text):
    return " ".join(str(text or "").split())


def _sha(text):
    # Hash the JSON-decoded string as received. The control plane signs text in the form encoding/json sends it (each
    # byte that is not valid UTF-8 as U+FFFD, desktop.GateText), so the decoded string is what was signed; never
    # re-encode or normalise it before hashing (H-CP-7). verify_request, verify_secrets and verify (a reply) rest on it.
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


def sign_request(key, task_id, ts, nonce, text):
    """The signature the control plane puts on a request it sends the gate (/request): its own label ("request"), so a
    signed request can never stand as a signed reply, nor a reply as a request."""

    return _mac(key, ("request", task_id, ts, nonce, _sha(text)))


def verify_request(key, data, now=None):
    """Whether `data` (a parsed /request body) is a request the control plane signed, fresh and not seen before:
    a reason it is refused, or ""."""

    if not key:
        return "this gate has no key"
    try:
        task_id, ts, nonce, text, sig = (str(data["task_id"]), str(data["ts"]), str(data["nonce"]), str(data["text"]),
                                         str(data["sig"]))
    except (KeyError, TypeError):
        return "incomplete"
    if not hmac.compare_digest(sign_request(key, task_id, ts, nonce, text).encode("utf-8"), sig.encode("utf-8")):
        return "bad signature"
    return _fresh(ts, nonce, now)


def sign_secrets(key, task_id, ts, nonce, values):
    """The control plane's signature on a task's secret values (/secrets): its own label, so it stands for nothing
    else; the values are signed as the SHA-256 of each one's own SHA-256, NUL-terminated, in order (no JSON to match
    across languages)."""

    return _mac(key, ("secrets", task_id, ts, nonce, _sha("".join(_sha(v) + "\0" for v in values))))


def sign_ended(key, task_id, ts, nonce):
    """The control plane's signature on "this task has ended" (/task-ended, desktop.SignGateTaskEnded, H-CP-11): its
    own label, so it stands for nothing else. The gate then lets go of the task's secret values and its request copy
    (review round 3, R2-B2)."""

    return _mac(key, ("task-ended", task_id, ts, nonce))


def verify_ended(key, data, now=None):
    """Whether `data` (a parsed /task-ended body) is the control plane's, fresh and not seen before: why not, or ""."""

    if not key:
        return "this gate has no key"
    try:
        task_id, ts, nonce, sig = str(data["task_id"]), str(data["ts"]), str(data["nonce"]), str(data["sig"])
    except (KeyError, TypeError):
        return "incomplete"
    if not hmac.compare_digest(sign_ended(key, task_id, ts, nonce).encode("utf-8"), sig.encode("utf-8")):
        return "bad signature"
    return _fresh(ts, nonce, now)


def verify_secrets(key, data, now=None):
    """Whether `data` (a parsed /secrets body) is the control plane's, fresh and not seen before: why not, or ""."""

    if not key:
        return "this gate has no key"
    try:
        task_id, ts, nonce, sig = str(data["task_id"]), str(data["ts"]), str(data["nonce"]), str(data["sig"])
        values = data["secrets"]
    except (KeyError, TypeError):
        return "incomplete"
    if not isinstance(values, list) or not all(isinstance(v, str) for v in values):
        return "incomplete"
    if not hmac.compare_digest(sign_secrets(key, task_id, ts, nonce, values).encode("utf-8"), sig.encode("utf-8")):
        return "bad signature"
    return _fresh(ts, nonce, now)


# The tasks whose secret values the gate keeps: every task the control plane has not said is over (/task-ended). The
# gate cannot tell an open task from an ended one by counting requests - a task waiting on a person while newer requests
# queue behind it, a long request taking amendments - so a count let an open task's values go and its command reached
# the question in the clear (review round 3, R2-B2). Until the control plane says a task ended, its values are kept
# for SECRET_KEEP_SECONDS after they last came, and the store is held under SECRET_STORE_BYTES (oldest tasks first).
# Keeping too much only withholds more: the safe direction.
SECRET_KEEP_SECONDS = 24 * 3600
SECRET_STORE_BYTES = 4 * 1024 * 1024
SECRET_VALUES = 1000
_store_lock = threading.Lock()


class _locked_store:
    """The gate's stores, one writer at a time: a lock for this process's threads, and a file lock beside them."""

    def __enter__(self):
        from decurix_agent import filelock
        _store_lock.acquire()
        try:
            self.handle = open(str(approval.gate_secrets_path()) + ".lock", "a+")
            filelock.exclusive(self.handle)
        except BaseException:
            _store_lock.release()
            raise
        return self

    def __exit__(self, *exc):
        try:
            self.handle.close()
        finally:
            _store_lock.release()


def _write_private(path, data):
    """Write the gate's own file whole: a temporary file of this writer's, mode 0600, flushed, then put in place."""

    import tempfile
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(json.dumps(data, ensure_ascii=False))
            f.flush()
            os.fsync(f.fileno())
        try:
            os.chmod(tmp, 0o600)
        except OSError:
            pass
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


class StoreUnreadable(OSError):
    """A store of the gate's that is there and cannot be read: it is kept as it is, never written over."""


def _read_store(path):
    """A store of the gate's as a dict: {} when it is not there. One that is there and cannot be read raises
    StoreUnreadable - writing over it would drop what it holds (review round 3: an unreadable store was wiped)."""

    try:
        text = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return {}
    except OSError as exc:
        raise StoreUnreadable(f"{path.name} cannot be read: {exc}") from exc
    try:
        data = json.loads(text)
    except ValueError as exc:
        raise StoreUnreadable(f"{path.name} is not valid JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise StoreUnreadable(f"{path.name} is not an object")
    return data


def _secret_tasks(data, now=None):
    """{task: {"values", "at"}} of a secrets store as read (the round-2 shape, {task: [values]}, is read too), less the
    tasks whose values came more than SECRET_KEEP_SECONDS ago, and held under SECRET_STORE_BYTES, oldest first out."""

    now = time.time() if now is None else now
    tasks = data.get("tasks") if isinstance(data.get("tasks"), dict) else {
        t: {"values": v, "at": now} for t, v in data.items() if isinstance(v, list)}
    out = {}
    for task, entry in tasks.items():
        if not isinstance(entry, dict) or not isinstance(entry.get("values"), list):
            continue
        at = entry.get("at") if isinstance(entry.get("at"), (int, float)) else now
        if now - at <= SECRET_KEEP_SECONDS:
            out[str(task)] = {"values": [str(v) for v in entry["values"]], "at": at}
    while out and len(json.dumps({"tasks": out}, ensure_ascii=False).encode("utf-8")) > SECRET_STORE_BYTES:
        out.pop(min(out, key=lambda t: out[t]["at"]))
    return out


def keep_secrets(task_id, values, now=None):
    """Add a task's signed secret values to the gate's own copy (approval.shown_by_gate withholds them). Read, merge
    and write under one lock: two posts at once both keep their values (review round 2, B5)."""

    path = approval.gate_secrets_path()
    now = time.time() if now is None else now
    with _locked_store():
        tasks = _secret_tasks(_read_store(path), now)
        kept = (tasks.pop(task_id, None) or {}).get("values", [])
        kept = list(dict.fromkeys(kept + [v.strip() for v in values if v.strip()]))[-SECRET_VALUES:]
        tasks[task_id] = {"values": kept, "at": now}
        _write_private(path, {"tasks": _secret_tasks({"tasks": tasks}, now)})


def end_task(task_id, now=None):
    """The control plane says the task ended (/task-ended): its secret values are let go, and so are any past their
    age; the request copy goes too when it is that task's. A task the gate holds nothing for is no error. The age and
    size bounds stay as the backstop: the control plane keeps notices it could not deliver in memory only."""

    path = approval.gate_secrets_path()
    with _locked_store():
        tasks = _secret_tasks(_read_store(path), now)
        tasks.pop(str(task_id), None)
        _write_private(path, {"tasks": tasks})
        copy = approval.gate_request_path()
        try:
            if str(_read_store(copy).get("task_id") or "") == str(task_id):
                copy.unlink()
        except FileNotFoundError:
            pass
        except StoreUnreadable as exc:
            log(f"the request copy is kept: {exc}")


def keep_request(task_id, text):
    """Keep the latest signed request as the gate's own copy (approval.gate_request_text): what a site the request
    names is read from, never the worker's task file. The tasks' secret values are not let go here: a new request
    says nothing of whether an older task is over (review round 3, R2-B2)."""

    with _locked_store():
        path = approval.gate_request_path()
        try:
            previous = _read_store(path).get("task_id") or ""
        except StoreUnreadable as exc:
            log(f"the request copy is written anew: {exc}")  # (it holds no secret; the next request replaces it)
            previous = ""
        _write_private(path, {"task_id": task_id, "text": text, "at": time.time(), "previous": previous})


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
    if not hmac.compare_digest(sign(key, task_id, ts, nonce, text, asked, kind, request_id).encode("utf-8"),
                               sig.encode("utf-8")):
        return "bad signature"
    return _fresh(ts, nonce, now)


def _fresh(ts, nonce, now=None):
    """"" when a signed body's time is within SKEW_SECONDS and its nonce unused (now used up), else why not."""

    now = time.time() if now is None else now
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
        path, floor = _nonce_store["path"], _nonce_store["floor"]
        if path is None:
            return ""
        if sent <= floor:
            # The store could not be read at start: what was seen before it is unknown, so a body signed before the
            # gate started may be a replay (fail closed; a body signed since is decided as ever).
            return "replayed"
        try:
            _write_private(path, {"nonces": dict(_nonces)})
        except OSError as exc:
            log(f"the seen nonces could not be kept ({exc}): the signed body is refused")
            return "nonce not kept"
    return ""


# The nonces the gate has seen, kept in its own directory (0700, files 0600, written whole) so a restart does not forget
# them: a body signed less than SKEW_SECONDS before a restart was otherwise accepted again after it. Set by main() (in
# the gate's process only); with no path they are kept in memory alone (a test of the signatures).
NONCES_FILE = "gate-nonces.json"
_nonce_store = {"path": None, "floor": 0.0}


def use_nonce_store(path, now=None):
    """Keep the seen nonces at `path` from now on, and take in those it holds that are not past their expiry. One that
    is there and cannot be read is written over at the next nonce, and every body signed before now is refused: what
    it held is unknown."""

    from pathlib import Path
    now = time.time() if now is None else now
    path = Path(path)
    floor = 0.0
    try:
        held = _read_store(path).get("nonces")
        held = held if isinstance(held, dict) else {}
    except StoreUnreadable as exc:
        log(f"the seen nonces are unknown ({exc}): bodies signed before this start are refused")
        held, floor = {}, now
    with _lock:
        for nonce, until in held.items():
            if isinstance(until, (int, float)) and until >= now:
                _nonces[str(nonce)] = max(float(until), _nonces.get(str(nonce), 0.0))
        _nonce_store["path"], _nonce_store["floor"] = path, floor


def _record(text, request_id, outcome, reading):
    with _lock:
        now = time.time()
        _settled[:] = [o for o in _settled if now - o["at"] < OUTCOME_KEEP_SECONDS]
        _settled.append({"text": _norm(text), "request_id": request_id, "outcome": outcome, "reading": reading,
                         "at": now, "taken_id": False, "taken_text": False})
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
    What one signed reply settled - (seen, outcome, reading) - taken once by request id (TheBot keeps the id of the
    request it paused on) and once by the reply's text (the agent's watcher, which received the same text): a take of
    one kind never hides the reply from the other (the module's note). Waits up to `wait`
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
                flag = "taken_id" if request_id else "taken_text"
                if o[flag] or time.time() - o["at"] >= OUTCOME_KEEP_SECONDS:
                    continue
                if (request_id and o["request_id"] == request_id) or (not request_id and text and o["text"] == text):
                    o[flag] = True
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
        if privacy.HELD_BACK in (description, about):
            # Words that could not be checked for secrets - any part of them: the command, the typed text, what it
            # acts on (approval.from_material) - are never shown, and a person is never asked to approve an action
            # they cannot see: it is refused, a no to this one action that waits on nobody (review round 3, R2-B3).
            # "refused" tells soul to resume at once (H-SOUL-8).
            return dict(decision_json(approval.Decision(False, reason=REFUSED_REASON)), refused=True,
                        reason=REFUSED_REASON)
        # The gate's own reason, or a fixed one: the caller's reason is the worker's words, and these words are the
        # gate's signed question (round-4 recheck: "it only saves a draft" could stand in the gate's voice). The
        # worker's text still reaches the person, as the worker's note.
        approval._gate_task.id = str(material.get("task_id") or "")
        try:
            reason = approval.reason_from_material(material) or GATE_REASON
        finally:
            approval._gate_task.id = ""
        return decision_json(approval._decide_local(fingerprint, description, reason, element, about))
    if op == "site_known":
        # From the gate's own copy of the signed request only (approval.site_known in the gate).
        return {"known": approval.site_known(str(call.get("host") or ""), task_id=str(call.get("task_id") or ""),
                                             url=str(call.get("url") or "") or None)}
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
        if self.path not in ("/reply", "/request", "/secrets", "/task-ended") or self.headers.get("Origin") is not None:
            return self._send(404)
        try:
            length = int(self.headers.get("Content-Length") or "0")
            if not 0 < length <= MAX_BODY:
                return self._send(400)
            data = json.loads(self.rfile.read(length).decode("utf-8"))
        except (ValueError, OSError):
            return self._send(400)
        data = data if isinstance(data, dict) else {}
        if self.path == "/secrets":
            why = verify_secrets(self.key, data)
            if why:
                log(f"secret values were refused: {why}")
                return self._send(403)
            try:
                keep_secrets(str(data["task_id"]), data["secrets"])
            except OSError as exc:
                log(f"could not keep the secret values: {exc}")
                return self._send(500)
            return self._send(204)
        if self.path == "/task-ended":
            why = verify_ended(self.key, data)
            if why:
                log(f"a task's end was refused: {why}")
                return self._send(403)
            try:
                end_task(str(data["task_id"]))
            except OSError as exc:
                log(f"could not let go of the task's secret values: {exc}")
                return self._send(500)
            return self._send(204)
        if self.path == "/request":
            why = verify_request(self.key, data)
            if why:
                log(f"a request was refused: {why}")
                return self._send(403)
            try:
                keep_request(str(data["task_id"]), str(data["text"]))
            except OSError as exc:
                log(f"could not keep the request: {exc}")
                return self._send(500)
            return self._send(204)
        why = verify(self.key, data)
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
    use_nonce_store(os.path.join(os.environ.get("DECURIX_DIR") or "/var/lib/decurix-gate", NONCES_FILE))
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
