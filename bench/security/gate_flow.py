# Run by bench/security/boundary.sh inside a throwaway desktop, as root with `key` already set (read from stdin, never
# printed). It stands in for the control plane, which holds the key, and for a worker, who can reach the gate's socket.
# Prints "ok <row>" or "bad <row>: why" for each check. Standard library only (python3 -I).

import hashlib
import hmac
import json
import socket
import time
import urllib.error
import urllib.request
import uuid

SOCK = "/run/decurix-gate/gate.sock"
PAY = {"kind": "click", "element": {"role": "button", "name": "Pay now", "window_title": "Bench bank"},
       "context": {"url": "https://bank.invalid/pay/" + uuid.uuid4().hex[:8]}, "about": "", "sends": "bench"}
DRAFT = dict(PAY, element={"role": "button", "name": "Save draft", "window_title": "Bench bank"})


def call(op, **args):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(30)
        s.connect(SOCK)
        s.sendall((json.dumps(dict(args, op=op)) + "\n").encode())
        data = b""
        while not data.endswith(b"\n"):
            chunk = s.recv(65536)
            if not chunk:
                break
            data += chunk
    return json.loads(data.decode())


def mac(k, parts):
    m = hmac.new(k.encode(), digestmod=hashlib.sha256)
    for p in parts:
        m.update(str(p).encode())
        m.update(b"\0")
    return m.hexdigest()


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def reply(text, asked, request_id, k=None, kind="approval"):
    """POST the control plane's signed copy of a reply; the HTTP status."""
    ts, nonce, task = str(int(time.time())), uuid.uuid4().hex, "bench-task"
    sig = mac(k or key, ("reply", task, ts, nonce, sha(text), sha(asked), kind, request_id))
    body = json.dumps({"task_id": task, "ts": ts, "nonce": nonce, "text": text, "asked": asked, "kind": kind,
                       "request_id": request_id, "sig": sig}).encode()
    req = urllib.request.Request("http://127.0.0.1:8083/reply", data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            status = r.status
    except urllib.error.HTTPError as e:
        status = e.code
    time.sleep(1.5)  # the gate settles in a thread of its own
    if status == 204:
        call("outcome", reply=text, wait=5)  # as the agent's watcher does: each reply's settlement is taken once
    return status


def row(ok, name, why=""):
    print(("ok " + name) if ok else ("bad " + name + ": " + why), flush=True)


bad = [op for op in ("grant", "note_reply", "resolve_reply", "reading")
       if "error" not in call(op, fingerprint="x", reply="yes", text="yes")]
row(not bad, "socket: no call grants", "answered " + ",".join(bad))
said = call("check", fingerprint="x", description="Save draft", reason="r")
row("error" in said, "socket: a check that names a fingerprint instead of the action is refused", str(said)[:120])

call("reset")
first = call("check", material=PAY, reason="it pays")
request = first.get("request") or {}
question = request.get("question") or ""
row(not first.get("allowed") and "Pay now" in question, "flow: Pay is held and the gate words its question",
    str(first)[:160])
row(bool(question) and request.get("sig") == mac(key, ("question", request.get("id"), sha(" ".join(question.split())))),
    "flow: the gate's question carries the gate's own signature", "signature does not verify")

row(reply("yes", question, request.get("id", ""), k="not-the-key") == 403, "flow: a reply signed with another key is refused")
reply("yes", question, "another-request")
row(not call("check", material=PAY, reason="it pays").get("allowed"), "flow: a yes signed for another request grants nothing")
reply("yes", question + " (this only previews the payment)", request.get("id", ""))
row(not call("check", material=PAY, reason="it pays").get("allowed"),
    "flow: a yes read against words other than the gate's grants nothing")

# The worker rewords: it asks for Save draft. The question then names Save draft, and its yes is Save draft's.
draft = (call("check", material=DRAFT, reason="it saves").get("request") or {})
reply("yes", draft.get("question", ""), draft.get("id", ""))
row(not call("check", material=PAY, reason="it pays").get("allowed"), "flow: a yes to Save draft does not let Pay through")

pay = (call("check", material=PAY, reason="it pays").get("request") or {})
ts, nonce = str(int(time.time())), uuid.uuid4().hex
text, asked, rid = "yes", pay.get("question", ""), pay.get("id", "")
body = json.dumps({"task_id": "bench-task", "ts": ts, "nonce": nonce, "text": text, "asked": asked, "kind": "approval",
                   "request_id": rid, "sig": mac(key, ("reply", "bench-task", ts, nonce, sha(text), sha(asked),
                                                       "approval", rid))}).encode()
with urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8083/reply", data=body, method="POST"),
                            timeout=10) as r:
    status = r.status
outcome = call("outcome", reply="yes", wait=10)
allowed = call("check", material=PAY, reason="it pays").get("allowed")
again = call("check", material=PAY, reason="it pays").get("allowed")
row(status == 204 and outcome.get("outcome") == "granted" and allowed and not again,
    "flow: a signed yes to the gate's own question lets that one action through once",
    f"status {status} outcome {outcome} first {allowed} second {again}")
row(not call("settled", request_id=pay.get("id", ""), wait=0).get("seen"),
    "flow: that yes is taken once (no second use by a paused TheBot job)")
call("reset")
