"""
What a desktop keeps from a person's reply is copied to the control plane as it is kept (R8 of the plan of
2026-11-03; PLAN-P2 C7, D4; messaging.post_learned).

A skill correction, a run answer or a lesson cites the control plane's judgement of the reply it came from
(`derived_from`, the judgement id /request carried): the control plane decides the author from it. No reply to cite,
no post. A 409 is dropped; a server error is kept in the run folder and sent again.
"""

import json
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, messaging, skill_task, state, task_context
from decurix_agent.computer import skills

TASK = "t00000000000000e1"


class _ControlPlane(BaseHTTPRequestHandler):
    status = 201
    posts = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)))
        _ControlPlane.posts.append((self.path, body))
        self.send_response(_ControlPlane.status)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


class PostLearned(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        _ControlPlane.posts, _ControlPlane.status = [], 201
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _ControlPlane)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.logged = []
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(config, "CONTROL_PLANE_URL", f"http://127.0.0.1:{self.server.server_address[1]}"),
                  mock.patch.object(messaging, "log", lambda line: self.logged.append(line)),
                  mock.patch.object(state, "current_task_id", TASK)):
            p.start()
            self.addCleanup(p.stop)

    def post(self, *args, **kw):
        # (The reply's own judgement, captured with it: here, the one the test's reply came with.)
        kw.setdefault("derived_from", task_context.reply_judgement(TASK))
        thread = messaging.post_learned(*args, **kw)
        if thread is not None:
            thread.join(10)
        return thread

    def queued(self):
        path = self.root / "runs" / TASK / "learned.jsonl"
        return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()] if path.exists() else []

    def test_it_cites_the_reply(self):
        task_context.set_reply_judgement(TASK, "j_0123456789abcdef")
        self.post("correction", "enter-a-bill", 3, "", "the ledger is Purchases, not Expenses")
        path, body = _ControlPlane.posts[-1]
        self.assertEqual(path, "/api/desktops/learned")
        self.assertRegex(body.pop("client_id"), r"^[0-9a-f]{32}$")
        self.assertEqual(body, {"task_id": TASK, "kind": "correction", "skill": "enter-a-bill", "skill_version": 3,
                                "derived_from": "j_0123456789abcdef", "question": "",
                                "text": "the ledger is Purchases, not Expenses"})
        self.assertEqual(self.queued(), [])

    def test_no_reply_to_cite_posts_nothing(self):
        self.assertIsNone(self.post("lesson", "", 0, "which ledger?", "Purchases"))
        self.assertEqual(_ControlPlane.posts, [])
        self.assertTrue(any("no reply to cite" in line for line in self.logged))

    def test_a_409_is_dropped(self):
        task_context.set_reply_judgement(TASK, "j_0123456789abcdef")
        _ControlPlane.status = 409
        self.post("run_answer", "s", 1, "which?", "the first")
        self.assertEqual(len(_ControlPlane.posts), 1)
        self.assertEqual(self.queued(), [])

    def test_a_server_error_is_kept_and_sent_again(self):
        task_context.set_reply_judgement(TASK, "j_0123456789abcdef")
        _ControlPlane.status = 503
        self.post("run_answer", "s", 1, "which?", "the first")
        self.assertEqual(len(self.queued()), 1)
        _ControlPlane.status = 201
        messaging.resend_learned()
        self.assertEqual(len(_ControlPlane.posts), 2)
        self.assertEqual(self.queued(), [])
        first, again = (body["client_id"] for _, body in _ControlPlane.posts)
        self.assertEqual(first, again, "a line is sent again under its own id, for the control plane to keep once")

    def test_two_resends_at_once_post_a_line_once(self):
        task_context.set_reply_judgement(TASK, "j_0123456789abcdef")
        _ControlPlane.status = 503
        self.post("run_answer", "s", 1, "which?", "the first")
        _ControlPlane.posts.clear()
        _ControlPlane.status = 201
        real, entered = messaging._post_learned, threading.Event()

        def slow(body):
            entered.set()
            threading.Event().wait(0.3)  # (the second thread starts while this one is posting)
            return real(body)
        with mock.patch.object(messaging, "_post_learned", slow):
            first = threading.Thread(target=messaging.resend_learned)
            first.start()
            entered.wait(5)
            second = threading.Thread(target=messaging.resend_learned)
            second.start()
            first.join(10)
            second.join(10)
        self.assertEqual(len(_ControlPlane.posts), 1)
        self.assertEqual(self.queued(), [])

    def test_a_correction_during_a_skill_run_is_posted(self):
        task_context.set_reply_judgement(TASK, "j_0123456789abcdef")
        skill = {"name": "enter-a-bill", "version": 2, "steps": []}
        with mock.patch.object(skills, "load", lambda name: dict(skill)), mock.patch.object(skills, "save"), \
                mock.patch.object(state, "reply_judgement", "j_0123456789abcdef"), \
                mock.patch.object(state, "reply_secret", False):
            skill_task._correct({"skill": dict(skill)}, "use the Purchases ledger")
        for _ in range(100):
            if _ControlPlane.posts:
                break
            threading.Event().wait(0.05)
        self.assertEqual(_ControlPlane.posts[-1][1]["kind"], "correction")
        self.assertEqual(_ControlPlane.posts[-1][1]["skill_version"], 2)


if __name__ == "__main__":
    unittest.main()
