"""
The save gates of computer/core.py decide from structure and plain verbs, not from words fitted to one workflow
(review 2026-11-03, E-28 / D-29).
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import core


class TheSaveGatesUsePlainWords(unittest.TestCase):
    def test_a_new_record_page_is_told_by_its_address(self):
        self.assertTrue(core._NEW_RECORD_PAGE.search("page is now https://app.example/thing/new"))
        self.assertIsNone(core._NEW_RECORD_PAGE.search('1 element(s) appeared: heading "New customer entry"'))

    def test_a_save_is_a_plain_verb_of_keeping(self):
        self.assertTrue(core._SAVE_CONTROL.search("Save"))
        self.assertIsNone(core._SAVE_CONTROL.search("Onboard"))

    def test_a_job_title_word_is_a_label_word_like_any_other(self):
        self.assertIn("recruiter", core._words_of("Recruiter"))


if __name__ == "__main__":
    unittest.main()
