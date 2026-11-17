"""
One question is put to the person at most twice in a task; asked a third time, the engine is given their answer and
told to finish or say what is missing (engine_questions._question_gate). Found in the learning curves (2026-11-17): a
repair asked the same question again and again, each time answered "I don't know that", until the case timed out.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import engine_questions, guidance, state  # noqa: E402

ASKED = "The Period list has no March after choosing Quarter 4. How should I proceed?"


class AskedAgain(unittest.TestCase):
    def setUp(self):
        for p in (mock.patch.object(state, "paused_skill", None), mock.patch.object(state, "paused_replay", None),
                  mock.patch("decurix_agent.long_task.active", return_value=False),
                  mock.patch("decurix_agent.task_context.current_stage", return_value=(None, None))):
            p.start()
            self.addCleanup(p.stop)
        self.engine = "thebot"

    def test_a_third_ask_is_answered_from_the_first_two(self):
        said = [(ASKED, "I don't know that."), (ASKED, "I don't know that.")]
        with mock.patch.object(guidance, "exchanges", return_value=said), \
                mock.patch("decurix_agent.harness.continue_after_query") as resumed:
            self.assertTrue(engine_questions._question_gate(ASKED, "query", self.engine))
        text = resumed.call_args.args[0]
        self.assertIn("asked the person this 2 times", text)
        self.assertIn("I don't know that.", text)
        self.assertIn("Do not ask it again", text)

    def test_asked_once_it_still_goes_to_the_person(self):
        with mock.patch.object(guidance, "exchanges", return_value=[(ASKED, "no idea")]), \
                mock.patch.object(engine_questions, "_answer_engine_repeat") as repeated:
            engine_questions._question_gate(ASKED, "query", self.engine)
        repeated.assert_not_called()

    def test_another_question_is_not_this_one(self):
        self.assertFalse(engine_questions._same_question(ASKED, "Which signatory should file the return?"))
        self.assertTrue(engine_questions._same_question(ASKED, ASKED + "  "))


    def test_a_repair_past_its_budget_reports_and_asks_nobody(self):
        # Found (2026-11-17): a repair that spent its actions asked the person for more, again and again.
        with mock.patch.object(guidance, "exchanges", return_value=[]),                 mock.patch("decurix_agent.repairs.budget_refusal", return_value="this repair has used its 12 actions"),                 mock.patch("decurix_agent.harness.continue_after_query") as resumed:
            self.assertTrue(engine_questions._question_gate("Can the action limit be removed?", "query", self.engine))
        self.assertIn("end it now with your report", resumed.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
