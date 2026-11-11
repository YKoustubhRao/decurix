"""thebot_harness.resume_waiting: after a restart, a task waiting on the person whose paused job soul kept (GET
/api/paused) is waited on again rather than closed as lost; anything not known leaves the old path (reported as
interrupted). Found live 2026-11-11: soul kept the job, but the restarted agent closed the task as "the question is
gone" before the person could answer."""

import unittest
import urllib.error
from unittest import mock

from decurix_agent import messaging, state, task_context, thebot_harness

TASK = "t0123456789abcdef"
RECORD = {"task_id": TASK, "request": "push the branch"}


class ResumeWaitingTests(unittest.TestCase):
    def setUp(self):
        self.saved = (state.waiting_for_query, state.current_task, state.last_query_kind, thebot_harness._approval_asked)
        self.addCleanup(self.restore)
        for target, value in (("start", None), ("mark_active", None)):
            patch = mock.patch.object(task_context, target, return_value=value)
            patch.start()
            self.addCleanup(patch.stop)
        state.waiting_for_query = False

    def restore(self):
        state.waiting_for_query, state.current_task, state.last_query_kind, thebot_harness._approval_asked = self.saved

    def run_with(self, waiting, answers):
        with mock.patch.object(messaging, "waiting_tasks", return_value=waiting), \
                mock.patch.object(thebot_harness, "_get_json", side_effect=answers) as get, \
                mock.patch.object(thebot_harness.time, "sleep"):
            return thebot_harness.resume_waiting(RECORD), get

    def test_a_kept_approval_is_waited_on_again(self):
        resumed, get = self.run_with([TASK], [{"paused": True, "kind": "approval", "question": "Run git push?"}])
        self.assertTrue(resumed)
        self.assertTrue(state.waiting_for_query)
        self.assertEqual(state.last_query_kind, "approval")
        self.assertEqual(thebot_harness._approval_asked, "Run git push?")
        self.assertIn("/api/paused?session=", get.call_args[0][0])

    def test_a_kept_question_is_waited_on_again(self):
        resumed, _ = self.run_with([TASK], [{"paused": True, "kind": "input", "question": "Which year?"}])
        self.assertTrue(resumed)
        self.assertEqual(state.last_query_kind, "query")
        self.assertIsNone(thebot_harness._approval_asked)

    def test_nothing_kept_is_not_resumed(self):
        resumed, _ = self.run_with([TASK], [{"paused": False}])
        self.assertFalse(resumed)
        self.assertFalse(state.waiting_for_query)

    def test_a_task_not_waiting_on_the_person_is_not_resumed(self):
        resumed, get = self.run_with([], [{"paused": True, "kind": "approval"}])
        self.assertFalse(resumed)
        get.assert_not_called()

    def test_soul_coming_up_is_waited_for(self):
        down = urllib.error.URLError("connection refused")
        resumed, get = self.run_with([TASK], [down, down, {"paused": True, "kind": "approval", "question": "q"}])
        self.assertTrue(resumed)
        self.assertEqual(get.call_count, 3)

    def test_a_soul_that_never_answers_is_not_resumed(self):
        with mock.patch.object(thebot_harness, "RESUME_WAIT_SECONDS", 0):
            resumed, _ = self.run_with([TASK], [urllib.error.URLError("down")] * 3)
        self.assertFalse(resumed)
        self.assertFalse(state.waiting_for_query)

    def test_an_older_soul_without_the_route_is_not_resumed(self):
        refused = urllib.error.HTTPError("u", 404, "not found", {}, None)
        resumed, _ = self.run_with([TASK], [refused])
        self.assertFalse(resumed)

    def test_unknown_waiting_is_not_resumed(self):
        with mock.patch.object(messaging, "waiting_tasks", side_effect=OSError("down")):
            self.assertFalse(thebot_harness.resume_waiting(RECORD))


if __name__ == "__main__":
    unittest.main()
