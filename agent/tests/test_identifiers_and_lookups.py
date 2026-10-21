"""
Round 5 P6 (2026-10-20).
BU3: the cross-check listed "Harbour Hotels Group refund email file" - 1015_E02.eml in two workbooks, 1015_E02 in the
memo - as "Figures that do not agree", and a finished run became not done. A name or a code is not a figure.
(CB's pending-lookup check was deleted with the harness audit, 2026-10-21: finding the record is the request's and
the operator's rule, never a word list in code.)
"""

import tempfile
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task

P6 = ("Can you triage the support inbox? The 22 emails are in P6_support_inbox/inbox (.eml). Order details are in the "
      "Back Office portal (http://host.docker.internal:8701, Orders).\n1. support_triage_tracker.xlsx: one row per "
      "email - Email, From, Order No (our SO number from the portal), Customer, Action, Notes.")


class Identifiers(unittest.TestCase):
    def test_names_and_codes_are_not_figures(self):
        for value in ("1015_E02.eml", "1015_E02", "'1015_E02.eml'", "SO-5102", "HH/PO/7745", "INV-2026-001",
                      "refunds_over_5000_for_approval.xlsx", "EMP-2041"):
            self.assertTrue(long_task.identifier_like(value), value)
        for value in ("Rs 480,000", "-4.5 lakh", "4.5L", "12%", "02/11/2026", "480000", "1,078,139.62", "+5.3 lakh",
                      "(4,48,679)", "Rs. 35,960"):
            self.assertFalse(long_task.identifier_like(value, True), value)

    def test_the_live_report_has_no_disagreement(self):
        with tempfile.TemporaryDirectory() as tmp:
            files = {"support_triage_tracker.txt": "A3='1015_E02.eml' Harbour Hotels Group refund 35,960",
                     "refunds_over_5000_for_approval.txt": "A2='1015_E02.eml' Harbour Hotels Group 35,960",
                     "manager_summary.txt": "Harbour's refund email 1015_E02 (SO-5102) asks for Rs 35,690."}
            outputs = []
            for name, text in files.items():
                (Path(tmp) / name).write_text(text, encoding="utf-8")
                outputs.append(("s", str(Path(tmp) / name)))
            same_file = {"figure": "Harbour Hotels Group refund email file", "places": [
                {"file": "support_triage_tracker.txt", "text": "A3='1015_E02.eml'", "value": "1015_E02.eml"},
                {"file": "refunds_over_5000_for_approval.txt", "text": "A2='1015_E02.eml'", "value": "1015_E02.eml"},
                {"file": "manager_summary.txt", "text": "refund email 1015_E02 (SO-5102)", "value": "1015_E02"}]}
            amount = {"figure": "Harbour refund", "places": [
                {"file": "support_triage_tracker.txt", "text": "Harbour Hotels Group refund 35,960", "value": "35,960"},
                {"file": "manager_summary.txt", "text": "asks for Rs 35,690", "value": "Rs 35,690"}]}
            found, _ = long_task.model_disagreements(outputs, ask=lambda s, p: {"figures": [same_file]})
            self.assertEqual(found, [])
            # An amount is still compared.
            found, _ = long_task.model_disagreements(outputs, ask=lambda s, p: {"figures": [same_file, amount]})
            self.assertEqual([f["figure"] for f in found], ["Harbour refund"])
        self.assertIn("never list one", long_task.CROSS_SYSTEM)


if __name__ == "__main__":
    unittest.main()
