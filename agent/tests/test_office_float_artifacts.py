"""
CZ (round 5 P2, 2026-10-21, series 12): stage 2 was refused twice, fairly, for "numbers written as a program
prints them: 7640.220000000001, 40123.60000000001", and the worker did not correct it. office now writes a float's
rounding error as the figure it is - in workbooks (create, update) and in the text of documents, decks and PDFs it
makes - and never changes a figure that really has many digits. The fact check still catches a file a raw script wrote.
"""

import tempfile
import unittest
from pathlib import Path

from needs import require

require("openpyxl", "python-docx")  # every test here reads or writes workbooks or documents
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import verify
from decurix_agent.tools import office


class CleanNumbers(unittest.TestCase):
    def test_rounding_errors_are_the_figure_they_are(self):
        for raw, clean in ((7640.220000000001, 7640.22), (40123.60000000001, 40123.6), (1.9999999999999998, 2.0),
                           (0.30000000000000004, 0.3)):
            self.assertEqual(office.clean_number(raw), clean)
        for real in (0.333333333333, 1078139.62, 12.5, 2.0, 1e-07):
            self.assertEqual(office.clean_number(real), real)
        self.assertEqual(office.clean_text("Rs 40123.60000000001 and 7640.220000000001; 0.333333333333"),
                         "Rs 40123.60 and 7640.22; 0.333333333333")

    def test_through_office_create_and_update(self):
        with tempfile.TemporaryDirectory() as tmp:
            book = Path(tmp) / "order_working.xlsx"
            office.create_workbook(str(book), [{"name": "Orders", "rows": [["Order", "Total"],
                                                                             ["WA-1810", 7640.220000000001],
                                                                             ["HH/PO/7781", "40123.60000000001"],
                                                                             ["Ratio", 0.333333333333]]}])
            office.update_workbook(str(book), edits=[{"sheet": "Orders", "cell": "C2", "value": 50123.600000000006}])
            sheet = openpyxl.load_workbook(book).active
            self.assertEqual(sheet["B2"].value, 7640.22)
            self.assertEqual(sheet["B3"].value, 40123.6)
            self.assertEqual(sheet["B4"].value, 0.333333333333)
            self.assertEqual(sheet["C2"].value, 50123.6)
            doc = Path(tmp) / "replies.docx"
            office.create_document(str(doc), [{"paragraph": "Invoice total Rs 40123.60000000001 with GST."}])
            text = office.read_document(str(doc))["text"]
            self.assertIn("Rs 40123.60 with GST", text)
            self.assertNotIn("40123.60000000001", text)

    def test_a_raw_script_file_is_still_caught(self):
        self.assertTrue(verify.program_numbers("Total 7640.220000000001"))


if __name__ == "__main__":
    unittest.main()
