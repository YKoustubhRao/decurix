"""
Keys go to the element's own window or nowhere (review 2026-11-03, D-27): a window that cannot be found, or that
cannot be told apart from another of a similar title, refuses rather than typing into whatever is in front.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import core
from decurix_agent.computer.errors import ComputerError

WINDOWS = [{"id": "1", "title": "Untitled 10 - Sheets"}, {"id": "2", "title": "Untitled 1 - Sheets (copy)"}]


class KeysGoToTheirWindow(unittest.TestCase):
    def focus(self, title, windows=WINDOWS):
        with mock.patch.object(core.screen, "windows", return_value=windows), \
                mock.patch.object(core.xinput, "activate") as activate:
            try:
                core._focus_window({"window_title": title})
            except ComputerError:
                return None
            return activate.call_args.args[0]

    def test_the_exact_window_is_brought_forward(self):
        self.assertEqual(self.focus("Untitled 10 - Sheets"), "1")

    def test_a_title_inside_two_windows_refuses(self):
        self.assertIsNone(self.focus("Untitled 1"))

    def test_a_window_that_is_gone_refuses(self):
        self.assertIsNone(self.focus("Another - Sheets"))

    def test_one_window_whose_title_holds_another_number_is_not_its_window(self):
        self.assertIsNone(self.focus("Invoice 104 - Sheets", windows=[{"id": "3", "title": "Invoice 1042 - Sheets"}]))

    def test_one_near_title_is_its_window(self):
        self.assertEqual(self.focus("Untitled 1 - Sheets (copy)*", windows=WINDOWS[1:]), "2")


if __name__ == "__main__":
    unittest.main()
