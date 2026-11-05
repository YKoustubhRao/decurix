"""
Whether a task starts without asking is the person's decision, made once while it is taught (learning._approval) -
not a guess from the words of a later request ("Purchase Invoice" is a document, not a purchase).
"""

import unittest
from unittest import mock

from decurix_agent import skill_task, state, thebot_harness
from decurix_agent.computer import learning, skills

from test_skills import TeachingBase


class Choosing(unittest.TestCase):
    def test_what_a_person_says(self):
        def chosen(text):
            return learning._choose("approval", text)

        for text in ("ask first", "Ask me each time", "check with me please", "I want to approve it"):
            self.assertEqual(chosen(text), "ask", text)
        for text in ("go ahead", "just do it", "don't ask", "do not ask me", "no need to ask", "without asking"):
            self.assertEqual(chosen(text), "go", text)
        # Neither, or both: asked again, never guessed.
        for text in ("yes", "hmm", "ask first but also go ahead"):
            self.assertIsNone(chosen(text), text)

    def test_an_unclear_answer_takes_the_cautious_one_after_two_tries(self):
        skill = learning.build([self._demo()])
        question = learning._approval()
        self.assertFalse(learning.answer(skill, question, "hmm")["understood"])
        result = learning.answer(skill, question, "dunno")
        self.assertTrue(result["understood"])
        self.assertEqual(skill["approval"], "ask")

    def test_it_is_asked_last_before_the_name(self):
        skill = learning.build([self._demo()])
        ids = [q["id"] for q in learning.questions(skill)]
        self.assertEqual(ids[-2:], ["approval", "confirm"])

    def test_the_last_question_says_what_was_chosen(self):
        skill = learning.build([self._demo()])
        skill["approval"] = "go"
        self.assertIn("without checking with you first", learning._confirm(skill)["text"])
        skill["approval"] = "ask"
        self.assertIn("check with you for a yes before I start", learning._confirm(skill)["text"])

    @staticmethod
    def _demo():
        from test_skills import demo
        return demo("demo-20260914T101500-aaaaaa", "Acme")


class Running(TeachingBase):
    def taught(self, approval):
        self.teach("the customer is in the list", "yes", "changes", "yes")
        skill = skills.load("add-a-customer")
        skill["approval"] = approval
        skills.save(skill)
        self.screen_fake = self.screen()
        self.asked.clear()

    def test_a_task_taught_to_ask_first_waits_for_a_yes_and_then_runs(self):
        self.taught("ask")
        skill_task.perform('/skill add-a-customer customer_name="Globex"')

        self.assertEqual(self.asked[-1][1], "approval")
        self.assertIn("check with you before I start", self.asked[-1][0])
        self.assertIn("Globex", self.asked[-1][0])
        self.assertEqual(self.screen_fake.acted, [])

        skill_task.resume("yes")
        self.assertTrue(self.screen_fake.acted)

    def test_a_no_to_starting_does_nothing(self):
        self.taught("ask")
        skill_task.perform('/skill add-a-customer customer_name="Globex"')
        skill_task.resume("no")

        self.assertIn("Stopped, as you said, before starting", self.finals[-1][0])
        self.assertEqual(self.screen_fake.acted, [])

    def test_a_task_taught_to_go_ahead_just_runs(self):
        self.taught("go")
        skill_task.perform('/skill add-a-customer customer_name="Globex"')
        self.assertTrue(self.screen_fake.acted)
        self.assertFalse(any(kind == "approval" for _, kind in self.asked))

    def test_a_task_taught_before_this_was_asked_runs_as_it_always_did(self):
        self.taught(None)
        skill_task.perform('/skill add-a-customer customer_name="Globex"')
        self.assertTrue(self.screen_fake.acted)
        self.assertFalse(any(kind == "approval" for _, kind in self.asked))


class TellingTheEngine(unittest.TestCase):
    def preapproved(self, skill, run_extra=None):
        seen = []
        run = dict({"skill": dict({"name": "n", "title": "T"}, **skill), "values": {}}, **(run_extra or {}))
        with mock.patch.object(skill_task, "_engine", side_effect=lambda c, f: seen.append(state.request_preapproved)), \
                mock.patch.object(skill_task.guidance, "begin"), \
                mock.patch.object(skill_task, "_procedure", return_value="x"), \
                mock.patch.object(skill_task, "progress"):
            skill_task._by_model(run)
        self.assertFalse(state.request_preapproved, "the flag is for one request only")
        return seen[0]

    def test_the_engine_is_told_only_when_the_person_said_so(self):
        self.assertTrue(self.preapproved({"approval": "go"}))
        self.assertTrue(self.preapproved({"approval": "ask"}, {"approved": True}))
        self.assertFalse(self.preapproved({"approval": "ask"}))
        self.assertFalse(self.preapproved({"approval": None}))

    def test_thebot_is_never_sent_an_approval_in_the_body(self):
        # A yes is one action's (CLAUDE.md): soul takes no approval from a request body, so none is sent - not for a
        # preapproved request, not for a reply (review 2026-11-02 removed the field).
        sent = []

        def post(url, body, *a, **k):
            sent.append(body)
            return {}

        self.addCleanup(setattr, state, "request_preapproved", False)
        for pre, reply in ((True, False), (True, True), (False, False)):
            state.request_preapproved = pre
            with mock.patch.object(thebot_harness, "_post_json", post):
                thebot_harness._run_thebot("do it", reply=reply)
            self.assertNotIn("approved", sent[-1], (pre, reply))


if __name__ == "__main__":
    unittest.main()


class ConfirmingAnUncheckedResult(TeachingBase):
    """"I cannot confirm this is done" is answered by words about the result, not by a yes meant for something else."""

    def ask(self):
        from decurix_agent import verify
        unsure = {"verdict": verify.UNSURE, "seen": "the rows are not visible", "why": "", "confidence": 0.4}
        with mock.patch.object(skill_task.verify, "judge", return_value=unsure), \
                mock.patch.object(skill_task.evidence, "live", lambda scale=1: None), \
                mock.patch.object(skill_task.evidence, "observed", lambda task_id=None: []):
            skill_task.check_unchecked("Entered the bill.", "skill")
        self.assertIn("cannot confirm", self.asked[-1][0].lower())

    def test_a_plain_yes_asks_again_and_done_confirms(self):
        self.ask()
        skill_task.resume("yes - that only opens the list")
        self.assertEqual(self.finals, [])
        self.assertIn("reply `done`", self.asked[-1][0])
        self.assertIsNotNone(state.paused_skill)

        skill_task.resume("done")
        self.assertIn("You confirmed it was done", self.finals[-1][0])

    def test_saying_it_worked_confirms(self):
        self.ask()
        skill_task.resume("it worked")
        self.assertIn("You confirmed it was done", self.finals[-1][0])
