"""
A click on a native drop-down (<select>) opens a list drawn outside the page, whose entries no look can read: found
live (2026-10-25), the engine then clicked them by looking - 15 such clicks averaged 32 s in one task. `gui click` on
one now answers with its options and `gui select`, and clicks nothing; a list that opens in the page is still clicked.
"""

import unittest
from unittest import mock

from decurix_agent.computer import cli
from decurix_agent.computer.errors import ComputerError

STATE = {"elements": [
    {"ref": "e31", "role": "combobox", "name": "Place of supply", "source": "cdp", "handle": {"backend_id": 7}},
    {"ref": "e32", "role": "combobox", "name": "Party A/c name", "source": "cdp", "handle": {"backend_id": 8}},
    {"ref": "e33", "role": "button", "name": "Accept", "source": "cdp", "handle": {"backend_id": 9}},
]}


class NativeDropdown(unittest.TestCase):
    def setUp(self):
        for p in (mock.patch.object(cli.core, "_load_state", lambda: STATE),
                  mock.patch.object(cli.cdp, "native_options",
                                    lambda e: ["Karnataka", "Kerala", "Goa"] if e["ref"] == "e31" else None)):
            p.start()
            self.addCleanup(p.stop)

    def test_a_native_dropdown_is_chosen_by_name_not_opened(self):
        with self.assertRaises(ComputerError) as caught:
            cli._refuse_native_dropdown("e31")
        text = str(caught.exception)
        self.assertIn("Not clicked", text)
        self.assertIn('gui select e31 "<option>"', text)
        self.assertIn('"Kerala"', text)

    def test_a_list_in_the_page_and_other_controls_are_clicked(self):
        cli._refuse_native_dropdown("e32")   # a combobox that is no <select>: its list opens in the page
        cli._refuse_native_dropdown("e33")   # a button
        cli._refuse_native_dropdown("e99")   # a ref the last look does not have

    def test_a_look_that_fails_leaves_the_click(self):
        with mock.patch.object(cli.cdp, "native_options", side_effect=RuntimeError("no page")):
            cli._refuse_native_dropdown("e31")


if __name__ == "__main__":
    unittest.main()
