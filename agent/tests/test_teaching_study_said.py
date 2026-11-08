"""
A lesson whose study by the model gave nothing says so, rather than going on in silence with a lesson that asks only
what the recording leaves open. Found live (2026-11-08): a reasoning model spent both its budgets thinking, the study
was dropped and the person could not tell why the lesson asked so little.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_teaching_study_said
"""

import unittest
from unittest import mock

from decurix_agent import teaching


class AStudyThatGaveNothing(unittest.TestCase):

    def _learn(self, studied):
        said, asked = [], []
        skill = {"name": "enter-an-order", "steps": [], "params": []}
        with mock.patch.object(teaching, "_sources", return_value=[({"id": "demo-1"}, [])]), \
                mock.patch.object(teaching.skills, "draft_for", return_value=None), \
                mock.patch.object(teaching.learning, "build", return_value=skill), \
                mock.patch.object(teaching.study, "available", return_value=True), \
                mock.patch.object(teaching.study, "study", return_value=studied), \
                mock.patch.object(teaching, "_settle_from_study"), \
                mock.patch.object(teaching, "_ask", lambda s, *a, **k: asked.append(s)), \
                mock.patch.object(teaching.progress, "emit", said.append):
            teaching._learn(["demo-1"], "enter-an-order")
        return skill, said, asked

    def test_the_person_is_told_and_the_lesson_still_asks(self):
        skill, said, asked = self._learn(None)
        self.assertNotIn("understanding", skill)
        self.assertEqual(asked, [skill])
        self.assertTrue(any("could not study the recording" in line for line in said), said)

    def test_a_study_that_worked_says_nothing_of_the_kind(self):
        skill, said, asked = self._learn({"questions": []})
        self.assertIn("understanding", skill)
        self.assertFalse(any("could not study" in line for line in said), said)


if __name__ == "__main__":
    unittest.main()
