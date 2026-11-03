"""
Unit tests for teaching a desktop a skill: generalising several
demonstrations, the questions asked about what a recording cannot say, and a
skill that stops to ask when it is unsure. None of it needs a display or a
model.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import contextlib
import io
import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import FakeScreen, event, field

from decurix_agent import http_api, playbooks, skill_task, state
from decurix_agent.computer import cli, learning, mcp_server, recording, replay, skill_engine, skills


def demo(session, customer, banner=False, title="Add a customer"):
    """A demonstration: open Customers, type a name and a password, maybe dismiss a banner, save."""

    dismiss = [event(3, "click", field("button", "Dismiss banner"))] if banner else []
    save = event(3 + len(dismiss), "click", field("button", "Save"), approved=True,
                 expect={"appeared": [{"role": "alert", "name": "Saved"}]})

    events = [
        event(0, "click", field("link", "Customers"), expect={"appeared": [{"role": "heading", "name": "Customers"}]}),
        event(1, "type", field("textbox", "Customer name"), text=customer,
              expect={"values": [{"role": "textbox", "name": "Customer name", "value": customer}]}),
        event(2, "type", field("textbox", "Password", secret="password field"), text=None,
              withheld={"text": "password field"}),
        *dismiss,
        save,
    ]
    meta = {"id": session, "title": title, "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "actors": ["human"], "screen": [1280, 720]}

    return meta, events


def answer_all(skill, replies=None):
    """Answer every question with `replies` (by question kind) or a sensible default; return the last result."""

    replies = dict({"goal": "the customer shows in the list", "start": "yes", "variable": "changes",
                    "optional": "sometimes", "coordinates": "keep", "unsupported": "manual", "approval": "go ahead", "confirm": "yes"}, **(replies or {}))
    result = None

    while (question := learning.next_question(skill)) is not None:
        result = learning.answer(skill, question, replies[question["kind"]])
        if result["decision"]:
            break

    return result


class Building(unittest.TestCase):

    def test_one_demonstration_leaves_typed_text_to_be_asked_about(self):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])

        self.assertEqual([(p["name"], p["varies"], p["default"]) for p in skill["params"]],
                         [("customer_name", None, "Acme")])
        self.assertEqual([q["id"] for q in learning.questions(skill)],
                         ["goal", "start", f"variable:{skill['steps'][1]['id']}", "approval", "confirm"])

    def test_a_secret_field_becomes_a_step_a_person_does(self):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        step = skill["steps"][2]

        self.assertEqual(step["kind"], "manual")
        self.assertIn("yourself", step["reason"])
        self.assertNotIn("password", [p["name"] for p in skill["params"]])

    def test_what_differs_between_demonstrations_is_a_variable_and_what_only_some_have_is_optional(self):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme", banner=True),
                                demo("demo-20260914T101600-bbbbbb", "Globex")])

        (param,) = skill["params"]
        self.assertEqual((param["varies"], param["default"], param["example"]), (True, None, "Acme"))

        banner = next(s for s in skill["steps"] if s["target"]["name"] == "Dismiss banner")
        self.assertEqual((banner["seen"], skill["demos"]), (1, 2))

        questions = learning.questions(skill)
        self.assertNotIn("variable", [q["kind"] for q in questions])
        # Only the step some demonstrations lacked is asked about - not every step of a skill learned from two.
        self.assertEqual([q["step"] for q in questions if q["kind"] == "optional"], [banner["id"]])

    def test_a_step_is_expected_to_leave_only_what_every_demonstration_left(self):
        first, second = demo("demo-20260914T101500-aaaaaa", "Acme"), demo("demo-20260914T101600-bbbbbb", "Globex")
        first[1][0]["expect"] = {"appeared": [{"role": "heading", "name": "Customers"}, {"role": "dialog", "name": "Welcome"}]}

        skill = learning.build([first, second])

        self.assertEqual(skill["steps"][0]["expect"], [{"type": "element", "role": "heading", "name": "Customers"}])

    def test_a_recording_with_nothing_a_replay_can_run_cannot_be_learned_from(self):
        meta, _ = demo("demo-20260914T101500-aaaaaa", "Acme")
        with self.assertRaises(ValueError):
            learning.build([(meta, [])])

    def test_notes_are_kept_beside_the_step_they_came_before(self):
        meta, events = demo("demo-20260914T101500-aaaaaa", "Acme")
        events.insert(1, {"i": 0.5, "kind": "note", "actor": "human", "outcome": "done", "text": "Names are in the CRM"})

        skill = learning.build([(meta, events)])

        self.assertEqual(skill["notes"], [{"text": "Names are in the CRM", "before": skill["steps"][1]["id"]}])
        self.assertIn("Names are in the CRM", learning.questions(skill)[0]["text"])


class Answering(unittest.TestCase):

    def setUp(self):
        self.skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])

    def ask(self, kind):
        return next(q for q in learning.questions(self.skill) if q["kind"] == kind)

    def test_replies_are_understood_whichever_way_they_are_put(self):
        choose = learning._choose

        self.assertEqual(choose("variable", "it's the same every time"), "same")
        self.assertEqual(choose("variable", "doesn't change"), "same")
        self.assertEqual(choose("variable", "a different one each time"), "changes")
        self.assertEqual(choose("optional", "only sometimes"), "sometimes")
        self.assertEqual(choose("optional", "not always"), "sometimes")
        self.assertEqual(choose("optional", "always"), "always")
        # Two answers at once is not an answer.
        self.assertIsNone(choose("variable", "same, no wait, it changes"))
        self.assertIsNone(choose("variable", "hmm"))

    def test_same_fixes_the_text_and_changes_makes_it_something_each_run_must_give(self):
        learning.answer(self.skill, self.ask("variable"), "same")
        self.assertEqual((self.skill["params"][0]["varies"], self.skill["params"][0]["default"]), (False, "Acme"))
        self.assertEqual(skills.required(self.skill), [])

        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        learning.answer(skill, next(q for q in learning.questions(skill) if q["kind"] == "variable"), "it changes")
        self.assertEqual([p["name"] for p in skills.required(skill)], ["customer_name"])
        self.assertIsNone(skill["params"][0]["default"])

    def test_a_reply_that_is_not_understood_is_asked_again_then_answered_cautiously(self):
        question = self.ask("variable")

        first = learning.answer(self.skill, question, "hmm")
        self.assertFalse(first["understood"])
        self.assertNotIn(question["id"], self.skill["answers"])

        second = learning.answer(self.skill, question, "dunno")
        self.assertTrue(second["understood"])
        self.assertIn("cautious", second["note"])
        self.assertTrue(self.skill["params"][0]["varies"])

    def test_a_result_that_echoes_a_changing_value_follows_it_instead_of_being_baked_in(self):
        # What the page said after the demonstration typed "Acme". Left as recorded,
        # every run with another customer would be told its result never appeared.
        save = next(s for s in self.skill["steps"] if s["kind"] == "click" and "Save" in s["summary"])
        save["expect"] = [{"type": "element", "role": "status", "name": "Saved Acme"},
                          {"type": "element", "role": "heading", "name": "Customers"}]

        learning.answer(self.skill, self.ask("variable"), "changes")

        self.assertEqual([c["name"] for c in save["expect"]], ["Saved {customer_name}", "Customers"])

        shown = {"elements": [{"role": "status", "name": "Saved Globex Corp"}, {"role": "heading", "name": "Customers"}]}
        self.assertEqual(replay.unmet(save["expect"], shown, {"customer_name": "Globex Corp"}), [])

        wrong = {"elements": [{"role": "status", "name": "Saved Acme"}, {"role": "heading", "name": "Customers"}]}
        self.assertEqual([c["name"] for c in replay.unmet(save["expect"], wrong, {"customer_name": "Globex Corp"})],
                         ["Saved {customer_name}"])

        # No value for the placeholder: it must not pass on something never seen.
        self.assertEqual(len(replay.unmet(save["expect"], shown, {})), 1)

    def test_a_value_said_to_stay_the_same_leaves_the_result_check_alone(self):
        save = next(s for s in self.skill["steps"] if s["kind"] == "click" and "Save" in s["summary"])
        save["expect"] = [{"type": "element", "role": "status", "name": "Saved Acme"}]

        learning.answer(self.skill, self.ask("variable"), "same")

        self.assertEqual(save["expect"][0]["name"], "Saved Acme")

    def test_the_goal_is_kept_in_words_and_can_be_skipped(self):
        learning.answer(self.skill, self.ask("goal"), "the customer is in the list")
        self.assertEqual(self.skill["done_when"], "the customer is in the list")

        learning.answer(self.skill, self.ask("goal"), "skip")
        self.assertIsNone(self.skill["done_when"])

    def test_what_a_person_says_about_a_step_goes_on_that_step(self):
        result = learning.answer(self.skill, self.ask("goal"),
                                 "The customer is in the list. Step 2 types the customer's name; the last step saves it")
        steps = self.skill["steps"]

        self.assertEqual(steps[1]["intent"], "Step 2 types the customer's name")
        self.assertEqual(steps[-1]["intent"], "the last step saves it")
        self.assertEqual(self.skill["done_when"], "The customer is in the list.")
        self.assertIn("step", self.skill["explanation"])
        self.assertIn("2", result["note"])
        self.assertIn("you said: \"Step 2 types", learning.overview(self.skill))

        learning.answer(self.skill, self.ask("goal"), "skip")
        self.assertFalse(any(s.get("intent") for s in steps))
        self.assertIsNone(self.skill["explanation"])

    def test_a_click_at_a_spot_can_be_skipped_or_handed_to_a_person(self):
        for reply, kind in (("skip", None), ("i'll do it by hand", "manual")):
            meta, events = demo("demo-20260914T101500-aaaaaa", "Acme")
            events.insert(1, event(0.5, "click", None, point=[300, 200]))
            skill = learning.build([(meta, events)])
            before = len(skill["steps"])

            question = next(q for q in learning.questions(skill) if q["kind"] == "coordinates")
            learning.answer(skill, question, reply)

            if kind is None:
                self.assertEqual(len(skill["steps"]), before - 1)
                self.assertEqual([s["n"] for s in skill["steps"]], list(range(1, before)))
            else:
                self.assertEqual(skill["steps"][1]["kind"], "manual")

    def test_a_skill_is_only_ready_once_a_person_says_so(self):
        result = answer_all(self.skill, {"confirm": "yes"})
        self.assertEqual((result["decision"], self.skill["status"]), ("save", skills.READY))

        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        self.assertEqual(answer_all(skill, {"confirm": "no"})["decision"], "discard")
        self.assertEqual(skill["status"], skills.DRAFT)

    def test_an_unclear_confirmation_saves_nothing_and_can_be_asked_again(self):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        # Any other reply is a name; one with nothing to name it by is not.
        result = answer_all(skill, {"confirm": "?"})

        self.assertEqual((result["decision"], skill["status"]), ("hold", skills.DRAFT))
        self.assertEqual(learning.next_question(skill)["kind"], "confirm")

    def test_approval_is_told_not_asked(self):
        text = self.ask("confirm")["text"]
        self.assertIn("always stop and ask you before", text)
        self.assertIn("Save", text)
        self.assertIn("stop and let you do", text)


class Store(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT
        skills.ROOT = Path(self.tmp.name)

    def tearDown(self):
        skills.ROOT = self.saved
        self.tmp.cleanup()

    def ready(self, title, session="demo-20260914T101500-aaaaaa"):
        skill = learning.build([demo(session, "Acme", title=title)])
        skill["status"] = skills.READY
        return skills.save(skill)

    def test_names_are_made_from_titles_and_never_collide(self):
        self.assertEqual(skills.slug("Pay an invoice!"), "pay-an-invoice")
        self.assertEqual(skills.unique_name("Pay an invoice", taken={"pay-an-invoice"}), "pay-an-invoice-2")
        self.assertFalse(skills.valid("../etc"))

    def test_a_skill_runs_only_by_its_name_and_words_only_suggest(self):
        self.ready("Pay an invoice")
        self.ready("Add a customer", "demo-20260914T101600-bbbbbb")

        self.assertEqual(skills.find("pay-an-invoice")[0]["name"], "pay-an-invoice")
        self.assertEqual(skills.find("Pay an invoice")[0]["name"], "pay-an-invoice")
        # Words in common only suggest (review 2026-11-03, D-16): "remove a customer" never runs "Add a customer".
        skill, candidates = skills.find("please pay that invoice")
        self.assertIsNone(skill)
        self.assertEqual([s["name"] for s in candidates], ["pay-an-invoice"])
        skill, candidates = skills.find("remove a customer")
        self.assertIsNone(skill)
        self.assertEqual([s["name"] for s in candidates], ["add-a-customer"])
        self.assertEqual(skills.find("something else entirely"), (None, []))

    def test_two_skills_that_fit_equally_are_never_chosen_between(self):
        self.ready("Add a customer")
        self.ready("Add a supplier", "demo-20260914T101600-bbbbbb")

        skill, candidates = skills.find("add")
        self.assertIsNone(skill)
        self.assertEqual(len(candidates), 2)

    def test_an_unfinished_skill_is_found_again_by_the_recordings_it_came_from(self):
        draft = skills.save(learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")]))

        self.assertEqual(skills.draft_for(["demo-20260914T101500-aaaaaa"])["name"], draft["name"])
        self.assertIsNone(skills.draft_for(["demo-20260914T101600-bbbbbb"]))
        # And a draft is never run.
        self.assertIsNone(skills.find(draft["name"])[0])


class Optional(unittest.TestCase):

    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved

    def program(self, banner_present):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme", banner=True),
                                demo("demo-20260914T101600-bbbbbb", "Globex")])
        answer_all(skill, {"optional": "sometimes"})
        elements = [field("link", "Customers"), field("textbox", "Customer name"), field("button", "Save")]
        if banner_present:
            elements.append(field("button", "Dismiss banner"))
        replay.core = FakeScreen(elements, after={"adds": [field("heading", "Customers"), field("alert", "Saved")]})
        return skills.program(skill), replay.core

    def test_an_optional_step_that_is_not_on_the_screen_is_left_out(self):
        program, fake = self.program(banner_present=False)
        result = replay.run(program, params={"customer_name": "Initech"}, start=4, report=lambda line: None)

        self.assertIn(result["outcome"], replay.SUCCESS, result["text"])
        self.assertEqual(result["skipped"], 1)
        self.assertEqual([k for k, _ in fake.acted], ["click"])
        self.assertIn("left out", result["text"])

    def test_an_optional_step_that_is_there_is_done(self):
        program, fake = self.program(banner_present=True)
        result = replay.run(program, params={"customer_name": "Initech"}, start=4, report=lambda line: None)

        self.assertEqual(result["skipped"], 0)
        self.assertEqual(len(fake.acted), 2)

    def test_an_optional_step_with_two_candidates_still_halts(self):
        program, fake = self.program(banner_present=True)
        # Neither is the element recorded, so the path settles nothing.
        fake.elements[-1:] = [field("button", "Dismiss banner", path="one"), field("button", "Dismiss banner", path="two")]

        result = replay.run(program, params={"customer_name": "Initech"}, start=4, report=lambda line: None)

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("does not guess", result["text"])


class Requests(unittest.TestCase):

    def test_learn_requests_are_parsed(self):
        self.assertEqual(skill_task.parse_learn("/learn demo-1 demo-2 name=pay"), (["demo-1", "demo-2"], "pay"))
        self.assertEqual(skill_task.parse_learn("/learn"), ([], None))
        self.assertIsNone(skill_task.parse_learn("/learnt something"))
        self.assertIsNone(skill_task.parse_learn("please /learn this"))

    def test_skill_requests_are_parsed(self):
        self.assertEqual(skill_task.parse_skill('/skill pay the invoice customer="Acme Ltd" amount=40'),
                         ("pay the invoice", {"customer": "Acme Ltd", "amount": "40"}))
        self.assertEqual(skill_task.parse_skill("/skills"), ("", {}))
        self.assertEqual(skill_task.parse_skill("/skill list"), ("", {}))
        self.assertIsNone(skill_task.parse_skill("/skillful"))
        self.assertTrue(skill_task.handles("/skill x"))
        self.assertFalse(skill_task.handles("/replay x"))


class LearnedMemoryDispatch(unittest.TestCase):
    def test_optional_memory_write_does_not_block_skill_completion(self):
        started = threading.Event()
        release = threading.Event()
        returned = threading.Event()

        def slow_memory(_skill):
            started.set()
            release.wait(timeout=2)

        def launch():
            skill_task._remember_async({"name": "saved-skill"})
            returned.set()

        with mock.patch.object(skill_task, "_remember", slow_memory):
            caller = threading.Thread(target=launch)
            caller.start()
            try:
                self.assertTrue(started.wait(timeout=0.2))
                self.assertTrue(returned.wait(timeout=0.2))
            finally:
                release.set()
                caller.join(timeout=1)


class TeachingBase(unittest.TestCase):
    """A desktop with a recordings store, a skills store and a scripted screen, and messaging caught."""

    SESSION = "demo-20260914T101500-aaaaaa"

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = (recording.ROOT, skills.ROOT, replay.core, skill_task.core,
                      replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS, state.current_task)
        recording.ROOT, skills.ROOT = Path(self.tmp.name) / "recordings", Path(self.tmp.name) / "skills"
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0
        state.paused_skill = None
        playbooks.begin()  # (the commands of an earlier test's task are no part of this one: skill_task._gate reads them)

        self.asked, self.finals, self.remembered = [], [], []
        patches = [
            mock.patch.object(skill_task, "remember", lambda title, summary, category="workflow": self.remembered.append((title, summary, category))),
            mock.patch.object(skill_task, "_remember_async", lambda skill: skill_task._remember(skill)),
            mock.patch.object(skill_task, "ask_for_query", self._ask_for_query),
            mock.patch.object(skill_task, "write_final_reply", lambda text, kind="final", engine=None, **kw: self.finals.append((text, kind))),
            mock.patch.object(skill_task.usage, "add"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def _ask_for_query(self, text, kind="query", engine=None, **kw):
        # Whether to start the task without asking is its own question (learning._approval): these lessons are
        # about everything else, so the person here always says go ahead. test_approval_question.py asks it.
        if learning.APPROVAL_OPENING in text:
            return skill_task.resume("go ahead")
        self.asked.append((text, kind))

    def tearDown(self):
        (recording.ROOT, skills.ROOT, replay.core, skill_task.core,
         replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS, state.current_task) = self.saved
        state.paused_skill = None
        self.tmp.cleanup()

    def confirm_done(self):
        """An unchecked run is not a success until someone says so (verify.py): say that it is done."""

        self.assertIn("confirm this is done", self.asked[-1][0])
        skill_task.resume("done")

    def record(self, session=SESSION, customer="Acme", made=None):
        meta, events = made or demo(session, customer)
        recording.ensure(session, title=meta["title"])
        for e in events:
            recording.append(session, e)
        recording.update(session, status="complete")

    def teach(self, *replies):
        self.record()
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in replies:
            skill_task.resume(reply)

    def screen(self, *extra, **kwargs):
        elements = [field("link", "Customers"), field("textbox", "Customer name"), field("button", "Save"), *extra]
        fake = FakeScreen(elements, after={"adds": [field("heading", "Customers"), field("alert", "Saved")]}, **kwargs)
        replay.core = skill_task.core = fake
        return fake


class Teaching(TeachingBase):
    """The whole loop through skill_task: learn from a recording by answering, then run what was learned."""

    def test_learning_asks_one_question_at_a_time_and_saves_only_on_a_yes(self):
        self.teach("the customer is in the list", "yes", "changes")

        self.assertEqual([kind for _, kind in self.asked], ["query"] * 4)
        self.assertIn("learned 4 steps", self.asked[0][0])
        self.assertIn("What should I call it?", self.asked[3][0])
        self.assertIn("`yes` keeps \"Add a customer\"", self.asked[3][0])
        self.assertEqual(self.finals, [])
        self.assertEqual(skills.load("add-a-customer")["status"], skills.DRAFT)

        skill_task.resume("yes")

        self.assertEqual(self.finals[-1][1], "final")
        self.assertIn("/skill add-a-customer customer_name=", self.finals[-1][0])
        self.assertEqual(skills.load("add-a-customer")["status"], skills.READY)

    def test_the_last_question_takes_a_name(self):
        self.teach("the customer is in the list", "yes", "changes")

        skill_task.resume("Add a CRM customer")
        # A name is put back to the person, and only their yes saves it (review 2026-11-03, D-21).
        self.assertEqual(skills.load("add-a-crm-customer")["status"], skills.DRAFT)
        self.assertIn('I will save it as "Add a CRM customer"', self.asked[-1][0])
        skill_task.resume("yes")

        self.assertIn("as `add-a-crm-customer`", self.finals[-1][0])
        saved = skills.load("add-a-crm-customer")
        self.assertEqual((saved["title"], saved["status"]), ("Add a CRM customer", skills.READY))
        # The goal a request is matched against is still what was recorded.
        self.assertEqual(saved["goal"], "Add a customer")
        # The draft it was kept under while being taught is gone.
        with self.assertRaises(FileNotFoundError):
            skills.load("add-a-customer")

    def test_a_name_that_looks_like_an_answer_is_still_a_name(self):
        self.teach("the customer is in the list", "yes", "changes")

        skill_task.resume("Delete old customers")
        skill_task.resume("yes")

        self.assertEqual(skills.load("delete-old-customers")["status"], skills.READY)

    def test_a_name_given_when_teaching_is_offered_back(self):
        self.record()
        skill_task.perform(f'/learn {self.SESSION} name="Onboard a client"')
        for reply in ("the customer is in the list", "yes", "changes"):
            skill_task.resume(reply)

        self.assertIn("I will save it as \"Onboard a client\"", self.asked[-1][0])
        skill_task.resume("yes")
        self.assertEqual(skills.load("onboard-a-client")["status"], skills.READY)

    def test_a_saved_skill_lands_in_the_desktops_learned_memory_and_a_draft_does_not(self):
        self.teach("the customer is in the list", "yes", "changes")

        self.assertEqual(self.remembered, [])

        skill_task.resume("yes")

        self.assertEqual(len(self.remembered), 1)
        title, summary, category = self.remembered[0]
        self.assertEqual((title, category), ("Skill: Add a customer", "skill"))
        self.assertIn("/skill add-a-customer customer_name=", summary)
        self.assertIn("Stops for approval", summary)

    def test_a_discarded_lesson_is_not_remembered(self):
        self.teach("the customer is in the list", "yes", "changes")
        skill_task.resume("no, throw it away")

        self.assertEqual(self.remembered, [])

    def test_an_interrupted_lesson_picks_up_with_what_is_left(self):
        self.teach("the customer is in the list", "yes")
        skill_task.resume("stop")

        self.assertIn("kept", self.finals[-1][0])

        self.asked.clear()
        skill_task.perform(f"/learn {self.SESSION}")

        self.assertIn("Picking up where we left off", self.asked[0][0])
        self.assertIn("same every time, or does it change", self.asked[0][0])

    def test_a_reply_that_is_not_understood_is_put_again(self):
        self.teach("the customer is in the list", "yes", "hmm")

        self.assertIn("I did not catch that", self.asked[-1][0])
        self.assertIn("same every time, or does it change", self.asked[-1][0])

    def test_there_is_nothing_to_learn_from_when_no_recording_exists(self):
        skill_task.perform("/learn")
        self.assertEqual(self.finals[-1][1], "error")
        self.assertIn("no finished demonstration", self.finals[-1][0])

        skill_task.perform("/learn demo-20260914T101500-zzzzzz")
        self.assertIn("no recording", self.finals[-1][0])

    def test_running_a_skill_asks_for_what_changes_and_a_person_for_the_secret(self):
        self.teach("the customer is in the list", "yes", "changes", "yes")
        fake = self.screen()
        self.asked.clear()

        skill_task.perform("/skill add a customer")
        self.assertIn("I need: customer_name", self.asked[-1][0])

        skill_task.resume("Globex")
        self.assertIn("yourself", self.asked[-1][0])
        self.assertEqual(fake.acted[1][1]["text"], "Globex")

        skill_task.resume("done")
        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertIn("every step's expected result was on the screen", self.finals[-1][0])
        self.assertEqual(skills.load("add-a-customer")["runs"], 1)

    def test_a_value_given_in_the_request_is_not_asked_for(self):
        self.teach("skip", "yes", "changes", "yes")
        self.screen()
        self.asked.clear()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertNotIn("I need", self.asked[-1][0])

    def test_a_skill_that_does_not_start_where_it_did_asks_before_doing_anything(self):
        self.teach("skip", "yes", "changes", "yes")
        fake = self.screen()
        fake.observation = lambda: {"elements": fake.elements, "windows": [{"title": "Inbox - Mail"}],
                                    "active_title": "Inbox - Mail", "size": [1280, 720], "page_url": "http://mail.test/"}
        self.asked.clear()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertIn("Carry on from here anyway", self.asked[-1][0])
        self.assertEqual(fake.acted, [])

        skill_task.resume("no")
        self.assertIn("Nothing was done", self.finals[-1][0])
        self.assertEqual(fake.acted, [])

    def test_a_skill_never_goes_through_an_action_the_approval_gate_stopped(self):
        self.teach("skip", "yes", "changes", "yes")
        save = field("button", "Save")
        self.screen(approval_on=save["path"])
        self.asked.clear()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        skill_task.resume("done")

        self.assertEqual(self.asked[-1][1], "approval")

    def taught_button(self, name="Save"):
        """A ready skill of one click, for the tests of what happens when it is stuck."""

        meta = {"id": self.SESSION, "title": "Save the form", "kind": "demonstration", "status": "complete",
                "started_at": 1789380000, "screen": [1280, 720]}
        skill = learning.build([(meta, [event(0, "click", field("button", name))])])
        skill["status"] = skills.READY
        skills.save(skill)
        return skill["name"]

    def stuck(self):
        name = self.taught_button()
        fake = self.screen()
        fake.elements[:] = [field("button", "Archive")]
        self.asked.clear()

        skill_task.perform(f"/skill {name}")
        return name, fake

    def test_a_control_that_is_gone_is_asked_about_and_a_correction_that_works_is_remembered(self):
        name, fake = self.stuck()

        self.assertIn("is not on the screen", self.asked[-1][0])
        self.assertIn("`retry`", self.asked[-1][0])

        skill_task.resume('use "Archive" instead')
        self.confirm_done()

        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertIn("remembered", self.finals[-1][0])
        learned = skills.load(name)
        self.assertEqual(learned["steps"][0]["target"]["name"], "Archive")
        self.assertEqual(learned["version"], 2)

    def test_skipping_a_step_is_this_run_only_and_teaches_nothing(self):
        name, fake = self.stuck()

        skill_task.resume("skip")
        self.confirm_done()

        self.assertEqual(self.finals[-1][1], "final")
        self.assertIn("nothing was left to do after step 1", self.finals[-1][0])
        self.assertEqual(skills.load(name)["steps"][0]["target"]["name"], "Save")
        self.assertEqual(fake.acted, [])

    def test_a_reply_that_is_no_help_is_asked_once_more_and_then_the_run_stops_short(self):
        name, fake = self.stuck()

        skill_task.resume("hmm")
        self.assertIn("I did not catch that", self.asked[-1][0])
        self.assertEqual(self.finals, [])

        skill_task.resume("dunno")
        self.assertEqual(self.finals[-1][1], "error")
        self.assertIn("rather not guess", self.finals[-1][0])
        self.assertEqual(fake.acted, [])
        self.assertEqual(skills.load(name)["steps"][0]["target"]["name"], "Save")

    def test_stop_ends_the_run_where_it_is(self):
        name, fake = self.stuck()
        skill_task.resume("stop")
        self.assertIn("Stopped before step 1", self.finals[-1][0])

    def test_asking_for_a_skill_that_does_not_exist_lists_what_does(self):
        self.taught_button()
        skill_task.perform("/skill launch the rockets")

        self.assertEqual(self.finals[-1][1], "error")
        self.assertIn("I do not know a skill", self.finals[-1][0])
        self.assertIn("save-the-form", self.finals[-1][0])

        skill_task.perform("/skills")
        self.assertEqual(self.finals[-1][1], "final")
        self.assertIn("`save-the-form`", self.finals[-1][0])


def rows(session, items=("Pens", "Paper", "Ink")):
    """A demonstration that adds one row per item to an order: [click Add row, type the item] in a row."""

    events = [event(0, "click", field("link", "Orders"))]

    for n, item in enumerate(items):
        events.append(event(1 + 2 * n, "click", field("button", "Add row")))
        events.append(event(2 + 2 * n, "type", field("textbox", "Item"), text=item,
                            expect={"values": [{"role": "textbox", "name": "Item", "value": item}]}))

    events.append(event(1 + 2 * len(items), "click", field("button", "Save")))
    meta = {"id": session, "title": "Fill an order", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "screen": [1280, 720]}

    return meta, events


class Loops(unittest.TestCase):

    SESSION = "demo-20260914T101500-aaaaaa"

    def test_a_block_repeated_with_different_text_is_a_loop_to_ask_about(self):
        skill = learning.build([rows(self.SESSION)])

        (loop,) = skill["loops"]
        self.assertEqual((loop["length"], loop["reps"], loop["enabled"]), (2, 3, None))
        # The typed fields are not asked about one by one until the loop is decided.
        self.assertEqual([q["id"] for q in learning.questions(skill)], ["goal", "start", "loop:l1", "approval", "confirm"])
        self.assertIn("Pens", learning.questions(skill)[2]["text"])

    def test_the_same_text_each_time_is_not_data_and_not_a_loop(self):
        self.assertEqual(learning.build([rows(self.SESSION, ("Pens", "Pens", "Pens"))])["loops"], [])
        self.assertEqual(learning.build([rows(self.SESSION, ("Pens",))])["loops"], [])

    def test_each_keeps_one_repetition_and_makes_what_it_types_a_list(self):
        skill = learning.build([rows(self.SESSION)])
        learning.answer(skill, learning.next_question(skill), "x")  # goal
        learning.answer(skill, learning.next_question(skill), "yes")  # start

        result = learning.answer(skill, learning.next_question(skill), "for each item")

        self.assertTrue(result["understood"])
        self.assertEqual([s["n"] for s in skill["steps"]], [1, 2, 3, 4])
        (param,) = skill["params"]
        self.assertEqual((param["list"], param["varies"], param["examples"]), (True, True, ["Pens", "Paper", "Ink"]))
        self.assertEqual(param["step"], 3)
        self.assertEqual([s["n"] for s in skill["steps"] if s.get("repeat")], [2, 3])
        self.assertIn("[for each item]", learning.overview(skill))

    def test_fixed_leaves_every_step_and_asks_about_each_field_as_usual(self):
        skill = learning.build([rows(self.SESSION)])
        learning.answer(skill, learning.next_question(skill), "x")
        learning.answer(skill, learning.next_question(skill), "yes")
        learning.answer(skill, learning.next_question(skill), "just those")

        self.assertEqual(len(skill["steps"]), 8)
        # The three typed fields are asked about together (test_learn_documents), not one by one.
        left = [q for q in learning.questions(skill) if q["id"] not in skill["answers"]]
        self.assertEqual([q["kind"] for q in left], ["variables", "approval", "confirm"])
        self.assertEqual(len(left[0]["values"]), 3)

    def looped(self):
        skill = learning.build([rows(self.SESSION)])
        answer_all(skill, {"loop": "each"})
        return skill

    def test_a_run_writes_the_loop_out_once_per_item(self):
        skill = self.looped()
        program, values = skills.expand(skill, {"item": "Tea; Milk\nSugar"})

        self.assertEqual(len(program["steps"]), 1 + 2 * 3 + 1)
        self.assertEqual([p["name"] for p in program["params"]], ["item__1", "item__2", "item__3"])
        self.assertEqual({k: v for k, v in values.items()}, {"item__1": "Tea", "item__2": "Milk", "item__3": "Sugar"})
        self.assertEqual([s["n"] for s in program["steps"]], list(range(1, 9)))
        self.assertEqual([p["step"] for p in program["params"]], [3, 5, 7])
        self.assertTrue(program["steps"][4]["summary"].endswith("(item 2 of 3)"))
        # The recorded check that the field holds what was typed follows its item.
        self.assertEqual([c["param"] for s in program["steps"] for c in s["expect"] if c["type"] == "value"],
                         ["item__1", "item__2", "item__3"])
        # The skill itself is untouched by being expanded.
        self.assertEqual(len(skill["steps"]), 4)

    def test_a_result_check_that_follows_a_list_value_follows_each_items_copy(self):
        skill = self.looped()
        in_block = set(skill["loops"][0]["blocks"][0])
        add = next(s for s in skill["steps"] if s["id"] in in_block and s["kind"] == "click")
        add["expect"] = [{"type": "element", "role": "status", "name": "Added {item}"}]

        program, _ = skills.expand(skill, {"item": "Tea; Milk"})

        names = [c["name"] for s in program["steps"] for c in s["expect"] if c["type"] == "element" and "{" in c["name"]]
        self.assertEqual(names, ["Added {item__1}", "Added {item__2}"])
        self.assertEqual(add["expect"][0]["name"], "Added {item}")   # the skill itself is untouched

    def test_carrying_on_needs_only_what_is_still_to_run_except_with_a_loop(self):
        plain = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        answer_all(plain)

        self.assertEqual([p["name"] for p in skills.needed_from(plain, 1)], ["customer_name"])
        self.assertEqual(skills.needed_from(plain, 4), [])
        # A loop's steps are numbered again for each item, so no step number says what is still to come.
        self.assertEqual([p["name"] for p in skills.needed_from(self.looped(), 7)], ["item"])

    def test_lists_that_do_not_line_up_are_a_problem_to_say_so(self):
        skill = {"params": [{"name": "a", "list": True}, {"name": "b", "list": True}]}

        self.assertIn("same length", skills.list_problem(skill, {"a": "1;2", "b": "1"}))
        self.assertIn("a has no items", skills.list_problem(skill, {"a": ";", "b": "1"}))
        self.assertIsNone(skills.list_problem(skill, {"a": "1;2", "b": "3;4"}))
        self.assertIn("more than", skills.list_problem({"params": [{"name": "a", "list": True}]}, {"a": ";".join("x" * 201)}))


def order_lines(session, lines=(("Bolts", "12"), ("Washers", "6"))):
    """An order entered line by line, the income account typed the same on every line (GnuCash's invoice)."""

    events = [event(0, "click", field("menuitem", "New Invoice"))]
    for description, qty in lines:
        n = len(events)
        events += [
            event(n, "type", field("textbox", "Description"), text=description),
            event(n + 1, "type", field("textbox", "Income Account"), text="Income:Sales"),
            event(n + 2, "type", field("textbox", "Quantity"), text=qty),
        ]
    events.append(event(len(events), "click", field("button", "Post")))
    meta = {"id": session, "title": "Enter an order", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "screen": [1280, 720]}
    return meta, events


class LoopsWithAFixedField(unittest.TestCase):
    """Found teaching a GnuCash invoice: every line types the same account, which is procedure, not data."""

    def looped(self):
        skill = learning.build([order_lines("demo-20260914T101500-aaaaaa")])
        answer_all(skill, {"loop": "each"})
        return skill

    def test_what_every_repetition_types_alike_stays_as_typed(self):
        skill = self.looped()

        by_example = {p["example"]: p for p in skill["params"]}
        self.assertEqual((by_example["Income:Sales"]["varies"], by_example["Income:Sales"].get("list")), (False, None))
        self.assertEqual(by_example["Income:Sales"]["default"], "Income:Sales")
        self.assertTrue(by_example["Bolts"]["list"])
        self.assertTrue(by_example["12"]["list"])

    def test_a_run_needs_only_the_lists_and_types_the_fixed_field_on_every_item(self):
        skill = self.looped()
        lists = {p["name"] for p in skill["params"] if p.get("list")}
        fixed = next(p["name"] for p in skill["params"] if not p.get("list"))
        values = {name: ("Nuts; Pins; Clips" if "12" not in str(p["examples"]) else "4; 4; 2")
                  for name, p in ((p["name"], p) for p in skill["params"] if p.get("list"))}
        self.assertEqual(len(lists), 2)

        program, flat = skills.expand(skill, values)

        typed = [s.get("param") for s in program["steps"] if s["kind"] == "type"]
        self.assertEqual(len(typed), 9)
        self.assertEqual(typed.count(fixed), 3)
        self.assertNotIn(fixed, flat)        # the run was not asked for it: the skill knows it
        self.assertEqual(sorted(v for k, v in flat.items() if k.endswith("__3")), ["2", "Clips"])


class LoopFlow(TeachingBase):

    def taught(self):
        self.record(made=rows(self.SESSION))
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("the rows are in the order", "yes", "each", "yes"):
            skill_task.resume(reply)
        return skills.load("fill-an-order")

    def screen_of_rows(self, add="Add row"):
        elements = [field("link", "Orders"), field("button", add), field("textbox", "Item"), field("button", "Save")]
        fake = FakeScreen(elements)
        replay.core = skill_task.core = fake
        return fake

    def test_a_skill_taught_a_loop_is_asked_for_a_list_and_does_each_item(self):
        skill = self.taught()
        self.assertEqual(skill["status"], skills.READY)
        self.assertIn('/skill fill-an-order item="a;b;c"', self.finals[-1][0])

        fake = self.screen_of_rows()
        self.asked.clear()
        skill_task.perform("/skill fill an order")

        self.assertIn("one per item, separated by `;`", self.asked[-1][0])

        skill_task.resume("Tea;Milk")

        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertEqual([kw["text"] for kind, kw in fake.acted if kind == "type"], ["Tea", "Milk"])
        self.assertEqual([kind for kind, _ in fake.acted].count("click"), 1 + 2 + 1)

    def test_a_list_with_no_items_is_asked_for_again_and_never_run(self):
        self.taught()
        fake = self.screen_of_rows()
        self.asked.clear()

        skill_task.perform("/skill fill an order")
        skill_task.resume(";")

        self.assertIn("has no items", self.asked[-1][0])
        self.assertEqual(fake.acted, [])

        skill_task.resume("Tea;Milk")
        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])

    def test_a_correction_made_in_one_item_holds_for_every_item(self):
        self.taught()
        fake = self.screen_of_rows(add="Add line")
        self.asked.clear()

        skill_task.perform('/skill fill-an-order item="Tea;Milk"')
        self.assertIn("is not on the screen", self.asked[-1][0])

        skill_task.resume('use "Add line"')
        self.confirm_done()

        self.assertEqual(self.finals[-1][1], "final", self.finals[-1][0])
        self.assertEqual([kw["text"] for kind, kw in fake.acted if kind == "type"], ["Tea", "Milk"])
        learned = skills.load("fill-an-order")
        self.assertEqual(len([s for s in learned["steps"] if s.get("target", {}).get("name") == "Add line"]), 1)


