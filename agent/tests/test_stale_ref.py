"""A gone ref whose element is still on the screen under a new ref resolves to it (found live 2026-10-26: an approved
click after the review had walked away came back as "no longer on the screen")."""
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import elements, shown
from decurix_agent.computer.errors import ComputerError


class StaleRefs(unittest.TestCase):
    def test_a_gone_ref_resolves_to_the_same_named_element_once(self):
        screen = [{"ref": "e9", "role": "button", "name": "PROCEED TO PAYMENT", "path": "p1"},
                  {"ref": "e10", "role": "link", "name": "Back", "path": "p2"}]
        with mock.patch.object(shown, "named", lambda ref: ["button", "PROCEED TO PAYMENT"] if ref == "e3" else None):
            self.assertEqual(elements.find(screen, ref="e3")["ref"], "e9")
            with self.assertRaises(ComputerError):
                elements.find(screen, ref="e4")
        two = screen + [{"ref": "e11", "role": "button", "name": "PROCEED TO PAYMENT", "path": "p3"}]
        with mock.patch.object(shown, "named", lambda ref: ["button", "PROCEED TO PAYMENT"]):
            with self.assertRaises(ComputerError):
                elements.find(two, ref="e3")


if __name__ == "__main__":
    unittest.main()
