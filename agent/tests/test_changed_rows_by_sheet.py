"""What a task changed in a workbook is told sheet by sheet (independent review, 2026-10-22)."""
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

from decurix_agent import produced, source_guard


class TestChangedRowsKeepTheirSheet(unittest.TestCase):
    """A changed cell was kept as "c97=", which matched row 97 of every sheet: the judge was shown other sheets'
    untouched rows as "what this task changed"."""

    def setUp(self):
        folder = Path(tempfile.mkdtemp())
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "Returns"
        other = wb.create_sheet("Clients")
        for r in range(1, 101):
            ws.cell(r, 1, f"return row {r}")
            ws.cell(r, 3, r)
            other.cell(r, 1, f"client row {r}")
            other.cell(r, 3, r * 10)
            other.cell(r, 29, f"far right {r}")   # AC: its address ends like C's
        self.before = folder / "before.xlsx"
        wb.save(self.before)
        ws.cell(97, 3, "changed by the task")
        self.path = folder / "book.xlsx"
        wb.save(self.path)
        patcher = mock.patch.object(source_guard, "before_copy", return_value=self.before)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_only_the_changed_sheets_rows_are_shown_as_changed(self):
        self.assertEqual(produced.changed_cells(self.path), [("Returns", "C97")])
        self.assertEqual(produced.changed_rows_by_sheet(self.path), {"Returns": {97}})
        text = produced.changed_rows_text(self.path)
        self.assertIn("changed by the task", text)
        self.assertNotIn("client row 97", text)
        self.assertNotIn("Clients", text)

    def test_the_sample_keeps_the_changed_cell_on_its_own_sheet_by_whole_address(self):
        body = "\n".join(
            ["book.xlsx: sheets ['Returns', 'Clients']", "=== Sheet 'Returns': used range A1:C100 (100 rows x 3 columns) ==="]
            + [f"A{r}=return row {r}  C{r}={r}" for r in range(1, 101)]
            + ["=== Sheet 'Clients': used range A1:AC100 (100 rows x 29 columns) ==="]
            + [f"A{r}=client row {r}  C{r}={r * 10}  AC{r}=far right {r}" for r in range(1, 101)])
        shown = produced.sample_table(body, 1200, cells=[("Returns", "C9")])
        self.assertIn("A9=return row 9  C9=9", shown)
        self.assertNotIn("client row 9  ", shown)    # the same address on another sheet
        self.assertNotIn("far right 9\n", shown + "\n")

    def test_the_workbooks_are_compared_once_per_saved_version(self):
        produced._CHANGES.clear()
        real = openpyxl.load_workbook
        with mock.patch.object(openpyxl, "load_workbook", side_effect=real) as loads:
            produced.changed_cells(self.path)
            produced.changed_rows_by_sheet(self.path)
        self.assertEqual(loads.call_count, 2)       # the copy from before and the file, once
        wb = openpyxl.load_workbook(self.path)
        wb["Clients"]["A5"] = "now changed too"
        wb.save(self.path)
        self.assertEqual(produced.changed_rows_by_sheet(self.path), {"Returns": {97}, "Clients": {5}})


if __name__ == "__main__":
    unittest.main()
