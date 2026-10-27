import datetime as dt
import tempfile
import unittest
from pathlib import Path

from decurix_agent.tools import office


# Found live (2026-10-21): office read wrote dates as datetime.datetime(2026, 7, 4, 0, 0), which nothing reads as a date.
class TestADateCellIsReadAsADate(unittest.TestCase):

    def test_dates_and_times_are_written_plainly(self):
        import openpyxl
        wb = openpyxl.Workbook()
        ws = wb.active
        ws["A1"] = dt.datetime(2026, 7, 4)
        ws["B1"] = dt.datetime(2026, 7, 4, 9, 30)
        ws["C1"] = "COASTAL PROVISIONS"
        ws["D1"] = 20800
        path = Path(tempfile.mkdtemp()) / "dates.xlsx"
        wb.save(path)
        text = office.read_document(str(path))["text"]
        self.assertIn("A1=2026-07-04 ", text + " ")
        self.assertIn("B1=2026-07-04 09:30:00", text)
        self.assertIn("C1='COASTAL PROVISIONS'", text)
        self.assertNotIn("datetime.datetime", text)


if __name__ == "__main__":
    unittest.main()
