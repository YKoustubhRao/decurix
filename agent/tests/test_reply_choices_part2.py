"""
A reply to a question with set answers is read once (approval.read_choice): code decides only a reply that is, whole,
one of a choice's plain phrases; any other reply is the model's reading, and what it adds to the choice is kept.
Found (audit, 2026-10-24): word lists anchored at a reply's first words stopped a run on "stop asking, just use last
month's rate", took "no problem, done" for a stop, "not yet, the code hasn't come" for done, and kept only "same" of
"same, except for one customer it's 5%".
"""

import json
import unittest
from unittest import mock

from decurix_agent import guidance, llm_client, long_task, replay_task, state
from decurix_agent.computer import approval, learning


def reads_as(choice, instruction=""):
    """llm_client patched to read every reply as `choice`, with `instruction`."""
    answer = {"content": json.dumps({"choice": choice, "instruction": instruction})}
    return [mock.patch.object(llm_client, "available", lambda: True),
            mock.patch.object(llm_client, "chat", mock.Mock(return_value=(answer, {})))]


class Patched:
    def __init__(self, patches):
        self.patches = patches

    def __enter__(self):
        return [p.start() for p in self.patches]

    def __exit__(self, *exc):
        for p in self.patches:
            p.stop()


def no_model():
    return Patched([mock.patch.object(llm_client, "available", lambda: False),
                    mock.patch.object(llm_client, "chat", side_effect=AssertionError("asked a model"))])


