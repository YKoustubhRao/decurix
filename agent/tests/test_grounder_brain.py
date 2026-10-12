"""
With no grounding model set up, "find it by looking" is answered by the
worker's own hosted model when that model reads pictures.
"""

import os
import unittest
from unittest import mock

from decurix_agent import llm_client
from decurix_agent.computer import grounder
from decurix_agent.computer.errors import ComputerError
from decurix_agent.computer.screen import Frame


def frame(width=400, height=300):
    return Frame(width, height, bytes(width * height * 3))


class LookingWithTheBrain(unittest.TestCase):
    def setUp(self):
        self.asked = []
        self.reply = '{"found": true, "x": 120, "y": 80, "what": "the bell icon"}'

        def chat(messages, **kwargs):
            self.asked.append(messages)
            return {"content": self.reply}, {}

        for patch in (mock.patch.object(grounder, "_specialist", lambda: False),
                      mock.patch.object(llm_client, "available", lambda: True),
                      mock.patch.object(llm_client, "chat", chat),
                      mock.patch.dict(os.environ, {"DECURIX_BRAIN_VISION": "1"})):
            patch.start()
            self.addCleanup(patch.stop)

    def test_it_is_offered_and_answers_with_a_point_on_the_screen(self):
        self.assertTrue(grounder.available())
        self.assertEqual(grounder.locate(frame(), "the bell icon at the top right"), (120, 80))
        sent = self.asked[0][1]["content"]
        self.assertIn("the bell icon at the top right", sent[0]["text"])
        self.assertEqual(sent[1]["type"], "image_url", "the screen goes with the question, ruler drawn on it")

    def test_not_on_the_screen_is_said_not_guessed(self):
        self.reply = '{"found": false, "why": "There is no bell icon on the screen."}'
        with self.assertRaises(ComputerError) as refused:
            grounder.locate(frame(), "the bell icon")
        self.assertIn("no bell icon", str(refused.exception))

    def test_a_point_off_the_screen_or_no_point_is_refused(self):
        for reply in ('{"found": true, "x": 4000, "y": 80}', '{"found": true}', "it is near the top"):
            self.reply = reply
            with self.assertRaises(ComputerError):
                grounder.locate(frame(), "the bell icon")

    def test_a_brain_that_reads_no_pictures_is_not_asked(self):
        with mock.patch.dict(os.environ, {"DECURIX_BRAIN_VISION": ""}):
            self.assertFalse(grounder.available())
            with self.assertRaises(ComputerError) as refused:
                grounder.locate(frame(), "the bell icon")
        self.assertIn("no grounding model", str(refused.exception))
        self.assertEqual(self.asked, [])

    def test_no_hosted_model_no_looking(self):
        with mock.patch.object(llm_client, "available", lambda: False):
            self.assertFalse(grounder.available())


if __name__ == "__main__":
    unittest.main()
