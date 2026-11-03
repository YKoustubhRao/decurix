"""
A secret is remembered before anything writes it, and nothing the desktop keeps after the task holds it (FIX-PY-1;
contract C14; live check F-S2/F-S3 of 2026-11-03).

Found live: a request the person marked secret was kept whole in the command file's handled copy (`.done`) and in the
task file's `.request` copy; a bare reply to a password question reached the agent's log and the reply file. Now:
- /request's secret flag remembers the values first, so the log line, the reply banner, progress lines and the job to
  soul see them already withheld;
- the handled copy of the command file, the `.request` copy and the active-task record hold the withheld form;
- the job to soul names the secret values as data (`secrets`), and its prompt bytes are what they were.
Values here are made up, of the input class "a bare one-word reply".
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

from decurix_agent import config, http_api, messaging, progress, state, task_context, thebot_harness, watchers
from decurix_agent import workflow_task
from decurix_agent.computer import privacy

TOKEN = "test-token-for-the-agent-api"
TASK = "t00000000000000d1"
VALUE = "Quartz58lantern"


class SecretFirst(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "command").mkdir()
        self.logged = []
        for p in (mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: TOKEN, "DECURIX_DIR": str(self.root)}),
                  mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(config, "COMMAND_DIR", self.root / "command"),
                  mock.patch.object(task_context, "CURRENT_FILE", str(self.root / "current-task")),
                  mock.patch.object(messaging, "log", lambda line: self.logged.append(str(line))),
                  mock.patch.object(state, "current_task_id", TASK),
                  mock.patch.object(state, "waiting_for_query", True)):
            p.start()
            self.addCleanup(p.stop)
        privacy._known_cache.clear()
        self.addCleanup(privacy._known_cache.clear)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def send(self, content, secret=True):
        body = {"content": content, "task_id": TASK, "secret": secret}
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_address[1]}/request", data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN}, method="POST")
        with urllib.request.urlopen(request, timeout=5) as response:
            self.assertEqual(response.status, 200)
        return next(p for p in (self.root / "command").iterdir() if p.suffix == ".txt")

    def kept(self):
        """Everything the desktop keeps on disk under the test's folders."""

        return "".join(p.read_text(encoding="utf-8", errors="replace") for p in self.root.rglob("*") if p.is_file()
                       and p.name != "withheld-secrets")

    def test_a_bare_reply_flagged_secret(self):
        path = self.send(VALUE)
        self.assertNotIn(VALUE, "\n".join(self.logged))           # "Received request: ..." came after remembering
        watchers._mark_handled(path)                              # the watcher's handled copy
        task_context.start(TASK, request=VALUE)                   # the .request copy
        task_context.mark_active(TASK, request=VALUE)
        self.assertTrue((self.root / "command" / (path.name + ".done")).exists())
        self.assertNotIn(VALUE, self.kept())
        posted = []
        with mock.patch.object(progress.urllib.request, "urlopen",
                               side_effect=lambda r, timeout=None: posted.append(json.loads(r.data)) or mock.MagicMock()):
            progress._post(f"Typing {VALUE} into the field")
        self.assertNotIn(VALUE, posted[0]["text"])

    def test_a_request_flagged_secret_keeps_only_the_withheld_form(self):
        request = f"Sign in to the staff portal as mira.k with {VALUE} and download the roster"
        path = self.send(request)
        watchers._mark_handled(path)
        task_context.start(TASK, request=request)
        done = (self.root / "command" / (path.name + ".done")).read_text(encoding="utf-8")
        self.assertNotIn(VALUE, done)
        self.assertIn("[withheld]", done)
        self.assertNotIn(VALUE, (self.root / "current-task.request").read_text(encoding="utf-8"))

    def test_an_unmarked_request_is_kept_as_it_was(self):
        path = self.send("Download the roster", secret=False)
        watchers._mark_handled(path)
        self.assertEqual((self.root / "command" / (path.name + ".done")).read_text(encoding="utf-8"),
                         "Download the roster")

    def test_the_job_to_soul_names_the_secrets_and_its_prompt_is_unchanged(self):
        self.send(VALUE)
        bodies = []
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        with mock.patch.object(thebot_harness, "_post_json", side_effect=lambda url, body: bodies.append(body) or {}), \
                mock.patch.object(thebot_harness, "_build_context", return_value=""), \
                mock.patch.object(workflow_task, "_active", None):
            thebot_harness._run_thebot(VALUE, reply=True)
        self.assertIn(VALUE, bodies[-1]["secrets"])
        self.assertEqual(bodies[-1]["prompt"], VALUE)              # soul needs the value; the prompt is as it was


class TheWithheldFormStaysReadable(unittest.TestCase):
    def test_a_quoted_value_after_a_quoted_label(self):
        line = f'Typed into textbox "Password" "{VALUE}" in "Staff portal - Chromium"'
        out = privacy.withhold_known(line)
        self.assertNotIn(VALUE, out)
        self.assertEqual(out, 'Typed into textbox "Password" "[withheld]" in "Staff portal - Chromium"')

    def test_a_label_with_no_value_is_left_alone(self):
        self.assertEqual(privacy.withhold_known('the "Password" field is empty'), 'the "Password" field is empty')


if __name__ == "__main__":
    unittest.main()
