import tempfile
import unittest
from pathlib import Path
from unittest import mock

import openpyxl

from decurix_agent import produced, source_guard
from decurix_agent.tools import office


# ============================================================
# GST suite T10 (2026-10-22): openpyxl's insert_rows put a September block
# above August's; August's formulas moved 17 rows down word for word and went
# on reading the rows September had taken. Nothing noticed.
# ============================================================

def month_block(ws, top, title, opening_ref):
    ws.cell(top, 1, title)
    ws.cell(top + 1, 1, "Output tax")
    ws.cell(top + 1, 2, 1000)
    ws.cell(top + 2, 1, "Opening credit")
    ws.cell(top + 2, 2, opening_ref)
    ws.cell(top + 3, 1, "Credit available")
    ws.cell(top + 3, 2, f"=B{top + 1}+B{top + 2}")
    ws.cell(top + 4, 1, "Closing credit")
    ws.cell(top + 4, 2, f"=ROUND(B{top + 3}-B{top + 1},2)")


class TestFormulasMovedWithoutTheirReferences(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "COMPUTATION"
        ws["B1"] = 5000                     # an earlier month's closing
        month_block(ws, 3, "AUGUST", "=B1")  # rows 3-7, its closing at B7
        self.before = self.dir / "before.xlsx"
        wb.save(self.before)

    def after(self, build):
        wb = openpyxl.load_workbook(self.before)
        build(wb["COMPUTATION"])
        path = self.dir / "after.xlsx"
        wb.save(path)
        return path

    def test_rows_put_in_above_a_block_without_moving_its_references_are_found(self):
        def insert_above(ws):
            ws.insert_rows(3, amount=6)                   # moves cells, not the references to them
            month_block(ws, 3, "SEPTEMBER", "=B1")        # a new block where August was
        found = office.moved_formula_breaks(self.before, self.after(insert_above))
        self.assertEqual(len(found), 1)
        self.assertIn("COMPUTATION: 2 formula(s) are 6 rows lower", found[0])
        self.assertIn("B12 =B4+B5 was at B6", found[0])

    def test_a_block_added_below_with_its_own_references_is_clean(self):
        def append_below(ws):
            month_block(ws, 10, "SEPTEMBER", "=B7")
        self.assertEqual(office.moved_formula_breaks(self.before, self.after(append_below)), [])

    def test_a_real_insert_that_moves_the_references_is_clean(self):
        def insert_properly(ws):
            ws.insert_rows(3, amount=6)
            ws["B11"], ws["B12"], ws["B13"] = "=B1", "=B10+B11", "=ROUND(B12-B10,2)"  # moved with their rows
            month_block(ws, 3, "SEPTEMBER", "=B1")
        self.assertEqual(office.moved_formula_breaks(self.before, self.after(insert_properly)), [])

    def test_a_formula_reading_only_rows_above_the_change_keeps_its_words(self):
        def insert_above(ws):
            ws.insert_rows(3, amount=6)
            ws["B12"], ws["B13"] = "=B10+B11", "=ROUND(B12-B10,2)"  # fixed; B11 "=B1" rightly unchanged
        self.assertEqual(office.moved_formula_breaks(self.before, self.after(insert_above)), [])

    def test_an_untouched_formula_reading_rows_another_sheet_moved_is_found(self):
        # GST suite T10, second run: 12 rows put into SALES at its total row left COMPUTATION's "=SALES!B4" behind.
        wb = openpyxl.load_workbook(self.before)
        sales = wb.create_sheet("SALES")
        sales["B2"], sales["B3"], sales["B4"] = 600, 400, "=SUM(B2:B3)"
        wb["COMPUTATION"]["C4"] = "=SALES!B4"
        wb.save(self.before)

        def insert_into_sales(wb):
            wb["SALES"].insert_rows(4, amount=3)
        after = self.dir / "after.xlsx"
        changed = openpyxl.load_workbook(self.before)
        insert_into_sales(changed)
        changed.save(after)
        found = office.moved_formula_breaks(self.before, after)
        self.assertEqual(len(found), 1, found)
        self.assertIn("COMPUTATION: 1 formula(s) this task did not change read rows of SALES from row 4 down", found[0])
        self.assertIn("C4 =SALES!B4", found[0])

    def test_references_are_read_with_their_sheets_and_range_ends(self):
        self.assertEqual(office._row_refs("=SUM(SALES!I47:I53)+C72-'Old, sheet'!$B$2&\"A1\""),
                         [("SALES", 47, False), ("SALES", 53, False), (None, 72, False), ("Old, sheet", 2, True)])

    def test_the_gate_compares_with_the_copy_kept_before_the_task(self):
        def insert_above(ws):
            ws.insert_rows(3, amount=6)
        after = self.after(insert_above)
        with mock.patch.object(source_guard, "before_copy", return_value=self.before):
            found = produced.moved_breaks([str(after)])
        self.assertEqual(len(found), 1)
        self.assertTrue(found[0].startswith("after.xlsx: COMPUTATION:"))
        with mock.patch.object(source_guard, "before_copy", return_value=None):
            self.assertEqual(produced.moved_breaks([str(after)]), [])


if __name__ == "__main__":
    unittest.main()
