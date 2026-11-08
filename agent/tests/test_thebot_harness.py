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


class TestTheOrganisationsKnowledgeReachesEveryRequest(unittest.TestCase):
    """An org rule ("never pay without a PO") reached only skill runs and `gui record`: a request no skill covers
    never saw it. It now goes with every job as standing context, read once per task."""

    def setUp(self):
        from decurix_agent import guidance, org_knowledge, task_context
        thebot_harness._org_for_task.update(task=None, lines=[])
        self.addCleanup(thebot_harness._org_for_task.update, task=None, lines=[])
        self.task = "t0000000000000001"
        self.lines = ["Rule (org): never pay without a purchase order", "Fact (task): the client is Ledgerly"]
        self.asked = []
        self.guided = ""
        patches = [
            patch.object(task_context, "current", side_effect=lambda: self.task),
            patch.object(org_knowledge, "cached",
                         side_effect=lambda worker_id=None, max_age=0, task_id=None:
                         self.asked.append(task_id) or list(self.lines)),
            patch.object(thebot_harness, "get_identity_context", return_value="IDENTITY"),
            patch.object(thebot_harness, "get_memory_context", return_value=""),
            patch.object(thebot_harness, "taught_context", return_value="TAUGHT"),
            patch.object(thebot_harness.app_skills, "context", return_value=""),
            patch.object(guidance, "text", side_effect=lambda: self.guided),
            patch.object(guidance, "get", return_value={}),
            patch.object(thebot_harness, "plan_context", return_value=""),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_the_lines_are_in_the_context_not_the_request(self):
        context = thebot_harness._build_context()
        self.assertIn("- Rule (org): never pay without a purchase order", context)
        self.assertIn("- Fact (task): the client is Ledgerly", context)
        self.assertLess(context.index("TAUGHT"), context.index("never pay without"))
        self.assertLess(context.index("never pay without"), context.index(thebot_harness.SAFETY_PREAMBLE.strip()[:40]))
        self.assertEqual(self.asked, [self.task])

    def test_every_job_of_a_task_sends_the_same_lines(self):
        first = thebot_harness._build_context()
        self.lines = ["Rule (org): something added while the task ran"]
        self.assertEqual(thebot_harness._build_context(), first)
        self.assertEqual(self.asked, [self.task])
        self.task = "t0000000000000002"  # the next task reads them again
        self.assertIn("something added while the task ran", thebot_harness._build_context())

    def test_a_taught_procedure_that_carries_them_does_not_get_them_twice(self):
        self.guided = "PROCEDURE\nRule (org): never pay without a purchase order\nFact (task): the client is Ledgerly"
        context = thebot_harness._build_context()
        self.assertEqual(context.count("never pay without a purchase order"), 1)
        self.assertNotIn(thebot_harness.org_knowledge.CONTEXT_HEADER, context)

    def test_nothing_known_adds_nothing(self):
        self.lines = []
        self.assertNotIn(thebot_harness.org_knowledge.CONTEXT_HEADER, thebot_harness._build_context())

    def test_a_secret_in_a_line_is_withheld(self):
        self.lines = ["Fact (org): the portal password: Zq9-hunter22"]
        self.assertNotIn("Zq9-hunter22", thebot_harness._build_context())

    def test_the_operator_prompt_names_handoff_who(self):
        import pathlib
        go = (pathlib.Path(__file__).resolve().parents[1] / "thebot/internal/skills/operator.go").read_text(encoding="utf-8")
        self.assertIn("handoff who", go)
