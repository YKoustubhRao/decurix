"""
A workflow artifact's "contains" check (workflow_runner._contains), review round 3, R-3: every digit string was compared
as a float, so a different 18-digit account number passed, and "00123" passed on "Ref 123". A number that would lose
information as an amount (a leading zero, a leading "+", more than 15 digits) is matched digit for digit as a whole
token; amounts compare exactly.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.workflow_runner import _contains


class AnIdentifierIsMatchedAsItself(unittest.TestCase):
    def test_a_different_long_account_number_does_not_pass(self):
        self.assertFalse(_contains("Account 123456789012345678", "123456789012345679"))
        self.assertFalse(_contains("Account 123456789012345678", 123456789012345679))
        self.assertTrue(_contains("Account 123456789012345678", "123456789012345678"))
        self.assertTrue(_contains("Account 123456789012345678", 123456789012345678))

    def test_a_leading_zero_is_part_of_it(self):
        self.assertFalse(_contains("Ref 123", "00123"))
        self.assertTrue(_contains("Ref 00123 paid", "00123"))
        self.assertFalse(_contains("Ref 001234", "00123"))


class AnAmountIsStillAnAmount(unittest.TestCase):
    def test_amounts_in_any_grouping(self):
        self.assertTrue(_contains("Total: 1,500.00", "1500"))
        self.assertTrue(_contains("Total: 1,500.00", 1500))
        self.assertFalse(_contains("Total: 1,500.00", "500"))
        self.assertTrue(_contains("Pay 0.50 now", "0.5"))


if __name__ == "__main__":
    unittest.main()
