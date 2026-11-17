"""
A plain yes backs what it said yes to (records._answers_text): the person who answers "Yes, go ahead." to a plan that
writes each item's values has given those values, so a saved field the plan named is backed by the person. A yes with
anything more (a change, a condition) backs only its own words, and a no backs nothing of the question.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import person_record  # noqa: E402
from decurix_agent.computer import approval, records  # noqa: E402

PLAN = ('Before I change anything, this is how I will do it:\n'
        '- Item 1: bill_number="1054"; party_name="Sharada Residency"; sales_ledger="Sales@18%"\n'
        "Reply `yes` to go ahead, or say what to change.")


def answered(text):
    return mock.patch.object(person_record, "answers", lambda task_id=None: [{"id": "j1", "asked": PLAN, "text": text}])


class PlainYes(unittest.TestCase):
    def test_plain_yes_is_only_yes(self):
        for text in ("Yes, go ahead.", "ok", "yes", "Go ahead", "Sure, proceed."):
            self.assertTrue(approval.plain_yes(text), text)
        for text in ("", "No", "yes but use Sales@12%", "yes, if the totals match", "don't", "use Sales@12%"):
            self.assertFalse(approval.plain_yes(text), text)

    def test_a_plain_yes_backs_the_plan_it_approved(self):
        with answered("Yes, go ahead."):
            text = records._answers_text("t1")
        self.assertIn("Yes, go ahead.", text)
        self.assertTrue(records.stands_in("Sales@18%", text))

    def test_a_yes_with_a_change_backs_only_its_own_words(self):
        with answered("Yes, but the ledger is Sales@12%"):
            text = records._answers_text("t1")
        self.assertFalse(records.stands_in("Sales@18%", text))
        self.assertTrue(records.stands_in("Sales@12%", text))

    def test_a_no_backs_nothing_of_the_question(self):
        with answered("No, stop."):
            self.assertNotIn("Sales@18%", records._answers_text("t1"))

    def test_the_persons_own_words_leave_out_the_plan_they_said_yes_to(self):
        # Found live (2026-11-16): "1079 is missing ... or proceed?" in a plan said yes to was taken as the person
        # naming the missing bill, and it was never asked for.
        with answered("Yes, go ahead."):
            self.assertEqual(records._answers_text("t1", approved=False), "Yes, go ahead.")

    def test_a_yes_with_no_question_recorded_backs_only_itself(self):
        with mock.patch.object(person_record, "answers", lambda task_id=None: [{"id": "j1", "asked": "", "text": "yes"}]):
            self.assertEqual(records._answers_text("t1"), "yes")


if __name__ == "__main__":
    unittest.main()
