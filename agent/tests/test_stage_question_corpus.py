"""
A stage's question goes, in this order: what only the person can give never goes anywhere else (person_keeps); figures
it gives that do not add up are rechecked (own arithmetic, in code); then ONE reading by the main brain says whom it is
for (long_task.route_question - harness audit, 2026-10-21: six phrase lists kept missing the next wording, and fought).
Code checks only that a pointer the model gives is the request's own sentence, system, file or column, word for word.

Every live wording in fixtures/stage_questions.json runs through the whole of long_task.take_question here, the model
standing in with the reading the case records: whatever route it gives lands where it must, a pointer that is not the
request's own goes to the person, and a question reaches the person after at most MAX_SEND_BACKS send-backs.
"""

import json
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import long_task, workflow_task

CORPUS_PATH = fixture_paths.fixture("stage_questions.json")
CORPUS = json.loads(CORPUS_PATH.read_text(encoding="utf-8")) if CORPUS_PATH.is_file() else {"requests": {}, "cases": []}
RUN, STEP = "run-corpus", "s2-work"
# The reading the main brain gives for each outcome the corpus records.
ROUTE_OF = {"settled": "SETTLED", "own_reading": "OWN_READING", "source": "SOURCE", "own_tools": "OWN_TOOLS",
            "unchecked": "UNCHECKED", "in_inputs": "IN_INPUTS", "person": "PERSON", "false_figures": "PERSON",
            "carry_on": "PERSON", "approval": "PERSON"}


class FakeRunner:
    def __init__(self, decision):
        self.step = {"id": STEP, "title": "The work", "instruction": "Do the work.", "decision": decision}
        self.carried, self.parked = [], []

    def status(self, run_id):
        return {"run_id": RUN, "spec": {"planned": True, "steps": [self.step]},
                "steps": {STEP: {"status": "running", "attempts": 1}}}

    def park(self, run_id, step_id, question, report=None):
        self.parked.append(question)

    def carry_on(self, run_id, step_id, said, field=None):
        self.carried.append((field, said))


class Pipeline:
    """take_question with the run, the ledger and the model stood in; what became of each question."""

    def __init__(self, case, routes=None):
        self.case = case
        self.data = {"request": CORPUS["requests"].get(case["request"], case["request"]), "questions": []}
        self.runner = FakeRunner(bool(case.get("decision")))
        self.to_person, self.model_calls = [], []
        self.routes = list(routes or [])

    def pointer(self, route):
        case = self.case
        if route == "SETTLED":
            return case.get("settled_by", "")
        if route == "SOURCE":
            systems = long_task.request_systems(self.data["request"])
            return systems[0] if systems else ""
        if route == "IN_INPUTS":
            return next(iter(case.get("headers") or {}), "")
        return ""

    def ask_json(self, system, prompt, max_tokens=4000):
        self.model_calls.append(system)
        if system is long_task.ROUTE_SYSTEM:
            route = self.routes.pop(0) if self.routes else ROUTE_OF.get(self.case.get("expect"), "PERSON")
            return {"for": route, "pointer": self.pointer(route), "other_means": False}
        return {}

    def update(self, run_id, change):
        return change(self.data)

    def ask(self, question=None):
        case = self.case
        headers = case.get("headers") or {}
        before = len(self.runner.carried)
        with mock.patch.object(workflow_task, "_active", (RUN, STEP, None)), \
                mock.patch.object(workflow_task, "_pending", None), \
                mock.patch.object(long_task, "_run", {"run_id": RUN, "task_file": None}), \
                mock.patch.object(long_task, "_runner", lambda: self.runner), \
                mock.patch.object(long_task, "ledger", lambda run_id: self.data), \
                mock.patch.object(long_task, "_update", self.update), \
                mock.patch.object(long_task, "_ask_json", self.ask_json), \
                mock.patch.object(long_task, "_charge", lambda *a: None), \
                mock.patch.object(long_task, "_input_headers", lambda request: headers), \
                mock.patch.object(long_task, "run_inputs", lambda request: []), \
                mock.patch.object(long_task, "_decided", lambda *a: False), \
                mock.patch.object(long_task, "_park", lambda run_id, step_id, q, *a, **k: self.to_person.append(q)), \
                mock.patch.object(long_task, "_new_question", lambda *a, **k: "q1"), \
                mock.patch.object(long_task, "_stage_start", {}):
            took = long_task.take_question(question or case["question"], case.get("kind", "query"))
        if len(self.runner.carried) > before:
            return self.layer(*self.runner.carried[-1])
        if self.to_person:
            return "person"
        return "approval" if not took else "?"

    @staticmethod
    def layer(field, said):
        if field == "settled":
            return "settled"
        if field == "sent_back":
            return "own_reading"
        if field == "mechanics":
            return "source" if said.startswith("SOURCE:") else "own_tools"
        if field == "recheck":
            if said.startswith("Check it first:"):
                return "unchecked"
            return "in_inputs" if "inputs have it" in said else "false_figures"
        return "carry_on"


