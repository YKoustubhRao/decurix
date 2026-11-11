"""
On a slow host a list is given longer to answer a search (selecting.list_answer_wait): found live (2026-11-08, load 24,
a look ~8 s), two readings of a list that had not yet answered read the same and every search was taken to have found
nothing, the right entry among them.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_slow_list_answer.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import selecting


class ASlowHost(unittest.TestCase):

    def test_the_wait_follows_how_long_a_look_takes(self):
        self.assertEqual(selecting.list_answer_wait(0.2), selecting.MIN_LIST_ANSWER)
        self.assertEqual(selecting.list_answer_wait(8.0), selecting.SLOW_LOOKS * 8.0)
        self.assertEqual(selecting.list_answer_wait(-1), selecting.MIN_LIST_ANSWER)


if __name__ == "__main__":
    unittest.main()


class AnEntryByItsOwnName(unittest.TestCase):
    """An entry the list writes as its name, then its name again with its group, is the entry its name names."""

    def test_the_entry_whose_own_name_is_the_value_is_picked(self):
        entry = {"name": "Sharma & Co Chartered Accountants Sharma & Co Chartered Accountants, All Supplier Groups"}
        self.assertIs(selecting._pick([entry], "Sharma & Co Chartered Accountants"), entry)

    def test_a_longer_name_is_still_another_entry(self):
        entry = {"name": "Statutory Audit & GST Compliance Statutory Audit & GST Compliance, Services"}
        self.assertIsNone(selecting._pick([entry], "Statutory Audit"))
