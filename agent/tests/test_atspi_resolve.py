"""An AT-SPI element is found again by its index path only when the accessible there is still the one looked at: the
same role, its own name and its process. A committing or gated control that is no longer there is not clicked where it
was drawn (review 2026-11-02: a stale element pressed another widget)."""
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import atspi, core
from decurix_agent.computer.errors import ComputerError


class Node:
    def __init__(self, role, name="", children=(), pid=100):
        self.role, self.name, self.children, self.pid = role, name, list(children), pid

    def get_role_name(self): return self.role
    def get_name(self): return self.name
    def get_child_at_index(self, index): return self.children[index] if index < len(self.children) else None
    def get_process_id(self): return self.pid


class Lib:
    def __init__(self, desktop):
        self.desktop = desktop

    def get_desktop(self, n):
        return self.desktop


def element(name="Submit", role="push button", pid=100):
    return {"role": "button", "name": name, "source": "atspi", "box": [10, 10, 80, 30], "window_title": "Order",
            "handle": {"indices": [0, 0], "raw_role": role, "own_name": name, "pid": pid, "actions": ["click"]}}


class Resolve(unittest.TestCase):
    def resolve(self, desktop, el):
        with mock.patch.object(atspi, "_lib", lambda: Lib(desktop)):
            return atspi.resolve(el)

    def test_the_same_widget_is_found(self):
        button = Node("push button", "Submit")
        self.assertIs(self.resolve(Node("desktop", children=[Node("frame", "Order", [button])]), element()), button)

    def test_another_widget_at_the_same_place_is_not(self):
        desktop = Node("desktop", children=[Node("frame", "Order", [Node("push button", "Delete")])])
        self.assertIsNone(self.resolve(desktop, element()))
        other_app = Node("desktop", children=[Node("frame", "Order", [Node("push button", "Submit", pid=200)])])
        self.assertIsNone(self.resolve(other_app, element()))


class ACommittingPressIsNotMadeBlind(unittest.TestCase):
    def test_a_submit_no_longer_there_is_not_clicked_where_it_was(self):
        with mock.patch.object(atspi, "default_action", return_value=None), \
                mock.patch.object(atspi, "resolve", return_value=None), \
                mock.patch.object(core, "_focus_window", lambda e: None), \
                mock.patch.object(core.xinput, "click") as click:
            with self.assertRaises(ComputerError):
                core._perform("click", element(), None, None, [], None, None, None, False)
            click.assert_not_called()
            # A plain control is still pressed where it is drawn.
            core._perform("click", element(name="Next"), None, None, [], None, None, None, False)
            click.assert_called_once()


if __name__ == "__main__":
    unittest.main()
