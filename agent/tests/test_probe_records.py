"""
Finding AY (2026-10-20, P2 on a portal): the worker saved a test order (SO-5102, Cafe Bloom, customer PO "TEST",
one line, 149.00) "while checking how the site saves orders", in a portal that cannot delete orders. A form holding a
placeholder is never sent; a test record saved anyway is reported at the top as wrong in the application.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import long_task
from decurix_agent.computer import cdp, core

LIVE_APPROVAL = ("I would delete the wrong extra test order in the portal: SO-5102 for Cafe Bloom (PO TEST ...) because "
                 "I created it while checking how the site saves orders. Deleting it cannot be undone.")


class APlaceholderIsNeverSent(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        patch = mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "form-check.json")
        patch.start()
        self.addCleanup(patch.stop)
        self.save = {"ref": "e2081", "name": "Save order", "path": "/form/button", "source": "cdp"}

    def check(self, fields, checked=False):
        with mock.patch.object(cdp, "submit_form_fields", return_value={"form": "New order", "fields": fields}):
            return core._form_check(self.save, checked=checked)

    def test_the_live_test_order_is_refused_even_checked(self):
        fields = [["Customer", "Cafe Bloom"], ["Customer PO number", "TEST"], ["Product line 1", "MUG-12 - Ceramic mug 12oz"],
                  ["Quantity line 1", "1"], ["Unit price line 1", "149"]]
        for checked in (False, True):
            said = self.check(fields, checked=checked)
            self.assertIn("Not pressed", said)
            self.assertIn("Customer PO number: TEST", said)
            self.assertIn("Never save, submit or post a record to find out how an application behaves", said)

    def test_placeholders_and_real_values(self):
        self.assertEqual(core.placeholder_fields([["PO", "test"], ["Name", "dummy"], ["Note", "xxx"], ["Reference", "123"],
                                                   ["Customer PO number", "abc"], ["Remarks", "asdf"]]),
                         ["PO: test", "Name: dummy", "Note: xxx", "Reference: 123", "Customer PO number: abc",
                          "Remarks: asdf"])
        self.assertEqual(core.placeholder_fields([["Quantity line 1", "123"], ["Customer PO number", "WA-1810"],
                                                   ["Customer", "Test Kitchen Ltd"], ["Unit price", "149"]]), [])

    def test_a_small_probe_form_is_refused_and_a_small_real_one_sent(self):
        self.assertIn("Not pressed", self.check([["Customer PO number", "TEST"]]))
        self.assertEqual(self.check([["Search", "Cafe Bloom"]]), "")


class ATestRecordSavedAnywayIsReportedAtTheTop(base.LongRunBase):
    def test_the_live_approval_puts_it_in_the_report(self):
        self.assertTrue(long_task.PROBE_RECORD.search(LIVE_APPROVAL))
        self.assertIsNone(long_task.PROBE_RECORD.search("Should I hold the order for Cafe Bloom that I created?"))
        self.engine.asks["s1-register"] = LIVE_APPROVAL
        long_task.perform(base.LONG, None)
        data = long_task.ledger(self.run_id())
        self.assertTrue(any("a test record was saved in the application" in w for w in data.get("wrong_in_app", [])))

    def test_the_stage_rule_says_a_test_record_is_wrong_in_the_application(self):
        self.assertIn("A record the request did not ask for (a test or a try) is wrong in the application too",
                      long_task.APP_ENTRY_RULE)


if __name__ == "__main__":
    unittest.main()
