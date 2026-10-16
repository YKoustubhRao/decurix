"""An action that changes a field away from a value this task typed says so."""

import unittest
from unittest import mock

from decurix_agent.computer import core


def field(path, name, value):
    return {"path": path, "role": "textbox", "name": name, "value": value}


class AFieldTheFormRefilled(unittest.TestCase):
    def note(self, typed, changed, acted=None):
        with mock.patch("decurix_agent.computer.evidence.entered", return_value=typed):
            return core._overwritten_note({"changed": changed}, acted or {"path": "product"})

    def test_a_price_typed_then_refilled_by_the_product_is_named(self):
        # Found live (2026-10-14): the sheet's 2.50 became the catalogue's 2.75 when the product was picked.
        price = field("price", "Unit price", "2.75")
        said = self.note(["2.50", "100"], [(price, [("value", "2.50", "2.75")])])
        self.assertIn('"2.50" to "2.75"', said)
        self.assertIn("type it again", said)

    def test_a_change_to_what_nobody_typed_says_nothing(self):
        price = field("price", "Unit price", "2.75")
        self.assertEqual(self.note(["100"], [(price, [("value", "", "2.75")])]), "")
        self.assertEqual(self.note(["100"], [(price, [("value", "2.60", "2.75")])]), "")

    def test_the_field_acted_on_is_not_a_loss(self):
        product = field("product", "Product", "NB-A5")
        self.assertEqual(self.note(["NB"], [(product, [("value", "NB", "NB-A5")])], acted={"path": "product"}), "")


if __name__ == "__main__":
    unittest.main()
