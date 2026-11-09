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


class ADateTheDocumentNamesItsMonth(unittest.TestCase):
    """A digit date declared through gui record that names one date the document writes with its month named is kept
    as the document writes it (records.declare), so the save can tell which day the form's digits are. Found live
    (2026-11-08): kept as "12-10-2026", the save was refused against the identical form text."""

    def test_the_record_keeps_the_documents_wording(self):
        from decurix_agent.computer import records
        with mock.patch.object(task_context, "current", return_value="t1234567890abcdef"),                 mock.patch.object(task_context, "app_date_order", return_value="day"),                 mock.patch.object(records, "_source_text", return_value="TAX INVOICE  Invoice No: AIR-911402  Date: 12-Oct-2026"),                 mock.patch.object(records, "load", return_value=[]), mock.patch.object(records, "_save"),                 mock.patch.object(records, "_key_on_an_app_page", return_value=""),                 mock.patch.object(records, "_backings"), mock.patch.object(records, "_source_units", return_value=([], [])):
            rec, _ = records.declare("AIR-911402", "/home/headless/work/bill.pdf",
                                     {"Supplier Invoice No": "AIR-911402", "Supplier Invoice Date": "12-10-2026"})
        self.assertEqual(rec["fields"]["Supplier Invoice Date"]["intent"], "12-Oct-2026")



if __name__ == "__main__":
    unittest.main()
