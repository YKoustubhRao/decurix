"""
A second record that shares only a name with one this task saved is not refused (review 2026-11-03, D-12): two bills
to one customer for one item are two records. The same email or the same typed reference still is; a name-only match
is evidence for the final check.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import core


def typed(text, label=""):
    return {"kind": "type", "outcome": "done", "text": text, "change": "", "element": {"name": label}}


NEW = {"kind": "click", "outcome": "done", "change": "page is now https://app.example/doc/new"}
SAVED = {"kind": "click", "outcome": "done", "element": {"name": "Save"},
         "change": "page is now https://app.example/doc/DOC-0001; Saved"}


def bill(*extra):
    return [NEW, typed("Client One Services"), typed("Consulting"), typed("1")] + list(extra)


class ASecondRecordByName(unittest.TestCase):
    def test_a_name_only_match_is_not_refused(self):
        self.assertIsNone(core.already_entered(bill() + [SAVED] + bill()))

    def test_a_name_only_match_is_evidence(self):
        self.assertEqual(core.possibly_entered(bill() + [SAVED] + bill()), ("Client One Services Consulting", "DOC-0001"))
        events = bill() + [SAVED] + bill()
        notes = []
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"), \
                mock.patch("decurix_agent.computer.evidence.note", side_effect=notes.append):
            self.assertEqual(core._duplicate_record({"name": "Save"}), "")
        self.assertTrue(notes and "Client One Services" in notes[0])

    def test_the_same_typed_reference_is_still_refused(self):
        ref = typed("R-7001", "Ref No.")
        self.assertIsNotNone(core.already_entered(bill(ref) + [SAVED] + bill(ref)))

    def test_the_same_email_is_still_refused(self):
        mail = typed("one@client.example")
        self.assertIsNotNone(core.already_entered(bill(mail) + [SAVED] + bill(mail)))


if __name__ == "__main__":
    unittest.main()
