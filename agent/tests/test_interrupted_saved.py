"""
A task the desktop restarted under, after it had saved records, is reported with what it saved - never "send the
request again to start it over", which would enter them twice (watchers._saved_before_restart). Found live
(2026-11-08): two bills saved correctly before a restart were reported "not done - start it over".

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_interrupted_saved.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import watchers
from decurix_agent.computer import records


class ARestartAfterASave(unittest.TestCase):

    def test_what_was_saved_is_said_and_no_resend_is_advised(self):
        saved = [{"key": "NXG-2026-8931", "saved_at": 1.0, "saved": "PINV-26-00010",
                  "fields": {"Supplier": {"intent": "NexGen"}, "Supplier Invoice No": {"intent": "NXG-2026-8931"}},
                  "readback": {"seen": ["Supplier", "Supplier Invoice No"]}}]
        with mock.patch.object(records, "load", return_value=saved):
            note = watchers._saved_before_restart("t1234567890abcdef")
        self.assertIn("NXG-2026-8931 saved in PINV-26-00010 (read back with every declared value shown)", note)
        self.assertIn("do not send the same request again", note)

    def test_nothing_saved_keeps_the_plain_note(self):
        with mock.patch.object(records, "load", return_value=[{"key": "A", "saved_at": None}]):
            self.assertEqual(watchers._saved_before_restart("t1234567890abcdef"), "")


if __name__ == "__main__":
    unittest.main()
