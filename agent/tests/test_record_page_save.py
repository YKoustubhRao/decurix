"""
A save a web application answers only by moving from the new record's page to the saved record's own page is a save:
the declared record is read back after it (core._became_a_record). Found live (2026-11-08, ERPNext): every bill's
save answered "page is now .../PINV-26-00011" with no "saved", so no bill was read back ("declared but never saved").

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_record_page_save.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import core

NEW = {"page_url": "http://erp.test/desk/purchase-invoice/new-purchase-invoice-tobukjtcgr"}


class ASaveThatMovesToTheRecord(unittest.TestCase):

    def test_new_form_to_the_records_page_is_a_save(self):
        self.assertTrue(core._became_a_record(NEW, 'active window is now "X - PINV-26-00011"; '
                                                   "page is now http://erp.test/desk/purchase-invoice/PINV-26-00011; 3 appeared"))
        self.assertTrue(core._became_a_record({"page_url": "http://books.test/v/new"}, "page is now http://books.test/v/1054"))

    def test_another_new_form_an_error_or_a_record_to_a_record_is_not(self):
        self.assertFalse(core._became_a_record(NEW, "page is now http://erp.test/desk/purchase-invoice/new-purchase-invoice-zz12"))
        self.assertFalse(core._became_a_record(NEW, "page is now http://erp.test/desk/purchase-invoice/PINV-26-00011; "
                                                    "error: Mandatory fields required"))
        self.assertFalse(core._became_a_record({"page_url": "http://erp.test/desk/purchase-invoice/PINV-26-00010"},
                                               "page is now http://erp.test/desk/purchase-invoice/PINV-26-00011"))
        self.assertFalse(core._became_a_record({}, "page is now http://erp.test/desk/purchase-invoice/PINV-26-00011"))


if __name__ == "__main__":
    unittest.main()
