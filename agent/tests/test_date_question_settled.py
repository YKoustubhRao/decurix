"""
A date-order question that was settled since it was raised is not asked (task_context.date_question), and a taught
run's own question that the date question displaced is asked again rather than answered with the note about the date
(skill_task.ask_again). Found live (2026-11-08): a task run again under the same id asked an earlier attempt's date
question in place of its plan, though the organisation's order had arrived; the reply "1" was then read as a change to
the plan and the bill went to the model by hand.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_date_question_settled.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import skill_task, state, task_context

QUESTION = {"side": "app", "value": "11-10-2026", "options": {"1": "day", "2": "month"},
            "readings": {"day": "11 October 2026", "month": "10 November 2026"}}


class ASettledQuestion(unittest.TestCase):

    def test_an_app_side_question_is_gone_once_the_applications_order_is_known(self):
        with mock.patch.object(task_context, "_context", return_value={"date_question": QUESTION}), \
                mock.patch.object(task_context, "app_date_order", return_value=None):
            self.assertEqual(task_context.date_question("t1234567890abcdef"), QUESTION)
        with mock.patch.object(task_context, "_context", return_value={"date_question": QUESTION}), \
                mock.patch.object(task_context, "app_date_order", return_value="day"):
            self.assertIsNone(task_context.date_question("t1234567890abcdef"))


class TheRunsOwnQuestion(unittest.TestCase):

    def tearDown(self):
        state.paused_skill = None

    def test_is_asked_again_word_for_word(self):
        asked = []
        with mock.patch.object(skill_task, "ask_for_query", lambda text, **kw: asked.append((text, kw))):
            skill_task._pause({"skill": {}}, "plan", "Before I change anything: ... Reply `yes` to go ahead.")
            self.assertTrue(skill_task.ask_again())
        self.assertEqual(asked[0][0], asked[1][0])
        self.assertEqual(state.paused_skill["kind"], "plan")

    def test_nothing_paused_is_nothing_to_ask(self):
        state.paused_skill = None
        self.assertFalse(skill_task.ask_again())


if __name__ == "__main__":
    unittest.main()
