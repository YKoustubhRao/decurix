"""
A record a task saved that nothing read back against a source is named in the report and never under a checked done
(records.saved_unread, final_gate). Found live (2026-11-09): a worker that declared nothing created a party's ledger with
its state left unset, and the run ended done on the model's word.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import records

SAVED = "\"Ledger saved.\" appeared; page is now http://books.example.test/company/1/ledgers/31"


def events(*changes):
    out = []
    for change in changes:
        out += [{"kind": "open", "outcome": "done", "target": "http://books.example.test/company/1/ledgers/new",
                 "change": "page is now http://books.example.test/company/1/ledgers/new"},
                {"kind": "click", "outcome": "done", "change": change}]
    return out


def ledger(readback):
    return [{"key": "Hampi Residency", "saved_at": 1.0, "saved": " ".join(SAVED.split()), "readback": readback}]


class SavedUnread(unittest.TestCase):
    def run_with(self, ledger_records, *changes):
        with mock.patch.object(records, "load", return_value=ledger_records), \
                mock.patch("decurix_agent.computer.core._saved", lambda change: "saved" in change.lower()):
            return records.saved_unread("t1", events=events(*changes))

    def test_a_save_no_declaration_covers_is_unread(self):
        self.assertEqual(self.run_with([], SAVED), ["http://books.example.test/company/1/ledgers/31"])

    def test_a_declared_record_read_back_whole_is_read(self):
        self.assertEqual(self.run_with(ledger({"seen": ["Name"], "absent": []}), SAVED), [])

    def test_a_record_read_back_with_a_value_missing_is_unread(self):
        self.assertEqual(len(self.run_with(ledger({"seen": ["Name"], "absent": ["State"]}), SAVED)), 1)

    def test_nothing_saved_is_nothing_unread(self):
        self.assertEqual(self.run_with([]), [])


if __name__ == "__main__":
    unittest.main()
