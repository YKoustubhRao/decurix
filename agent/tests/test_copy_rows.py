import tempfile
import unittest
from pathlib import Path

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl
from openpyxl.styles import PatternFill

from decurix_agent.tools import office


# ============================================================
# The next period's block of a register made from the last one (office update copy_rows), as a copy and paste.
# Found live (2026-10-23/24): a new month's block typed by hand came out with another month's row in it, and its
# labels and totals pointing at the wrong rows.
# ============================================================

def register(path):
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Calc"
    other = wb.create_sheet("Inputs")
    yellow = PatternFill("solid", fgColor="FFFF00")
    for top, month in ((1, "JULY"), (7, "AUGUST")):
        ws.cell(top, 1, f"{month} 2026").fill = yellow
        ws.cell(top + 1, 1, "Opening credit")
        ws.cell(top + 1, 2, 500 if top == 1 else f"=B{top - 2}")        # August opens with July's closing
        ws.cell(top + 2, 1, "Tax payable")
        ws.cell(top + 2, 2, 120)                                         # a figure typed each month
        ws.cell(top + 3, 1, "Purchases credit")
        ws.cell(top + 3, 2, f"=Inputs!B{top}")                           # reads another sheet
        ws.cell(top + 4, 1, "Closing credit")
        ws.cell(top + 4, 2, f"=B{top + 1}+B{top + 3}-B{top + 2}")
        ws.cell(top + 5, 1, f"Return for {month} 2026 filed")
        ws.cell(top + 5, 3, f"Filed on 11/0{top}/2026. Ref AA29082600{top}")    # a fact of that month only
    other["B1"], other["B7"] = 30, 40
    wb.save(path)


class TestCopyRows(unittest.TestCase):

    def setUp(self):
        self.path = Path(tempfile.mkdtemp()) / "register.xlsx"
        register(self.path)

    def test_the_last_block_becomes_the_next_with_formulas_moved_labels_replaced_and_figures_to_fill(self):
        said = office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "7:12", "to": 13,
                                                             "replace": {"AUGUST": "SEPTEMBER"}}],
                                      edits=[{"sheet": "Calc", "cell": "B15", "value": 95}])
        self.assertIn("copied rows 7-12 of Calc to rows 13-18", said)
        self.assertIn("check each reads the new period's rows there: B16 =Inputs!B13", said)
        self.assertIn("typed figures left empty to fill: B15", said)
        ws = openpyxl.load_workbook(self.path)["Calc"]
        self.assertEqual(ws["A13"].value, "SEPTEMBER 2026")
        self.assertEqual(ws["A18"].value, "Return for SEPTEMBER 2026 filed")
        self.assertIsNone(ws["C18"].value)                               # August's date and reference: never copied
        self.assertIn("C18", said)
        self.assertEqual(ws["B14"].value, "=B11")                       # opens with August's closing
        self.assertEqual(ws["B17"].value, "=B14+B16-B15")
        self.assertEqual(ws["B15"].value, 95)                            # filled by the edit in the same update
        self.assertEqual(ws["A13"].fill.fgColor.rgb, ws["A7"].fill.fgColor.rgb)   # the format came along
        self.assertEqual(ws["A7"].value, "AUGUST 2026")                  # the block copied is left as it was

    def test_a_dated_row_is_an_entry_and_nothing_typed_in_it_is_copied(self):
        import datetime
        wb = openpyxl.load_workbook(self.path)
        ws = wb["Calc"]
        ws["D9"], ws["E9"], ws["F9"] = datetime.datetime(2026, 8, 7), "MALABAR SPICES", "=E9"   # a bill's line
        wb.save(self.path)
        said = office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "7:12", "to": 13,
                                                             "replace": {"AUGUST": "SEPTEMBER"}}])
        ws = openpyxl.load_workbook(self.path)["Calc"]
        self.assertIsNone(ws["E15"].value)            # August's party, never September's
        self.assertIsNone(ws["D15"].value)
        self.assertIsNone(ws["A15"].value)            # the dated row's own label is an entry's too
        self.assertEqual(ws["F15"].value, "=E15")     # its formulas are layout, and move
        self.assertIn("E15", said)

    def test_it_never_writes_over_a_cell_that_holds_something(self):
        with self.assertRaises(office.OfficeError) as raised:
            office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "1:6", "to": 5}])
        self.assertIn("nothing was written", str(raised.exception))
        with self.assertRaises(office.OfficeError) as raised:
            office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "1:6", "to": 7}])
        self.assertIn("would write over A7", str(raised.exception))
        self.assertEqual(openpyxl.load_workbook(self.path)["Calc"]["A7"].value, "AUGUST 2026")

    def test_figures_can_be_kept_and_a_bad_spec_writes_nothing(self):
        office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "7:12", "to": 13, "keep_figures": True}])
        self.assertEqual(openpyxl.load_workbook(self.path)["Calc"]["B15"].value, 120)
        for bad in ({"sheet": "Calc", "rows": "7-x", "to": 13}, {"sheet": "Nope", "rows": "7:12", "to": 13},
                    {"sheet": "Calc", "rows": "7:12", "to": "13"}):
            with self.assertRaises(office.OfficeError):
                office.update_workbook(self.path, copy_rows=[bad])


if __name__ == "__main__":
    unittest.main()