class EngineAccess(unittest.TestCase):
    """`gui skills` / `gui skill` / `gui do` and the MCP tools: what an engine is given to use a taught skill."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = (skills.ROOT, replay.core, skill_engine.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS)
        skills.ROOT = Path(self.tmp.name)
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        answer_all(skill)
        skills.save(skill)

        self.fake = FakeScreen([field("link", "Customers"), field("textbox", "Customer name"), field("button", "Save")],
                               after={"adds": [field("heading", "Customers"), field("alert", "Saved")]})
        replay.core = skill_engine.core = self.fake

    def tearDown(self):
        (skills.ROOT, replay.core, skill_engine.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS) = self.saved
        self.tmp.cleanup()

    def gui(self, *argv):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = cli.run(list(argv))
        return code, out.getvalue()

    def test_skills_are_listed_and_described(self):
        code, listing = self.gui("skills")
        self.assertEqual(code, 0)
        self.assertIn("add-a-customer", listing)
        self.assertIn("needs customer_name", listing)

        self.assertIn("add-a-customer", self.gui("skills", "customer")[1])
        self.assertIn("No skills", self.gui("skills", "rockets")[1])

        code, shown = self.gui("skill", "add-a-customer")
        self.assertEqual(code, 0)
        self.assertIn("gui do add-a-customer --param customer_name=", shown)
        self.assertIn("[stops for approval]", shown)
        self.assertIn('The person who taught it said: "the customer shows in the list"', shown)

    def test_a_run_that_needs_a_value_does_nothing_and_says_which(self):
        code, output = self.gui("do", "add-a-customer")

        self.assertEqual(code, 1)
        self.assertIn("needs customer_name", output)
        self.assertIn("ask the human", output)
        self.assertEqual(self.fake.acted, [])

    def test_a_run_goes_through_the_replay_and_says_how_to_carry_on_past_a_step_a_person_does(self):
        code, output = self.gui("do", "add-a-customer", "--param", "customer_name=Globex")

        self.assertEqual(code, 1)
        self.assertIn("needs_human", output)
        self.assertIn("continue with: gui do add-a-customer --from 4", output)
        self.assertEqual(skills.load("add-a-customer")["runs"], 0)

        code, output = self.gui("do", "add-a-customer", "--from", "4")

        self.assertEqual(code, 0, output)
        self.assertEqual(skills.load("add-a-customer")["runs"], 1)

    def test_an_unknown_skill_is_an_error_that_points_at_the_list(self):
        with self.assertRaises(cli.ComputerError) as caught:
            self.gui("do", "launch-the-rockets")
        self.assertIn("gui skills", str(caught.exception))

    def test_a_draft_is_never_run(self):
        skill = learning.build([demo("demo-20260914T101600-bbbbbb", "Acme", title="Half taught")])
        skills.save(skill)

        result = skill_engine.run(skill)
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("never finished being taught", result["text"])
        self.assertEqual(self.fake.acted, [])

    def test_the_mcp_tools_list_show_and_run(self):
        (listing,), error = mcp_server.call_tool("skills", {})
        self.assertFalse(error)
        self.assertIn("add-a-customer", listing["text"])

        (shown,), _ = mcp_server.call_tool("skills", {"name": "add-a-customer"})
        self.assertIn("gui do add-a-customer", shown["text"])

        (refused,), error = mcp_server.call_tool("run_skill", {"name": "add-a-customer"})
        self.assertTrue(error)
        self.assertIn("needs customer_name", refused["text"])

        (paused,), error = mcp_server.call_tool("run_skill", {"name": "add-a-customer", "params": {"customer_name": "Globex"}})
        self.assertTrue(error)
        self.assertIn("call run_skill with from=4", paused["text"])

        (done,), error = mcp_server.call_tool("run_skill", {"name": "add-a-customer", "from": 4})
        self.assertFalse(error, done["text"])

    def test_the_run_skill_tool_stops_for_approval_the_way_replay_does(self):
        save = field("button", "Save")
        self.fake.approval_on = save["path"]

        (paused,), error = mcp_server.call_tool("run_skill", {"name": "add-a-customer", "from": 4})

        self.assertFalse(error)
        self.assertTrue(paused["text"].startswith("NEEDS_APPROVAL:"))
        self.assertIn("run_skill again with from=4", paused["text"])


class Endpoints(unittest.TestCase):
    """What the control plane reaches on the desktop: /skills and the two requests that start teaching and doing."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT, recording.ROOT
        skills.ROOT, recording.ROOT = Path(self.tmp.name) / "skills", Path(self.tmp.name) / "recordings"

        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        answer_all(skill)
        skills.save(skill)
        skills.save(learning.build([demo("demo-20260914T101600-bbbbbb", "Acme", title="Half taught")]))

        for session in ("demo-20260914T101500-aaaaaa", "demo-20260914T101700-cccccc"):
            recording.ensure(session, title="x")
            recording.append(session, {"kind": "click", "outcome": "done"})

        self.sent = []
        patch = mock.patch.object(http_api, "save_request",
                                  lambda content, task_id="": self.sent.append((content, task_id)) or Path(f"{len(self.sent)}.txt"))
        patch.start()
        self.addCleanup(patch.stop)

        # A desktop's agent serves only with its token (http_api.refusal); the control plane sends it.
        token = mock.patch.dict(os.environ, {"DECURIX_AGENT_TOKEN": "test-agent-token"})
        token.start()
        self.addCleanup(token.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        skills.ROOT, recording.ROOT = self.saved
        self.tmp.cleanup()

    def call(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        # JSON, as the control plane sends it: :8081 refuses a write that is not (http_api.refusal).
        request = urllib.request.Request(self.base + path, data=data, method="POST" if body is not None else "GET",
                                         headers={"Content-Type": "application/json",
                                                  "Authorization": "Bearer test-agent-token"})
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as err:
            with err:
                return err.code, json.loads(err.read() or b"{}")

    def test_skills_are_listed_and_one_is_shown_with_its_steps(self):
        status, body = self.call("/skills")
        self.assertEqual(status, 200)
        by_name = {s["name"]: s for s in body["skills"]}
        self.assertEqual(by_name["add-a-customer"]["status"], "ready")
        self.assertEqual(by_name["add-a-customer"]["params"], [{"name": "customer_name", "label": "Customer name", "list": False, "example": "Acme"}])
        self.assertEqual((by_name["add-a-customer"]["gated"], by_name["add-a-customer"]["by_hand"]), (1, 1))
        self.assertEqual(by_name["half-taught"]["status"], "draft")

        status, body = self.call("/skills/add-a-customer")
        self.assertEqual(status, 200)
        self.assertEqual([s["n"] for s in body["skill"]["outline"]], [1, 2, 3, 4])
        self.assertTrue(body["skill"]["outline"][3]["consequential"])

        self.assertEqual(self.call("/skills/no-such-skill")[0], 404)
        # A name that is not one never touches a file.
        self.assertEqual(self.call("/skills/..%2Fetc")[0], 404)

    def test_running_a_skill_sends_the_request_the_watcher_understands(self):
        status, body = self.call("/skills/add-a-customer/run", {"task_id": "t0123456789abcdef", "params": {"customer_name": "Acme Ltd"}})

        self.assertEqual(status, 200)
        self.assertEqual(self.sent, [('/skill add-a-customer customer_name="Acme Ltd"', "t0123456789abcdef")])
        self.assertEqual(skill_task.parse_skill(body["content"]), ("add-a-customer", {"customer_name": "Acme Ltd"}))

    def test_a_skill_that_was_never_finished_is_not_run(self):
        self.assertEqual(self.call("/skills/half-taught/run", {"params": {}})[0], 409)
        self.assertEqual(self.sent, [])

    def test_teaching_names_the_recordings_and_checks_they_exist(self):
        status, body = self.call("/recordings/demo-20260914T101500-aaaaaa/learn",
                                 {"task_id": "t0123456789abcdef", "sessions": ["demo-20260914T101700-cccccc"]})

        self.assertEqual(status, 200)
        self.assertEqual(body["content"], "/learn demo-20260914T101500-aaaaaa demo-20260914T101700-cccccc")
        self.assertEqual(skill_task.parse_learn(body["content"])[0], ["demo-20260914T101500-aaaaaa", "demo-20260914T101700-cccccc"])

        self.assertEqual(self.call("/recordings/demo-20260914T101500-zzzzzz/learn", {})[0], 404)
        self.assertEqual(self.call("/recordings/demo-20260914T101500-aaaaaa/learn", {"sessions": ["../x"]})[0], 400)
        self.assertEqual(len(self.sent), 1)

    def test_a_skill_is_deleted(self):
        self.assertEqual(self.call("/skills/add-a-customer/delete", {})[0], 200)
        self.assertEqual(self.call("/skills/add-a-customer")[0], 404)
        self.assertEqual(self.call("/skills/add-a-customer/delete", {})[0], 404)

    def test_request_text_reads_back_as_written(self):
        text = skill_task.request_text("fill-an-order", {"item": "Tea\nMilk\n", "note": 'say "hi"', "empty": " "})

        self.assertEqual(text, '/skill fill-an-order item="Tea;Milk" note=\'say "hi"\'')
        self.assertEqual(skill_task.parse_skill(text), ("fill-an-order", {"item": "Tea;Milk", "note": 'say "hi"'}))

        with self.assertRaises(ValueError):
            skill_task.request_text("x", {"a": "both \" and '"})
        with self.assertRaises(ValueError):
            skill_task.request_text("x", {"not a name": "v"})


if __name__ == "__main__":
    unittest.main()
