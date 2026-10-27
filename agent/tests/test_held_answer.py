"""A reply that opens with "it was my answer" and goes on with the answer is the answer (found live 2026-10-27)."""
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task
from decurix_agent.computer import approval


class HeldAnswer(unittest.TestCase):
    def test_it_was_my_answer_with_the_answer_after_it(self):
        with mock.patch.object(approval, "read_choice", side_effect=AssertionError("the model was asked")):
            self.assertEqual(long_task._choice("It was my answer to your question. Yes - show me the exact draft "
                                               "text and the number of differences first; then I will approve."), "answer")
            self.assertEqual(long_task._choice("it was my answer"), "answer")
        with mock.patch.object(approval, "read_choice", lambda *a, **k: {"choice": "after", "instruction": ""}):
            self.assertEqual(long_task._choice("finish that one first, then do this"), "after")


if __name__ == "__main__":
    unittest.main()
