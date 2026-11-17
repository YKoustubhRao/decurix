"""
What the person taught reaches soul as their words (thebot_harness._taught_said): found in the learning curves
(2026-11-16), the filler guard refused the taught "NA" every month and the verifier, shown no rule, called a correct
entry wrong.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_taught_said.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import thebot_harness


def skill():
    return {"name": "file-it", "title": "File it", "steps": [{"n": 1, "kind": "click"}],
            "notes": [{"text": "The HSN summary goes in its B2C tab (services: UQC NA, quantity 0)."}],
            "understanding": {"dialogue": [{"q": "Is that right?", "a": "A buyer in another state above one lakh goes to Table 5."}],
                              "rules": ["[fields] UQC NA for services", "[approvals] File with EVC needs my OK"]},
            "corrections": [{"at": 1, "text": "Read the bills from the folder the request names."}]}


class TheTaughtWordsGoWithTheJob(unittest.TestCase):

    def test_notes_answers_corrections_and_rules_follow_this_task_s_own_replies(self):
        run = {"skill": skill()}
        with mock.patch.object(thebot_harness.guidance, "get", return_value={"run": run}), \
                mock.patch("decurix_agent.person_record.answers", return_value=[{"asked": "OTP?", "text": "604217"}]):
            said = thebot_harness._earlier_said()
        self.assertEqual(said[0]["said"], "604217")  # (this task's own replies first: soul keeps the first twenty)
        joined = " ".join(s["said"] for s in said)
        for words in ("UQC NA, quantity 0", "Table 5", "from the folder", "File with EVC needs my OK"):
            self.assertIn(words, joined)
        self.assertLessEqual(len(said), thebot_harness.MAX_EARLIER_SAID)

    def test_a_job_no_taught_task_guides_sends_only_its_replies(self):
        with mock.patch.object(thebot_harness.guidance, "get", return_value=None), \
                mock.patch("decurix_agent.person_record.answers", return_value=[]):
            self.assertEqual(thebot_harness._earlier_said(), [])


if __name__ == "__main__":
    unittest.main()
