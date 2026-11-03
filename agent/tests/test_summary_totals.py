"""
DC (round 5 P1 on pc3, 2026-10-21, series 12): the GSTR-3B sheet gave Eligible ITC CGST/SGST/IGST as 0 while the same
workbook's Purchase Register counted 38,534.40 / 38,534.40 / 14,339.00 over its eligible rows (BCP-7781 once, NS-0457
cancelled, SP-2291 kept out); the net payable followed the 0. An advisory fact: a summary figure in one sheet against
the column its label names in another, over the rows that sheet itself counts.
"""

import tempfile
import unittest
from pathlib import Path

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths

from decurix_agent import long_task, stage_facts


class TheLiveWorkbooks(unittest.TestCase):
    def test_the_series_12_itc_is_named(self):
        found = stage_facts.summary_vs_detail([str(fixture_paths.need("p1_gstr3b", "series12_wrong.xlsx"))])
        self.assertEqual(len(found), 3, found)
        self.assertIn("\"Eligible ITC CGST\" is 0.00", found[0])
        self.assertIn("is 38,534.40", found[0])
        self.assertIn("is 14,339.00", found[2])

    def test_the_right_run_has_none(self):
        self.assertEqual(stage_facts.summary_vs_detail([str(fixture_paths.need("p1_gstr3b", "p1b_right.xlsx"))]), [])


class Shapes(unittest.TestCase):
    def book(self, itc):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / "close.xlsx"
        wb = openpyxl.Workbook()
        reg = wb.active
        reg.title = "Purchase Register"
        for row in (["Bill", "CGST (Rs)", "Status"], ["NS-0452", 2880, "Included; ITC eligible"],
                    ["SP-2291", 5494.5, "Included as billed; keep ITC out until credit note"],
                    ["BCP-7781", 9162, "Included; ITC eligible"], ["BCP-7781", 9162, "Duplicate; excluded"],
                    ["NS-0457", 1000, "Cancelled; excluded"]):
            reg.append(row)
        gst = wb.create_sheet("GSTR-3B")
        for row in (["Section", "Head", "Amount"], ["Output tax", "CGST", 59729.23], ["Eligible ITC", "CGST", itc],
                    ["Blocked ITC kept out", "CGST", 5494.5]):
            gst.append(row)
        wb.save(path)
        return [str(path)]

    def test_a_wrong_summary_is_named_and_a_right_one_passes(self):
        found = stage_facts.summary_vs_detail(self.book(0))
        self.assertEqual(len(found), 1, found)
        self.assertIn("is 12,042.00", found[0])  # 2,880 + 9,162: the duplicate, the cancelled and the held left out
        self.assertEqual(stage_facts.summary_vs_detail(self.book(12042)), [])

    def test_it_is_advisory(self):
        self.assertFalse(long_task.is_blocking("summary_totals"))


if __name__ == "__main__":
    unittest.main()
