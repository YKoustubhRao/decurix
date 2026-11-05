"""
An extra row of a list is found as that row's control only when each demonstrated row had a control of its own there
(review round 2, D-30): a field drawn in every row takes its row's place among its name; a control the rows share - an
add-row press, or an editor drawn only in the active row at one path - is found by what it is.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import skills


def step(role, name, path):
    return {"kind": "click", "target": {"role": role, "name": name, "path": path}}


class AControlOfItsOwnRow(unittest.TestCase):
    def test_a_field_in_every_row_is_that_rows(self):
        self.assertTrue(skills._per_row(step("textbox", "Qty", "table > row 2 > textbox Qty"),
                                        step("textbox", "Qty", "table > row 1 > textbox Qty")))

    def test_an_add_row_press_is_shared(self):
        self.assertFalse(skills._per_row(step("button", "Add row", "table > button Add row"),
                                         step("button", "Add row", "table > button Add row")))

    def test_an_editor_drawn_only_in_the_active_row_is_shared(self):
        self.assertFalse(skills._per_row(step("textbox", "Cell editor", "grid > textbox Cell editor"),
                                         step("textbox", "Cell editor", "grid > textbox Cell editor")))

    def test_unknown_is_not_a_rows_own(self):
        self.assertFalse(skills._per_row(step("textbox", "Qty", "x"), None))
        self.assertFalse(skills._per_row(step("textbox", "Qty", "a"), step("textbox", "Other", "b")))


if __name__ == "__main__":
    unittest.main()
