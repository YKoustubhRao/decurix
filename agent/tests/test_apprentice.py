"""
A worker that learns its tasks with the model, runs them on its own once they
run smoothly, and never reports a success it has not seen.
"""

import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import config_stub  # noqa: F401  (first: a task file of this test process's own)
import test_skills as ts
from test_recordings import event, field

from decurix_agent import engine_questions, final_gate, guidance, messaging, skill_task, state, study, verify
from decurix_agent.computer import cli, evidence, experience, learning, skills


def verdict(kind, seen="", why=""):
    return {"verdict": kind, "confidence": 0.9 if kind == verify.DONE else 0.5, "seen": seen, "why": why}


class Maturity(unittest.TestCase):
    """How many clean runs a skill needs before it is run without the model."""

    def test_a_skill_is_learned_until_it_has_run_cleanly_enough_times_in_a_row(self):
        skill = {}
        for n in range(experience.TRUST_AFTER):
            self.assertEqual(experience.stage(skill), experience.APPRENTICE)
            experience.note_verdict(skill, "model", True, checked_by="model", basis=["model", "code"])
        self.assertEqual(experience.stage(skill), experience.TRUSTED)

    def test_a_failure_or_a_rescue_starts_the_count_again(self):
        for spoiler in ({"ok": False}, {"ok": True, "helped": True}, {"ok": True, "checked_by": None}):
            skill = {}
            for _ in range(experience.TRUST_AFTER):
                experience.note_verdict(skill, "replay", True, checked_by="screen", basis=["screen", "ledger"])
            experience.note_verdict(skill, "replay", spoiler["ok"], checked_by=spoiler.get("checked_by", "screen"),
                                    helped=spoiler.get("helped", False),
                                    basis=["ledger"] if spoiler.get("checked_by", "screen") else [])
            with self.subTest(spoiler):
                self.assertEqual(experience.stage(skill), experience.APPRENTICE)
                self.assertEqual(experience.clean_streak(skill), 0)


