"""
Which steps stop for the person's OK is read from the person's own words (learning.take_gates, _gates): found live
(2026-11-10, a lesson on a return filed from its books), the person's explanation said to ask before "Save to portal",
"File with EVC" and "Verify" and not to stop before "Proceed to file"; it was kept as text only, and the skill was
saved gating 55 (Proceed to file / summary) and 59 (File with EVC), not 42 (Save to portal) or 63 (Verify).

The model reads only what the person means and copies the names they wrote; which steps those are is decided in code,
by each step's recorded label. The mapping is put back in one line and only a yes keeps it; a name no step has is asked
about alone; a reply that names steps is never answered "I did not catch that". The live question, reply and steps are
in fixtures/lesson-gates-live.json, word for word.

    cd agent/tests && PYTHONPATH=.. python3 -m unittest test_lesson_gates_own_words
"""

import copy
import json
import unittest
from pathlib import Path
from unittest import mock

import test_skills as ts  # stubs decurix_agent.config first, and gives a lesson to run through skill_task

from decurix_agent import llm_client, skill_task, state, study
from decurix_agent.computer import learning, skills

_FIXTURE = Path(__file__).parent / "fixtures" / "lesson-gates-live.json"
if not _FIXTURE.is_file():
    # (fixtures copied from live runs are never committed - .gitignore - and a test that needs one skips without it)
    raise unittest.SkipTest("fixtures/lesson-gates-live.json is not on this machine")
LIVE = json.loads(_FIXTURE.read_text(encoding="utf-8"))

# What a model reads from the live reply: the names as the person wrote them (the code maps them onto steps).
LIVE_READING = {"ask": ["Save to portal", "File with EVC", "Verify"], "no_ask": ["the summary page", "Proceed to file"]}

LIVE_LINE = "I will ask before step 42 Save to portal, 59 File with EVC, 63 Verify; not before 55 Proceed to file / summary"


def live_skill():
    return {"name": "file-gstr-1-from-the-books", "title": "File GSTR-1 from the books", "named": True,
            "steps": copy.deepcopy(LIVE["steps"]), "params": [], "answers": {"goal": "skip"}, "unclear": {}, "demos": 1,
            "loops": [], "notes": []}


def by_n(skill):
    return {s["n"]: s for s in skill["steps"]}


class Model:
    """A stand-in model: the lesson's reading of which steps need an OK gets `gates`; a choice reading gets `choice`."""

    def __init__(self, gates=None, choice="unclear"):
        self.gates, self.choice, self.calls = gates or {"ask": [], "no_ask": []}, choice, []

    def chat(self, messages, purpose, **kw):
        self.calls.append((purpose, messages[-1]["content"]))
        if purpose == "py.lesson.gates":
            return {"content": json.dumps(self.gates)}, {}
        return {"content": json.dumps({"choice": self.choice, "instruction": ""})}, {}

    def __enter__(self):
        self.patches = [mock.patch.object(llm_client, "available", return_value=True),
                        mock.patch.object(llm_client, "chat", self.chat)]
        for p in self.patches:
            p.start()
        return self

    def __exit__(self, *exc):
        for p in self.patches:
            p.stop()


NO_MODEL = mock.patch.object(llm_client, "available", return_value=False)


def goal_question():
    return {"id": "goal", "kind": "goal", "step": None, "text": LIVE["question"], "default": None}


def taught_live():
    """The live lesson after the person's explanation: the steps they named are put back, nothing stored yet."""

    skill = live_skill()
    with Model(LIVE_READING) as model:
        result = learning.answer(skill, goal_question(), LIVE["reply"])
    return skill, result, model


