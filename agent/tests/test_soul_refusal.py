"""
soul refuses with 400 a job it will not take (more secret values than it accepts, or one too long). The harness reports
that as a refusal in soul's words - not as a soul outage, and with no retry - and sends soul only values within its
limits (thebot_harness._job_secrets).
"""

import io
import unittest
import urllib.error
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import state, task_context, thebot_harness, workflow_task

TASK = "t00000000000000f3"


class SoulsRefusal(unittest.TestCase):
    def setUp(self):
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        for p in (mock.patch.object(thebot_harness, "_build_context", return_value=""),
                  mock.patch.object(workflow_task, "_active", None),
                  mock.patch.object(state, "current_task_id", TASK)):
            p.start()
            self.addCleanup(p.stop)

    def refuse(self, url, body):
        raise urllib.error.HTTPError(url, 400, "Bad Request", {}, io.BytesIO(b'{"error": "too many secret values"}'))

    def test_a_400_is_a_refusal_reported_once_never_an_outage(self):
        finals, failed = [], []
        with mock.patch.object(thebot_harness, "_post_json", side_effect=self.refuse) as post, \
                mock.patch.object(thebot_harness, "write_final_reply", side_effect=lambda t, **k: finals.append((t, k))), \
                mock.patch.object(thebot_harness, "_fail_on_backend", side_effect=lambda e: failed.append(e)):
            job, error = thebot_harness._run_thebot("do the work", reply=False)
            thebot_harness._handle_job(job, error)
        self.assertIsInstance(error, thebot_harness.SoulRefused)
        self.assertEqual(post.call_count, 1)                   # not retried
        self.assertEqual(failed, [])                           # not a backend failure
        self.assertIn("too many secret values", finals[-1][0])
        self.assertEqual(finals[-1][1]["kind"], "error")

    def test_a_secret_too_long_for_soul_refuses_the_task_and_sends_nothing(self):
        # Review round 2: dropping the long value let soul keep it unredacted. It fails closed instead.
        task_context._secret_values[TASK] = ["short", "x" * (thebot_harness.JOB_SECRET_BYTES + 1)]
        self.addCleanup(task_context._secret_values.pop, TASK, None)
        with mock.patch.object(thebot_harness, "_post_json") as post:
            job, error = thebot_harness._run_thebot("do the work", reply=False)
        post.assert_not_called()
        self.assertIsInstance(error, thebot_harness.SoulRefused)
        self.assertIn("not started", error)


if __name__ == "__main__":
    unittest.main()
