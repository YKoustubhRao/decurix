"""
A taught task whose document comes with the request: code declares the record it makes from what the document gave,
before anything is typed (skill_task._declare_taught_record), so the save is compared with the document and read back
whoever types it; with every value read and the record declared, the run replays (the model on call) instead of
handing the whole task to the model, and while still being learned it puts its plan to the person first. Found live
(2026-11-08, ERPNext bills): left to the model, each bill cost 48-98 turns and 1.3-4.2M tokens, the model never
declared its record, and so no run could ever count towards trust.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_taught_record.py
"""

import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import guidance, skill_task, state
from decurix_agent.computer import skills

BILL = ("TAX INVOICE  Northwind Traders Ltd  Invoice No: NW-2026-118  Date: 03-Oct-2026\n"
        "1  Copier paper A4   20  x 250.00   5,000.00")


def field_step(n, role, name, param=None, kind="type"):
    step = {"n": n, "id": f"s{n}", "kind": kind, "target": {"role": role, "name": name, "path": f'{role} "{name}"'},
            "summary": f"{kind} {name}"}
    if param:
        step["param"] = param
    return step


def bill_skill():
    """A form taught once: an address, a party picked from a list, the bill's number and date, one line."""

    steps = [field_step(1, None, "", "url"), field_step(2, "combobox", "Supplier", "supplier"),
             field_step(3, "textbox", "Supplier Invoice No", "invoice_no"),
             field_step(4, "textbox", "Supplier Invoice Date", "invoice_date"),
             field_step(5, "combobox", "Item", "item"), field_step(6, "textbox", "Qty", "qty"),
             field_step(7, "button", "Save", kind="click")]
    params = [
        {"name": "url", "label": "Typed text", "default": "erp.example/app/bill/new", "example": "erp.example/app/bill/new",
         "varies": False, "step": 1},
        {"name": "supplier", "label": "Supplier", "default": None, "example": "Acme", "varies": True, "step": 2},
        {"name": "invoice_no", "label": "Supplier Invoice No", "default": None, "example": "A-1", "varies": True, "step": 3},
        {"name": "invoice_date", "label": "Supplier Invoice Date", "default": None, "example": "01-10-2026",
         "varies": True, "step": 4},
        {"name": "item", "label": "Item", "default": None, "example": "Paper", "varies": True, "step": 5},
        {"name": "qty", "label": "Qty", "default": None, "example": "1", "varies": True, "step": 6},
    ]
    return {"name": "enter-bill", "title": "Enter a bill", "status": skills.READY, "steps": steps, "params": params,
            "loops": [], "experience": {}, "approval": None}


def filled_values():
    return {"url": "erp.example/app/bill/new", "supplier": "Northwind Traders Ltd", "invoice_no": "NW-2026-118",
            "invoice_date": "03-10-2026", "item": "Copier paper A4", "qty": "20"}