@fixture_paths.skip_unless(CORPUS_PATH.is_file(), f"fixture not here: {CORPUS_PATH}")
class TheCorpus(unittest.TestCase):
    def test_every_live_question_ends_where_its_reading_sends_it(self):
        self.assertGreaterEqual(len(CORPUS["cases"]), 20)
        for case in CORPUS["cases"]:
            with self.subTest(case["name"]):
                self.assertEqual(Pipeline(case).ask(), case["expect"], case["question"][:120])

    def test_what_only_the_person_can_give_is_never_put_to_a_model_or_sent_back(self):
        for case in CORPUS["cases"]:
            if long_task.person_keeps(case["question"]):
                pipeline = Pipeline(case, routes=["OWN_TOOLS"])
                self.assertEqual(pipeline.ask(), "person", case["name"])
                self.assertNotIn(long_task.ROUTE_SYSTEM, pipeline.model_calls, case["name"])

    def test_a_pointer_that_is_not_the_requests_own_goes_to_the_person(self):
        case = {"request": "Enter the orders in the portal (http://localhost:8701).", "question": "Which price?"}
        pipeline = Pipeline(case, routes=["SETTLED"])
        pipeline.case = dict(case, settled_by="Use the catalogue price.")  # (no such sentence in the request)
        self.assertEqual(pipeline.ask(), "person")
        pipeline = Pipeline(dict(case, headers={"Mobile": ["t.xlsx"]}), routes=["IN_INPUTS"])
        pipeline.pointer = lambda route: "Phone number"  # (not a column of the inputs)
        self.assertEqual(pipeline.ask(), "person")

    def test_a_decision_stages_question_is_the_persons_unless_it_is_about_its_own_tools(self):
        case = {"request": "P4", "question": "Which of the two managers should I choose?", "decision": True}
        self.assertEqual(Pipeline(case, routes=["SETTLED"]).ask(), "person")
        self.assertEqual(Pipeline(case, routes=["OWN_TOOLS"]).ask(), "own_tools")

    def test_no_model_no_send_back(self):
        case = {"request": "P4", "question": "The editor window is frozen."}
        pipeline = Pipeline(case)

        def broken(*a, **k):
            raise RuntimeError("no model")
        pipeline.ask_json = broken
        self.assertEqual(pipeline.ask(), "person")


@fixture_paths.skip_unless(CORPUS_PATH.is_file(), f"fixture not here: {CORPUS_PATH}")
class TheBounceCap(unittest.TestCase):
    def test_a_question_reaches_the_person_after_two_send_backs_whatever_routes_fire(self):
        case = {"request": "P4", "headers": {"Mobile": ["joiners_tracker_Nov2026.xlsx"]},
                "question": "My parse of the tracker cannot get the Mobile column, and the editor window is frozen."}
        pipeline = Pipeline(case, routes=["OWN_READING", "OWN_TOOLS", "IN_INPUTS", "IN_INPUTS"])
        outcomes = [pipeline.ask() for _ in range(4)]
        self.assertEqual(outcomes[:2], ["own_reading", "own_tools"])
        self.assertEqual(outcomes[2:], ["person", "person"])
        self.assertEqual(pipeline.data["bounces"][STEP], long_task.MAX_SEND_BACKS)

    def test_an_own_tools_question_asked_again_goes_to_the_person_said_as_what_failed(self):
        case = {"request": "P4", "headers": {"Mobile": ["joiners_tracker_Nov2026.xlsx"]},
                "question": "The editor window is frozen while I type the Mobile column into the form."}
        pipeline = Pipeline(case, routes=["OWN_TOOLS", "OWN_TOOLS"])
        self.assertEqual(pipeline.ask(), "own_tools")
        self.assertEqual(pipeline.ask(), "person")
        self.assertIn("could not get past", pipeline.to_person[-1])

    def test_a_question_about_something_not_yet_checked_is_sent_back_once(self):
        # Round 5 P4 (2026-10-21, DD): asked again after checking, it reaches the person.
        case = {"request": "P4", "question": "I have not yet confirmed whether that exact manager exists as a selectable "
                                             "option in the portal. Which manager should I choose if it is not available?"}
        pipeline = Pipeline(case, routes=["UNCHECKED", "UNCHECKED"])
        self.assertEqual(pipeline.ask(), "unchecked")
        self.assertIn("Ask only if, after checking, it truly isn't", pipeline.runner.carried[-1][1])
        self.assertEqual(pipeline.ask(), "person")