class TheLiveReplyMapsOntoItsSteps(unittest.TestCase):

    def test_the_live_question_and_reply_map_to_42_59_63_asked_and_55_not(self):
        skill, result, model = taught_live()
        self.assertTrue(result["understood"])
        self.assertNotIn("did not catch", result["note"])
        self.assertEqual([p for p, _ in model.calls], ["py.lesson.gates"])
        self.assertIn(LIVE["reply"], model.calls[0][1], "the reading is given the person's words whole")

        pending = skill["gates_pending"]
        self.assertEqual(sorted(pending["ask"]), ["s42", "s59", "s63"])
        self.assertEqual(pending["no_ask"], ["s55"])
        self.assertEqual(pending["unmapped"], [])

        asked = learning.next_question(skill)
        self.assertEqual(asked["kind"], "gates", "put back right after the answer that said it")
        self.assertTrue(asked["text"].startswith(LIVE_LINE + " - right?"), asked["text"])

        # Nothing is stored until the person says yes to it.
        steps = by_n(skill)
        self.assertEqual([steps[n]["consequential"] for n in (42, 55, 59, 63)], [False, True, True, False])

    def test_a_yes_stores_exactly_that(self):
        skill, _, _ = taught_live()
        before = {s["n"]: s["consequential"] for s in skill["steps"]}
        with NO_MODEL:
            result = learning.answer(skill, learning.next_question(skill), "yes")
        self.assertTrue(result["understood"])
        self.assertIn(LIVE_LINE, result["note"])
        steps = by_n(skill)
        self.assertEqual({n: steps[n]["consequential"] for n in (42, 55, 59, 63)},
                         {42: True, 55: False, 59: True, 63: True})
        others = {n: v for n, v in before.items() if n not in (42, 55, 59, 63)}
        self.assertEqual({n: steps[n]["consequential"] for n in others}, others, "steps nobody named are as they were")
        self.assertNotIn("gates_pending", skill)
        self.assertEqual(skill["answers"]["gates"], "yes")
        # Kept as the person's confirmed answer, which the gate holds whatever a step's flag says later.
        approvals = skill["approvals"]
        self.assertTrue(approvals["confirmed"])
        self.assertEqual([(a["step"], a["name"]) for a in approvals["ask"]],
                         [(42, "Save to portal"), (59, "File with EVC"), (63, "Verify")])
        self.assertEqual([(a["step"], a["name"]) for a in approvals["no_ask"]], [(55, "Proceed to file / summary")])
        self.assertEqual(approvals["said"], [" ".join(LIVE["reply"].split())[:600], "yes"])
        self.assertNotEqual(learning.next_question(skill)["kind"], "gates")
        # The last question says it as stored.
        confirm = next(q for q in learning.questions(skill) if q["kind"] == "confirm")
        self.assertIn('42. click button "Save to portal"', confirm["text"])
        self.assertIn('63. click button "Verify"', confirm["text"])
        self.assertNotIn("Proceed to file", confirm["text"])

    def test_names_map_by_the_steps_own_labels(self):
        skill = live_skill()
        cases = {"Save to portal": [42], "the Save to portal button": [42], "SAVE TO PORTAL": [42],
                 '"File with EVC"': [59], "the `Verify` button": [63], "Proceed to file": [55],
                 "Update names": [35], "step 30": [30], "Search": [18, 50], "File": [], "Submit to bank": []}
        for name, expected in cases.items():
            with self.subTest(name=name):
                self.assertEqual([s["n"] for s in learning._steps_named(skill, name)], expected)

    def test_a_name_the_reply_does_not_say_is_never_taken_from_the_reading(self):
        skill = live_skill()
        with Model({"ask": ["Save to portal", "Sign in"], "no_ask": []}):
            learning.take_gates(skill, "q", "Ask me before Save to portal.")
        self.assertEqual(skill["gates_pending"]["ask"], ["s42"])
        self.assertEqual(skill["gates_pending"]["unmapped"], [])


