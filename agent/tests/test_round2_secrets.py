"""
The secrets and gate review's round 2 (2026-11-04): what each finding pinned, as a test that failed on the code before.

- B4: a value is withheld whole before anything cuts it, and the gate's question withholds a value's written forms
  (shell-quoted, escaped inside double quotes, percent-encoded, JSON-escaped).
- B5: two /secrets posts at once both keep their values.
- B7: what is kept from a reply cites that reply's judgement, captured with it; none, no post.
- B8: what post_learned sends holds no remembered value and no spelled-out credential.
- The signature labels: a body signed as a request never verifies as secret values, nor the other way.
Values are made up, of the input classes named.
"""

import json
import shlex
import tempfile
import threading
import time
import unittest
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, gate_server as gate, lessons, messaging, state, task_context
from decurix_agent.computer import approval, privacy

TASK = "t00000000000000f4"
KEY = "test-gate-key"
VALUE = 'Pq7$x"9`!z'  # a password with shell metacharacters


class InTheGate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(config, "DECURIX_DIR", Path(tmp.name)), mock.patch.object(approval, "IN_GATE", True)):
            p.start()
            self.addCleanup(p.stop)


class WithheldBeforeCut(InTheGate):
    def test_a_command_with_the_value_across_the_cut(self):
        gate.keep_secrets(TASK, ["Mx4kRw9tQv"])
        command = "echo " + "a" * 590 + " Mx4kRw9tQv"
        _, description, _, _ = approval.from_material({"kind": "command", "command": command})
        self.assertNotIn("Mx4k", description)

    def test_a_typed_value_across_the_quote_cut(self):
        gate.keep_secrets(TASK, ["Mx4kRw9tQv"])
        text = "b" * 50 + "Mx4kRw9tQv"
        _, description, _, _ = approval.from_material({"kind": "type", "text": text,
                                                       "element": {"role": "textbox", "name": "Note"}})
        self.assertNotIn("Mx4k", description)

    def test_each_written_form_of_a_value(self):
        gate.keep_secrets(TASK, [VALUE])
        double = "".join("\\" + c if c in '\\"$`' else c for c in VALUE)
        for command in (f"gui type e3 {shlex.quote(VALUE)}", f'gui type e3 "{double}"',
                        f"curl https://portal.example.test/login?pw={urllib.parse.quote(VALUE, safe='')}",
                        f"curl -d {json.dumps({'pw': VALUE})} https://portal.example.test"):
            _, description, _, _ = approval.from_material({"kind": "command", "command": command})
            with self.subTest(command=command[:30]):
                self.assertNotIn("Pq7", description)


class TwoPostsAtOnce(InTheGate):
    def test_both_tasks_values_are_kept(self):
        errors = []

        def post(task, value):
            try:
                for _ in range(20):
                    gate.keep_secrets(task, [value])
            except Exception as exc:  # noqa: BLE001
                errors.append(exc)
        a = threading.Thread(target=post, args=("t00000000000000a1", "Alpha71value"))
        b = threading.Thread(target=post, args=("t00000000000000a2", "Bravo72value"))
        a.start(), b.start()
        a.join(), b.join()
        self.assertEqual(errors, [])
        kept = approval.gate_secret_values()
        self.assertIn("Alpha71value", kept)
        self.assertIn("Bravo72value", kept)


class TheLabels(unittest.TestCase):
    def test_a_request_signature_never_verifies_secrets_nor_the_other_way(self):
        ts, nonce = str(time.time()), uuid.uuid4().hex
        # /request and /secrets carry the same number of parts: only the label tells them apart.
        as_request = gate.sign_request(KEY, TASK, ts, nonce, "x")
        self.assertEqual(gate.verify_secrets(KEY, {"task_id": TASK, "ts": ts, "nonce": nonce, "secrets": ["x"],
                                                   "sig": as_request}), "bad signature")
        as_secrets = gate.sign_secrets(KEY, TASK, ts, nonce, ["x"])
        self.assertEqual(gate.verify_request(KEY, {"task_id": TASK, "ts": ts, "nonce": nonce, "text": "x",
                                                   "sig": as_secrets}), "bad signature")


class _ControlPlane(BaseHTTPRequestHandler):
    posts = []

    def do_POST(self):
        _ControlPlane.posts.append(json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0))))
        self.send_response(201)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *a):
        pass


class WhatIsKeptFromAReply(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        _ControlPlane.posts = []
        server = ThreadingHTTPServer(("127.0.0.1", 0), _ControlPlane)
        threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.dict("os.environ", {"DECURIX_DIR": str(self.root)}),
                  mock.patch.object(config, "CONTROL_PLANE_URL", f"http://127.0.0.1:{server.server_address[1]}"),
                  mock.patch.object(task_context, "CURRENT_FILE", str(self.root / "current-task")),
                  mock.patch.object(state, "current_task_id", TASK)):
            p.start()
            self.addCleanup(p.stop)
        privacy._known_cache.clear()
        self.addCleanup(privacy._known_cache.clear)

    def wait(self, n=1):
        for _ in range(400):
            if len(_ControlPlane.posts) >= n:
                return
            time.sleep(0.05)

    def test_the_post_holds_no_secret(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        thread = messaging.post_learned("lesson", "", 0, "Which login?",
                                        "Use Copper71meadow, password: Zebra-42-quiet", derived_from="j_0123456789abcdef")
        thread.join(10)
        body = json.dumps(_ControlPlane.posts[-1])
        self.assertNotIn("Copper71meadow", body)
        self.assertNotIn("Zebra-42-quiet", body)

    def test_a_lesson_cites_its_own_reply_even_after_a_later_one(self):
        def distil(question, reply, task="", skill=""):
            task_context.set_reply_judgement(TASK, "j_bbbbbbbbbbbbbbbb")  # a second reply arrives meanwhile
            return ("Ledger", "Use the Purchases ledger.")
        with mock.patch.object(lessons, "distil", distil), mock.patch.object(lessons, "remember", lambda *a, **k: True):
            lessons._keep("Which ledger?", "Purchases", "", "", task_id=TASK, judgement_id="j_aaaaaaaaaaaaaaaa")
        self.wait()
        self.assertEqual(_ControlPlane.posts[-1]["derived_from"], "j_aaaaaaaaaaaaaaaa")

    def test_a_reply_with_no_judgement_posts_nothing(self):
        task_context.set_reply_judgement(TASK, "j_aaaaaaaaaaaaaaaa")
        task_context.set_reply_judgement(TASK, None)  # the next message came with none: no citation is inherited
        self.assertEqual(task_context.reply_judgement(TASK), "")
        self.assertIsNone(messaging.post_learned("lesson", "", 0, "q", "a", derived_from=""))
        self.assertEqual(_ControlPlane.posts, [])


if __name__ == "__main__":
    unittest.main()


class AQuestionThatCannotBeShownIsRefused(InTheGate):
    def test_held_back_words_refuse_the_action(self):
        with mock.patch.object(approval, "shown_by_gate", lambda text: privacy.HELD_BACK if text else text):
            said = gate.handle({"op": "check", "material": {"kind": "command", "command": "rm -rf /tmp/x"}})
        self.assertFalse(said["allowed"])
        self.assertIsNone(said["request"])
