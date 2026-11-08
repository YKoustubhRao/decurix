"""
A lesson reply that makes its choice and then explains it ("keep - that spot is the Party A/c name field") is that
choice (learning._leading_choice): found live (2026-11-08), the lesson answered "I did not catch that" and only a bare
"keep" was taken.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_lesson_explained_choice.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import learning


def question():
    return {"steps": [{"n": 4, "id": "s4"}, {"n": 9, "id": "s9"}]}


class AnExplainedChoice(unittest.TestCase):

    def test_a_choice_then_its_reason_is_the_choice(self):
        self.assertEqual(learning._coordinate_choices(question(), "keep - that spot is the Party A/c name field"),
                         {"s4": "keep", "s9": "keep"})
        self.assertEqual(learning._coordinate_choices(question(), "skip 9: it was a stray click; keep 4"),
                         {"s4": "keep", "s9": "skip"})

    def test_words_that_make_no_choice_are_still_not_read_as_one(self):
        self.assertIsNone(learning._coordinate_choices(question(), "that spot is the Party A/c name field"))
        self.assertIsNone(learning._leading_choice("coordinates", "keep"))  # (a bare choice is _choose's)


if __name__ == "__main__":
    unittest.main()