class AskedOutrightWhenNotSaid(unittest.TestCase):
    """A lesson where the person said nothing of it asks which steps need their OK, near the end."""

    def setUp(self):
        self.skill = live_skill()
        self.asked = learning.next_question(self.skill)

    def test_it_is_asked_with_the_steps_as_learned(self):
        self.assertEqual(self.asked["kind"], "gates")
        self.assertTrue(self.asked["text"].startswith(learning.GATES_OPENING + " As I learned it: I will ask before "
                                                      "step 55 Proceed to file / summary, 59 File with EVC."),
                        self.asked["text"])
        kinds = [q["kind"] for q in learning.questions(self.skill)]
        self.assertEqual(kinds[-3:], ["gates", "approval", "confirm"])

    def test_a_yes_confirms_the_steps_as_learned(self):
        with NO_MODEL:
            learning.answer(self.skill, self.asked, "yes")
        self.assertTrue(self.skill["approvals"]["confirmed"])
        self.assertEqual([a["step"] for a in self.skill["approvals"]["ask"]], [55, 59])

    def test_an_answer_in_words_is_put_back_before_it_is_kept(self):
        with Model(LIVE_READING):
            result = learning.answer(self.skill, self.asked, "Ask me for approval only before Save to portal, before "
                                     "File with EVC and before Verify. Don't stop before Proceed to file.")
        self.assertTrue(result["understood"])
        self.assertNotIn("approvals", self.skill)
        again = learning.next_question(self.skill)
        self.assertTrue(again["text"].startswith(LIVE_LINE + " - right?"), again["text"])

    def test_never_made_sure_of_keeps_the_asking_side_and_confirms_nothing(self):
        with NO_MODEL:
            for _ in range(learning.MAX_UNCLEAR):
                result = learning.answer(self.skill, self.asked, "hmm, whatever you think")
        self.assertIn("cautious", result["note"])
        self.assertNotIn("approvals", self.skill)
        self.assertTrue(by_n(self.skill)[55]["consequential"])


class ACorrectionIsReadAgain(unittest.TestCase):

    def test_also_step_30_is_read_and_put_back(self):
        skill, _, _ = taught_live()
        with NO_MODEL:
            result = learning.answer(skill, learning.next_question(skill), "also step 30")
            self.assertTrue(result["understood"])
            asked = learning.next_question(skill)
            self.assertEqual(asked["kind"], "gates")
            self.assertIn("I will ask before step 30 click 57,473, 42 Save to portal, 59 File with EVC, 63 Verify; "
                          "not before 55", asked["text"])
            learning.answer(skill, asked, "yes")
        self.assertTrue(by_n(skill)[30]["consequential"])
        self.assertTrue(by_n(skill)[63]["consequential"])

    def test_a_correction_in_words_is_read_by_the_model_and_put_back(self):
        skill, _, _ = taught_live()
        asked = learning.next_question(skill)
        with Model({"ask": [], "no_ask": ["Verify"]}) as model:
            result = learning.answer(skill, asked, "Actually no need to ask before Verify, the OTP is the check")
        self.assertTrue(result["understood"])
        self.assertIn("I will ask before step 42 Save to portal, 59 File with EVC; not before 55 Proceed to file / "
                      "summary, 63 Verify", learning.next_question(skill)["text"])
        self.assertIn(LIVE_LINE, model.calls[0][1], "read against the line it corrects")

    def test_numbers_and_choices_need_no_model(self):
        skill, _, _ = taught_live()
        with NO_MODEL:
            learning.answer(skill, learning.next_question(skill), "yes but not 63")
            self.assertIn("not before 55 Proceed to file / summary, 63 Verify", learning.next_question(skill)["text"])


class ANameNoStepHasIsAskedAboutAlone(unittest.TestCase):

    def setUp(self):
        self.skill = live_skill()
        with Model({"ask": ["Save to portal", "Submit to bank"], "no_ask": []}):
            self.assertTrue(learning.take_gates(self.skill, "q", "Ask before Save to portal and Submit to bank."))

    def test_only_the_unmapped_name_is_asked_about(self):
        asked = learning.next_question(self.skill)
        self.assertEqual(asked["kind"], "gates")
        self.assertTrue(asked["text"].startswith("I couldn't find a step for \"Submit to bank\" - which step is it?"),
                        asked["text"])
        self.assertIn("So far: I will ask before step 42 Save to portal, 55", asked["text"])

    def test_its_number_maps_it_and_then_the_line_is_put_back(self):
        with NO_MODEL:
            result = learning.answer(self.skill, learning.next_question(self.skill), "it is step 56")
            self.assertTrue(result["understood"])
            asked = learning.next_question(self.skill)
        self.assertTrue(asked["text"].startswith("I will ask before step 42 Save to portal, 55 Proceed to file / summary, "
                                                 "56 File statement, 59 File with EVC - right?"), asked["text"])

    def test_its_label_maps_it(self):
        with NO_MODEL:
            learning.answer(self.skill, learning.next_question(self.skill), "the File statement link")
        self.assertIn("s56", self.skill["gates_pending"]["ask"])

    def test_none_leaves_it_out_and_says_so(self):
        with NO_MODEL:
            result = learning.answer(self.skill, learning.next_question(self.skill), "none")
        self.assertIn('"Submit to bank" is left out', result["note"])
        self.assertEqual(self.skill["gates_pending"]["unmapped"], [])
        self.assertEqual(learning.next_question(self.skill)["text"][:len("I will ask before")], "I will ask before")


