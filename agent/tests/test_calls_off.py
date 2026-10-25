"""A reply to "is it done?" that begins with "no" and says what to do is not a call-off (2026-10-24)."""
import json
import unittest
from unittest import mock

from decurix_agent import llm_client, skill_task


class CallsOff(unittest.TestCase):
    def test_a_plain_stop_ends_it_without_a_model(self):
        with mock.patch.object(llm_client, "chat", side_effect=AssertionError("asked")):
            for reply in ("no", "stop", "cancel", "no, leave it", "never mind", "Stop."):
                self.assertTrue(skill_task._calls_off(reply), reply)
            for reply in ("retry", "it is done", "fix row 3"):
                self.assertFalse(skill_task._calls_off(reply), reply)

    def test_a_no_that_says_what_to_do_is_read(self):
        reply = "no, not done - those 2 files are junk. delete both and download 197 and 198 again one at a time"
        answer = {"content": json.dumps({"calls_off": False})}
        with mock.patch.object(llm_client, "available", lambda: True), \
                mock.patch.object(llm_client, "chat", lambda *a, **k: (answer, {})):
            self.assertFalse(skill_task._calls_off(reply, "the files are identical"))

    def test_with_no_model_their_words_are_what_to_do_next(self):
        with mock.patch.object(llm_client, "available", lambda: False):
            self.assertFalse(skill_task._calls_off("no, download them again one by one"))


if __name__ == "__main__":
    unittest.main()
