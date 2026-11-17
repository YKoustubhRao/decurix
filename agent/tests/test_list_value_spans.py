"""
A list parameter's value (one entry per item, "a; b; c") is carried into a step's recorded words only when every item
gives the same value: found in the learning curves (2026-11-16), a month's closing check looked for a link named
"Ledger vouchers: Sales@18%; Sales@18%; Sales@18%; Sales@18%" - the joined list - and stopped every run there.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_list_value_spans.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import mimic


def skill():
    return {"params": [{"name": "sales_ledger", "example": "Sales@12%", "list": True},
                       {"name": "bill_month", "example": "October 2026"}]}


class AListValueInARecordedName(unittest.TestCase):

    def test_one_value_for_every_item_is_carried_in(self):
        spans = mimic.value_spans(skill(), {"sales_ledger": "Sales@18%; Sales@18%; Sales@18%", "bill_month": "December 2026"})
        self.assertIn(("Sales@12%", "Sales@18%"), [(s["old"], s["new"]) for s in spans])

    def test_items_that_differ_carry_nothing_in(self):
        spans = mimic.value_spans(skill(), {"sales_ledger": "Sales@18%; Sales@12%", "bill_month": "December 2026"})
        self.assertEqual([s["old"] for s in spans], ["October 2026"])


if __name__ == "__main__":
    unittest.main()
