"""A code whose letters are joined to its digits is not a number with a decoration (found live 2026-10-26)."""
import unittest

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import core


class DecoratedNumbers(unittest.TestCase):
    def test_signs_and_units_beside_a_number_are_decorations(self):
        for text in ("Rs 3,306", "Rs.3306", "3306 INR", "15 kg", "$ 1,250.50", "1250.50%"):
            self.assertTrue(core._decorated_number(text, "1"), text)

    def test_a_code_is_not_a_decoration(self):
        for text in ("ABC-3306", "INV3306", "SO/26-27/0012", "2026-09-26", "A1"):
            self.assertFalse(core._decorated_number(text, "1"), text)
        self.assertFalse(core._decorated_number("3306", "1"))
        self.assertFalse(core._decorated_number("Rs 3306", "Alpha"))


if __name__ == "__main__":
    unittest.main()
