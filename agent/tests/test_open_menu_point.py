"""A point inside an open menu is on the menu, not on a window the menu hangs over."""

import unittest

from decurix_agent.computer import elements as el


VIEWER_BUTTON = {"role": "button", "name": "Name", "box": (200, 100, 300, 60), "window_title": "Excel", "states": []}
MENU_ITEM = {"role": "menuitem", "name": "Open...", "box": (240, 118, 120, 18), "window_title": "GIMP", "states": []}
WINDOWS = [{"title": "GIMP", "box": (0, 0, 230, 400)}, {"title": "Excel", "box": (150, 50, 800, 600)}]


class AnOpenMenuCovers(unittest.TestCase):
    def test_the_menu_item_is_found_not_the_window_behind(self):
        menu = [(235, 110, 200, 300)]
        self.assertIs(el.at_point([VIEWER_BUTTON, MENU_ITEM], 250, 126, WINDOWS, menu), MENU_ITEM)

    def test_a_menu_whose_items_are_not_listed_leaves_nothing_to_refuse(self):
        menu = [(235, 110, 200, 300)]
        self.assertIsNone(el.at_point([VIEWER_BUTTON], 250, 126, WINDOWS, menu))

    def test_without_a_menu_the_window_in_front_wins_as_before(self):
        # Excel's window is in front of GIMP's at that point.
        self.assertIs(el.at_point([VIEWER_BUTTON, MENU_ITEM], 250, 126, WINDOWS), VIEWER_BUTTON)


if __name__ == "__main__":
    unittest.main()
