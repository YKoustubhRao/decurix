"""
A field in a table of entries says which row it is in, a look says which apps
are on the desktop, and a long run of Tab is refused.
"""

import unittest

from decurix_agent.computer import cdp, cli
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError

METRICS = {"scx": 0, "scy": 0}


def field(name, box, role="textbox"):
    return {"role": role, "name": name, "box": list(box), "path": f"Orders > {role} \"{name}\"", "states": [], "value": ""}


class WhichRow(unittest.TestCase):
    def number(self, found, rows):
        cdp._number_rows(found, rows, METRICS, 0, 0, 1)
        return found

    def test_a_field_takes_the_row_the_page_reports_for_its_place(self):
        quantity = field("Quantity", (300, 200, 80, 30))
        self.number([quantity], [[300, 200, 80, 30, 2]])
        self.assertEqual(quantity["row"], 2)
        self.assertEqual(quantity["path"], 'Orders > row 2 > textbox "Quantity"')

    def test_the_same_field_in_two_rows_is_two_things(self):
        first, second = field("Quantity", (300, 200, 80, 30)), field("Quantity", (300, 240, 80, 30))
        self.number([first, second], [[300, 200, 80, 30, 1], [300, 240, 80, 30, 2]])
        self.assertNotEqual(first["path"], second["path"])

    def test_a_field_outside_any_table_is_left_alone(self):
        customer = field("Customer", (40, 90, 200, 30), role="combobox")
        self.number([customer], [[300, 200, 80, 30, 1]])
        self.assertNotIn("row", customer)
        self.assertEqual(customer["path"], 'Orders > combobox "Customer"')

    def test_what_is_not_a_field_is_never_given_a_row(self):
        heading = field("Items", (300, 200, 80, 30), role="heading")
        self.number([heading], [[300, 200, 80, 30, 1]])
        self.assertNotIn("row", heading)

    def test_a_page_that_answers_nonsense_names_no_row(self):
        for answer in (None, "rows", [[1, 2, 3]], [[300, 200, 80, 30, "2"]], [[300, 200, 80, 30, True]],
                       [[300, 200, 80, 30, 0]], [["a", 200, 80, 30, 1]]):
            quantity = field("Quantity", (300, 200, 80, 30))
            self.number([quantity], answer)
            self.assertNotIn("row", quantity, answer)

    def test_the_row_is_said_wherever_the_field_is_named(self):
        quantity = dict(field("Quantity", (300, 200, 80, 30)), row=3, ref="e7", states=["focused"])
        self.assertEqual(el.describe(quantity), 'textbox "Quantity" in row 3 (e7)')
        self.assertEqual(el.element_line(quantity), 'e7 textbox "Quantity" [row 3, focused]')


class AppsOnTheDesktop(unittest.TestCase):
    def render(self, apps):
        return el.render({"size": [1280, 720], "active_title": "", "windows": [], "groups": [], "elements": [],
                          "notes": [], "apps": apps})

    def test_a_look_names_them_and_how_to_open_one(self):
        shown = self.render(["Books (Host)", "Google Chrome"])
        self.assertIn('gui open "NAME"', shown)
        self.assertIn('"Books (Host)", "Google Chrome"', shown)

    def test_no_apps_no_line(self):
        self.assertNotIn("Apps on this desktop", self.render([]))


class WalkingWithTab(unittest.TestCase):
    def test_a_few_tabs_are_keys_like_any_other(self):
        cli._refuse_blind_walk(["tab"])
        cli._refuse_blind_walk(["ctrl+a", "Backspace", "tab", "tab"])
        cli._refuse_blind_walk(["tab"] * cli.MAX_WALK)

    def test_a_long_run_is_refused_before_anything_is_pressed(self):
        with self.assertRaises(ComputerError) as refused:
            cli._refuse_blind_walk(["Tab"] * 40)
        self.assertIn("40 Tab presses", str(refused.exception))
        self.assertIn("gui observe", str(refused.exception))

    def test_tabs_with_other_keys_between_them_are_not_one_run(self):
        cli._refuse_blind_walk(["tab", "tab", "tab", "enter", "tab", "tab", "tab"])


class WhereTheKeyboardStays(unittest.TestCase):
    """A demonstration's typing keeps its field across a chord that only edits the text."""

    def test_select_all_then_typing_is_typing_into_the_same_field(self):
        from decurix_agent.computer import demonstration

        date = {"role": "textbox", "name": "Date"}
        focus = demonstration._focus_after({"kind": "click"}, date, (False, None))
        focus = demonstration._focus_after({"kind": "key", "keys": ["ctrl+a"]}, date, focus)
        self.assertEqual(focus, (True, date))
        for chord in ("ctrl+c", "ctrl+v", "shift+Home", "ctrl+shift+Left", "ctrl+BackSpace"):
            self.assertEqual(demonstration._focus_after({"kind": "key", "keys": [chord]}, date, focus), (True, date), chord)

    def test_a_chord_that_does_something_else_still_loses_it(self):
        from decurix_agent.computer import demonstration

        date = {"role": "textbox", "name": "Date"}
        for chord in ("ctrl+s", "alt+Tab", "ctrl+Tab", "shift+Tab", "ctrl+w", "alt+a"):
            self.assertEqual(demonstration._focus_after({"kind": "key", "keys": [chord]}, date, (True, date)), (True, None), chord)


if __name__ == "__main__":
    unittest.main()
