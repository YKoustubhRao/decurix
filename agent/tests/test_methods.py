"""A kind of task's method: the plan of a checked run, with what the person said, offered next time (2026-10-23)."""
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import methods


def job_with(plan):
    return {"results": [{"SkillName": "operator", "Meta": {"plan": plan}}]}


PLAN = {"scope": "the month's bills, serials 451 to 458", "sources": ["the register", "the client's folder"],
        "outputs": ["the return on the portal"], "commits": ["file the return"],
        "personSaid": "also chk the client chat for anything cancelled since 01/10"}


class Methods(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patcher = mock.patch.object(methods, "ROOT", Path(self.tmp.name))
        patcher.start()
        self.addCleanup(patcher.stop)
        methods.begin()

    def test_a_checked_run_keeps_its_plan_and_the_next_of_its_kind_is_given_it(self):
        methods.note_job(job_with(PLAN))
        self.assertTrue(methods.keep("File Udupi Traders' September return on the portal from their workbook"))
        said = methods.context("File Karavali Stores' October return on the portal from their workbook")
        self.assertIn(methods.MARKER, said)
        self.assertIn("the client's folder", said)
        self.assertIn("client chat for anything cancelled", said)
        for value in ("451", "458", "01/10"):
            self.assertNotIn(value, said, "a run's value went into the method")
        self.assertEqual(methods.context("Summarise the meeting notes into a memo"), "", "another kind of task")

    def test_nothing_is_kept_without_a_plan_that_writes_or_commits(self):
        self.assertIsNone(methods.keep("file the return on the portal"))
        methods.note_job(job_with(dict(PLAN, commits=[], outputs=[])))
        self.assertIsNone(methods.keep("file the return on the portal"))

    def test_a_kind_kept_again_is_brought_up_to_date(self):
        methods.note_job(job_with(PLAN))
        name = methods.keep("File Udupi Traders' September return on the portal from their workbook")
        methods.begin()
        methods.note_job(job_with(dict(PLAN, sources=["the register", "the client's folder", "the bank statement"],
                                       personSaid="the challan is in the cash ledger")))
        self.assertEqual(methods.keep("File Karavali Stores' October return on the portal from their workbook"), name)
        said = methods.context("File Manipal Interiors' November return on the portal from their workbook")
        self.assertIn("the bank statement", said)
        self.assertIn("anything cancelled", said)
        self.assertIn("challan is in the cash ledger", said)
        self.assertIn("2 earlier checked run(s)", said)

    def test_an_answer_is_kept_with_the_question_it_answered(self):
        # Found live (2026-11-08): an answer kept alone reached the next task as the person's standing word about it.
        methods.note_job(job_with(dict(PLAN, ask="OK to enter the bills as planned, each with your OK first?",
                                       personSaid="I don't know that, so I can't give it to you.")))
        methods.keep("File Udupi Traders' September return on the portal from their workbook")
        said = methods.context("File Karavali Stores' October return on the portal from their workbook")
        self.assertIn('Asked on an earlier run: "OK to enter the bills as planned, each with your OK first?" - the '
                      'person answered: "I don\'t know that, so I can\'t give it to you."', said)
        self.assertNotIn("The person said about this plan", said)

    def test_an_answer_kept_before_questions_were_kept_still_reads(self):
        methods.note_job(job_with(PLAN))
        methods.keep("File Udupi Traders' September return on the portal from their workbook")
        said = methods.context("File Karavali Stores' October return on the portal from their workbook")
        self.assertIn("client chat for anything cancelled", said)

    def test_a_new_task_forgets_the_last_ones_plan(self):
        methods.note_job(job_with(PLAN))
        methods.begin()
        self.assertIsNone(methods.keep("file the return on the portal"))


if __name__ == "__main__":
    unittest.main()
