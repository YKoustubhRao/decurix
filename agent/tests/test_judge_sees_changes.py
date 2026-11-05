import tempfile
import unittest
from pathlib import Path
from unittest import mock

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

from decurix_agent import produced, source_guard


# ============================================================
# Found live (2026-10-22): the report gave a credit carried forward the
# saved workbook contradicted, and the check passed it - it was shown the
# sheet's first 60 rows, and the new month was rows 86-99.
# ============================================================

class TestTheJudgeSeesWhatTheTaskChanged(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "COMPUTATION"
        for r in range(1, 81):
            ws.cell(r, 1, f"line {r}")
            ws.cell(r, 2, r * 10)
        self.before = self.dir / "before.xlsx"
        wb.save(self.before)
        ws.cell(86, 1, "SEPTEMBER 2026")
        ws.cell(97, 1, "Closing credit carried forward")
        ws.cell(97, 4, 20862.14)
        self.after = self.dir / "book.xlsx"
        wb.save(self.after)

    def test_the_rows_the_task_changed_are_shown_with_their_saved_values(self):
        with mock.patch.object(source_guard, "before_copy", return_value=self.before):
            shown = produced.text([str(self.after)])
        self.assertIn("[What this task changed in it", shown)
        self.assertIn("A97='Closing credit carried forward'  D97=20862.14", shown)
        self.assertNotIn("A5='line 5'  B5=50\n[What", shown)

    def test_without_a_copy_from_before_the_whole_sheet_is_still_read(self):
        with mock.patch.object(source_guard, "before_copy", return_value=None):
            shown = produced.text([str(self.after)])
        self.assertNotIn("[What this task changed", shown)
        self.assertIn("D97=20862.14", shown)   # past office read's first 60 rows


if __name__ == "__main__":
    unittest.main()
