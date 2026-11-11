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


class ValuesInClauses(unittest.TestCase):
    """A values reply in clauses, each with its reason, decides each value (learning._clause_choices)."""

    def question(self):
        return {"values": [{"id": f"s{i}"} for i in range(1, 6)]}

    def test_each_clause_decides_its_values(self):
        got = learning._variable_choices(self.question(), "3 and 4 change; 1, 2 and 5 stay (5 is the client's GSTIN)")
        self.assertEqual(got, {"s1": "same", "s2": "same", "s3": "changes", "s4": "changes", "s5": "same"})

    def test_a_value_left_unnamed_or_named_twice_is_not_guessed(self):
        self.assertIsNone(learning._variable_choices(self.question(), "3 and 4 change; 1 and 2 stay"))
        self.assertIsNone(learning._variable_choices(self.question(), "3 and 4 change; 3, 1, 2 and 5 stay"))

    def test_a_single_clause_with_its_reason_in_brackets_still_reads(self):
        got = learning._variable_choices(self.question(), "3 and 4 change (the others are the client's own)")
        self.assertEqual(got["s3"], "changes")
        self.assertEqual(got["s1"], "same")


if __name__ == "__main__":
    unittest.main()
