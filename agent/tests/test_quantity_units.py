import unittest

from decurix_agent import binding, skill_task


class AQuantityWithItsUnit(unittest.TestCase):
    """Found live: a bill's "20 Reams" was read whole for a quantity field, refused as not a number, and a person asked."""

    def test_one_number_and_the_words_of_its_unit_is_that_number_for_a_field_shown_to_take_one(self):
        for value, number in (("20 Reams", "20"), ("2 Chairs", "2"), ("2 Units", "2"), ("1.5 kg", "1.5"), ("1,250 pcs", "1,250"),
                              ("Rs. 360.00", "360.00"), ("INR 1,250", "1,250"), ("$40", "40"), ("360.00/-", "360.00")):
            self.assertEqual(binding.as_written(value, "1", ()), number, value)

    def test_nothing_is_guessed(self):
        # A field that was shown text keeps its text; two numbers, or words before the number, are left as read.
        self.assertEqual(binding.as_written("20 Reams", "Box of 10", ()), "20 Reams")
        for value in ("20 x 30", "Invoice 20", "PO 7731", "Acme Ltd", "20", "04-10-2026"):
            self.assertEqual(binding.as_written(value, "1", ()), value, value)
        self.assertEqual(binding.as_written("20 Reams", None, ()), "20 Reams")


class ASecondAttemptDoesNotForceAFigure(unittest.TestCase):
    def test_the_rule_is_part_of_what_every_second_attempt_is_told(self):
        rule = skill_task.NOT_YOURS_TO_FORCE
        for part in ("is not yours to force", "the person said to leave", "say both figures", "ask (HELP)"):
            self.assertIn(part, rule)


if __name__ == "__main__":
    unittest.main()


class AListOfRecordsIsAPlace(unittest.TestCase):
    """Found live: looking for an earlier copy of a bill, a worker was stopped for a yes at "Purchase Invoice List"."""

    def test_a_link_to_where_such_records_are_looked_at_goes_through_and_an_act_still_stops(self):
        from decurix_agent.computer import approval
        link = lambda name: {"role": "link", "name": name, "path": "page > main"}
        for place in ("Purchase Invoice List", "Purchase Order Report", "Purchase Invoice", "Purchase Receipt Summary"):
            self.assertIsNone(approval.assess("click", link(place)), place)
        for act in ("Purchase now", "Purchase Invoice and pay", "Send invoice", "Pay now"):
            self.assertIsNotNone(approval.assess("click", link(act)), act)
        self.assertIsNotNone(approval.assess("click", {"role": "button", "name": "Purchase Invoice List", "path": "page"}))
