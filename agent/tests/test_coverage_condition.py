"""
CW (round 5 P6 on pc3, 2026-10-21): the blocking coverage check refused "List refunds over Rs 5,000 for approval" twice
- "its log has 3 of 12: no row for Cafe Bloom, Lotus Yoga Centre ..." - taking the tracker's 12 customers for "every
record". A stage that selects by a condition is checked only against the records meeting it, where code can decide
the condition; otherwise nothing is decided. A file-making stage is told to add the missing rows.
"""

import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths

from decurix_agent import long_task


class Runner:
    def __init__(self, work):
        self.work = work

    def context(self, state):
        return {"work_dir": str(self.work)}


class ConditionalCoverage(unittest.TestCase):
    def setUp(self):
        live = fixture_paths.need("p6_refund_list")
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.work = Path(tmp.name)
        for name in ("support_triage_tracker.xlsx", "refunds_over_5000_for_approval.xlsx"):
            shutil.copy(live / name, self.work / name)
        stage = json.loads((live / "stage2.json").read_text(encoding="utf-8"))
        self.step = {"id": "s2", "title": stage["title"], "instruction": stage["instruction"], "depends_on": ["s1"],
                     "checks": [{"path": "refunds_over_5000_for_approval.xlsx", "format": "xlsx"}]}
        self.state = {"steps": {"s1": {"evidence": [{"path": "support_triage_tracker.xlsx"}]}}}
        patch = mock.patch.object(long_task, "_runner", lambda: Runner(self.work))
        patch.start()
        self.addCleanup(patch.stop)

    def check(self):
        return long_task.coverage_problems(self.state, self.step, [str(self.work / "refunds_over_5000_for_approval.xlsx")])

    def test_the_live_list_of_three_passes(self):
        self.assertTrue(long_task.covers_every(self.step))
        self.assertEqual(self.check(), [])

    def test_a_list_missing_one_over_the_threshold_is_refused(self):
        path = self.work / "refunds_over_5000_for_approval.xlsx"
        book = openpyxl.load_workbook(path)
        sheet = book.worksheets[0]
        for row in range(sheet.max_row, 1, -1):
            if any("SO-5106" in str(c.value) for c in sheet[row]):
                sheet.delete_rows(row)
        book.save(path)
        found = self.check()
        self.assertEqual(len(found), 1, found)
        self.assertIn("every record its condition takes", found[0])
        self.assertIn("Add the missing rows to the file", found[0])
        self.assertNotIn("check the application", found[0])

    def test_a_condition_code_cannot_read_decides_nothing(self):
        self.step["instruction"] = "Draft a reminder to every customer whose papers are complete, and list them."
        self.assertEqual(long_task._condition_test(self.step["instruction"]), "unknown")
        self.assertEqual(self.check(), [])

    def test_the_thresholds(self):
        test = long_task._condition_test("From every email in the tracker that is a Refund over Rs 5,000, list them.")
        self.assertTrue(test(["Refund", "List refund for approval: Rs 35,960.00 (policy, over Rs 5,000)"]))
        self.assertFalse(test(["Refund", "Approve refund Rs 1,490.00 (policy, up to Rs 5,000)"]))
        self.assertFalse(test(["Complaint", "Order value Rs 62,910.00"]))  # not a Refund
        self.assertIsNone(long_task._condition_test("Enter every approved-in-the-file order in the portal.".replace(
            "approved-in-the-file ", "")))


if __name__ == "__main__":
    unittest.main()
