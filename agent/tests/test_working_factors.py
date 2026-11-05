"""
Finding CH (2026-10-20, round 5 P3, a regression from CF): the working check took a result for the first quantity x the
first rate and refused a correct AP register twice - "194,880.00, but 120 x 1,450.00 is 174,000.00" (12% GST on top)
and "35,280.00, but 100 x 420.00" (84 received x 420). It now uses every factor the working records and refuses only
what no combination of them gives; a working it cannot read is not decided.
"""

import shutil
import tempfile
import unittest
from pathlib import Path

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import stage_facts

LIVE = fixture_paths.fixture("p3_AP_register_live.xlsx")  # the run's own AP_register.xlsx


class TheLiveRegister(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.path = Path(tmp.name) / "AP_register.xlsx"
        shutil.copy(fixture_paths.need(LIVE.name), self.path)

    def test_every_right_line_passes(self):
        self.assertEqual(len(stage_facts.working_rows([str(self.path)])), 15)
        self.assertEqual(stage_facts.working_problems([str(self.path)]), [])

    def test_a_wrong_amount_is_refused(self):
        book = openpyxl.load_workbook(self.path)
        sheet = book["Working"]
        header = [c.value for c in sheet[1]]
        col = header.index("Payable amount") + 1
        sheet.cell(row=2, column=col, value=184880)  # 120 x 1,450 x 1.12 is 194,880
        book.save(self.path)
        found = stage_facts.working_problems([str(self.path)])
        self.assertEqual(len(found), 1, found)
        self.assertIn("APX-5521", found[0])
        self.assertIn("Payable amount 184,880.00, which no combination of its own factors", found[0])


class TheLiveExceptions(unittest.TestCase):
    """CQ (round 5 P3, 2026-10-20): the AP exceptions' Working sheet, its derived cells as formulas, refused twice
    ("44,100.00, which no combination of its own factors gives"; "the price for APX-5521 ... APX-5521.pdf:line 1").
    Must pass - and the P6 1,800 for 60 @ 35 must still be caught (Combinations)."""

    def test_the_formula_working_passes(self):
        path = fixture_paths.need("p3_AP_exceptions_live.xlsx")
        self.assertEqual(stage_facts.working_problems([str(path)]), [])


class Combinations(unittest.TestCase):
    def table(self, header, rows):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / "work.xlsx"
        book = openpyxl.Workbook()
        book.active.title = "Working"
        book.active.append(header)
        for row in rows:
            book.active.append(row)
        book.save(path)
        return [str(path)]

    def test_received_quantity_with_tax(self):
        made = self.table(["Bill", "Bill qty", "Qty received", "Rate", "GST %", "Amount"],
                          [["GPS/2026/0412", 100, 84, 420, 5, 37044.0]])  # 84 x 420 x 1.05
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_every_quantity_is_tried(self):
        # CQ (round 5 P3, 2026-10-20): a bill amount uses the billed quantity, the payable amount the payable one.
        made = self.table(["Bill", "Bill qty", "Payable qty", "Rate", "Amount"], [["SPH-0219", 500, 450, 180, 90000]])
        self.assertEqual(stage_facts.working_problems(made), [])
        made = self.table(["Bill", "Bill qty", "Payable qty", "Rate", "Amount"], [["SPH-0219", 500, 450, 180, 85000]])
        self.assertEqual(len(stage_facts.working_problems(made)), 1)

    def test_a_formula_result_is_its_own_arithmetic(self):
        # The live AP_exceptions.xlsx shape: derived cells as formulas, with no value kept (written by openpyxl).
        made = self.table(["Bill", "Bill qty", "Payable qty", "Bill rate", "GST %", "Taxable billed formula",
                           "Bill amount formula", "Dispute amount formula"],
                          [["GPS/2026/0412", 100, 80, 420, 5, "=ROUND(B2*D2,2)", "=ROUND(F2*(1+E2/100),2)",
                            "=ROUND(G2-B2*0,2)"]])
        self.assertEqual(stage_facts.working_problems(made), [])
        self.assertEqual(stage_facts.evaluate("=ROUND(F2*(1+E2/100),2)", 2, [None, 100, 80, 420, 5, 42000]), 44100.0)
        self.assertIsNone(stage_facts.evaluate("=VLOOKUP(A2,X!A:B,2)", 2, ["k"]))
        self.assertIsNone(stage_facts.evaluate("=B3*2", 2, [None, 5]))  # another row's cell: not decided

    def test_a_discount(self):
        made = self.table(["Order", "Qty", "Unit price", "Discount %", "Amount"], [["SO-5101", 60, 185, 5, 10545]])
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_a_column_of_no_known_kind_is_not_decided(self):
        made = self.table(["Order", "Qty", "Unit price", "Freight", "Amount"], [["SO-5101", 60, 185, 400, 11500]])
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_the_p6_lanyards_are_still_refused(self):
        made = self.table(["Order No", "Item", "Qty", "Unit price", "Result"], [["SO-5103", "LAN-S", 60, 35, 1800]])
        found = stage_facts.working_problems(made)
        self.assertEqual(len(found), 1)
        self.assertIn("60 x 35.00 is 2,100.00", found[0])


if __name__ == "__main__":
    unittest.main()
