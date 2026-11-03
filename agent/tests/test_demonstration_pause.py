"""
What the person types just after pressing Pause is the pause's: a private step, never lost (review 2026-11-03, D-30).
The flags of input while paused were cleared on the processing thread a moment after the reader had started setting
them, so a password typed in that moment left no private step and the learned skill logged in with none.
"""

import unittest
from unittest import mock

from decurix_agent.computer import demonstration


class InputJustAfterPause(unittest.TestCase):
    def test_input_read_before_the_pause_is_processed_is_kept(self):
        rec = demonstration.Recorder.__new__(demonstration.Recorder)
        rec.latest = None
        rec.paused = False
        rec._on_pause()
        # The reader thread sees the person's keys while paused, before the processing thread runs _paused().
        rec._input_while_paused = True
        rec._typed_while_paused = True
        with mock.patch.object(demonstration, "_read_active", lambda: {"pause_reason": "a password"}):
            rec._paused()
        self.assertTrue(rec._input_while_paused)
        self.assertTrue(rec._typed_while_paused)

    def test_a_new_pause_starts_with_nothing_counted(self):
        rec = demonstration.Recorder.__new__(demonstration.Recorder)
        rec._input_while_paused = rec._typed_while_paused = True
        rec._on_pause()
        self.assertFalse(rec._input_while_paused)
        self.assertFalse(rec._typed_while_paused)
        self.assertTrue(rec.paused)


if __name__ == "__main__":
    unittest.main()
