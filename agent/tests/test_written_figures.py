"""
The code-decided figure checks read the notation an Indian firm writes (E-24) and never take an identifier for a
program's number (E-25). Both checks block, so each wrong case must be found and each right one let through.
"""

import unittest

from decurix_agent import verify


class IndianGroupingAndTheRupeeSign(unittest.TestCase):
    def test_a_wrong_sum_in_indian_grouping_or_rupees_is_found(self):
        for text, said in (("1,00,000 + 50,000 = 1,60,000", "it comes to 1,50,000"),
                           ("₹1,200 + ₹300 = ₹1,600", "it comes to 1,500"),
                           ("12,34,567 - 34,567 = 12,10,000", "it comes to 12,00,000")):
            with self.subTest(text=text):
                found = verify.false_sums(text)
                self.assertEqual(len(found), 1, found)
                self.assertIn(said, found[0])

    def test_a_right_sum_in_that_notation_is_not(self):
        for text in ("1,00,000 + 50,000 = 1,50,000", "₹1,200 + ₹300 = ₹1,500",
                     "12,34,567 - 34,567 = 12,00,000", "Rs 1,00,000.50 + 2 = 1,00,002.50"):
            with self.subTest(text=text):
                self.assertEqual(verify.false_sums(text), [])

    def test_ranges_and_rates_are_still_not_sums(self):
        for text in ("Entered rows 2-10 = 9 rows.", "Invoices 101-110 = 10 invoices", "TDS @ 10 = 1,000 on 10,000"):
            with self.subTest(text=text):
                self.assertEqual(verify.false_sums(text), [])


class ProgramNumbers(unittest.TestCase):
    def test_an_identifier_shaped_like_scientific_notation_is_not_a_program_number(self):
        for text in ("Deliver to Flat 3E2, Block B", "Part 12E5 ordered", "Unit 4e1"):
            with self.subTest(text=text):
                self.assertEqual(verify.program_numbers(text), [])

    def test_what_a_program_prints_is_still_found(self):
        self.assertEqual(verify.program_numbers("hours 3E+1, rate 1.5e-05, net 1672.7400000000002"),
                         ["1672.7400000000002", "3E+1", "1.5e-05"])


if __name__ == "__main__":
    unittest.main()
