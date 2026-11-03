"""
Review 2026-11-03: a 9-11 digit account number, and one given as a JSON number, was written into a workbook as a number,
so the sheet showed 1.23E+09 and dropped leading zeros. A value under (or beside) a label that names an identifier -
an account, a phone, a postal code, an ID - is written as text; a figure under a quantity's label stays a number.
"""

import tempfile
import unittest
from pathlib import Path

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.tools import office

try:
    import openpyxl
except ImportError:  # pragma: no cover - the office venv has it
    openpyxl = None


class LabelKind(unittest.TestCase):
    def test_identifier_labels(self):
        for label in ("Account No.", "Bank Account Number", "A/c", "IFSC Code", "Mobile", "Phone (office)", "PIN code",
                      "Pincode", "GSTIN", "PAN", "UTR", "Ref", "Customer ID", "Aadhaar", "Phone 2", "HSN/SAC"):
            self.assertEqual(office.label_kind(label), "id", label)

    def test_quantity_labels(self):
        for label in ("Amount", "Amount (Rs)", "Account Balance", "Total", "Qty", "Taxable Value", "GST",
                      "Number of units", "No. of invoices", "Opening Balance"):
            self.assertEqual(office.label_kind(label), "quantity", label)

    def test_other_labels(self):
        for label in ("Customer", "Sita Phone Traders", "Date", "Value", None, 12, ""):
            self.assertIsNone(office.label_kind(label), label)


class CellValues(unittest.TestCase):
    def test_an_identifier_label_makes_digits_text(self):
        for v in ("1234567890", 1234567890, 12345678901.0, "560001", 9876543210):
            got = office._cell_value(office._labelled_value(v, "Account No"))
            self.assertIsInstance(got, str, v)
        self.assertEqual(office._cell_value(office._labelled_value(1234567890, "Mobile")), "1234567890")

    def test_a_quantity_or_no_label_keeps_numbers(self):
        self.assertEqual(office._cell_value(office._labelled_value("1234567890", "Amount")), 1234567890)
        self.assertEqual(office._cell_value(office._labelled_value(1234567890, "Customer")), 1234567890)
        self.assertEqual(office._cell_value(office._labelled_value(4500.5, "Account No")), 4500.5)
        self.assertEqual(office._cell_value(office._labelled_value(3, "S. No")), 3)  # a short serial stays a number

    def test_a_label_beside_counts_only_in_a_label_value_row(self):
        self.assertIsInstance(office._cell_value(office._labelled_value(1234567890, "Value", "Account No", 2)), str)
        self.assertEqual(office._cell_value(office._labelled_value(1200, "Amount", "Sita Phone", 2)), 1200)
        self.assertEqual(office._cell_value(office._labelled_value(123456, "Taxable Value", "Phone Traders", 2)),
                         123456)
        self.assertEqual(office._cell_value(office._labelled_value(123456, None, "Phone Traders", 4)), 123456)

    def test_a_json_number_too_long_to_keep_is_text(self):
        self.assertEqual(office._cell_value(50100123456789), "50100123456789")
        self.assertEqual(office._cell_value(125000000), 125000000)


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class Workbooks(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_create_writes_identifiers_as_text_and_figures_as_numbers(self):
        path = self.dir / "payees.xlsx"
        office.create_workbook(str(path), [{"name": "Payees", "rows": [
            ["Payee", "Account No", "IFSC", "Mobile", "Amount"],
            ["Acme", 1234567890, "HDFC0001234", "9876543210", 450000],
            ["Sita Phone Traders", "00123456789", "ICIC0000025", 9123456780, "1250.50"],
        ]}, {"name": "Details", "header": False, "rows": [["Account No", 12345678901], ["Amount", 99000]]}])
        ws = openpyxl.load_workbook(str(path))["Payees"]
        self.assertEqual(ws["B2"].value, "1234567890")
        self.assertEqual(ws["B3"].value, "00123456789")
        self.assertEqual(ws["D2"].value, "9876543210")
        self.assertEqual(ws["D3"].value, "9123456780")
        self.assertEqual(ws["E2"].value, 450000)
        self.assertEqual(ws["E3"].value, 1250.5)
        details = openpyxl.load_workbook(str(path))["Details"]
        self.assertEqual(details["B1"].value, "12345678901")  # a "label, value" row: the label beside it counts
        self.assertEqual(details["B2"].value, 99000)

    def test_update_writes_under_an_identifier_column_as_text(self):
        path = self.dir / "book.xlsx"
        office.create_workbook(str(path), [{"name": "Data", "rows": [["Payee", "Account No", "Amount"],
                                                                     ["Acme", "1234567890", 100]]}])
        office.update_workbook(str(path), edits=[{"sheet": "Data", "cell": "B3", "value": 98765432101},
                                                 {"sheet": "Data", "cell": "C3", "value": "2500"},
                                                 {"sheet": "Data", "cell": "A3", "value": "Bharat"}])
        ws = openpyxl.load_workbook(str(path))["Data"]
        self.assertEqual(ws["B3"].value, "98765432101")
        self.assertEqual(ws["C3"].value, 2500)


if __name__ == "__main__":
    unittest.main()
