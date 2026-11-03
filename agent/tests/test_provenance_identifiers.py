"""
An identifier is held by a source only when the source writes those digits as one identifier (review 2026-11-03,
E-28): never digits joined across cells or words, and never with leading digits cut off unless the writing marks them
as a prefix ("+91", a trunk "0"). The check blocks, so both directions matter.
"""

import unittest

from decurix_agent import provenance


class Identifiers(unittest.TestCase):
    def test_digits_across_cells_and_words_are_not_an_identifier(self):
        held = provenance.Sources(["Qty 12 Rate 3456 Total 789", "a,b\n12,3456789\n"])
        self.assertFalse(held.holds("ident", "2345678"))
        self.assertFalse(held.holds("ident", "1234567"))

    def test_a_part_of_a_longer_number_is_not_it(self):
        held = provenance.Sources(["Account 912345678901"])
        self.assertFalse(held.holds("ident", "2345678901"))
        self.assertTrue(held.holds("ident", "912345678901"))

    def test_the_same_identifier_however_grouped_or_prefixed_is_held(self):
        held = provenance.Sources(["Call +91 98765 43210 or 022-2345-6789."])
        for written in ("9876543210", "98765 43210", "+91 98765 43210", "02223456789"):
            with self.subTest(written=written):
                self.assertTrue(held.holds("ident", written))
        trunk = provenance.Sources(["Phone 0 98765 43210"])
        self.assertTrue(trunk.holds("ident", "9876543210"))


if __name__ == "__main__":
    unittest.main()
