"""
Finding BM (2026-10-20, round 5 P4): a portal stage given only the previous stage's output asked the person for the
joiners' mobile numbers - a column of joiners_tracker_Nov2026.xlsx, which the request had attached. Every stage is now
told the request's own input files, and a question for a column of one of them goes back to the stage with where it is.
"""

from needs import needs
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import attachments, config, long_task

HEADERS = ["Employee", "Department", "Mobile", "DOB", "Personal email", "Bank", "Account no", "IFSC"]


@needs("openpyxl")
class Stages(base.LongRunBase):
    def request(self):
        import openpyxl
        folder = Path(config.WORK_DIR) / "P4_onboarding_Nov"
        folder.mkdir(parents=True, exist_ok=True)
        tracker = folder / "joiners_tracker_Nov2026.xlsx"
        book = openpyxl.Workbook()
        book.active.append(HEADERS)
        book.active.append(["Asha Rao", "Sales", "98200 11111", "1994-02-03", "asha@example.com", "HDFC", "123456", "HDFC0001"])
        book.save(tracker)
        self.tracker = tracker
        self.policy = folder / "portal_guide.txt"
        self.policy.write_text("How to use the HR portal.", encoding="utf-8")
        return ("Onboard the November joiners: enter each in the HR portal and write the welcome letters.\n\n"
                + attachments.MARK + f"\n  {tracker}\n  {self.policy}\n")

    def use_plan(self):
        # The planner gave the portal stage one input - the guide - and not the tracker (the live shape).
        self.plan = {"long": True, "title": "Onboarding", "stages": [
            {"id": "prep", "title": "Joiner list", "does": "Make joiners.csv.", "outputs": [{"file": "joiners.csv"}]},
            {"id": "portal", "title": "Enter joiners in the portal", "does": "Enter each joiner in the portal.",
             "inputs": ["portal_guide.txt"], "outputs": [{"file": "portal_log.csv"}], "depends_on": ["prep"]}]}

    def test_every_stage_is_told_the_requests_own_inputs(self):
        self.use_plan()
        long_task.perform(self.request(), None)
        portal = [p for p in self.engine.prompts if p.startswith("Stage 2 of 2")][-1]
        self.assertIn("The request's own input files - sources this stage may read", portal)
        self.assertIn(str(self.tracker), portal)

    def test_a_question_for_a_column_of_an_input_goes_back_with_where_it_is(self):
        self.use_plan()
        self.route_reply = {"for": "IN_INPUTS", "pointer": "Mobile"}
        self.engine.asks["s2-portal"] = ("I'm blocked because the portal onboarding form needs fields the provided stage "
                                         "output does not include. On step 1 it requires at least Mobile phone.")
        long_task.perform(self.request(), None)
        asked = [s["reply"] for s in self.sent if s["kind"] == "query"]
        self.assertFalse(any("Mobile phone" in a for a in asked), asked)
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 2 of 2")][-1]
        self.assertIn("The request's own inputs have it: Mobile is a column of joiners_tracker_Nov2026.xlsx", prompt)
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s2-portal"]["status"], "completed")

    def test_a_question_for_what_no_input_has_reaches_the_person(self):
        self.use_plan()
        self.engine.asks["s2-portal"] = "The portal needs each joiner's PAN, which none of the files gives. What are they?"
        long_task.perform(self.request(), None)
        asked = [s["reply"] for s in self.sent if s["kind"] == "query"]
        self.assertTrue(any("PAN" in a for a in asked), asked)

    def test_general_words_are_not_taken_for_columns(self):
        self.request()
        request = "Onboard them.\n\n" + attachments.MARK + f"\n  {self.tracker}\n"
        self.assertEqual(long_task.fields_in_inputs("Which department's portal should I use?", request),
                         ["Department is a column of joiners_tracker_Nov2026.xlsx"])
        self.assertEqual(long_task.fields_in_inputs("What name should the form have?", request), [])


if __name__ == "__main__":
    unittest.main()
