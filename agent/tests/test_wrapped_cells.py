"""
A value read from a table cell that its PDF wraps over several lines, with the row's other columns read in between,
is held by that document (binding.held_by / wrapped_in): found live (2026-11-08), every bill's item description was
dropped from the fill as "in nothing this run was given", and the model entered every bill by hand.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_wrapped_cells.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import binding

# As the office tools read the bill (spacing kept): the description cell over four lines, qty/rate/amount in between.
BILL = ("   Qty                         Rate             Amount\n\n\n                       Executive A4\n"
        "                       Printing Paper 75\n                1                                          4802"
        "               20 Reams                  ₹350.00             ₹7,000.00\n"
        "                       GSM (Box of 10\n                       Reams)\n\n   2   High Back Ergonomic\n"
        "   Lumbar Support   4   ₹12,500.00   ₹50,000.00\n   Office Mesh Chairs\n")


class AWrappedCell(unittest.TestCase):

    def test_a_description_wrapped_over_lines_with_columns_between_is_held(self):
        self.assertTrue(binding.held_by("Executive A4 Printing Paper 75 GSM (Box of 10 Reams)", [BILL]))
        self.assertTrue(binding.held_by("High Back Ergonomic Lumbar Support Office Mesh Chairs", [BILL]))

    def test_words_out_of_order_or_missing_are_not(self):
        self.assertFalse(binding.held_by("Printing Paper Executive A4", [BILL]))
        self.assertFalse(binding.held_by("Executive A3 Printing Paper", [BILL]))
        self.assertFalse(binding.held_by("Executive A4 Printing Paper 80 GSM", [BILL]))

    def test_words_far_apart_are_not_one_value(self):
        far = "Executive summary of the year." + " filler" * 200 + " Printing Paper was bought."
        self.assertFalse(binding.held_by("Executive Printing Paper", [far]))

    def test_one_word_and_numbers_are_as_before(self):
        self.assertTrue(binding.held_by("Reams", [BILL]))
        self.assertFalse(binding.held_by("Ream", [BILL]))
        self.assertTrue(binding.held_by("7000", [BILL]))


class ADateWrittenAnotherWay(unittest.TestCase):
    """A digit date filled the way the field takes it is held by the document's date written another way
    (binding.date_in): found live (2026-11-08), "09-Oct-2026" filled as "09-10-2026" was dropped from every bill."""

    BILL = "TAX INVOICE  Invoice No: TC/2026/0455   Date: 09-Oct-2026   Due: 08-Nov-2026"

    def test_the_reading_that_names_the_documents_date_holds_it(self):
        self.assertTrue(binding.held_by("09-10-2026", [self.BILL]))
        self.assertEqual(binding.date_in("09-10-2026", [self.BILL]), "09-Oct-2026")
        self.assertEqual(binding.date_in("10-09-2026", [self.BILL]), "09-Oct-2026")  # month first: the same day

    def test_a_date_the_document_does_not_write_is_not_held(self):
        self.assertFalse(binding.held_by("11-10-2026", [self.BILL]))
        self.assertEqual(binding.date_in("Copier paper", [self.BILL]), "")

    def test_two_readings_naming_two_dates_of_the_document_hold_neither(self):
        both = "Bill date 09-Oct-2026, delivered 10-Sep-2026"
        self.assertEqual(binding.date_in("09-10-2026", [both]), "")

    def test_the_applications_order_when_known_decides_the_reading(self):
        from unittest import mock
        from decurix_agent import task_context
        with mock.patch.object(task_context, "app_date_order", return_value="month"):
            self.assertEqual(binding.date_in("09-10-2026", [self.BILL]), "")  # Sep 10: not this bill's date
            self.assertEqual(binding.date_in("10-09-2026", [self.BILL]), "09-Oct-2026")


if __name__ == "__main__":
    unittest.main()
