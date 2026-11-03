"""A stage its check keeps refusing reaches the person as the points in dispute - what the work says, what the check
says, the source - and a reply that settles them binds the check, so a weak brain plus the person's judgement reaches
the result a strong brain reaches alone (the owner's "intelligence matchup", 2026-10-21)."""

import unittest
from unittest import mock

from decurix_agent import long_task

STEP = {"id": "s1-audit", "title": "Audit every claim line", "instruction": "Check each line against the policy."}
FINDING = ("The check found: CL-4113 allowed Rs 720 but the policy's Tier 1 meal limit is Rs 756 a day; "
           "CL-4125 has no reason for its cut.")
POINTS = [{"point": "CL-4113 allowed amount", "worker": "Rs 720 (Tier 2 limit)", "check": "Rs 756 (Tier 1 city)",
           "source": "Policy 4.2: Tier 1 cities - meals Rs 756 a day"},
          {"point": "CL-4125 reason", "worker": "cut with no reason", "check": "needs a reason", "source": ""}]


class StuckDisputes(unittest.TestCase):
    def test_the_person_is_asked_to_settle_each_point_with_its_source(self):
        text = long_task.stuck_question(STEP, 2, FINDING, points=POINTS)
        self.assertTrue(long_task.is_stuck_question(text))  # still a stuck stage's question everywhere it is read
        self.assertIn("1. CL-4113 allowed amount - the work says: Rs 720 (Tier 2 limit); the check says: Rs 756", text)
        self.assertIn("Source: Policy 4.2", text)
        self.assertIn("Which is right?", text)
        self.assertIn("\"try again\"", text)

    def test_without_points_the_finding_is_given_as_it_is(self):
        text = long_task.stuck_question(STEP, 2, FINDING)
        self.assertIn("What the check found: CL-4113 allowed Rs 720", text)
        self.assertNotIn("Which is right?", text)

    def test_the_points_come_from_one_reading_and_bad_ones_are_dropped(self):
        found = {"points": POINTS + [{"point": "a tool error", "worker": "", "check": "x"}, "junk"]}
        with mock.patch.object(long_task, "_ask_json", lambda *a, **k: found), \
                mock.patch.object(long_task, "_charge", lambda *a: None):
            self.assertEqual([p["point"] for p in long_task.dispute_points("run", STEP, FINDING, "report")],
                             ["CL-4113 allowed amount", "CL-4125 reason"])

        def broken(*a, **k):
            raise RuntimeError("no model")
        with mock.patch.object(long_task, "_ask_json", broken), mock.patch.object(long_task, "_charge", lambda *a: None):
            self.assertEqual(long_task.dispute_points("run", STEP, FINDING, "report"), [])

    def test_every_reply_binds_the_check_but_a_bare_retry_skip_or_stop(self):
        # E-20 (review 2026-11-03): a word count decided it, so "use 18%" and "no, it is 5%" never reached the check.
        # The reply's one reading (stuck_choice, kept with the decision) decides.
        question = long_task.stuck_question(STEP, 2, FINDING, points=POINTS)
        binds = [("1: the work is right, Tier 2 applies to Pune", "answer", ""), ("use 18%", "unclear", ""),
                 ("no, it is 5%", "answer", ""), ("try again with Rs 500", "retry", "with Rs 500")]
        go_on = [("try again", "retry", ""), ("skip it", "skip", ""), ("stop", "stop", "")]
        data = {"decisions": [{"question": question, "answer": a, "choice": c, "instruction": i}
                              for a, c, i in binds + go_on], "amendments": []}
        text = long_task.binding_text(data)
        for said, _, _ in binds:
            self.assertIn(f"the person settled it: \"{said}\"", text)
        for said, _, _ in go_on:
            self.assertNotIn(f"the person settled it: \"{said}\"", text)


if __name__ == "__main__":
    unittest.main()
