"""The workflow brief (2026-10-27): a lesson settles every topic a run of the work turns on - from the recording, as a
guess the person confirms, or by asking - and the person sees where each rule came from."""
import unittest

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import study
from decurix_agent.computer import learning


class TheBrief(unittest.TestCase):
    def test_every_topic_is_there_and_a_missing_one_is_unknown(self):
        brief = study._brief([
            {"topic": "filing", "rule": "Keep the files in the month's folder, named like the earlier ones.",
             "basis": "seen", "evidence": "step 4: the folder list"},
            {"topic": "matching", "rule": "Find the customer by identifier first.", "basis": "assumed"},
            {"topic": "changes", "rule": "", "basis": "seen"},
            {"topic": "nonsense", "rule": "x", "basis": "seen"},
        ])
        self.assertEqual([b["topic"] for b in brief], study.BRIEF_IDS)
        by = {b["topic"]: b for b in brief}
        self.assertEqual(by["filing"]["basis"], "seen")
        self.assertEqual(by["matching"]["basis"], "assumed")
        self.assertEqual(by["changes"]["basis"], "unknown")   # no rule: unknown, whatever it claimed
        self.assertEqual(by["sources"]["basis"], "unknown")   # left out: unknown

    def test_guesses_are_marked_and_unknowns_are_asked_first(self):
        brief = study._brief([
            {"topic": "filing", "rule": "Keep the files in the month's folder.", "basis": "seen", "evidence": "step 4"},
            {"topic": "matching", "rule": "Find the customer by identifier first.", "basis": "assumed"},
        ])
        rules = study._brief_rules(brief)
        self.assertEqual(rules, ["[filing] Keep the files in the month's folder.",
                                 "[matching] Find the customer by identifier first. (my assumption)"])
        own = [{"id": "q1", "text": "Is step 7 always needed?", "why": "", "topic": ""},
               {"id": "q2", "text": "What if a bill is cancelled?", "why": "", "topic": "changes"}]
        asked = study._brief_questions(brief, own)
        ids = [q["id"] for q in asked]
        self.assertNotIn("b_changes", ids)                    # the model's own question covers it
        self.assertIn("b_sources", ids)
        self.assertIn("b_done", ids)
        self.assertEqual(ids[-2:], ["q1", "q2"])               # the brief's gaps first

    def test_the_person_sees_where_each_rule_came_from_and_a_yes_confirms_the_guesses(self):
        brief = study._brief([
            {"topic": "filing", "rule": "Keep the files in the month's folder.", "basis": "seen", "evidence": "step 4"},
            {"topic": "matching", "rule": "Find the customer by identifier first.", "basis": "assumed"},
        ])
        skill = {"understanding": {"summary": "Enter the month's bills.", "brief": brief,
                                   "rules": study._brief_rules(brief)}}
        shown = learning.understood_text(skill)
        self.assertIn("1. [filing] Keep the files in the month's folder. - seen: step 4", shown)
        self.assertIn("2. [matching] Find the customer by identifier first. - my guess, please confirm", shown)
        self.assertIn("Still not settled - tell me if you can: sources", shown)  # (no question waits on it)
        study.confirm(skill)
        self.assertNotIn("(my assumption)", " ".join(skill["understanding"]["rules"]))
        self.assertEqual({b["topic"]: b["basis"] for b in skill["understanding"]["brief"]}["matching"], "confirmed")


class AnAnswerSettlesItsTopic(unittest.TestCase):
    """Found live (2026-10-27): a topic's question was settled by an earlier answer; the brief still said 'asked next'."""

    def test_the_topic_of_an_answered_question_is_answered(self):
        brief = study._brief([{"topic": "filing", "rule": "Keep files in the month folder.", "basis": "seen"}])
        u = {"summary": "s", "brief": brief, "rules": study._brief_rules(brief),
             "questions": [{"id": "b_exceptions", "topic": "exceptions", "text": "What if a value cannot be read?"}]}
        study._settle_topics(u, {"exceptions"}, "What if a value cannot be read?", "Leave it out and tell me.")
        by = {b["topic"]: b for b in u["brief"]}
        self.assertEqual(by["exceptions"]["basis"], "answered")
        self.assertIn("[exceptions] Leave it out and tell me.", u["rules"])
        shown = learning.understood_text({"understanding": u, "answers": {"study:b_exceptions": "x"}})
        self.assertIn("[exceptions] Leave it out and tell me. - as you told me", shown)
        self.assertNotIn("asked next: exceptions", shown)
        self.assertIn("Still not settled - tell me if you can: sources", shown)


if __name__ == "__main__":
    unittest.main()
