"""
What the control plane sends beside a request's words reaches the agent (PLAN-P2 C2, C6):
- soul's job body names the task the job works for ("taskId"), so soul tags its model calls with it;
- /request's "date_order" (day or month) is kept for the task, and anything else is ignored;
- /request's "judgement_id" (a person's reply, as the control plane recorded it) is kept for the task, and a malformed
  one is ignored.
"""

import json
import os
import tempfile
import threading
import unittest
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, http_api, state, task_context, thebot_harness, workflow_task

TOKEN = "test-token-for-the-agent-api"
TASK = "t0123456789abcdef"


class SoulsJobBodyNamesTheTask(unittest.TestCase):
    def setUp(self):
        self.bodies = []
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        for p in (mock.patch.object(thebot_harness, "_post_json",
                                    side_effect=lambda url, body: self.bodies.append(body) or {}),
                  mock.patch.object(thebot_harness, "_build_context", return_value=""),
                  mock.patch.object(workflow_task, "_active", None),
                  mock.patch.object(state, "current_task_id", TASK)):
            p.start()
            self.addCleanup(p.stop)

    def test_the_body_carries_the_current_task(self):
        thebot_harness._run_thebot("do the work", reply=False)
        self.assertEqual(self.bodies[-1]["taskId"], TASK)

    def test_no_task_sends_an_empty_id(self):
        with mock.patch.object(state, "current_task_id", ""), \
                mock.patch.object(task_context, "CURRENT_FILE", str(Path(tempfile.gettempdir()) / "no-such-task")):
            thebot_harness._run_thebot("do the work", reply=False)
        self.assertEqual(self.bodies[-1]["taskId"], "")


class TheRequestBodysExtraFields(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for p in (mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: TOKEN}),
                  mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(http_api, "save_request", lambda content, task_id="": Path("1.txt"))):
            p.start()
            self.addCleanup(p.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def send(self, **body):
        body = {"content": "enter the bill", "task_id": TASK, **body}
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_address[1]}/request", data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN}, method="POST")
        with urllib.request.urlopen(request, timeout=5) as response:
            self.assertEqual(response.status, 200)

    def test_a_date_order_reaches_the_task(self):
        self.send(date_order="month")
        self.assertEqual(task_context.sent_date_order(TASK), "month")
        self.send(date_order="day")
        self.assertEqual(task_context.sent_date_order(TASK), "day")

    def test_an_unknown_date_order_is_ignored(self):
        for value in ("DMY", "", 3, None, "day; rm"):
            self.send(date_order=value)
        self.assertIsNone(task_context.sent_date_order(TASK))

    def test_a_reply_judgement_is_kept_and_a_malformed_one_is_not(self):
        self.send(judgement_id="j_abc123DEF")
        self.assertEqual(task_context.reply_judgement(TASK), "j_abc123DEF")
        for value in ("j_", "x_abc123DEF", "j_abc/../../x", 7):
            self.send(judgement_id=value)
        self.assertEqual(task_context.reply_judgement(TASK), "j_abc123DEF")

    def test_an_invalid_task_keeps_nothing(self):
        self.send(task_id="../../etc", date_order="day")
        self.assertFalse((self.root / "runs").exists())


if __name__ == "__main__":
    unittest.main()
