"""
A run done once per document declares each document's record from that document (skill_task._declare_document_records),
and a run is "helped" only when a person stepped in or what it had reported done was reworked (final_gate._helped).
Found live (2026-11-09): a month of bills declared no record, so nothing was read back and no run counted; and every
gathered month counted as helped, because the gather, the replay and the closing check made more than one attempt.
"""

import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import final_gate, skill_task
from decurix_agent.computer import records

BILLS = {
    "1053 Pai Textiles.txt": "TAX INVOICE  Invoice No : 1053  Date 03-12-2026\nCustomer: Pai Textiles\nTaxable 30,000.00",
    "1054 Hegde Stores.txt": "TAX INVOICE  Invoice No : 1054  Date 09-12-2026\nCustomer: Hegde Stores\nTaxable 12,500.00",
    "1055 Rao Clinic.txt": "TAX INVOICE  Invoice No : 1055  Date 15-12-2026\nCustomer: Rao Clinic\nTaxable 8,000.00",
}


def month_skill():
    steps = [{"n": 1, "id": "s1", "kind": "type", "param": "voucher_number", "target": {"role": "textbox", "name": "Voucher No."}},
             {"n": 2, "id": "s2", "kind": "select", "param": "party_name", "target": {"role": "combobox", "name": "Party A/c name"}},
             {"n": 3, "id": "s3", "kind": "type", "param": "taxable_amount", "target": {"role": "textbox", "name": "Sales amount"}},
             {"n": 4, "id": "s4", "kind": "select", "param": "sales_ledger", "target": {"role": "combobox", "name": "Sales ledger"}},
             {"n": 5, "id": "s5", "kind": "click", "target": {"role": "button", "name": "Save voucher"}}]
    params = [{"name": "voucher_number", "label": "voucher no.", "list": True, "varies": True, "step": 1},
              {"name": "party_name", "label": "party a/c name", "list": True, "varies": True, "step": 2},
              {"name": "taxable_amount", "label": "sales amount", "list": True, "varies": True, "step": 3},
              {"name": "sales_ledger", "label": "sales ledger", "list": False, "varies": False, "step": 4,
               "default": "Sales@18%"}]
    return {"name": "enter-month", "steps": steps, "params": params,
            "loops": [{"id": "l1", "enabled": True, "blocks": [["s1", "s2", "s3", "s4", "s5"]]}]}


class DocumentRecords(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.docs = []
        for name, text in BILLS.items():
            path = Path(self.tmp.name) / name
            path.write_text(text, encoding="utf-8")
            self.docs.append(str(path))
        for p in (mock.patch.object(records, "LEDGER_DIR", Path(self.tmp.name) / "ledger"),
                  mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"),
                  mock.patch("decurix_agent.task_context.current_request", lambda: "Enter the month's bills"),
                  mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: ""),
                  mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []),
                  mock.patch.object(records, "_key_on_an_app_page", lambda key, source, task_id=None: ""),
                  mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day"),
                  mock.patch.object(skill_task.progress, "emit", lambda *a, **k: None)):
            p.start()
            self.addCleanup(p.stop)

    def run_for(self, left_out=()):
        values = {"voucher_number": "1053; 1054; 1055", "party_name": "Pai Textiles; Hegde Stores; Rao Clinic",
                  "taxable_amount": "30000.00; 12500.00; 8000.00", "sales_ledger": "Sales@18%"}
        if left_out:
            values = {"voucher_number": "1053; 1055", "party_name": "Pai Textiles; Rao Clinic",
                      "taxable_amount": "30000.00; 8000.00", "sales_ledger": "Sales@18%"}
        return {"skill": month_skill(), "values": values, "context": "Enter the month's bills", "documents": self.docs,
                "left_out": [{"document": d, "kind": "skip", "why": "cancelled"} for d in left_out], "by_hand": []}

    def test_each_document_declares_its_own_record_from_its_own_file(self):
        keys = skill_task._declare_document_records(self.run_for())
        self.assertEqual(keys, ["1053", "1054", "1055"])
        declared = {r["key"]: r for r in records.load()}
        self.assertEqual(declared["1054"]["source"], self.docs[1])
        self.assertEqual(declared["1054"]["fields"]["Party A/c name"]["intent"], "Hegde Stores")
        self.assertEqual(declared["1054"]["fields"]["Sales amount"]["intent"], "12500.00")
        # A value no document writes (the ledger the person picks) is not declared from a document.
        self.assertNotIn("Sales ledger", declared["1054"]["fields"])

    def test_a_document_left_out_takes_no_record_and_the_items_stay_aligned(self):
        keys = skill_task._declare_document_records(self.run_for(left_out=(self.docs[1],)))
        self.assertEqual(keys, ["1053", "1055"])
        self.assertEqual({r["key"]: r["source"] for r in records.load()}["1055"], self.docs[2])

    def test_a_value_its_document_does_not_write_is_left_out_of_that_record(self):
        run = self.run_for()
        run["values"]["taxable_amount"] = "30000.00; 99999.00; 8000.00"
        skill_task._declare_document_records(run)
        self.assertNotIn("Sales amount", {r["key"]: r for r in records.load()}["1054"]["fields"])

    def test_each_record_is_found_by_its_own_number_on_the_form(self):
        skill_task._declare_document_records(self.run_for())
        self.assertEqual(records.open_record(records.load(), "Voucher No. 1054 Party Hegde Stores")["key"], "1054")


