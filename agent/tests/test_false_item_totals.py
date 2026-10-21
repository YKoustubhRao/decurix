"""
Finding AX (2026-10-20, P2): a stage told the person a saved order was wrong because its lines "should total 13129.00
ex GST from WB-500 40 @ 175.75, MUG-12 24 @ 149, NB-A5 30 @ 95" - they come to 13,456.00, the record's own total.
A stated total of a priced list is checked in code; a stage is sent back to recheck before telling the person
anything is wrong; any other question says what the figures really come to.

    PYTHONPATH="agent;agent/tests" python -m unittest test_false_item_totals
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import long_task, messaging, verify

LIVE = ("A record saved in the application is wrong and was left so: SO-5101 - portal total 13456.00 does not match the "
        "approved allocated Cafe Bloom lines, which should total 13129.00 ex GST from WB-500 40 @ 175.75, MUG-12 24 @ "
        "149, NB-A5 30 @ 95 - the person must open SO-5101 in the portal and correct the saved lines manually")


class PricedListTotals(unittest.TestCase):
    def test_the_live_claim_is_caught(self):
        found = verify.false_sums(LIVE)
        self.assertEqual(found, ["the total 13129.00 stated for 40 @ 175.75, 24 @ 149, 30 @ 95 is wrong: those come "
                                 "to 13,456.00"])

    def test_right_totals_pass(self):
        for text in (LIVE.replace("13129.00", "13456.00"),
                     "The lines total Rs 15,878.08 incl 18% GST from 40 @ 175.75, 24 @ 149, 30 @ 95.",
                     "Invoice totals 1,234.50 for 2 x 500, 1 x 234.50.",
                     "Totals of 2 orders from 3 x 4 and 5 x 6.",  # a count, not an amount
                     "A total of 500 from one line: 2 @ 250."):  # one item is not a list
            self.assertEqual(verify.false_sums(text), [], text)

    def test_a_stage_report_with_it_is_not_accepted(self):
        problems = long_task.code_problems([], LIVE, [], "Enter the orders in the portal.")
        self.assertTrue(any("13,456.00" in p for p in problems), problems)


class AQuestionWithFalseFiguresIsNotPassedOn(unittest.TestCase):
    def test_a_plain_question_says_what_the_figures_come_to(self):
        sent = []
        with mock.patch.object(messaging, "send_query", lambda filename, content, kind="", reply="", **k: sent.append(reply) or True), \
                mock.patch.object(messaging, "_query_gate", None), \
                mock.patch.object(long_task, "take_question", lambda problem, kind: False):
            messaging.ask_for_query(LIVE)
        self.assertTrue(sent)
        self.assertIn("Checked before asking you: the total 13129.00 stated for", sent[-1])
        self.assertIn("those come to 13,456.00", sent[-1])


class AStageIsSentBackToRecheck(unittest.TestCase):
    setUp = base.LongRun.setUp
    restore = base.LongRun.restore
    model = base.LongRun.model
    send = base.LongRun.send
    finals = base.LongRun.finals
    message = base.LongRun.message
    run_id = base.LongRun.run_id

    def test_the_live_question_goes_back_to_the_stage_once_and_never_to_the_person(self):
        self.engine.asks["s1-register"] = LIVE
        long_task.perform(base.LONG, None)
        asked = [s["reply"] for s in self.sent if s["kind"] in ("query", "approval")]
        self.assertFalse(any("should total 13129.00" in a for a in asked), asked)
        data = long_task.ledger(self.run_id())
        self.assertEqual(data["false_figures"]["s1-register"], 1)
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertTrue(any("13,456.00" in line for line in self.emitted))
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s1-register"]["status"], "completed")
        self.assertIn("Your own figures say otherwise: the total 13129.00 stated for", prompt)
        self.assertIn("Recheck them with a command over the sources", prompt)


if __name__ == "__main__":
    unittest.main()
