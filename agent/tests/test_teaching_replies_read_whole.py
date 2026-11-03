"""
A teaching reply is decided in code only when it is, whole, one plain answer (review 2026-11-03):

- D-21: a reply to "save it?" that is not a plain yes or no never saves the skill by itself. A name is put back to
  the person; "not yet" or a correction does not make a ready skill.
- D-22: a short negated reply is not read as the answer it negates. With no model it is asked again.
"""

import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_skills as ts  # noqa: E402 - stubs config and gives a demonstration to learn from

from decurix_agent import llm_client  # noqa: E402
from decurix_agent.computer import learning, skills  # noqa: E402

NO_MODEL = mock.patch.object(llm_client, "available", return_value=False)


class NegatedRepliesAreNotTheirOpposite(unittest.TestCase):
    def test_a_negated_short_reply_names_no_choice(self):
        with NO_MODEL:
            self.assertIsNone(learning._read_choice("approval", {"text": "q"}, "don't go ahead")[0])
            self.assertIsNone(learning._read_choice("approval", {"text": "q"}, "do not proceed")[0])
            self.assertIsNone(learning._read_choice("variable", {"text": "q"}, "not the same")[0])

    def test_a_plain_reply_is_still_its_choice(self):
        with NO_MODEL:
            self.assertEqual(learning._read_choice("approval", {"text": "q"}, "go ahead")[0], "go")
            self.assertEqual(learning._read_choice("approval", {"text": "q"}, "don't ask")[0], "go")
            self.assertEqual(learning._read_choice("approval", {"text": "q"}, "Ask me first.")[0], "ask")
            self.assertEqual(learning._read_choice("variable", {"text": "q"}, "same")[0], "same")


class ANameIsConfirmedBeforeItSaves(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        p = mock.patch.object(skills, "ROOT", Path(self.tmp.name))
        p.start()
        self.addCleanup(p.stop)
        self.skill = learning.build([ts.demo("demo-20260914T101500-aaaaaa", "Acme")])
        self.confirm = next(q for q in learning.questions(self.skill) if q["kind"] == "confirm")

    def test_not_yet_and_a_correction_do_not_save_a_skill(self):
        with NO_MODEL:
            for reply in ("not yet", "wait, step 3 is wrong"):
                with self.subTest(reply=reply):
                    result = learning.answer(self.skill, self.confirm, reply)
                    self.assertNotEqual(result["decision"], "save")
                    self.assertNotEqual(self.skill.get("status"), skills.READY)

    def test_a_name_is_put_back_and_a_plain_yes_saves_it(self):
        with NO_MODEL:
            result = learning.answer(self.skill, self.confirm, "Add a client record")
            self.assertIsNone(result["decision"])
            self.assertIn('save it as "Add a client record"', result["note"])
            again = next(q for q in learning.questions(self.skill) if q["kind"] == "confirm")
            self.assertNotIn(again["id"], self.skill["answers"], "the question is still open")
            self.assertIn('save it as "Add a client record"', again["text"])
            self.assertEqual(learning.answer(self.skill, again, "yes")["decision"], "save")

    def test_a_plain_no_throws_it_away(self):
        with NO_MODEL:
            self.assertEqual(learning.answer(self.skill, self.confirm, "no")["decision"], "discard")

    def test_with_a_model_a_reply_that_is_not_a_name_is_asked_again(self):
        with mock.patch.object(llm_client, "available", return_value=True), \
                mock.patch("decurix_agent.computer.approval.read_choice",
                           return_value={"choice": "unclear", "instruction": ""}):
            result = learning.answer(self.skill, self.confirm, "not yet")
        self.assertFalse(result["understood"])
        self.assertIsNone(result["decision"])


if __name__ == "__main__":
    unittest.main()