class Helped(unittest.TestCase):
    """Owner's decision 2026-11-09: help is a person stepping in, or rework of what the run had reported done."""

    def test_a_send_back_that_saved_nothing_is_no_help(self):
        run = {"retried_at": time.time()}
        with mock.patch.object(records, "load", return_value=[{"key": "1053", "saved_at": run["retried_at"] - 60}]), \
                mock.patch("decurix_agent.computer.evidence._task_events", return_value=[]):
            self.assertFalse(final_gate._helped(run, {"mode": "model"}))

    def test_a_send_back_that_saved_a_record_again_is_help(self):
        run = {"retried_at": time.time() - 30}
        with mock.patch.object(records, "load", return_value=[{"key": "1053", "saved_at": time.time()}]):
            self.assertTrue(final_gate._helped(run, {"mode": "model"}))

    def test_a_press_the_application_answered_as_saved_after_the_send_back_is_help(self):
        run = {"retried_at": time.time() - 30}
        later = time.strftime("%Y-%m-%dT%H:%M:%S+0000", time.gmtime(time.time()))
        events = [{"kind": "click", "outcome": "done", "at": later, "change": "\"Voucher saved.\" appeared"}]
        with mock.patch.object(records, "load", return_value=[]), \
                mock.patch("decurix_agent.computer.evidence._task_events", return_value=events), \
                mock.patch("decurix_agent.computer.core._saved", return_value=True):
            self.assertTrue(final_gate._helped(run, {"mode": "model"}))

    def test_a_person_and_a_takeover_are_help_and_more_attempts_alone_are_not(self):
        self.assertTrue(final_gate._helped({"asked_person": True}, {"mode": "model"}))
        self.assertTrue(final_gate._helped({}, {"mode": "takeover"}))
        self.assertFalse(final_gate._helped({"attempts": 3}, {"mode": "model"}))


if __name__ == "__main__":
    unittest.main()


class AmbiguousDates(unittest.TestCase):
    """A digit date that reads two ways is never declared while the application's date order is unknown - whatever the
    document writes (found live 2026-11-09: a bill writing the same "04-11-2026" as the form was declared, and every save
    of the month waited on "which day is it" until the case ended)."""

    def run_with(self, doc_text, order):
        run = {"context": ""}
        with mock.patch.object(records, "_source_text", lambda source: doc_text), \
                mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: order):
            return skill_task._declarable(run, {"Date": "04-11-2026", "Voucher No.": "1048"}, "bill.docx")

    def test_the_same_digits_in_the_document_are_not_declared_without_an_order(self):
        self.assertEqual(self.run_with("Invoice No : 1048  Invoice Date : 04-11-2026", None), {"Voucher No.": "1048"})

    def test_with_the_order_known_the_date_is_declared(self):
        self.assertIn("Date", self.run_with("Invoice No : 1048  Invoice Date : 04-11-2026", "day"))
