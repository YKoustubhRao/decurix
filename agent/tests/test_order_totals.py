"""
Finding CK (2026-10-20, round 5 P2 on pc3): stage 1 was refused for "Decision reason says 50,123.60, its working gives
17,980.00" - the reason states the ORDER's total incl GST (for the credit check), the working rows are its LINES, and
they were tied by references several lines share (the PO, a chat's id, a product code). A stated amount is compared
with the one working row its full key ties it to, or holds as a sum of one order's rows; otherwise nothing is decided.
"""

import shutil
import unittest

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo
from temp_case import TempDirCase

from decurix_agent import stage_facts

LIVE = fixture_paths.fixture("p2_order_working_live.xlsx")  # the run's order_working.xlsx (pc3)


class OrderTotals(TempDirCase):
    def test_the_live_sheet_has_no_finding(self):
        path = self.dir / "order_working.xlsx"
        shutil.copy(fixture_paths.need(LIVE.name), path)
        self.assertEqual(stage_facts.working_problems([str(path)]), [])

    def book(self, lines, reasons):
        path = self.dir / "w.xlsx"
        book = openpyxl.Workbook()
        book.active.title = "Order lines"
        book.active.append(["Customer", "Order", "SKU", "Decision reason"])
        for row in reasons:
            book.active.append(row)
        sheet = book.create_sheet("Working")
        sheet.append(["Customer", "Order", "SKU", "Quantity", "Unit price", "GST %", "Line ex GST", "Line amount incl GST"])
        for row in lines:
            sheet.append(row)
        book.save(path)
        return [str(path)]

    LINES = [["Harbour Hotels Group", "HH/PO/7781", "WB-500", 80, 175.75, 18, 14060, 16590.8],
             ["Harbour Hotels Group", "HH/PO/7781", "UMB-C", 40, 299, 12, 11960, 13395.2],
             ["Harbour Hotels Group", "HH/PO/7781", "HD-GR", 20, 899, 12, 17980, 20137.6]]

    def test_an_order_total_on_a_line_holds(self):
        reason = "Order incl GST Rs 50123.60 + outstanding Rs 190000.00 = Rs 240123.60, above credit limit"
        made = self.book(self.LINES, [["Harbour Hotels Group", "HH/PO/7781", sku, reason] for sku in ("WB-500", "UMB-C",
                                                                                                    "HD-GR")])
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_a_wrong_order_total_tied_by_shared_references_is_not_decided(self):
        reason = "Order incl GST Rs 51000.00 - above the credit limit"
        made = self.book(self.LINES, [["Harbour Hotels Group", "HH/PO/7781", "UMB-C", reason]])
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_a_line_tied_by_its_full_key_is_still_checked(self):
        lines = [["Lotus Yoga Centre", "SO-5103", "LAN-S", 60, 35, 0, 2100, 2100]]
        made = self.book(lines, [["Lotus Yoga Centre", "SO-5103", "LAN-S", "Refund Rs 1,800.00 approved"]])
        found = stage_facts.working_problems(made)
        self.assertEqual(len(found), 1, found)
        self.assertIn("says 1,800.00", found[0])


if __name__ == "__main__":
    unittest.main()
