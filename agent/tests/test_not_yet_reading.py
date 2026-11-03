"""
A reading of the person's reply decides whether a refusal is final; words never turn a plain no into "not yet"
(review 2026-11-03, E-28).
"""

import unittest
from unittest import mock

from decurix_agent import guidance
from decurix_agent.computer import approval


class TheReadingDecides(unittest.TestCase):
    def said(self, reading, text):
        with mock.patch.object(approval, "last_reading", lambda reply: reading):
            return guidance.not_yet(text)

    def test_a_plain_no_stays_a_no_whatever_words_follow(self):
        for text in ("No, not yet - I will do it myself first.", "No - stop. I will file it myself tomorrow."):
            with self.subTest(text=text):
                self.assertFalse(self.said({"decision": "no"}, text))

    def test_a_no_with_something_wanted_first_is_not_yet(self):
        self.assertTrue(self.said({"decision": "no", "wants": "the count checked"}, "No - check the count first."))
        self.assertTrue(self.said({"decision": "if", "condition": "after the count"}, "Only after the count."))

    def test_with_no_reading_the_words_still_say_not_yet(self):
        self.assertTrue(self.said({}, "No - do the check first, then ask again."))
        self.assertFalse(self.said({}, "No."))


if __name__ == "__main__":
    unittest.main()
