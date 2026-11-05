"""A gone ref whose control is drawn anew in the same window and document resolves to it, once (found live 2026-10-26:
an approved click after the review had walked away came back as "no longer on the screen") - and only there: re-bound
by its role and name anywhere, an approved Submit landed on another document's Submit (review 2026-11-02)."""
import unittest

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import core, elements
from decurix_agent.computer.errors import ComputerError

PAY = {"ref": "e3", "role": "button", "name": "PROCEED TO PAYMENT", "path": "old", "group": "cdp:tab1",
       "window_title": "Return - Chromium"}


def gone(*elements_then, url="https://portal.test/return/7", now=None):
    """What core.observe keeps of refs no longer on the screen, and the address each window shows now."""

    previous = {"elements": list(elements_then), "groups": [{"id": "cdp:tab1", "url": url}]}
    return core._gone(previous, [], [{"id": "cdp:tab1", "url": now or url}])


class StaleRefs(unittest.TestCase):
    def test_a_gone_ref_resolves_to_the_same_control_in_the_same_window_once(self):
        screen = [{"ref": "e9", "role": "button", "name": "PROCEED TO PAYMENT", "path": "p1", "group": "cdp:tab1",
                   "window_title": "Return - Chromium"},
                  {"ref": "e10", "role": "link", "name": "Back", "path": "p2", "group": "cdp:tab1",
                   "window_title": "Return - Chromium"}]
        where = gone(PAY)
        self.assertEqual(elements.find(screen, ref="e3", gone=where)["ref"], "e9")
        with self.assertRaises(ComputerError):
            elements.find(screen, ref="e4", gone=where)
        two = screen + [dict(screen[0], ref="e11", path="p3")]
        with self.assertRaises(ComputerError):
            elements.find(two, ref="e3", gone=where)

    def test_not_in_another_window_or_another_document(self):
        other_window = [{"ref": "e9", "role": "button", "name": "PROCEED TO PAYMENT", "path": "p1", "group": "cdp:tab2",
                         "window_title": "Other return - Chromium"}]
        with self.assertRaises(ComputerError):
            elements.find(other_window, ref="e3", gone=gone(PAY))
        # The same tab and title, but another document at its address: not re-bound.
        same_tab = [dict(other_window[0], group="cdp:tab1", window_title="Return - Chromium")]
        with self.assertRaises(ComputerError):
            elements.find(same_tab, ref="e3", gone=gone(PAY, now="https://portal.test/return/8"))
        self.assertEqual(elements.find(same_tab, ref="e3", gone=gone(PAY))["ref"], "e9")

    def test_a_ref_whose_window_was_never_kept_is_not_re_bound(self):
        screen = [{"ref": "e9", "role": "button", "name": "PROCEED TO PAYMENT", "path": "p1", "group": "cdp:tab1",
                   "window_title": "Return - Chromium"}]
        with self.assertRaises(ComputerError):
            elements.find(screen, ref="e3")
        with self.assertRaises(ComputerError):
            elements.find(screen, ref="e3", gone={"refs": {"e3": {"role": "button", "name": "PROCEED TO PAYMENT"}}})

    def test_what_is_kept_follows_the_looks(self):
        where = gone(PAY)
        self.assertIn("e3", where["refs"])
        # Back on the screen under its own ref: no longer gone.
        again = core._gone({"elements": [], "groups": [], "gone": where}, [dict(PAY)], [])
        self.assertNotIn("e3", again["refs"])


if __name__ == "__main__":
    unittest.main()
