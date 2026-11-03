"""
Whether a password may go to a site is decided from data the worker's user cannot create (docs/gate-boundary.md;
review relay of 2026-11-04).

approval.site_known read the request from the task file in /tmp, and the skills and recordings from ~/.decurix: all
the worker's user's to write, so a command the worker ran could make any site "named". On a desktop with an approval
gate it now asks the gate, and the gate answers only from its own copy of the request the control plane signed
(POST /request on the gate's port, its own signature label). With no signed copy, no site is named: the password is
asked about. Input classes only: a tampered task file, a body signed for another purpose, a body with no signature.
"""

import json
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
import uuid
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config
from decurix_agent import gate_server as gate
from decurix_agent.computer import approval

KEY = "test-gate-key"
TASK = "t00000000000000f1"
NAMED = "Sign in at https://portal.example.test/login as mira.k"


def signed_request(text, task=TASK, key=KEY):
    ts, nonce = str(time.time()), uuid.uuid4().hex
    return {"task_id": task, "ts": ts, "nonce": nonce, "text": text, "sig": gate.sign_request(key, task, ts, nonce, text)}


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  # The worker's own task file names the site: what a command of the worker could write there.
                  mock.patch("decurix_agent.task_context.current_request", lambda: NAMED),
                  mock.patch.object(approval, "_taught_text", lambda: NAMED.lower())):
            p.start()
            self.addCleanup(p.stop)


class InTheGate(Base):
    def setUp(self):
        super().setUp()
        p = mock.patch.object(approval, "IN_GATE", True)
        p.start()
        self.addCleanup(p.stop)

    def test_the_worker_written_task_file_names_nothing(self):
        self.assertFalse(approval.site_known("portal.example.test"))
        self.assertEqual(gate.handle({"op": "site_known", "host": "portal.example.test"}), {"known": False})

    def test_the_signed_copy_names_the_site(self):
        gate.keep_request(TASK, NAMED)
        self.assertTrue(approval.site_known("portal.example.test", task_id=TASK))
        self.assertFalse(approval.site_known("portal.example.test.other.test", task_id=TASK))
        self.assertEqual(gate.handle({"op": "site_known", "host": "portal.example.test", "task_id": TASK}),
                         {"known": True})

    def test_a_copy_of_another_task_names_nothing(self):
        # Review round 2: a new task whose copy did not arrive read the last task's request, and its sites.
        gate.keep_request("t00000000000000e9", NAMED)
        self.assertFalse(approval.site_known("portal.example.test", task_id=TASK))
        self.assertFalse(approval.site_known("portal.example.test"))
        self.assertEqual(gate.handle({"op": "site_known", "host": "portal.example.test", "task_id": TASK}),
                         {"known": False})


class TheSignature(unittest.TestCase):
    def test_a_signed_request_verifies_once(self):
        body = signed_request(NAMED)
        self.assertEqual(gate.verify_request(KEY, body), "")
        self.assertEqual(gate.verify_request(KEY, body), "replayed")

    def test_unsigned_wrongly_signed_or_stale(self):
        self.assertEqual(gate.verify_request(KEY, {"task_id": TASK, "text": NAMED}), "incomplete")
        self.assertEqual(gate.verify_request(KEY, signed_request(NAMED, key="another key")), "bad signature")
        old = signed_request(NAMED)
        self.assertEqual(gate.verify_request(KEY, old, now=time.time() + 10 * gate.SKEW_SECONDS), "stale")

    def test_a_body_signed_for_one_purpose_never_stands_for_the_other(self):
        request = signed_request(NAMED)
        as_reply = dict(request, asked="", kind="", request_id="")
        self.assertEqual(gate.verify(KEY, as_reply), "bad signature")
        ts, nonce = str(time.time()), uuid.uuid4().hex
        reply = {"task_id": TASK, "ts": ts, "nonce": nonce, "text": NAMED, "asked": "", "kind": "",
                 "sig": gate.sign(KEY, TASK, ts, nonce, NAMED, "", "")}
        self.assertEqual(gate.verify_request(KEY, reply), "bad signature")


class OverHTTP(Base):
    def setUp(self):
        super().setUp()
        for p in (mock.patch.object(gate.ReplyHandler, "key", KEY), mock.patch.object(approval, "IN_GATE", True)):
            p.start()
            self.addCleanup(p.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), gate.ReplyHandler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def post(self, body):
        request = urllib.request.Request(f"http://127.0.0.1:{self.server.server_address[1]}/request",
                                         data=json.dumps(body).encode(), method="POST",
                                         headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status
        except urllib.error.HTTPError as exc:
            return exc.code

    def test_a_signed_request_is_kept_and_an_unsigned_one_is_refused(self):
        self.assertEqual(self.post({"task_id": TASK, "text": NAMED}), 403)
        self.assertFalse(approval.site_known("portal.example.test", task_id=TASK))
        self.assertEqual(self.post(signed_request(NAMED)), 204)
        self.assertTrue(approval.site_known("portal.example.test", task_id=TASK))


class TheWorkersSide(Base):
    """A gated desktop's gui asks the gate; what the worker's files say does not change the answer."""

    def test_the_gate_decides(self):
        with mock.patch.object(approval, "remote", lambda: True), \
                mock.patch.object(approval, "gate_call", lambda op, **a: {"known": False}):
            self.assertFalse(approval.site_known("portal.example.test"))
        with mock.patch.object(approval, "remote", lambda: True), \
                mock.patch.object(approval, "gate_call", lambda op, **a: {"known": True}):
            self.assertTrue(approval.site_known("portal.example.test"))

    def test_a_gate_that_does_not_answer_knows_no_site(self):
        def down(op, **a):
            raise OSError("no gate")
        with mock.patch.object(approval, "remote", lambda: True), mock.patch.object(approval, "gate_call", down):
            self.assertFalse(approval.site_known("portal.example.test"))

    def test_a_desktop_with_no_gate_reads_as_before(self):
        with mock.patch.object(approval, "remote", lambda: False):
            self.assertTrue(approval.site_known("portal.example.test"))


if __name__ == "__main__":
    unittest.main()


class TheControlPlanesVectors(unittest.TestCase):
    """The control plane signs requests and secret values the way the gate verifies them, byte for byte (H-CP-3:
    internal/desktop/testdata/gate-signatures.json, non-ASCII text and a value with a quote and a backslash)."""

    def test_both_bodies_verify(self):
        path = Path(__file__).resolve().parents[2] / "internal" / "desktop" / "testdata" / "gate-signatures.json"
        if not path.exists():
            self.skipTest(f"{path} is not in this tree")
        vectors = json.loads(path.read_text(encoding="utf-8"))
        key, request, secrets = vectors["key"], vectors["request"], vectors["secrets"]
        self.assertEqual(gate.verify_request(key, request, now=float(request["ts"])), "")
        self.assertEqual(gate.verify_secrets(key, secrets, now=float(secrets["ts"])), "")
        self.assertEqual(gate.verify_request(key, dict(request, text=request["text"] + " "), now=float(request["ts"])),
                         "bad signature")
