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



class DatesWrittenEitherWay(unittest.TestCase):
    """Found live (2026-10-14): "03/10/2026" read as 10 March on forms that write dates day first."""

    def test_the_order_comes_from_dates_that_read_only_one_way(self):
        from decurix_agent.binding import date_order
        self.assertEqual(date_order(["Date of loss: 28/09/2026"]), "day")
        self.assertEqual(date_order(["Invoice date 09/28/2026"]), "month")
        self.assertIsNone(date_order(["Date of loss: 03/10/2026"]))
        self.assertIsNone(date_order(["28/09/2026 and 09/28/2026"]))

    def test_the_model_is_told_the_order_or_to_leave_an_unclear_date_out(self):
        from unittest import mock
        from decurix_agent import binding
        with mock.patch.object(binding, "_document_text", return_value="Date of loss: 28/09/2026"):
            self.assertIn("day first", binding._date_note({"documents": ["/w/teach.pdf"]}, ["Date of loss: 03/10/2026"]))
        with mock.patch.object(binding, "_document_text", return_value=""):
            self.assertIn("leave that value out", binding._date_note({"documents": []}, ["03/10/2026"]))

    def test_a_date_value_is_known_by_its_example_or_label(self):
        from decurix_agent.binding import _is_date_param
        self.assertTrue(_is_date_param({"name": "date_of_incident", "label": "Date of incident", "example": "2026-09-28"}))
        self.assertTrue(_is_date_param({"name": "x", "label": "Due", "example": "28/09/2026"}))
        self.assertFalse(_is_date_param({"name": "amount", "label": "Amount", "example": "4850.00"}))


if __name__ == "__main__":
    unittest.main()
