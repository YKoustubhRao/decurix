"""A request saved after the open question, yet not as its reply, means the question is stale."""

import time
import unittest

from decurix_agent import state, watchers


class AStaleQuestion(unittest.TestCase):
    def setUp(self):
        self.saved = state.query_asked_at

    def tearDown(self):
        state.query_asked_at = self.saved

    def name_at(self, t):
        return time.strftime("%Y%m%d_%H%M%S", time.localtime(t)) + "_abcd1234.t0123456789abcdef.txt"

    def test_a_request_after_the_question_drops_it(self):
        # Found live (2026-10-15): a new request sat unread for 34 minutes behind a question the control plane
        # had already closed.
        state.query_asked_at = time.time() - 60
        self.assertTrue(watchers._orphaned_question(self.name_at(time.time())))

    def test_a_request_queued_before_the_question_is_still_held(self):
        state.query_asked_at = time.time()
        self.assertFalse(watchers._orphaned_question(self.name_at(time.time() - 120)))

    def test_no_question_time_known_holds_as_before(self):
        state.query_asked_at = 0.0
        self.assertFalse(watchers._orphaned_question(self.name_at(time.time())))
        self.assertFalse(watchers._orphaned_question("odd-name.txt"))


if __name__ == "__main__":
    unittest.main()
