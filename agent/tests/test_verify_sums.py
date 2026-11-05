import unittest

from decurix_agent.verify import false_sums


class SumsInAReport(unittest.TestCase):
    def test_a_sum_that_does_not_come_to_what_it_says_is_named(self):
        self.assertEqual(false_sums("Atlas total matches: 750.00 + 300.00 + 84.00 = 1143.00"),
                         ["750.00 + 300.00 + 84.00 = 1143.00 is wrong: it comes to 1,134.00"])

    def test_right_sums_in_their_usual_shapes_pass(self):
        for text in ("450.00 + 90.00 + 102.60 = 642.60", "$1,200 - 150 = $1,050", "3 x 25.00 = 75.00",
                     "USD 1,000.50 + USD 99.50 = USD 1,100.00", "33.333 + 33.333 = 66.67", "8 - 3 = 5"):
            self.assertEqual(false_sums(text), [], text)

    def test_what_is_not_a_written_out_sum_is_left_alone(self):
        for text in ("due 2026-10-21 = Wednesday", "10/2 = 5", "growth 5 + 5 = 10%", "x=3+4", "invoice 12 + tax"):
            self.assertEqual(false_sums(text), [], text)

    def test_multiplication_comes_before_addition(self):
        self.assertEqual(false_sums("3 x 25.00 + 5 = 80.00"), [])
        self.assertTrue(false_sums("3 x 25.00 + 5 = 81.00"))

    def test_a_product_with_its_currency_is_a_product(self):
        # Round 5 P6 (2026-10-21): the right refund line was refused twice - "it comes to 6,035".
        self.assertEqual(false_sums("E03: Approve refund 60 x Rs 35 = Rs 2,100 (policy, up to Rs 5,000)."), [])
        for text in ("60 x Rs 35 = Rs 2,100", "60 X Rs. 35 = Rs. 2,100", "60 * INR 35 = INR 2,100", "60 @ Rs 35 = 2,100",
                     "40 @ 175.75 = 7,030.00", "2 x Rs 35 + 1 x Rs 30 = Rs 100"):
            self.assertEqual(false_sums(text), [], text)
        self.assertEqual(false_sums("60 x Rs 35 = Rs 2,000"), ["60 x Rs 35 = Rs 2,000 is wrong: it comes to Rs 2,100"])
        self.assertTrue(false_sums("40 @ 175.75 = 7,000.00"))

    def test_what_cannot_be_read_with_certainty_is_not_decided(self):
        for text in ("10 x 2 x (1+18%) = 23.60", "60 pcs x Rs 35 = Rs 2,000", "60 x 35 kg = 2,000"):
            self.assertEqual(false_sums(text), [], text)

    def test_the_priced_list_that_made_this_check_is_still_refused(self):
        # Round 5 P2 (2026-10-20): SO-5101's lines come to 13,456.00.
        found = false_sums("The order should total 13129.00 ex GST from WB-500 40 @ 175.75, MUG-12 24 @ 149, "
                           "NB-A5 30 @ 95.")
        self.assertEqual(found, ["the total 13129.00 stated for 40 @ 175.75, 24 @ 149, 30 @ 95 is wrong: those come to "
                                 "13,456.00"])


if __name__ == "__main__":
    unittest.main()


class NumbersAsAProgramPrintsThem(unittest.TestCase):
    def test_float_noise_and_scientific_notation_are_named(self):
        from decurix_agent.verify import program_numbers
        self.assertEqual(program_numbers("Net 1672.7400000000002, hours 3E+1, rate 1.5e-05"),
                         ["1672.7400000000002", "3E+1", "1.5e-05"])
        self.assertEqual(program_numbers("total 0.30000000000000004 and 2.9999999999999996"),
                         ["0.30000000000000004", "2.9999999999999996"])

    def test_what_people_write_is_left_alone(self):
        from decurix_agent.verify import program_numbers
        for text in ("Total 7,183.00", "rate 1.0823", "INV-2026-0094", "E101.pdf", "PO 1E5A", "x3E+1y", "version 2.10.3"):
            self.assertEqual(program_numbers(text), [], text)

    def test_a_bullet_dash_on_the_next_line_is_not_a_minus(self):
        # Found live (2026-10-27): a list of vouchers and their totals, one per line, read as subtractions.
        text = "Vouchers seen in the Day Book:\n- 1190 = 56,994.00\n- 1191 = 55,802.60\n- 1193 = 25,711.02"
        self.assertEqual(false_sums(text), [])
        self.assertEqual(false_sums("Total 750.00 + 300.00 + 84.00 = 1143.00"),
                         ["750.00 + 300.00 + 84.00 = 1143.00 is wrong: it comes to 1,134.00"])
