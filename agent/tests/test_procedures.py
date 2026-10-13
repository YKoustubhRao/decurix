import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import attachments, procedures

TAUGHT = ("You'll be doing our month-end bank reconciliation. Procedure:\n"
          "- Match each bank statement line to the cash book: same amount, bank date up to 5 days after the book date.\n"
          "- Make bank_rec_<YYYY-MM>.xlsx in my work folder with sheets Matched, Unmatched Bank, Unmatched Book and Summary.\n"
          "- Difference must be 0.00; if it isn't, tell me what you think is wrong.\nDo July 2026 now.")


class StandingProceduresAreKept(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = mock.patch.object(procedures, "DIR", Path(self.tmp.name) / "procedures")
        self.dir.start()
        self.model = mock.patch.object(procedures.llm_client, "available", return_value=False)
        self.model.start()
        self.memory = mock.patch("decurix_agent.messaging.remember", return_value=True)
        self.remembered = self.memory.start()

    def tearDown(self):
        self.memory.stop()
        self.model.stop()
        self.dir.stop()
        self.tmp.cleanup()

    def test_a_request_that_sets_a_procedure_is_told_apart_from_one_piece_of_work(self):
        self.assertTrue(procedures.sets_procedure(TAUGHT))
        self.assertFalse(procedures.sets_procedure("August 2026 bank rec please - files attached."))
        self.assertFalse(procedures.sets_procedure("Reconcile the attached statement against the cash book and tell me the difference."))

    def test_it_is_kept_and_a_later_request_for_the_same_work_gets_it_back(self):
        record = procedures.note(TAUGHT, task_id="t1")
        self.assertIsNotNone(record)
        self.assertIn("bank_rec_<YYYY-MM>.xlsx", record["procedure"])
        self.assertTrue(self.remembered.called)
        request = "August 2026 bank reconciliation please.\n\n" + attachments.block(["/home/headless/work/inbox/up-1/stmt.csv"])
        applied = procedures.apply(request)
        self.assertTrue(procedures.in_request(applied))
        self.assertIn("bank_rec_<YYYY-MM>.xlsx", applied)
        # The files stay last, where every reader of a request's attachments looks for them.
        self.assertEqual(attachments.files(applied), ["/home/headless/work/inbox/up-1/stmt.csv"])
        self.assertTrue(attachments.text(applied).startswith("August 2026 bank reconciliation please."))

    def test_other_work_and_a_request_that_gives_its_own_procedure_get_nothing(self):
        procedures.note(TAUGHT)
        self.assertEqual(procedures.apply("Draft a thank-you email to the auditors."), "Draft a thank-you email to the auditors.")
        own = TAUGHT.replace("July", "August")
        self.assertEqual(procedures.apply(own), own)

    def test_the_model_decides_which_procedure_and_a_low_confidence_answer_is_none(self):
        procedures.note(TAUGHT)
        with mock.patch.object(procedures.llm_client, "available", return_value=True), \
                mock.patch.object(procedures.study, "_call", return_value={"procedure": 1, "confidence": 0.95}):
            self.assertTrue(procedures.in_request(procedures.apply("usual drill for August please")))
        with mock.patch.object(procedures.llm_client, "available", return_value=True), \
                mock.patch.object(procedures.study, "_call", return_value={"procedure": 1, "confidence": 0.4}):
            self.assertFalse(procedures.in_request(procedures.apply("usual drill for August please")))


if __name__ == "__main__":
    unittest.main()
