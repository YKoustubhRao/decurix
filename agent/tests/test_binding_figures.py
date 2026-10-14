"""A figure read from a document for a field that takes a bare number is the number it stands for."""

import unittest

from decurix_agent.binding import as_number


class Figures(unittest.TestCase):
    def test_a_scale_word_is_what_it_says(self):
        # Found live (2026-10-14): a salary of "$72k" was entered as 72.
        for written, number in (("$72k", "72000"), ("72k", "72000"), ("$72k per year", "72000"), ("USD 1.5m", "1500000"),
                                ("Rs. 3 lakh", "300000"), ("2.5 crore", "25000000"), ("1.5 million", "1500000")):
            self.assertEqual(as_number(written, "58000"), number, written)

    def test_a_unit_is_left_off_and_a_short_m_is_not_a_million_without_a_currency(self):
        self.assertEqual(as_number("20 Reams", "1"), "20")
        self.assertEqual(as_number("20 m", "1"), "20")
        self.assertEqual(as_number("Rs. 360.00", "40000"), "360.00")

    def test_a_field_that_was_not_shown_a_bare_number_keeps_the_text(self):
        self.assertEqual(as_number("$72k", "about 70k"), "$72k")


if __name__ == "__main__":
    unittest.main()
