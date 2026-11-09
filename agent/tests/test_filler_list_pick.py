"""
A value chosen from the application's own list is not an invented filler (core._not_picked). Found live (2026-11-09):
a return's unit field set to the portal's own "NA" - the unit of a service, as the person chose in the demonstration -
was refused at every save as "a filler, not a value read from any source".
"""

import unittest

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import filler
from decurix_agent.computer import core

PICK = {"label": "UQC", "option": "NA", "taken": "", "offered": ["-- Select --", "NA", "NOS", "OTH"]}


class AListsOwnEntry(unittest.TestCase):
    def test_an_entry_the_list_offered_is_not_judged_as_filler(self):
        fields = [("UQC", "NA"), ("Description", "n/a")]
        self.assertEqual(core._not_picked(fields, picks=[PICK]), [("Description", "n/a")])
        self.assertEqual(filler.invented(core._not_picked(fields, picks=[PICK]), "File the return", ()),
                         [("Description", "n/a")])

    def test_the_entry_a_pick_took_counts_too_but_only_for_its_own_field(self):
        took = {"label": "Item", "option": "test kit", "taken": "Test", "offered": None}
        self.assertEqual(core._not_picked([("Item", "Test")], picks=[took]), [])
        self.assertEqual(core._not_picked([("Notes", "Test")], picks=[took]), [("Notes", "Test")])

    def test_a_value_no_list_offered_is_still_judged(self):
        self.assertEqual(core._not_picked([("UQC", "N/A")], picks=[dict(PICK, offered=["NOS", "OTH"])]),
                         [("UQC", "N/A")])


if __name__ == "__main__":
    unittest.main()