class TheOneReadingOfAChoice(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_a_plain_reply_is_decided_in_code_and_anything_else_without_a_model_is_unclear(self):
        with no_model():
            self.assertEqual(long_task.stuck_choice("Skip it, please.")["choice"], "skip")
            self.assertEqual(long_task.stuck_choice("stop")["choice"], "stop")
            self.assertEqual(long_task.stuck_choice("try again")["choice"], "retry")
            self.assertEqual(long_task.stuck_choice("stop asking, just use last month's rate")["choice"], "unclear")

    def test_a_reply_that_adds_to_its_choice_keeps_what_it_adds_and_is_read_once(self):
        with Patched(reads_as("retry", "use the second file")) as (_, chat):
            first = long_task.stuck_choice("retry but use the second file")
            again = long_task.stuck_choice("retry but use the second file")
        self.assertEqual(first, {"choice": "retry", "instruction": "use the second file"})
        self.assertEqual(again, first)
        self.assertEqual(chat.call_count, 1)

    def test_a_choice_the_question_did_not_offer_is_unclear(self):
        with Patched(reads_as("delete everything")):
            self.assertEqual(long_task.stuck_choice("do whatever")["choice"], "unclear")


class AStuckStage(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_words_that_only_start_like_a_stop_or_a_skip_go_to_the_stage(self):
        for reply in ("stop asking, just use last month's rate", "no need to recheck, the figure is right",
                      "stop after the first return, I'll do the second"):
            with Patched(reads_as("answer", reply)):
                self.assertEqual(long_task.stuck_choice(reply)["choice"], "answer", reply)


class AHeldRequest(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_the_options_in_the_questions_own_words_need_no_model(self):
        with no_model():
            self.assertEqual(long_task._choice("Finish that one first, then do the new one."), "after")
            self.assertEqual(long_task._choice("Drop that one and do this one instead."), "replace")
            self.assertEqual(long_task._choice("It was my answer to your question."), "answer")
            self.assertEqual(long_task._choice("Neither - that is a separate job."), "")

    def test_a_keyword_anywhere_is_no_longer_the_choice(self):
        reply = "don't cancel the first one, do this after"
        with Patched(reads_as("after")):
            self.assertEqual(long_task._choice(reply), "after")
        approval._CHOSEN.clear()
        reply = "it's a reply to your question - use 12% instead"
        with Patched(reads_as("answer", "use 12%")):
            self.assertEqual(long_task._choice(reply), "answer")


class CarryingOnAfterARestart(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_a_carry_on_that_adds_an_instruction_keeps_it(self):
        with no_model():
            self.assertEqual(long_task.resume_choice("ok, carry on")["choice"], "carry_on")
        with Patched(reads_as("carry_on", "stop after the first stage")):
            said = long_task.resume_choice("ok, but stop after the first stage")
        self.assertEqual(said, {"choice": "carry_on", "instruction": "stop after the first stage"})


class LettingGoIsAnAnswerToAnOpenQuestion(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_a_plain_stop_always_calls_it_off(self):
        with no_model():
            for reply in ("stop", "Cancel.", "abort it", "quit"):
                self.assertTrue(guidance.calls_off(reply, "Mark row 7 as B2C, or leave it?"), reply)

    def test_leave_it_to_an_either_or_question_is_its_answer(self):
        question = "Row 7 has no GSTIN - mark it B2C, or leave it?"
        with Patched(reads_as("answer")):
            self.assertFalse(guidance.calls_off("leave it", question))
        with no_model():
            self.assertFalse(guidance.calls_off("leave it", question))
            # With nothing asked, the same words call the task off, as before.
            self.assertTrue(guidance.calls_off("never mind"))
        approval._CHOSEN.clear()
        with Patched(reads_as("stop")):
            self.assertTrue(guidance.calls_off("forget it", "Which account should this go to?"))


class AStepDoneByHandInAReplay(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()
        self.asked, self.finals, self.runs = [], [], []
        self.patches = [
            mock.patch.object(replay_task, "ask_for_query", side_effect=lambda t, **k: self.asked.append(t)),
            mock.patch.object(replay_task, "write_final_reply", side_effect=lambda t, **k: self.finals.append(t)),
            mock.patch.object(replay_task, "_run", side_effect=lambda *a: self.runs.append(a)),
        ]
        for p in self.patches:
            p.start()
        self.addCleanup(lambda: [p.stop() for p in self.patches])
        self.addCleanup(setattr, state, "paused_replay", None)

    def pause(self):
        state.paused_replay = {"program": {"session": "s"}, "params": {}, "outcome": replay_task.replay.NEEDS_HUMAN,
                               "step": 3, "resume_from": 4, "text": "Sign in on the screen, then tell me."}

    def test_a_done_in_other_words_carries_on(self):
        for reply in ("no problem, done", "don't worry, logged in, carry on"):
            approval._CHOSEN.clear()
            self.pause()
            with Patched(reads_as("done")):
                replay_task.resume(reply)
            self.assertEqual(len(self.runs), 1, reply)
            self.runs.clear()
        self.assertEqual(self.finals, [])

    def test_not_yet_waits_on_the_same_step_and_asks_again(self):
        self.pause()
        with Patched(reads_as("not_done")):
            replay_task.resume("not yet, the code hasn't come")
        self.assertEqual(self.runs, [])
        self.assertIsNotNone(state.paused_replay)
        self.assertIn("carry on once that step is done", self.asked[-1])

    def test_plain_words_need_no_model(self):
        with no_model():
            self.pause()
            replay_task.resume("done")
            self.assertEqual(len(self.runs), 1)
            self.pause()
            replay_task.resume("stop")
        self.assertIn("the replay stopped before step 3", self.finals[-1])


class ATeachingAnswerThatSaysMore(unittest.TestCase):
    QUESTION = {"id": "q1", "kind": "variable", "text": "Is the customer the same every time, or does it change?"}

    def setUp(self):
        approval._CHOSEN.clear()

    def test_its_choice_is_taken_and_what_it_adds_is_kept_as_a_rule(self):
        skill = {"understanding": {"rules": []}}
        with Patched(reads_as("same", "for one customer it's 5%")):
            choice, added = learning._read_choice("variable", self.QUESTION, "same, except for one customer it's 5%")
        self.assertEqual((choice, added), ("same", "for one customer it's 5%"))
        learning._keep_added(skill, self.QUESTION, added)
        self.assertTrue(skill["understanding"]["rules"][0].startswith("for one customer it's 5%"))

    def test_short_replies_are_read_as_before_and_an_exception_is_not_guessed_without_a_model(self):
        with no_model():
            self.assertEqual(learning._read_choice("variable", self.QUESTION, "same"), ("same", ""))
            self.assertEqual(learning._read_choice("variable", self.QUESTION, "it changes every time"), ("changes", ""))
            self.assertEqual(learning._read_choice("variable", self.QUESTION, "same, except for one customer"),
                             (None, ""))


if __name__ == "__main__":
    unittest.main()
