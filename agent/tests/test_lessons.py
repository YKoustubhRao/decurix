import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import guidance, lessons, skill_task, state


class _Memory(unittest.TestCase):
    """A worker's memory folder, and a control plane that writes what it is told to remember into it."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.kept, self.says, self.shown = [], None, []

        def remember(title, summary, category="workflow"):
            self.kept.append((title, summary, category))
            (self.dir / f"memory-{len(self.kept):03d}.txt").write_text(
                f"Title: {title}\nCategory: {category}\nDesktopID: t\n\n{summary}\n", encoding="utf-8")
            return True

        def call(system, content, max_tokens=0):
            self.shown.append(content)
            if isinstance(self.says, Exception):
                raise self.says
            return self.says

        for p in (patch.object(lessons.config, "MEMORY_DIR", self.dir),
                  patch.object(lessons, "remember", remember),
                  patch.object(lessons.study, "_call", call),
                  patch.object(lessons.llm_client, "available", lambda: self.says is not None)):
            p.start()
            self.addCleanup(p.stop)

    def note(self, question, reply, **kw):
        thread = lessons.note(question, reply, **kw)
        if thread is not None:
            thread.join(5)
        return thread is not None


class TestKeeping(_Memory):
    def test_what_the_person_answers_is_distilled_into_a_lesson_and_kept(self):
        # Found live: "always choose first", answered during a run, was in no file afterwards.
        self.says = {"keep": True, "title": "Several people match a name",
                     "lesson": "When several people match a name, always choose the first one."}

        self.assertTrue(self.note("There are multiple recipients matching akshit gupta. Which one?",
                                  "always choose first", task="open linkedin and send akshit gupta \"hi\"", skill="jik"))

        title, text, category = self.kept[0]
        self.assertEqual((title, category), ("Lesson: Several people match a name", "lesson"))
        self.assertIn("always choose the first one", text)
        self.assertIn('The person answered: "always choose first"', text)      # their own words stay beside it
        self.assertIn("always choose first", self.shown[0])
        self.assertIn("always choose the first one", lessons.known())

    def test_of_a_long_answer_only_the_words_the_lesson_comes_from_are_kept(self):
        # Found live: an answer that told one run how to recover and also gave a rule was kept whole, and every
        # later task read "discard this draft, close the window and choose Leave" as if it were said to it.
        rule = "For each line type one key word and choose the entry by its name."
        reply = ("Discard this draft (close the window and choose Leave, nothing was saved) and start one fresh "
                 "record; the right one for this document was number 00030. " + rule)
        self.says = {"keep": True, "title": "Choose entries by name", "lesson": "Choose a list's entry by its name.",
                     "said": rule}
        self.assertTrue(self.note("I clicked the wrong entry. Discard or fix?", reply, skill="enter-a-record"))
        text = self.kept[-1][1]
        self.assertIn(f'The person answered: "{rule}"', text)
        for gone in ("Leave", "Discard", "00030", "I clicked the wrong entry"):
            self.assertNotIn(gone, text)

        # A quote that is not in the answer is not kept as the person's words.
        self.says = {"keep": True, "title": "Another", "lesson": "Do the other thing.", "said": "words never said"}
        self.assertTrue(self.note("Which way?", reply + " And another thing entirely, said at length here.", skill="x"))
        self.assertNotIn("The person answered", self.kept[-1][1])

    def test_the_model_is_shown_what_is_already_known_and_may_keep_nothing(self):
        self.says = {"keep": True, "title": "First match", "lesson": "Take the first match."}
        self.note("Which one?", "always the first")
        self.says = {"keep": False, "why": "already known"}

        self.note("Which of the two?", "the first, as I said")

        self.assertEqual(len(self.kept), 1)
        self.assertIn("Take the first match.", self.shown[1])

    def test_a_yes_a_retry_or_an_approval_is_not_a_lesson(self):
        self.says = {"keep": True, "title": "x", "lesson": "x"}
        for reply in ("yes", "no", "retry", "skip", "done", "2", "stop", "  OK. "):
            self.assertFalse(self.note("Carry on?", reply), reply)
        self.assertFalse(self.note("I need your OK before I click Send.", "yes, send it", kind="approval"))
        self.assertFalse(self.note("I need your OK before I click Send.", "no, do not send it", outcome="declined"))
        self.assertFalse(self.note("", "always choose first"))
        self.assertEqual((self.kept, self.shown), ([], []))

    def test_a_secret_is_never_kept_and_never_sent_to_be_read(self):
        for says in (None, {"keep": True, "title": "x", "lesson": "x"}):
            self.says = says
            self.assertFalse(self.note("What is the password for the portal?", "Tr0ub4dor&3"))
            self.assertFalse(self.note("LinkedIn wants a verification code. What is it?", "481 220"))
            self.assertFalse(self.note("Which key do I use?", "sk-live-51H8xK2eZvKYlo2C0aBcDeFgHiJkLmNoPqRsTuVwXyZ012345"))
        self.assertEqual((self.kept, self.shown), ([], []))

    def test_an_instruction_given_at_an_approval_is_looked_at(self):
        self.says = {"keep": False, "why": "this run only"}
        self.assertTrue(self.note("I need your OK before I click Send.",
                                  'your task was to send "decurix auto say hello", not hello', kind="approval", outcome="declined"))
        self.assertEqual(self.kept, [])

    def test_without_a_model_the_exchange_is_kept_as_it_was_said(self):
        self.note("Which customer list is this for?", "customers are under Sales, not Purchasing", task="add a customer")

        title, text, category = self.kept[0]
        self.assertEqual(category, "lesson")
        self.assertIn("Which customer list is this for?", text)
        self.assertIn("customers are under Sales, not Purchasing", text)

    def test_a_runs_own_question_is_only_looked_at_when_a_model_can_tell(self):
        self.assertFalse(self.note("I need a value for customer_name.", "Acme Corp", mechanical=True))
        self.says = {"keep": False, "why": "this run's value"}
        self.assertTrue(self.note("I need a value for customer_name.", "Acme Corp", mechanical=True))
        self.assertEqual(self.kept, [])

    def test_a_model_that_fails_loses_nothing_the_person_said(self):
        self.says = ValueError("not JSON")
        self.note("Which customer list is this for?", "customers are under Sales")
        self.assertIn("customers are under Sales", self.kept[0][1])

    def test_only_lessons_are_read_back_as_lessons(self):
        (self.dir / "memory-000.txt").write_text("Title: Skill: x\nCategory: skill\n\nTaught by demonstration.\n", encoding="utf-8")
        self.assertEqual(lessons.known(), "")


class TestAnsweringFromLessons(_Memory):
    """An engine's question in an ordinary task, settled by a lesson kept from an earlier one."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.told, self.judged = [], []
        self.answer = {"answered": True, "answer": "Take the first one.", "from": "always choose the first", "confidence": 0.9}

        def gate_call(system, content, max_tokens=0):
            self.judged.append(content)
            return self.answer

        skill_task._self_answered.update(task=None, count=0)
        for p in (patch.object(skill_task.study, "_call", gate_call),
                  patch.object(skill_task.llm_client, "available", lambda: True),
                  patch.object(skill_task.progress, "emit", lambda text: None),
                  patch.object(skill_task.task_context, "current", lambda: "t1"),
                  patch.object(skill_task.threading, "Thread",
                               lambda target, args, daemon: unittest.mock.Mock(start=lambda: self.told.append(args[0]))),
                  patch.object(state, "paused_skill", None), patch.object(state, "paused_replay", None)):
            p.start()
            self.addCleanup(p.stop)

    def lesson(self):
        (self.dir / "memory-001.txt").write_text(
            "Title: Lesson: matches\nCategory: lesson\n\nWhen several people match a name, always choose the first one.\n",
            encoding="utf-8")

    def test_a_kept_lesson_answers_the_question_and_nobody_is_asked(self):
        self.lesson()
        self.assertTrue(skill_task._question_gate("Two people are called Priya Shah. Which one?", "query", None))
        self.assertEqual(self.told, ["Take the first one."])
        self.assertIn("always choose the first one", self.judged[0])

    def test_a_lesson_that_says_to_ask_does_not_answer_the_question_for_the_person(self):
        self.lesson()
        for answer in ("Stop and ask the person; do not create a new entry.", "Check with them before choosing.",
                       "Ask me which one to use."):
            with self.subTest(answer=answer):
                self.answer = dict(self.answer, answer=answer)
                self.assertFalse(skill_task._question_gate("Nothing offered is the same thing. Which one?", "query", None))
        self.assertEqual(self.told, [])
        # It did not use up the task's two answers either.
        self.answer = dict(self.answer, answer="Take the first one.")
        self.assertTrue(skill_task._question_gate("Which one?", "query", None))

    def test_with_nothing_kept_the_person_is_asked_and_no_model_is(self):
        self.assertFalse(skill_task._question_gate("Two people are called Priya Shah. Which one?", "query", None))
        self.assertEqual((self.told, self.judged), ([], []))

    def test_never_an_approval_and_never_another_modules_question(self):
        self.lesson()
        self.assertFalse(skill_task._question_gate("I need your OK before I click Send.", "approval", None))
        self.assertFalse(skill_task._question_gate("Which value?", "query", "skill"))
        self.assertFalse(skill_task._question_gate("Which step?", "query", "replay"))
        with patch.object(state, "paused_skill", {"mode": "confirm"}):
            self.assertFalse(skill_task._question_gate("Is it done?", "query", None))
        self.assertEqual(self.judged, [])

    def test_at_most_twice_a_task(self):
        self.lesson()
        answers = [skill_task._question_gate(f"Which one ({n})?", "query", None) for n in range(4)]
        self.assertEqual(answers, [True, True, False, False])
        with patch.object(skill_task.task_context, "current", lambda: "t2"):
            self.assertTrue(skill_task._question_gate("Which one?", "query", None))


if __name__ == "__main__":
    unittest.main()