class NamingStepsIsNeverNotCaught(unittest.TestCase):

    def test_a_reply_naming_steps_with_no_model_is_told_which_it_named(self):
        skill, _, _ = taught_live()
        with NO_MODEL:
            result = learning.answer(skill, learning.next_question(skill), "Save to portal and Verify, please")
        self.assertFalse(result["understood"])
        self.assertNotIn("did not catch", result["note"])
        self.assertIn("You named step 42 Save to portal, 63 Verify", result["note"])

    def test_the_start_question_answered_with_steps_puts_them_back(self):
        skill = live_skill()
        start = {"id": "approval", "kind": "approval", "step": None, "text": learning._approval()["text"],
                 "default": "ask"}
        with Model({"ask": ["Save to portal", "Verify"], "no_ask": []}, choice="unclear"):
            result = learning.answer(skill, start, "Ask me before Save to portal and before Verify")
        self.assertTrue(result["understood"])
        self.assertNotIn("did not catch", result["note"])
        self.assertNotIn("approval", skill["answers"], "the start question comes again after")
        self.assertEqual(learning.next_question(skill)["kind"], "gates")

    def test_the_last_question_answered_with_steps_puts_them_back(self):
        skill = live_skill()
        confirm = next(q for q in learning.questions(skill) if q["kind"] == "confirm")
        with Model({"ask": ["Save to portal"], "no_ask": ["Proceed to file"]}, choice="unclear"):
            result = learning.answer(skill, confirm, "Also ask before Save to portal, and not before Proceed to file")
        self.assertTrue(result["understood"])
        self.assertIsNone(result["decision"])
        self.assertNotIn("did not catch", result["note"])
        self.assertEqual(learning.next_question(skill)["kind"], "gates")

    def test_a_plain_reply_says_nothing_of_steps(self):
        skill = live_skill()
        with Model(LIVE_READING) as model:
            for reply in ("yes", "skip", "no"):
                self.assertFalse(learning.take_gates(skill, "q", reply))
        self.assertEqual(model.calls, [])
        self.assertNotIn("gates_pending", skill)


class TheLessonPutsItBackAndKeepsIt(ts.TeachingBase):
    """The whole lesson through skill_task: said while explaining, put back next, kept on a yes, saved with the skill."""

    def test_said_in_the_explanation_is_put_back_next_and_saved_on_a_yes(self):
        self.record()
        replies = {"goal": "The customer shows in the list. Ask me before Customers every time; don't stop before "
                           "Save, saving it is the job.",
                   "gates": "yes", "start": "yes", "variable": "changes", "confirm": "yes"}
        kinds = []
        with Model({"ask": ["Customers"], "no_ask": ["Save"]}), mock.patch.object(study, "available", return_value=False):
            skill_task.perform(f"/learn {self.SESSION}")
            while state.paused_skill and state.paused_skill.get("mode") == "learn" and len(kinds) < 12:
                question = state.paused_skill["question"]
                kinds.append(question["kind"])
                if question["kind"] == "gates":
                    self.assertTrue(question["text"].startswith("I will ask before step 1 Customers; not before "
                                                                "4 Save - right?"), question["text"])
                skill_task.resume(replies[question["kind"]])
        self.assertEqual(kinds[:2], ["goal", "gates"])
        self.assertFalse(any("did not catch" in text for text, _ in self.asked))
        saved = skills.load(skills.all_skills()[0]["name"])
        self.assertEqual(saved["status"], skills.READY)
        steps = {(s.get("target") or {}).get("name"): s for s in saved["steps"]}
        self.assertTrue(steps["Customers"]["consequential"])
        self.assertFalse(steps["Save"]["consequential"])
        self.assertNotIn("gates_pending", saved)


if __name__ == "__main__":
    unittest.main()
