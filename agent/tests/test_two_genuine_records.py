"""
Two genuine identical records the request asked for are two records, not a duplicate (E-09, review 2026-11-03). The
duplicate check refused them and told the person to delete one. It is a duplicate only when the system's list shows
more numbers for a party than the stage's log entered for it, and then the person is asked, never told to delete.
"""

import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, os.path.dirname(__file__))

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task

REQUEST = "Enter both standing orders for Northgate Cafe (identical) in our order portal (http://portal.test:9000)."
STEP = {"id": "s2", "enters": "the order portal", "instruction": "Enter both orders in the order portal"}
ACTION = {"kind": "type", "outcome": "done", "at": "x", "expect": {"url": "http://portal.test:9000/new"},
          "change": "portal.test:9000"}
READ = ("Orders list\nSO-7101 Northgate Cafe 2026-10-20 Confirmed 1,200.00\n"
        "SO-7102 Northgate Cafe 2026-10-20 Confirmed 1,200.00")


class TwoGenuineRecords(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.log = Path(tmp.name) / "entry_log.csv"

    def found(self, log_text):
        self.log.write_text(log_text, encoding="utf-8")
        return long_task.system_entry_findings("r", STEP, [str(self.log)], REQUEST, {"t": 0},
                                               record=([ACTION], READ, READ))

    def test_two_logged_orders_shown_once_each_are_not_a_duplicate(self):
        found = self.found("Customer,Order ref,Status\nNorthgate Cafe,PO-81,Entered as SO-7101\n"
                           "Northgate Cafe,PO-82,Entered as SO-7102\n")
        self.assertFalse([t for c, t in found if c == "system_entry"], found)

    def test_one_logged_order_shown_twice_is_asked_about_never_deleted(self):
        found = self.found("Customer,Order ref,Status\nNorthgate Cafe,PO-81,Entered as SO-7101\n")
        blocking = [t for c, t in found if c == "system_entry"]
        self.assertEqual(len(blocking), 1, found)
        self.assertIn("ask the person", blocking[0])
        self.assertIn("never delete a record yourself", blocking[0])
        self.assertNotIn("must delete", blocking[0])


if __name__ == "__main__":
    unittest.main()
