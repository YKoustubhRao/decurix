"""A point on the screen is on the window in front there, not on one covered behind it."""

import unittest

from decurix_agent.computer import elements as el


class APointOnTheWindowInFront(unittest.TestCase):
    def test_a_sheet_behind_the_slide_is_not_what_was_clicked(self):
        # Found live (2026-10-15): a click on an Impress slide was taken as a click on the Calc sheet of a window
        # left open behind it.
        sheet = {"role": "sheet", "name": "Sheet sales_q3", "box": (0, 150, 1280, 500), "window_title": "sales.xlsx - Calc"}
        slide = {"role": "paragraph", "name": "Click to add Text", "box": (400, 360, 300, 60),
                 "window_title": "Untitled 1 - Impress"}
        windows = [{"title": "sales.xlsx - Calc", "box": (0, 0, 1280, 720)},
                   {"title": "Untitled 1 - Impress", "box": (0, 0, 1280, 720)}]
        self.assertIs(el.at_point([sheet, slide], 529, 392, windows), slide)
        self.assertIs(el.at_point([sheet, slide], 100, 200, windows), None)
        self.assertIs(el.at_point([sheet, slide], 100, 200), sheet)  # without windows, as before

    def test_an_element_of_no_listed_window_is_kept(self):
        menu = {"role": "menuitem", "name": "Save", "box": (10, 10, 100, 20), "window_title": ""}
        windows = [{"title": "A", "box": (0, 0, 1280, 720)}]
        self.assertIs(el.at_point([menu], 20, 15, windows), menu)


if __name__ == "__main__":
    unittest.main()
