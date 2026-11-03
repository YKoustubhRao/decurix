"""
DA (round 5 P2, 2026-10-21, series 12): the blocking entry check refused the portal stage twice - "do not show:
WA-1810, WA-1810, WA-1810, OS/PUR/2026/118" - though the portal held all five orders (SO-5101..SO-5105). The only page
read after the last entry was that order's "Saved" page; each other order's own "Saved ... SO-510x" page had been read
when it was saved. Now a record counts where the system showed it during the stage (by its own number when the log
gives it, else its name and reference on one line - a PO three orders share identifies none), and when the list was
not read after the last entry, the check asks for that read instead of calling records missing.
"""

import json
import tempfile
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths

from decurix_agent import long_task

REQUEST = "Enter every approved order in the Northwind Back Office portal (http://host.docker.internal:8701, user clerk)."
STEP = {"id": "s5", "title": "Enter every approved order in the portal and save entry log", "instruction": "x"}


class TheLiveReadBack(unittest.TestCase):
    def setUp(self):
        self.live = fixture_paths.need("p2_portal_series12")
        kept = json.loads((self.live / "record.json").read_text(encoding="utf-8"))
        self.record = (kept["actions"], kept["read"], kept["stage_read"])

    def check(self, record):
        return long_task.system_entry_problems("r", STEP, [str(self.live / "portal_entry_log.csv")], REQUEST, {"t": 0},
                                               record=record)

    def test_the_five_orders_the_portal_holds_pass(self):
        self.assertEqual(self.check(self.record), [])

    def test_one_the_system_never_showed_asks_for_the_list(self):
        actions, read, stage_read = self.record
        found = self.check((actions, read, stage_read.replace("SO-5104", "SO-XXXX")))
        self.assertEqual(len(found), 1, found)
        self.assertIn("list of records was not read after the last entry", found[0])
        self.assertIn("OS/PUR/2026/118", found[0])


class Matching(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.log = Path(tmp.name) / "portal_entry_log.csv"
        # No system numbers in this log: each record by its name and its reference together.
        self.log.write_text("customer,source_order,portal status\nCafe Bloom,WA-1810,Entered\n"
                            "Lotus Yoga Centre,WA-1810,Entered\nOakwood School,OS/PUR/2026/118,Entered\n",
                            encoding="utf-8")

    def check(self, listing):
        action = {"kind": "click", "outcome": "done", "expect": {"active_window": "Orders - Chromium"}}
        return long_task.system_entry_problems("r", STEP, [str(self.log)], REQUEST, {"t": 0},
                                               record=([action], listing, listing))

    def test_a_shared_reference_never_identifies_a_record_alone(self):
        listing = ("SO-5101 | Cafe Bloom | WA-1810 | 18/10/2026\nSO-5102 | Oakwood School | OS/PUR/2026/118\n"
                   "SO-5103 | Maple Leaf Bakery | WA-1810")
        found = self.check(listing)
        self.assertEqual(len(found), 1, found)
        self.assertIn("Lotus Yoga Centre", found[0] + " ".join(c[0] for c in long_task._claimed_records([str(self.log)])))
        self.assertNotIn("Cafe Bloom", found[0])
        self.assertNotIn("OS/PUR", found[0])

    def test_all_shown_by_name_and_reference_pass(self):
        listing = ("SO-5101 | Cafe Bloom | WA-1810\nSO-5102 | Lotus Yoga Centre | WA-1810\n"
                   "SO-5104 | Oakwood School | OS/PUR/2026/118")
        self.assertEqual(self.check(listing), [])


if __name__ == "__main__":
    unittest.main()
