"""
Whether digits go into a cell as a number or as text is decided by what a number would lose, never by the words of
the label beside them (review 2026-11-03, F-08). Label vocabularies wrote an amount beside "Amount credited to account"
as text, so SUM left it out. Now: a leading zero, a leading "+", or more digits than a spreadsheet keeps (15) makes
text; anything else is a number, and a long whole number (12 digits or more) is shown whole with the format "0"
rather than as 1.23E+11.
"""

import tempfile
import unittest
from pathlib import Path

import test_recordings  # noqa: F401 - stubs config first
from needs import needs

from decurix_agent.tools import office


class CellValues(unittest.TestCase):
    def test_digits_a_number_would_change_are_text(self):
        self.assertEqual(office._cell_value("00123456789"), "00123456789")      # a leading zero
        self.assertEqual(office._cell_value("+919876543210"), "+919876543210")  # a leading "+"
        self.assertEqual(office._cell_value("4111111111111111"), "4111111111111111")  # 16 digits
        self.assertEqual(office._cell_value(4111111111111111), "4111111111111111")    # given as a number too

    def test_digits_a_number_keeps_are_numbers_whatever_the_label(self):
        self.assertEqual(office._cell_value("1234567890"), 1234567890)
        self.assertEqual(office._cell_value(250000), 250000)
        self.assertEqual(office._cell_value("50100123456789"), 50100123456789)  # 14 digits: kept exactly
        self.assertEqual(office._cell_value("1250.50"), 1250.5)
        self.assertEqual(office._cell_value(3), 3)

    def test_a_long_whole_number_is_shown_whole(self):
        self.assertTrue(office._shown_whole(123456789012))
        self.assertFalse(office._shown_whole(12345678901))
        self.assertFalse(office._shown_whole(1234567890123.5))
        self.assertFalse(office._shown_whole(True))


@needs("openpyxl")
class Workbooks(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_a_figure_beside_any_label_adds_up(self):
        import openpyxl

        path = self.dir / "summary.xlsx"
        labels = ["Amount credited to account", "Payment to vendor account", "Total due on account", "Net amount ref"]
        office.create_workbook(str(path), [{"name": "Summary", "header": False,
                                            "rows": [[label, 250000] for label in labels]}])
        ws = openpyxl.load_workbook(str(path))["Summary"]
        self.assertEqual([ws[f"B{i}"].value for i in range(1, 5)], [250000] * 4)

    def test_create_keeps_what_a_number_would_lose_and_shows_long_numbers_whole(self):
        import openpyxl

        path = self.dir / "payees.xlsx"
        office.create_workbook(str(path), [{"name": "Payees", "rows": [
            ["Payee", "Account No", "Mobile", "Amount"],
            ["Party A", "00123456789", "+919876543210", 450000],
            ["Party B", 501001234567890, "9123456780", "1250.50"],
        ]}])
        ws = openpyxl.load_workbook(str(path))["Payees"]
        self.assertEqual(ws["B2"].value, "00123456789")
        self.assertEqual(ws["C2"].value, "+919876543210")
        self.assertEqual(ws["B3"].value, 501001234567890)
        self.assertEqual(ws["B3"].number_format, "0")
        self.assertEqual(ws["C3"].value, 9123456780)
        self.assertEqual(ws["D3"].value, 1250.5)

    def test_update_writes_by_the_value_and_shows_long_numbers_whole(self):
        import openpyxl

        path = self.dir / "book.xlsx"
        office.create_workbook(str(path), [{"name": "Data", "rows": [["Payee", "Account No", "Amount"],
                                                                     ["Party A", "00123", 100]]}])
        office.update_workbook(str(path), edits=[{"sheet": "Data", "cell": "B3", "value": 987654321012},
                                                 {"sheet": "Data", "cell": "B4", "value": "00987"},
                                                 {"sheet": "Data", "cell": "B5", "value": 12345678901234567},
                                                 {"sheet": "Data", "cell": "C3", "value": "2500"},
                                                 {"sheet": "Data", "cell": "A3", "value": "Party B"}])
        ws = openpyxl.load_workbook(str(path))["Data"]
        self.assertEqual(ws["B3"].value, 987654321012)
        self.assertEqual(ws["B3"].number_format, "0")
        self.assertEqual(ws["B4"].value, "00987")
        self.assertEqual(ws["B5"].value, "12345678901234567")
        self.assertEqual(ws["C3"].value, 2500)


if __name__ == "__main__":
    unittest.main()