class WithTheModel(ts.TeachingBase):
    """A taught skill run on a desktop whose engine can drive the screen with a hosted model."""

    def setUp(self):
        super().setUp()
        self.engine_calls, self.judged = [], []
        self.claims = []          # what the fake engine reports, in turn
        self.verdicts = []        # what the check says, in turn
        state.backend_ready = True
        guidance.end()
        self.acted_on_screen = True   # whether the fake engine "worked the screen" during the task

        def engine(request, task_file=None):
            self.engine_calls.append((request, guidance.text()))
            claim = self.claims.pop(0) if self.claims else "DONE"
            # What messaging.write_final_reply does with an engine's report.
            if not skill_task._gate(claim, "final", "thebot"):
                self.finals.append((claim, "final"))

        def judge(*args, **kwargs):
            self.judged.append(kwargs)
            return self.verdicts.pop(0) if self.verdicts else verdict(verify.UNSURE)

        for patch in (mock.patch.object(skill_task, "_engine_can_drive", lambda: True),
                      mock.patch.object(final_gate, "_did_screen_work", lambda: self.acted_on_screen),
                      mock.patch.object(skill_task, "_engine", engine),
                      mock.patch.object(skill_task.verify, "judge", judge),
                      mock.patch.object(skill_task.verify, "available", lambda: True),
                      mock.patch.object(study, "available", lambda: False),
                      mock.patch.object(skill_task.evidence, "live", lambda scale=1: b"now"),
                      mock.patch.object(skill_task.evidence, "final_frame", lambda skill: None)):
            patch.start()
            self.addCleanup(patch.stop)
        self.addCleanup(guidance.end)

    def taught_customer(self):
        self.teach("the customer is in the list", "yes", "changes", "yes")
        self.asked.clear()
        self.finals.clear()
        return skills.load("add-a-customer")

    def trust(self, name):
        skill = skills.load(name)
        for _ in range(experience.TRUST_AFTER):
            experience.note_verdict(skill, "replay", True, checked_by="screen", basis=["screen", "ledger"])
        skills.save(skill)

    def one_step(self, expect=False):
        extra = {"expect": {"appeared": [{"role": "alert", "name": "Saved"}]}} if expect else {}
        meta = {"id": self.SESSION, "title": "Save the form", "kind": "demonstration", "status": "complete",
                "started_at": 1789380000, "screen": [1280, 720]}
        skill = learning.build([(meta, [event(0, "click", field("button", "Save"), **extra)])])
        skill["status"] = skills.READY
        skill["explanation"] = "save it once"
        skills.save(skill)
        return skill["name"]

    # --- learning: the model does it, guided by the skill -------------------

    def test_a_skill_still_being_learned_is_done_by_the_model_with_the_skill_as_its_guide(self):
        self.taught_customer()
        fake = self.screen()
        self.verdicts = [verdict(verify.DONE, seen="Initech is in the customer list")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertEqual(fake.acted, [])                       # no replay: the model did it
        request, told = self.engine_calls[0]
        self.assertIn("add-a-customer", request)
        self.assertIn('customer_name = "Initech"', told)
        self.assertIn("gui skill add-a-customer --look N", told)
        self.assertIn("the customer is in the list", told)
        self.assertIn("never report DONE", told)
        self.assertIn("0 of 3 clean runs", told)

    def test_a_done_with_nothing_done_on_the_screen_is_never_put_to_the_check(self):
        # Found live: a taught search was answered "DONE" by an engine that had
        # touched nothing, and the check passed it - the demonstration's own
        # page was still open. A screen task with no screen action was not
        # done, whatever the screen shows: sent back once, then asked about.
        self.taught_customer()
        self.screen()
        self.acted_on_screen = False
        self.verdicts = [verdict(verify.DONE, seen="Acme is in the customer list")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertEqual(self.judged, [])                      # no picture was asked to prove anything
        self.assertEqual(len(self.engine_calls), 2)            # sent back once...
        # Sent back as a first attempt, not as work to find and correct: nothing of it exists.
        self.assertIn("KEPT NOTHING IN THE APPLICATION", self.engine_calls[1][1])
        self.assertNotIn("SECOND ATTEMPT", self.engine_calls[1][1])
        self.assertEqual(self.finals, [])                      # ...and never reported done
        self.assertIn("I do not think this is done", self.asked[-1][0])

    def test_what_done_looks_like_is_said_in_this_runs_values(self):
        # The lesson's words name the demonstration's value ("Acme"); the model
        # and the check are told this run's.
        self.teach("Acme is in the customer list", "yes", "changes", "yes")
        self.asked.clear()
        self.finals.clear()
        self.screen()
        self.verdicts = [verdict(verify.DONE, seen="Initech is in the customer list")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        told = self.engine_calls[0][1]
        self.assertIn("Initech is in the customer list", told)
        self.assertNotIn("Acme is in the customer list", told)
        self.assertIn("Initech is in the customer list", self.judged[0]["criteria"])

    def test_a_request_matched_to_a_taught_task_is_handed_over_as_one(self):
        # Found live: the person's own words (`search "sundar pichai"`) went to
        # the engine, which took them for a question and answered without
        # doing anything. The engine's operator takes "Do the task you were taught".
        skill = self.taught_customer()
        run = {"skill": skill, "values": {"customer_name": "Initech"}, "mimic": {"text": "add customer Acme", "id": "x"},
               "context": "add customer Initech"}

        request = skill_task._engine_request(run)

        self.assertTrue(request.startswith('Do the task you were taught, "Add a customer" (skill add-a-customer)'))
        self.assertIn('customer_name="Initech"', request)
        self.assertIn("The person asked: add customer Initech", request)

    def test_values_the_documents_gave_are_reported_when_the_model_did_the_task(self):
        # Found by bench/learn_documents: the replay's report said where each
        # value was read, the model's (a skill still being learned) did not.
        self.taught_customer()
        self.screen()
        self.verdicts = [verdict(verify.DONE, seen="Initech is in the customer list")]
        read = ({"customer_name": "Initech"}, {"customer_name": "PO.pdf, Bill To"}, None)

        with mock.patch.object(skill_task.binding, "available", return_value=True), \
                mock.patch.object(skill_task.binding, "fill", return_value=read):
            skill_task.perform("/skill add-a-customer\nsee /home/headless/work/PO.pdf")

        self.assertTrue(self.engine_calls, "the model did not do the task")
        text, kind = self.finals[-1]
        self.assertEqual(kind, "final")
        self.assertIn("Values read from the documents", text)
        self.assertIn("Bill To", text)

    def test_a_done_is_reported_only_once_the_screen_shows_it(self):
        self.taught_customer()
        self.screen()
        self.verdicts = [verdict(verify.DONE, seen="Initech is in the customer list")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        text, kind = self.finals[-1]
        self.assertEqual(kind, "final")
        self.assertIn("Checked on the screen: Initech is in the customer list", text)
        self.assertEqual(self.judged[0]["values"], {"customer_name": "Initech"})
        last = skills.load("add-a-customer")["experience"]["verdicts"][-1]
        self.assertEqual((last["mode"], last["ok"], last["checked_by"]), ("model", True, "model"))
        self.assertIsNone(guidance.get())

    def test_a_done_the_screen_does_not_show_is_tried_again_then_asked_never_reported(self):
        self.taught_customer()
        self.screen()
        self.verdicts = [verdict(verify.NOT_DONE, seen="the form still shows an empty name"),
                         verdict(verify.NOT_DONE, seen="a validation error under Customer name")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertEqual(len(self.engine_calls), 2)
        self.assertIn("the form still shows an empty name", self.engine_calls[1][1])
        self.assertEqual(self.finals, [])                       # never a success
        self.assertIn("I do not think this is done", self.asked[-1][0])
        self.assertIn("a validation error under Customer name", self.asked[-1][0])

    def test_an_unsure_check_asks_and_the_person_can_say_it_is_done(self):
        self.taught_customer()
        self.screen()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        self.assertIn("I cannot confirm this is done", self.asked[-1][0])
        self.assertEqual(self.finals, [])

        skill_task.resume("done")

        self.assertEqual(self.finals[-1][1], "final")
        last = skills.load("add-a-customer")["experience"]["verdicts"][-1]
        self.assertEqual((last["ok"], last["checked_by"], last["helped"]), (True, "person", True))
        self.assertEqual(skills.load("add-a-customer")["experience"]["verdicts"][-1]["mode"], "model")

    def test_what_the_person_says_is_wrong_is_remembered_and_tried_again(self):
        self.taught_customer()
        self.screen()
        self.verdicts = [verdict(verify.UNSURE), verdict(verify.DONE, seen="Initech saved")]

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        skill_task.resume("it went into the supplier list, customers are under Sales")

        saved = skills.load("add-a-customer")
        self.assertIn("customers are under Sales", saved["corrections"][-1]["text"])
        self.assertFalse(saved["experience"]["verdicts"][0]["ok"])
        self.assertIn("customers are under Sales", self.engine_calls[-1][1])
        self.assertIn("Checked on the screen", self.finals[-1][0])

    # --- a question the engine asks while doing a taught task ----------------

    def asking(self, question, kind="query", lesson_says=None):
        """Run the skill with an engine that stops to ask `question`; returns (answered itself, what it told the engine)."""

        self.taught_customer()
        self.screen()
        outcome, told, calls = [], [], []

        def engine(request, task_file=None):
            self.engine_calls.append((request, guidance.text()))
            outcome.append(skill_task._question_gate(question, kind, "thebot"))

        def call(system, content, max_tokens=0):
            calls.append(content)
            return lesson_says or {"answered": False}

        for patch in (mock.patch.object(skill_task, "_engine", engine),
                      mock.patch.object(study, "_call", call),
                      mock.patch.object(skill_task.llm_client, "available", lambda: True),
                      mock.patch.object(engine_questions.threading, "Thread",
                                        lambda target, args, daemon: mock.Mock(start=lambda: told.append(args[0])))):
            patch.start()
            self.addCleanup(patch.stop)

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        return outcome[0], told, calls

    def test_a_question_the_lesson_already_answers_is_not_put_to_the_person(self):
        answered, told, calls = self.asking(
            "Two customers match Initech. Which one?",
            lesson_says={"answered": True, "answer": "Take the first one.", "from": "choose the first", "confidence": 0.95})

        self.assertTrue(answered)
        self.assertEqual(told, ["Take the first one."])
        self.assertIn("the customer is in the list", calls[0])     # the model was shown what was taught
        self.assertNotIn("run_answers", skills.load("add-a-customer"))

    def test_an_unsure_reading_of_the_lesson_asks_the_person(self):
        answered, told, _ = self.asking(
            "Two customers match Initech. Which one?",
            lesson_says={"answered": True, "answer": "Take the first one.", "confidence": 0.5})

        self.assertFalse(answered)
        self.assertEqual(told, [])

    def test_an_approval_is_never_answered_from_the_lesson(self):
        answered, told, calls = self.asking(
            "I need your OK before I click Send.", kind="approval",
            lesson_says={"answered": True, "answer": "Yes.", "confidence": 1.0})

        self.assertFalse(answered)
        self.assertEqual((told, calls), ([], []))
        skill_task.learn_answer("yes")                              # a yes to an approval is not a lesson
        self.assertNotIn("run_answers", skills.load("add-a-customer"))

    def test_what_the_person_answers_during_a_run_is_kept_for_every_later_run(self):
        # Found live: "always choose first", answered during a run, reached
        # that run's engine and nothing else - the next run would ask again.
        answered, _, _ = self.asking("Two customers match Initech. Which one?")
        self.assertFalse(answered)

        skill_task.learn_answer("always choose first")
        skill_task.learn_answer("always choose first")              # the reply is taken once

        kept = skills.load("add-a-customer")["run_answers"]
        self.assertEqual([(x["q"], x["a"]) for x in kept],
                         [("Two customers match Initech. Which one?", "always choose first")])

        guidance.end()
        skill_task.perform('/skill add-a-customer customer_name="Globex"')
        self.assertIn("Which one? - the person answered: always choose first", self.engine_calls[-1][1])
        self.assertIn("never ask them the same thing again", self.engine_calls[-1][1])

    def test_no_means_a_failure_not_a_success(self):
        self.taught_customer()
        self.screen()

        skill_task.perform('/skill add-a-customer customer_name="Initech"')
        skill_task.resume("no")

        self.assertEqual(self.finals[-1][1], "error")

    def test_an_engine_that_fails_is_recorded_as_a_failure(self):
        self.taught_customer()
        self.screen()
        guidance.end()

        def failing(request, task_file=None):
            self.engine_calls.append(request)
            skill_task._gate("could not find the form", "error", "thebot")

        with mock.patch.object(skill_task, "_engine", failing):
            skill_task.perform('/skill add-a-customer customer_name="Initech"')

        self.assertFalse(skills.load("add-a-customer")["experience"]["verdicts"][-1]["ok"])
        self.assertIsNone(guidance.get())

    # --- trusted: replayed, the model on call -------------------------------

    def test_a_trusted_skill_is_replayed_and_the_model_takes_over_only_when_it_is_stuck(self):
        name = self.one_step()
        self.trust(name)
        fake = self.screen()
        fake.elements[:] = [field("button", "Archive")]
        self.verdicts = [verdict(verify.DONE, seen="saved")]

        skill_task.perform(f"/skill {name}")

        self.assertEqual(len(self.engine_calls), 1)
        self.assertIn("stopped there", self.engine_calls[0][1])
        self.assertIn("Carry on from step 1", self.engine_calls[0][1])
        last = skills.load(name)["experience"]["verdicts"][-1]
        self.assertEqual((last["ok"], last["helped"]), (True, True))
        self.assertEqual(experience.stage(skills.load(name)), experience.APPRENTICE)

    def test_a_trusted_replay_whose_every_result_showed_needs_no_model(self):
        name = self.one_step(expect=True)
        self.trust(name)
        fake = self.screen()
        fake.elements[:] = [field("button", "Save")]
        fake.after = {"adds": [field("alert", "Saved")]}

        skill_task.perform(f"/skill {name}")

        self.assertEqual(self.engine_calls, [])
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals[-1][1], "final")

    def test_a_trusted_replay_that_could_not_check_its_result_is_judged(self):
        name = self.one_step()
        self.trust(name)
        fake = self.screen()
        fake.elements[:] = [field("button", "Save")]
        self.verdicts = [verdict(verify.DONE, seen="the form says Saved")]

        skill_task.perform(f"/skill {name}")

        self.assertEqual(self.engine_calls, [])
        self.assertIn("Checked on the screen: the form says Saved", self.finals[-1][0])

    def test_a_trusted_replay_the_check_doubts_is_asked_about(self):
        name = self.one_step()
        self.trust(name)
        fake = self.screen()
        fake.elements[:] = [field("button", "Save")]
        self.verdicts = [verdict(verify.NOT_DONE, seen="an error: name required")]

        skill_task.perform(f"/skill {name}")

        # The model looks into it before a person is asked; with nothing more to go on, the person then is -
        # and the skill is one the model does from now on, with the replay as its tool.
        self.assertEqual(len(self.engine_calls), 1)
        self.assertEqual(self.finals, [])
        self.assertIn("I cannot confirm this is done", self.asked[-1][0])
        self.assertIn("an error: name required", skills.load(name)["supervised"])


class WithoutAModel(ts.TeachingBase):
    """No hosted model: a replay that could not check itself asks the person."""

    def test_an_unchecked_replay_is_never_reported_as_done(self):
        name = WithTheModel.one_step(self)
        fake = self.screen()
        fake.elements[:] = [field("button", "Save")]

        with mock.patch.object(skill_task.verify, "available", lambda: False), \
                mock.patch.object(skill_task.evidence, "live", lambda scale=1: None):
            skill_task.perform(f"/skill {name}")

        self.assertEqual(self.finals, [])
        self.assertIn("I cannot confirm this is done", self.asked[-1][0])


class StudiedLesson(ts.TeachingBase):
    """With a model, a lesson starts with it studying the demonstration and asking what it could not work out."""

    UNDERSTANDING = {
        "summary": "Adds a customer to the shop's customer list.",
        "steps": {"1": "opens the customer list"},
        "inputs": {"customer_name": "the new customer's company name, from the request"},
        "varies": {"customer_name": True},
        "done_when": "the new name is in the customer list",
        "incidental": [], "rules": [],
        "questions": [{"id": "q1", "text": "What if the customer already exists?", "why": ""},
                      {"id": "q2", "text": "Is a banner ever in the way?", "why": ""}],
        "follow_ups": 0, "dialogue": [],
    }

    def setUp(self):
        super().setUp()
        self.absorbed = []
        self.follow_ups = []

        def absorb(skill, question, answer):
            self.absorbed.append((question, answer))
            skill["understanding"].setdefault("rules", []).append(f"rule from: {answer}")
            return self.follow_ups.pop(0) if self.follow_ups else ""

        for patch in (mock.patch.object(study, "available", lambda: True),
                      mock.patch.object(study, "study",
                                        lambda skill: {k: (v.copy() if hasattr(v, "copy") else v)
                                                       for k, v in self.UNDERSTANDING.items()}),
                      mock.patch.object(study, "absorb", absorb)):
            patch.start()
            self.addCleanup(patch.stop)

    def test_the_model_shows_what_it_understood_and_asks_its_own_questions_one_at_a_time(self):
        self.record()
        skill_task.perform(f"/learn {self.SESSION}")

        self.assertIn("What I understood: Adds a customer", self.asked[-1][0])
        self.assertIn("Reply `yes` if it is right", self.asked[-1][0])

        skill_task.resume("yes")
        self.assertEqual(self.asked[-1][0], "What if the customer already exists?")
        self.assertEqual(self.absorbed, [])                     # a plain yes has nothing to fold in

        self.follow_ups = ["Should I update the existing one or stop?"]
        skill_task.resume("then do not add it again")
        self.assertEqual(self.absorbed[-1], ("What if the customer already exists?", "then do not add it again"))
        self.assertEqual(self.asked[-1][0], "Should I update the existing one or stop?")

        skill_task.resume("stop and tell me")
        self.assertEqual(self.asked[-1][0], "Is a banner ever in the way?")

    def test_the_value_question_says_what_the_model_thinks_each_value_is(self):
        self.record()
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("yes", "skip", "skip", "yes"):
            skill_task.resume(reply)

        self.assertIn("the new customer's company name", self.asked[-1][0])

    def test_the_last_question_shows_the_procedure_and_a_correction_is_not_a_name(self):
        self.record()
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("yes", "if it exists, do not add it", "skip", "yes", "changes"):
            skill_task.resume(reply)

        self.assertIn("What I understood", self.asked[-1][0])
        self.assertIn("rule from: if it exists, do not add it", self.asked[-1][0])

        skill_task.resume("no wait, always check the customer list for duplicates before saving")

        self.assertEqual(self.absorbed[-1][1], "no wait, always check the customer list for duplicates before saving")
        self.assertIn("here it is again", self.asked[-1][0])
        self.assertEqual(skills.load("add-a-customer")["status"], skills.DRAFT)

        skill_task.resume("yes")
        saved = skills.load("add-a-customer")
        self.assertEqual(saved["status"], skills.READY)
        self.assertIn("duplicates", " ".join(saved["understanding"]["rules"]))
        self.assertIn("the new name is in the customer list", skill_task._criteria(saved))


class Studying(unittest.TestCase):
    """study.py against a scripted model."""

    SKILL = {"name": "ask-chatgpt", "goal": 'chatgpt "what can you do"', "demos": 1,
             "params": [{"name": "typed_text", "label": "Typed text", "example": "what can you do", "varies": None}],
             "steps": [{"n": 1, "id": "s1", "kind": "double_click", "summary": "double click 71,264", "point": [71, 264]},
                       {"n": 2, "id": "s2", "kind": "type", "summary": "type {typed_text}", "param": "typed_text"}]}

    def model(self, *answers):
        replies = list(answers)
        calls = []

        def chat(messages, **kwargs):
            calls.append(messages)
            return {"content": replies.pop(0)}, {"prompt_tokens": 10, "completion_tokens": 5}

        patch = mock.patch.object(study.llm_client, "chat", chat)
        patch.start()
        self.addCleanup(patch.stop)
        for other in (mock.patch.object(study.llm_client, "available", lambda: True),
                      mock.patch.object(study.evidence, "step_frame", lambda step, which="before": None)):
            other.start()
            self.addCleanup(other.stop)
        return calls

    def test_what_the_model_works_out_is_kept_to_what_the_skill_has(self):
        self.model('Sure: {"summary": "Asks ChatGPT a question.", "steps": {"1": "opens Chrome", "9": "no such step"}, '
                   '"inputs": {"typed_text": "the question", "ghost": "x"}, "varies": {"typed_text": true}, '
                   '"done_when": "ChatGPT has answered", "incidental": [2, 40], "rules": [], '
                   '"questions": [{"id": "a", "text": "One chat per question?"}, {"text": "  "}]}')
        understood = study.study(dict(self.SKILL))

        self.assertEqual(understood["steps"], {"1": "opens Chrome"})
        self.assertEqual(understood["inputs"], {"typed_text": "the question"})
        self.assertEqual(understood["incidental"], [2])
        self.assertEqual([q["text"] for q in understood["questions"]], ["One chat per question?"])
        self.assertEqual(understood["questions"][0]["id"], "q1")

    def test_an_answer_is_folded_in_and_the_persons_rules_are_never_dropped(self):
        self.model('{"summary": "Asks ChatGPT.", "done_when": "answered", "rules": [], "questions": []}',
                   '{"summary": "Asks ChatGPT each question.", "done_when": "every question answered", '
                   '"rules": ["Ask each question in a new chat."], "follow_up": "Report each answer?"}',
                   '{"summary": "Asks ChatGPT each question.", "rules": [], "follow_up": ""}')
        skill = dict(self.SKILL)
        skill["understanding"] = study.study(skill)

        self.assertEqual(study.absorb(skill, "One chat per question?", "yes, new chat each time"), "Report each answer?")
        self.assertEqual(study.absorb(skill, "Report each answer?", "yes"), "")
        # The model's second answer returned no rules; the earlier ones stand,
        # and the person's words are kept as said.
        self.assertEqual(skill["understanding"]["rules"], ["Ask each question in a new chat."])
        self.assertEqual(skill["understanding"]["dialogue"][0], {"q": "One chat per question?", "a": "yes, new chat each time"})
        self.assertIn("yes, new chat each time", skill_task._criteria(skill))

    def test_questions_the_answers_already_settle_are_not_asked(self):
        self.model('{"summary": "x", "questions": [{"text": "One by one?"}, {"text": "Wait for each answer?"}, '
                   '{"text": "Report them all?"}]}',
                   '{"summary": "x", "rules": ["One at a time, waiting for each."], "settled": ["q2"]}')
        skill = dict(self.SKILL, answers={})
        skill["understanding"] = study.study(skill)
        skill["answers"]["study:q1"] = "yes, and wait for each answer"
        study.absorb(skill, "One by one?", "yes, and wait for each answer")
        self.assertEqual([q["id"] for q in skill["understanding"]["questions"]], ["q1", "q3"])

    def test_follow_ups_are_bounded(self):
        answer = '{"summary": "x", "rules": [], "follow_up": "and?"}'
        self.model('{"summary": "x", "questions": []}', *[answer] * (study.MAX_FOLLOW_UPS + 2))
        skill = dict(self.SKILL)
        skill["understanding"] = study.study(skill)
        asked = [study.absorb(skill, "q", "a") for _ in range(study.MAX_FOLLOW_UPS + 2)]
        self.assertEqual(sum(1 for a in asked if a), study.MAX_FOLLOW_UPS)

    def test_a_model_that_fails_loses_nothing_the_person_said(self):
        self.model('{"summary": "x", "questions": []}', "not json at all")
        skill = dict(self.SKILL)
        skill["understanding"] = study.study(skill)
        study.absorb(skill, "One chat per question?", "always a new chat")
        self.assertIn("always a new chat", skill["understanding"]["rules"][-1])

    def test_without_a_model_there_is_no_study(self):
        with mock.patch.object(study.llm_client, "available", lambda: False):
            self.assertIsNone(study.study(dict(self.SKILL)))


class Judging(unittest.TestCase):
    """verify.judge's own rules: only a confident done is done."""

    def judge_with(self, reply):
        with mock.patch.object(verify.llm_client, "available", lambda: True), \
                mock.patch.object(verify.llm_client, "chat", lambda messages, **kw: ({"content": reply}, {})):
            return verify.judge("add Initech", claim="done", live_png=b"png")

    def test_a_done_not_confident_enough_is_unsure(self):
        self.assertEqual(self.judge_with('{"verdict": "done", "confidence": 0.6, "seen": "a list", "why": "maybe"}')["verdict"],
                         verify.UNSURE)
        self.assertEqual(self.judge_with('{"verdict": "done", "confidence": 0.95, "seen": "Initech listed", "why": "y"}')["verdict"],
                         verify.DONE)

    def test_an_answer_that_cannot_be_read_is_unsure(self):
        self.assertEqual(self.judge_with("I think it worked!")["verdict"], verify.UNSURE)
        self.assertEqual(self.judge_with('{"verdict": "probably"}')["verdict"], verify.UNSURE)

    def test_no_model_is_unsure(self):
        with mock.patch.object(verify.llm_client, "available", lambda: False):
            self.assertEqual(verify.judge("x")["verdict"], verify.UNSURE)


class LookingAtAStep(ts.TeachingBase):
    def test_gui_skill_look_leaves_the_recorded_screen_as_the_commands_picture(self):
        name = WithTheModel.one_step(self)
        with tempfile.TemporaryDirectory() as tmp:
            shot = Path(tmp) / "shot.png"
            with mock.patch.dict(os.environ, {"DECURIX_GUI_SCREENSHOT": str(shot)}), \
                    mock.patch.object(evidence, "step_picture", lambda step: b"\x89PNG recorded"), \
                    mock.patch("builtins.print") as printed:
                code = cli.run(["skill", name, "--look", "1"])
            self.assertEqual(code, cli.EXIT_OK)
            self.assertEqual(shot.read_bytes(), b"\x89PNG recorded")
            said = " ".join(str(c.args[0]) for c in printed.call_args_list)
            self.assertIn("RECORDING's screen", said)


class AnyScreenTask(ts.TeachingBase):
    """Not only taught tasks: anything an engine did on the screen is checked before it is reported done."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.verdicts = []
        for patch in (mock.patch.object(skill_task.verify, "available", lambda: True),
                      mock.patch.object(skill_task.verify, "judge", lambda *a, **kw: self.verdicts.pop(0)),
                      mock.patch.object(skill_task.evidence, "live", lambda scale=1: b"now"),
                      mock.patch.object(skill_task, "_engine_can_drive", lambda: False)):
            patch.start()
            self.addCleanup(patch.stop)
        state.current_task = "open the invoice and approve it"

    def test_a_chat_answer_is_not_checked(self):
        # Not judged - nothing was read to judge it by - and said NOT checked, never sent as a checked result
        # (E-02/T-01, review 2026-11-03).
        with mock.patch.object(final_gate, "_did_screen_work", lambda: False), \
                mock.patch.object(skill_task.evidence, "observed", lambda *a, **k: []), \
                mock.patch.object(skill_task.evidence, "files_read", lambda *a, **k: []), \
                mock.patch.object(final_gate, "_commands_text", lambda: ""):
            self.assertTrue(skill_task._gate("The kernel is 6.6.", "final", "thebot"))
        self.assertTrue(self.finals[-1][0].startswith(skill_task.UNCHECKED_ANSWER))

    def test_screen_work_is_reported_done_only_once_seen(self):
        self.verdicts = [verdict(verify.DONE, seen="Invoice INV-1 shows Approved")]
        with mock.patch.object(final_gate, "_did_screen_work", lambda: True):
            self.assertTrue(skill_task._gate("Approved it.", "final", "thebot"))
        self.assertIn("Checked on the screen: Invoice INV-1 shows Approved", self.finals[-1][0])

    def test_screen_work_the_check_doubts_is_asked_about(self):
        self.verdicts = [verdict(verify.NOT_DONE, seen="the approve button is still there")]
        with mock.patch.object(final_gate, "_did_screen_work", lambda: True):
            self.assertTrue(skill_task._gate("Approved it.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("the approve button is still there", self.asked[-1][0])

        skill_task.resume("no")
        self.assertEqual(self.finals[-1][1], "error")


class FinalGate(unittest.TestCase):
    def test_a_gate_that_breaks_never_passes_a_success_off_as_checked(self):
        sent = []

        def broken(reply, kind, engine):
            raise RuntimeError("boom")

        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", Path(tmp)), \
                mock.patch.object(messaging, "send_query", lambda *a, **kw: sent.append(kw["reply"]) or True), \
                mock.patch.object(messaging, "_final_gate", broken):
            messaging.write_final_reply("All done.", kind="final")

        self.assertIn("Not checked", sent[0])


if __name__ == "__main__":
    unittest.main()


class Evidence(unittest.TestCase):
    """What the tools saw during a task, for the check: its screen actions and what was read off the screen."""

    def test_notes_and_actions_of_the_task_are_what_was_observed(self):
        from decurix_agent.computer import recording
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(evidence, "NOTES_DIR", Path(tmp) / "notes"), \
                mock.patch.object(recording, "ROOT", Path(tmp) / "rec"), \
                mock.patch.object(evidence.task_context, "current", lambda: "t0123456789abcdef"):
            recording.append("task-t0123456789abcdef", {"kind": "open", "summary": "open https://chatgpt.com/?q=a",
                                                        "outcome": "done"})
            evidence.note("ChatGPT was asked 'what is 7+8?'; ChatGPT says: 15")
            seen = evidence.observed()
        self.assertEqual(seen, ["action: open https://chatgpt.com/?q=a (done)",
                                "read off the screen: ChatGPT was asked 'what is 7+8?'; ChatGPT says: 15"])

    def test_what_an_action_found_is_kept_beside_it(self):
        # A duplicate check made along the way: the search is an action, its answer is what the screen did.
        from decurix_agent.computer import recording
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(evidence, "NOTES_DIR", Path(tmp) / "notes"), \
                mock.patch.object(recording, "ROOT", Path(tmp) / "rec"), \
                mock.patch.object(evidence.task_context, "current", lambda: "t0123456789abcdef"):
            recording.append("task-t0123456789abcdef", {"kind": "type", "summary": 'type "NXG-2026-8812" into textbox "Search"',
                                                        "outcome": "done", "change": '1 element(s) appeared: text "No Results found"'})
            recording.append("task-t0123456789abcdef", {"kind": "key", "summary": "press Escape", "outcome": "done",
                                                        "change": "x" * 1000})
            seen = evidence.observed()
        self.assertEqual(seen[0], 'action: type "NXG-2026-8812" into textbox "Search" (done) -> 1 element(s) appeared: text "No Results found"')
        self.assertLess(len(seen[1]), 300)

    def test_the_check_is_shown_what_was_observed(self):
        sent = []
        with mock.patch.object(verify.llm_client, "available", lambda: True), \
                mock.patch.object(verify.llm_client, "chat",
                                  lambda messages, **kw: sent.append(messages) or ({"content": '{"verdict": "unsure"}'}, {})):
            verify.judge("ask chatgpt", claim="15", observed=["read off the screen: ChatGPT says: 15"])
        self.assertIn("ChatGPT says: 15", sent[0][1]["content"][0]["text"])


class ReplayCommand(unittest.TestCase):
    """/replay runs a recording as recorded; one that could not see its results is not reported done either."""

    def setUp(self):
        self.finals, self.asked = [], []
        state.paused_skill = None
        state.current_task = "/replay demo-1"
        self.addCleanup(setattr, state, "paused_skill", None)
        from decurix_agent import replay_task
        self.replay_task = replay_task
        for patch in (mock.patch.object(skill_task, "write_final_reply",
                                        lambda text, kind="final", engine=None, **kw: self.finals.append((text, kind))),
                      mock.patch.object(skill_task, "ask_for_query",
                                        lambda text, kind="query", engine=None, **kw: self.asked.append((text, kind))),
                      mock.patch.object(skill_task.evidence, "live", lambda scale=1: None),
                      mock.patch.object(skill_task.evidence, "observed", lambda task_id=None: []),
                      mock.patch.object(replay_task.replay, "run", lambda program, params=None, start=1: {
                          "outcome": "completed_unverified", "text": "Replayed: 3 steps ran, 1 checked.",
                          "step": None, "resume_from": None})):
            patch.start()
            self.addCleanup(patch.stop)

    def test_without_a_model_it_asks(self):
        with mock.patch.object(skill_task.verify, "available", lambda: False):
            self.replay_task._run({"session": "demo-1"}, {}, 1)
        self.assertEqual(self.finals, [])
        self.assertIn("I cannot confirm this is done", self.asked[-1][0])

        skill_task.resume("no")
        self.assertEqual(self.finals[-1][1], "error")

    def test_with_a_model_that_sees_it_done_it_is_done(self):
        with mock.patch.object(skill_task.verify, "judge", lambda *a, **kw: verdict(verify.DONE, seen="3 rows saved")):
            self.replay_task._run({"session": "demo-1"}, {}, 1)
        self.assertIn("Checked on the screen: 3 rows saved", self.finals[-1][0])


class ADoubtShowsTheWholeReport(unittest.TestCase):
    def test_a_finding_at_the_end_of_a_long_report_reaches_the_person(self):
        claim = "Done. " + "Filled a field. " * 60 + "ID_scan.png does not belong to her - not uploaded."
        text = skill_task._doubt_text(claim, {"verdict": verify.UNSURE, "seen": "the attachments tab"})
        self.assertIn("ID_scan.png does not belong to her", text)
        self.assertIn("Reply `done`", text)
