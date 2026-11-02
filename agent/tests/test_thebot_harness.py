import unittest
from unittest.mock import patch

from decurix_agent import state, thebot_harness


def _answered(text):
    return {"outcome": "answered", "finalAnswer": text}


class TestWrittenApprovalPause(unittest.TestCase):
    """
    A NEEDS_APPROVAL: the model wrote in its own answer pauses the task in
    Decurix only - soul finished that job and holds no checkpoint - so the
    reply must carry the original task back, not be run on its own.
    """

    def setUp(self):
        self.calls = []
        self.queries = []
        self.finals = []
        self.jobs = []

        def fake_run(request, reply, extra_context="", approval_reading=None):
            call = {"request": request, "reply": reply, "context": extra_context}
            if approval_reading is not None:
                call["reading"] = approval_reading
            self.calls.append(call)
            return self.jobs.pop(0), None

        self.patches = [
            patch.object(thebot_harness, "_run_thebot", side_effect=fake_run),
            patch.object(thebot_harness, "ask_for_query", side_effect=lambda t, **k: self.queries.append((t, k.get("kind")))),
            patch.object(thebot_harness, "write_final_reply", side_effect=lambda t, **k: self.finals.append(t)),
            patch.object(thebot_harness.usage, "set_model"),
            patch.object(thebot_harness.usage, "add"),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        thebot_harness._written_pause = None
        state.current_task = None
        state.waiting_for_query = False

    def test_approval_sends_the_original_task_again_with_the_answer(self):
        self.jobs = [
            _answered('NEEDS_APPROVAL: I want to Google "Sanmayee is great".'),
            _answered("Top result: ..."),
        ]

        thebot_harness.perform_task('google "sanmayee is great"', None)
        self.assertEqual(self.queries, [('I want to Google "Sanmayee is great".', "approval")])

        thebot_harness.continue_after_query("proceed")

        second = self.calls[1]
        self.assertEqual(second["request"], 'google "sanmayee is great"')
        self.assertFalse(second["reply"])
        self.assertIn("that is approval", second["context"])
        self.assertIn("proceed", second["context"])
        # A yes is that one action's, never a waiver for the rest of the task (review 2026-11-02).
        self.assertIn("for that one action, once", second["context"])
        self.assertNotIn("do not ask for approval", second["context"].lower())
        self.assertIn("still needs its own approval", second["context"])
        self.assertEqual(self.finals, ["Top result: ..."])

    def test_a_non_approval_answer_is_passed_as_an_answer(self):
        self.jobs = [_answered("NEEDS_APPROVAL: Which account?"), _answered("done")]

        thebot_harness.perform_task("pay the bill", None)
        thebot_harness.continue_after_query("no, use the other one")

        self.assertNotIn("that is approval", self.calls[1]["context"])
        self.assertIn("no, use the other one", self.calls[1]["context"])

    def test_a_second_written_pause_still_carries_the_first_task(self):
        self.jobs = [
            _answered("NEEDS_APPROVAL: first"),
            _answered("NEEDS_APPROVAL: second"),
            _answered("finally"),
        ]

        thebot_harness.perform_task("the task", None)
        thebot_harness.continue_after_query("yes")
        thebot_harness.continue_after_query("yes")

        self.assertEqual([c["request"] for c in self.calls], ["the task", "the task", "the task"])
        self.assertIn('"second"', self.calls[2]["context"])

    def test_a_pause_soul_holds_is_resumed_in_soul(self):
        self.jobs = [
            {"outcome": "needs_approval", "question": "run rm -rf build?"},
            _answered("removed"),
        ]

        thebot_harness.perform_task("clean the build", None)
        thebot_harness.continue_after_query("yes")

        # The reply goes with its one reading, which soul acts on.
        self.assertEqual(self.calls[1], {"request": "yes", "reply": True, "context": "",
                                         "reading": {"decision": "yes", "condition": ""}})

    def test_a_new_task_forgets_a_written_pause(self):
        self.jobs = [_answered("NEEDS_APPROVAL: x"), _answered("ok")]

        thebot_harness.perform_task("one", None)
        thebot_harness.perform_task("two", None)

        self.assertIsNone(thebot_harness._written_pause)


if __name__ == "__main__":
    unittest.main()


class TestThePlanThePersonAnsweredIsCarriedThroughTheRequest(unittest.TestCase):
    """Found live (2026-10-24): the final check sent a task back, soul started a new goal and put the same plan of the
    work to the person again, half an hour after they had answered it ("ya same, carry on")."""

    PLAN = "Before I change anything, this is how I will do it:\n- Covering: file both returns\n- From: the workbook"

    def setUp(self):
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        self.request = "t1"
        patcher = patch.object(thebot_harness, "_request_id", side_effect=lambda: self.request)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_later_job_of_the_request_gets_the_plan_and_the_answer_as_agreed(self):
        from decurix_agent.long_task import PLAN_AGREED_MARKER
        self.assertEqual(thebot_harness.plan_context(), "")
        thebot_harness.note_question(self.PLAN)
        self.assertEqual(thebot_harness.plan_context(), "")      # not answered yet
        thebot_harness.note_reply("haan ok, but read his messages too")
        said = thebot_harness.plan_context()
        self.assertTrue(said.startswith(PLAN_AGREED_MARKER))
        self.assertIn("- Covering: file both returns", said)
        self.assertIn("Their answer: haan ok, but read his messages too", said)
        # Answers to later questions are not the plan's.
        thebot_harness.note_question("I need your OK before I click FILE.")
        thebot_harness.note_reply("yes")
        self.assertIn("haan ok, but read his messages too", thebot_harness.plan_context())

    def test_another_request_does_not_get_it(self):
        thebot_harness.note_question(self.PLAN)
        thebot_harness.note_reply("ok")
        self.request = "t2"
        self.assertEqual(thebot_harness.plan_context(), "")

    def test_the_reply_to_another_question_is_not_taken_for_the_plans(self):
        thebot_harness.note_question(self.PLAN)
        thebot_harness.note_question("Which client?")
        thebot_harness.note_reply("the first one")
        self.assertEqual(thebot_harness.plan_context(), "")

    def test_the_question_soul_writes_is_the_one_recognised(self):
        import pathlib
        go = (pathlib.Path(__file__).resolve().parents[1] / "thebot/internal/skills/plan.go").read_text(encoding="utf-8")
        self.assertIn(thebot_harness.PLAN_QUESTION.replace("\n", ""), go)
