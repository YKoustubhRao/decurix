"""
A desktop back from a restart closes each question an earlier life of its agent asked and it no longer holds
(watchers._close_lost_questions); never the one it does hold, and none when it cannot read which are open. Found live
(2026-11-09): a task read "waiting on you" for good over a question the restarted agent had forgotten.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import state, task_context, watchers

LOST, HELD = "t1111111111111111", "t2222222222222222"


class LostQuestions(unittest.TestCase):
    def setUp(self):
        self.closed = []

        def write(note, kind="final", interrupted=False, **_):
            self.closed.append((task_context.current(), kind, interrupted, note))

        for p in (mock.patch.object(watchers, "write_final_reply", write),
                  mock.patch.object(watchers, "_saved_before_restart", lambda task_id: "")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(task_context.start, "")
        saved = state.waiting_for_query
        self.addCleanup(setattr, state, "waiting_for_query", saved)

    def test_a_question_this_agent_no_longer_holds_closes_as_stopped(self):
        state.waiting_for_query = False
        task_context.start("")
        with mock.patch("decurix_agent.messaging.waiting_tasks", return_value=[LOST]):
            watchers._close_lost_questions()
        self.assertEqual(len(self.closed), 1)
        task_id, kind, interrupted, note = self.closed[0]
        self.assertEqual((task_id, kind, interrupted), (LOST, "error", True))
        self.assertIn("restarted while this waited on your answer", note)

    def test_the_question_it_holds_is_never_closed(self):
        state.waiting_for_query = True
        task_context.start(HELD)
        with mock.patch("decurix_agent.messaging.waiting_tasks", return_value=[HELD, LOST]):
            watchers._close_lost_questions()
        self.assertEqual([c[0] for c in self.closed], [LOST])
        self.assertEqual(task_context.current(), HELD, "the held task stays the current one")

    def test_what_the_task_had_saved_is_said(self):
        state.waiting_for_query = False
        with mock.patch("decurix_agent.messaging.waiting_tasks", return_value=[LOST]), \
                mock.patch.object(watchers, "_saved_before_restart", lambda task_id: "Saved before the restart: bill 1053"):
            watchers._close_lost_questions()
        self.assertEqual(self.closed[0][3], "Saved before the restart: bill 1053")

    def test_when_the_control_plane_cannot_be_read_nothing_is_closed(self):
        state.waiting_for_query = False
        with mock.patch("decurix_agent.messaging.waiting_tasks", side_effect=OSError("unreachable")):
            watchers._close_lost_questions()
        self.assertEqual(self.closed, [])


if __name__ == "__main__":
    unittest.main()
