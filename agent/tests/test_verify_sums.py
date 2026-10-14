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


if __name__ == "__main__":
    unittest.main()