class AHandBack(unittest.TestCase):
    """long_task.handed_back: "use your judgement" (the model's reading: hands_back) answers a question with what it
    proposed - but a decision stage's question stays the person's (round 5 P6, 2026-10-20)."""

    def run_state(self):
        return {"run_id": RUN, "spec": {"planned": True, "steps": [
            {"id": "s1", "title": "Refunds", "instruction": "List refunds over Rs 5,000 for my approval.",
             "decision": True},
            {"id": "s2", "title": "Tracker", "instruction": "Triage every email."}]},
            "steps": {"s1": {"status": "parked"}, "s2": {"status": "parked"}}}

    def test_kept_for_a_decision_and_proposed_default_otherwise(self):
        data = {"questions": [{"id": "q1", "stage": "s1", "text": "Approve the three refunds?"},
                              {"id": "q2", "stage": "s2", "text": "E07 has no order number. I would record it as "
                                                                  "SO-5107 unless you say otherwise."}]}
        verdict = {"answers": [{"question": "q1", "answer": "use your judgement", "hands_back": True},
                               {"question": "q2", "answer": "use your judgement", "hands_back": True}]}
        kept = long_task.handed_back(verdict, data, self.run_state())
        self.assertEqual([a["question"] for a in kept], ["q1"])
        self.assertIn("this is yours to decide: I keep it pending", verdict["answers"][0]["answer"])
        self.assertIn("I go with what I proposed: I would record it as SO-5107", " ".join(verdict["answers"][1]["answer"].split()))
        # A plain answer is left as it is.
        plain = {"answers": [{"question": "q1", "answer": "Approve all three."}]}
        self.assertEqual(long_task.handed_back(plain, data, self.run_state()), [])
        self.assertEqual(plain["answers"][0]["answer"], "Approve all three.")


if __name__ == "__main__":
    unittest.main()


class AlreadyDecided(unittest.TestCase):
    """A question about something the person already decided goes back to the stage with their own words (series 16
    H1, 2026-10-21: CL-4142 approved at Rs 1,200, then asked about again four times in 80 seconds)."""

    ANSWER = ("Approve Neha's airport taxi without the receipt, but pay only Rs 1,200 - that is the prepaid taxi fare "
              "from Delhi airport to her hotel. Note it as approved by me.")
    QUESTION = ("I need your decisions on these approval items: 1. CL-4122 - Kavya Raghavan - shared cab. "
                "2. CL-4142 - Neha Joshi - no receipt: approve at Rs 1,850.00, or decline.")

    def pipeline(self, pointer, decision=False):
        case = {"request": "Audit the expense claims; ask me where the policy needs my approval.",
                "question": self.QUESTION, "decision": decision}
        pipeline = Pipeline(case, routes=["DECIDED"])
        pipeline.data["decisions"] = [{"question": "CL-4142 has no receipt: approve or reject?", "answer": self.ANSWER}]
        pipeline.pointer = lambda route: pointer
        return pipeline

    def test_the_stage_applies_the_answer_and_asks_only_the_rest(self):
        pipeline = self.pipeline("pay only Rs 1,200 - that is the prepaid taxi fare from Delhi airport")
        self.assertEqual(pipeline.ask(), "settled")
        said = pipeline.runner.carried[-1][1]
        self.assertIn("They already answered", said)
        self.assertIn(self.ANSWER, said)  # the person's own words, whole - never the model's paraphrase
        self.assertIn("ask only about what it leaves open", said)

    def test_a_pointer_the_person_never_said_goes_to_the_person(self):
        self.assertEqual(self.pipeline("Approve CL-4142 in full").ask(), "person")

    def test_a_decision_stage_may_be_sent_back_with_the_persons_answer_too(self):
        self.assertEqual(self.pipeline("pay only Rs 1,200", decision=True).ask(), "settled")

    def test_once_per_stage_then_the_person_is_asked(self):
        pipeline = self.pipeline("pay only Rs 1,200")
        self.assertEqual(pipeline.ask(), "settled")
        pipeline.routes = ["DECIDED"]
        self.assertEqual(pipeline.ask(), "person")
