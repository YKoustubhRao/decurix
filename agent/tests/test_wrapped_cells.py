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


if __name__ == "__main__":
    unittest.main()