class TheTaughtRecord(unittest.TestCase):

    def setUp(self):
        self.declared = []
        patches = [
            mock.patch.object(skill_task.task_context, "current_files", return_value=["/home/headless/work/bill.pdf"]),
            mock.patch.object(skill_task.binding, "referenced_files", return_value=[]),
            mock.patch.object(skill_task.records, "_source_text", return_value=BILL),
            mock.patch.object(skill_task.records, "declare",
                              side_effect=lambda key, source, fields, **kw: self.declared.append((key, source, fields))
                              or ({"key": key}, [f"Record {key} declared"])),
            mock.patch.object(skill_task.progress, "emit"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def run_for(self, values=None):
        return {"skill": bill_skill(), "values": values or filled_values(), "context": "Enter this bill: ~/work/bill.pdf",
                "start": 1}

    def test_the_record_is_declared_from_its_document_by_its_number(self):
        run = self.run_for()
        self.assertEqual(skill_task._declare_taught_record(run), "NW-2026-118")
        key, source, fields = self.declared[0]
        self.assertEqual((key, source), ("NW-2026-118", "/home/headless/work/bill.pdf"))
        self.assertEqual(fields["Supplier Invoice No"], "NW-2026-118")
        self.assertEqual(fields["Supplier"], "Northwind Traders Ltd")
        self.assertNotIn("Typed text", fields)  # an address the demonstration kept, no fact of the document

    def test_a_date_is_declared_as_the_document_writes_it_when_the_applications_order_is_known(self):
        with mock.patch.object(skill_task.task_context, "app_date_order", return_value="day"):
            skill_task._declare_taught_record(self.run_for())
        self.assertEqual(self.declared[0][2]["Supplier Invoice Date"], "03-Oct-2026")

    def test_an_ambiguous_date_is_left_out_when_nobody_said_the_applications_order(self):
        """Found live (2026-11-08): declared, the save waited on a person for which day "11-10-2026" is, and the model
        retyped the date as "2026-10-11", which the application took as 20-10-2011."""

        with mock.patch.object(skill_task.task_context, "app_date_order", return_value=None):
            skill_task._declare_taught_record(self.run_for())
        self.assertNotIn("Supplier Invoice Date", self.declared[0][2])

    def test_a_document_named_in_the_requests_words_is_its_source(self):
        """Found live (2026-11-08): "Enter this bill ...: ~/work/BILL_14.pdf" brought no file, and nothing was declared."""

        with mock.patch.object(skill_task.task_context, "current_files", return_value=[]),                 mock.patch.object(skill_task.binding, "referenced_files", return_value=["/home/headless/work/BILL_14.pdf"]):
            self.assertEqual(skill_task._declare_taught_record(self.run_for()), "NW-2026-118")
        self.assertEqual(self.declared[0][1], "/home/headless/work/BILL_14.pdf")

    def test_a_value_the_document_does_not_hold_is_left_out_not_declared(self):
        values = dict(filled_values(), supplier="Someone Else Pvt Ltd")
        skill_task._declare_taught_record(self.run_for(values))
        self.assertNotIn("Supplier", self.declared[0][2])

    def test_a_label_given_two_different_values_is_left_out(self):
        skill = bill_skill()
        skill["steps"].append(field_step(8, "textbox", "Supplier Invoice Date", "invoice_date_2"))
        skill["params"].append({"name": "invoice_date_2", "label": "Supplier Invoice Date", "default": None,
                                "example": "01-10-2026", "varies": True, "step": 8})
        run = {"skill": skill, "values": dict(filled_values(), invoice_date_2="04-10-2026"), "context": "", "start": 1}
        self.assertNotIn("Supplier Invoice Date", skill_task._taught_fields(run))

    def test_without_a_number_or_with_several_documents_nothing_is_declared(self):
        skill = bill_skill()
        skill["steps"][2]["target"]["name"] = "Vendor"
        self.assertEqual(skill_task._declare_taught_record({"skill": skill, "values": filled_values(), "start": 1}), "")
        with mock.patch.object(skill_task.task_context, "current_files", return_value=["/a.pdf", "/b.pdf"]):
            self.assertEqual(skill_task._declare_taught_record(self.run_for()), "")
        self.assertEqual(self.declared, [])

    def test_a_refused_declaration_changes_nothing(self):
        with mock.patch.object(skill_task.records, "declare", side_effect=skill_task.ComputerError("not declared")):
            run = self.run_for()
            self.assertEqual(skill_task._declare_taught_record(run), "")
            self.assertEqual(run["declared"], "")


class ARunWithItsDocumentInHand(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT
        skills.ROOT = Path(self.tmp.name)
        state.paused_skill = None
        guidance.end()
        self.asked, self.by_model, self.executed = [], [], []
        patches = [
            mock.patch.object(skill_task, "ask_for_query", lambda text, kind="query", engine=None, **kw: self.asked.append(text)),
            mock.patch.object(skill_task, "write_final_reply", lambda *a, **kw: None),
            mock.patch.object(skill_task, "_by_model", lambda run, **kw: self.by_model.append(kw)),
            mock.patch.object(skill_task, "_execute", lambda run: self.executed.append(run)),
            mock.patch.object(skill_task, "_engine_can_drive", return_value=True),
            mock.patch.object(skill_task.learning, "start_checks", return_value=[]),
            mock.patch.object(skill_task.task_context, "current_files", return_value=["/home/headless/work/bill.pdf"]),
            mock.patch.object(skill_task.binding, "referenced_files", return_value=[]),
            mock.patch.object(skill_task.records, "_source_text", return_value=BILL),
            mock.patch.object(skill_task.records, "declare", lambda key, source, fields, **kw: ({"key": key}, [])),
            mock.patch.object(skill_task.progress, "emit"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def tearDown(self):
        skills.ROOT = self.saved
        state.paused_skill = None
        guidance.end()
        self.tmp.cleanup()

    def run_for(self, values):
        skill = bill_skill()
        return {"skill": skill, "values": values, "start": 1, "checked": False, "patches": [], "asked": 0, "stuck": 0,
                "ladder": skill_task._ladder(skill), "asked_ids": [], "context": "Enter this bill: ~/work/bill.pdf",
                "began": time.time(), "filled": True}

    def test_every_value_read_and_the_record_declared_it_replays_after_the_plan(self):
        run = self.run_for(filled_values())
        skill_task._prepare(run)
        self.assertEqual(run["declared"], "NW-2026-118")
        self.assertEqual((self.by_model, self.executed), ([], []))
        self.assertIn("NW-2026-118", self.asked[0])  # the plan, with the record as declared
        self.assertEqual(state.paused_skill["kind"], "plan")

        paused, state.paused_skill = state.paused_skill, None
        skill_task._run_reply(paused, "yes", None)
        self.assertEqual(len(self.executed), 1)
        self.assertEqual(self.by_model, [])
        self.assertFalse(run.get("asked_person"))  # a yes to the plan is no help: the run can still count

    def test_a_bill_of_fewer_lines_than_the_demonstration_still_replays(self):
        """Found live (2026-11-08): a one-line bill was handed to the model, the demonstration's second line having no
        values - a row the document does not have is no value to find."""

        skill = bill_skill()
        save = skill["steps"].pop()
        skill["steps"] += [field_step(7, "combobox", "Item", "item_2"), field_step(8, "textbox", "Qty", "qty_2")]
        save["n"] = 9
        skill["steps"].append(save)
        skill["params"] += [{"name": "item_2", "label": "Item", "default": None, "example": "Ink", "varies": True, "step": 7},
                            {"name": "qty_2", "label": "Qty", "default": None, "example": "2", "varies": True, "step": 8}]
        run = self.run_for(filled_values())
        run["skill"], run["ladder"] = skill, skill_task._ladder(skill)
        run["rows"] = {"count": 1, "names": [["item", "qty"]]}
        skill_task._prepare(run)
        self.assertEqual(run["declared"], "NW-2026-118")
        self.assertEqual(self.by_model, [])
        self.assertEqual(state.paused_skill["kind"], "plan")

    def test_a_value_the_documents_do_not_give_leaves_it_to_the_model(self):
        run = self.run_for(dict(filled_values(), item=""))
        skill_task._prepare(run)
        self.assertEqual(len(self.by_model), 1)
        self.assertEqual(self.executed, [])

    def test_a_change_to_the_plan_hands_it_to_the_model_with_the_declared_record(self):
        run = self.run_for(filled_values())
        skill_task._prepare(run)
        paused, state.paused_skill = state.paused_skill, None
        with mock.patch.object(skill_task.approval, "read_choice",
                               return_value={"choice": "changed", "instruction": "qty is 25"}):
            skill_task._run_reply(paused, "go ahead but qty is 25", None)
        self.assertEqual(self.executed, [])
        self.assertIn("declared again with their value", self.by_model[0]["note"])


if __name__ == "__main__":
    unittest.main()


class ACheckThatCouldNotBeMade(unittest.TestCase):
    """A final check the provider failed (a 504) is asked once more, then put to the person: never a finding that
    sends the skill under the model's eye for good. Found live (2026-11-08, ERPNext bill 11)."""

    def setUp(self):
        self.supervised, self.paused, self.by_model = [], [], []
        patches = [
            mock.patch.object(skill_task, "CHECK_RETRY_SECONDS", 0),
            mock.patch.object(skill_task, "_supervise", lambda run, why: self.supervised.append(why)),
            mock.patch.object(skill_task, "_pause", lambda run, kind, text, **kw: self.paused.append(kind)),
            mock.patch.object(skill_task, "_by_model", lambda run, **kw: self.by_model.append(kw)),
            mock.patch.object(skill_task, "_engine_can_drive", return_value=True),
            mock.patch.object(skill_task.verify, "available", return_value=True),
            mock.patch.object(skill_task, "_spot", return_value=False),
            mock.patch.object(skill_task.progress, "emit"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_two_failed_checks_ask_the_person_and_mark_nothing(self):
        failed = {"verdict": skill_task.verify.UNSURE, "confidence": 0.0, "seen": "", "why": "the check could not be made (504)",
                  "error": "504"}
        with mock.patch.object(skill_task, "_judge", return_value=failed) as judge:
            skill_task._finish({"skill": bill_skill(), "values": {}}, {"outcome": "completed_unverified", "text": "ran"})
        self.assertEqual(judge.call_count, 2)
        self.assertEqual((self.supervised, self.by_model, self.paused), ([], [], ["confirm"]))
