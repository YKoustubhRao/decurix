"""
A long code in the final check's words (a reference, an acknowledgement number) is reported as the screen's own text
writes it (skill_task._as_on_screen). Found in the learning curves (2026-11-17): a filing's ARN ending in the letter I
was read off the picture as a 1, and the report named a reference the portal never gave.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent import skill_task  # noqa: E402


class ACodeAsTheScreenWritesIt(unittest.TestCase):
    def test_a_code_a_character_off_takes_the_screens_characters(self):
        self.assertEqual(skill_task._as_on_screen("Filed, ARN AB1234567890I1X", "ARN AB1234567890IIX shown"),
                         "Filed, ARN AB1234567890IIX")

    def test_a_code_the_screen_holds_twice_alike_or_not_at_all_stays(self):
        self.assertEqual(skill_task._as_on_screen("ARN AB12345678", "AB12345679 AB12345670"), "ARN AB12345678")
        self.assertEqual(skill_task._as_on_screen("ref 12345678", ""), "ref 12345678")
        self.assertEqual(skill_task._as_on_screen("GSTIN 29ABCDE1234F1Z5", "GSTIN 29ABCDE1234F1Z5"),
                         "GSTIN 29ABCDE1234F1Z5")


if __name__ == "__main__":
    unittest.main()
