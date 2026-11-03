"""
A message the person marked secret (F-2, final review): the control plane adds "secret": true to /request, and the
agent remembers it before anything keeps or shows it - the secrets it spells out, the whole message, and each word of
a short one - so a later progress line and a recording withhold it.
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

from decurix_agent import config, http_api, progress
from decurix_agent.computer import privacy, recording

TOKEN = "test-token-for-the-agent-api"
WORD = "Kestrel77orbit"


class AMarkedMessageIsWithheldFromThenOn(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.saved = []
        for patch in (mock.patch.dict(os.environ, {"DECURIX_DIR": str(self.root), config.AGENT_TOKEN_ENV: TOKEN}),
                      mock.patch.object(http_api, "save_request",
                                        lambda content, task_id="", secret=False: self.saved.append(content) or Path("1.txt")),
                      mock.patch.object(recording, "ROOT", self.root / "recordings"),
                      # (the task's own secret values live beside its task file: one of each test's own)
                      mock.patch("decurix_agent.task_context.CURRENT_FILE", str(self.root / "current-task"))):
            patch.start()
            self.addCleanup(patch.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def send(self, body):
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_address[1]}/request", data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN}, method="POST")
        with urllib.request.urlopen(request, timeout=5) as response:
            self.assertEqual(response.status, 200)

    def posted_line(self, text):
        posted = []
        with mock.patch.object(progress.urllib.request, "urlopen",
                               side_effect=lambda r, timeout=None: posted.append(json.loads(r.data)) or mock.MagicMock()):
            progress._post(text)
        return posted[0]["text"]

    def test_a_short_marked_message_is_withheld_in_a_later_line_and_a_recording(self):
        self.send({"content": f"it is {WORD}", "task_id": "t0123456789abcdef", "secret": True})
        self.assertNotIn(WORD, self.posted_line(f"Typing {WORD} into the field"))
        recording.append("task-t0123456789abcdef", {"kind": "type", "text": WORD, "summary": f"type {WORD}"})
        kept = "".join(p.read_text(encoding="utf-8") for p in (self.root / "recordings").rglob("*") if p.is_file())
        self.assertNotIn(WORD, kept)

    def test_an_unmarked_message_is_not_remembered(self):
        self.send({"content": f"it is {WORD}", "task_id": "t0123456789abcdef"})
        self.assertIn(WORD, self.posted_line(f"Typing {WORD} into the field"))
