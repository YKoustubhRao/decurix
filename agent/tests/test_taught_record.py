"""
A taught task whose document comes with the request: code declares the record it makes from what the document gave,
before anything is typed (skill_task._declare_taught_record), so the save is compared with the document and read back
whoever types it; with every value read and the record declared, the run replays (the model on call) instead of
handing the whole task to the model, and while still being learned it puts its plan to the person first. Found live
(2026-11-08, ERPNext bills): left to the model, each bill cost 48-98 turns and 1.3-4.2M tokens, the model never
declared its record, and so no run could ever count towards trust.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_taught_record.py
"""

import json
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

    def test_a_value_the_documents_do_not_give_is_asked_about_alone_not_given_to_the_model(self):
        """Owner's ruling 2026-11-11 (was: the whole run went to the model): the one field is asked about, then the
        run replays. See ALeftField below."""

        run = self.run_for(dict(filled_values(), item=""))
        with mock.patch("decurix_agent.person_record.left_to_app", return_value=[]):
            skill_task._prepare(run)
        self.assertEqual((self.by_model, self.executed), ([], []))
        self.assertEqual(state.paused_skill["kind"], "lacking")
        self.assertIn('"Item"', self.asked[0])

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


class AModelServiceThatDoesNotAnswer(unittest.TestCase):
    """A request read against the taught tasks while the model service fails is not handed on as untaught: it ends,
    saying so, with nothing done (found live 2026-11-08: a 502 sent a taught bill to the generic operator)."""

    def test_the_request_ends_saying_why_and_nothing_runs(self):
        finals, engine = [], []
        with mock.patch.object(skill_task.request_reading, "available", return_value=True), \
                mock.patch.object(skill_task.request_reading, "read",
                                  side_effect=skill_task.request_reading.Unavailable("HTTP 502")), \
                mock.patch.object(skill_task, "write_final_reply", lambda text, **kw: finals.append((text, kw))), \
                mock.patch.object(skill_task, "_engine", lambda *a: engine.append(a)), \
                mock.patch.object(skill_task, "_begin"), mock.patch.object(skill_task.progress, "emit"):
            skill_task.perform_mimic("Enter this bill: ~/work/bill.pdf")
        self.assertEqual(engine, [])
        self.assertEqual(finals[0][1]["kind"], "error")
        self.assertIn("Nothing was done", finals[0][0])


class AnUnreadableReading(unittest.TestCase):
    """An empty answer to the reading of a request is asked once more with room (found live 2026-11-08: an empty answer
    sent a taught bill to the generic operator); twice unreadable, the recordings' own words decide, as before."""

    def test_asked_once_more_then_not_handed_on(self):
        from decurix_agent import request_reading
        calls = []

        def unreadable(system, content, max_tokens=0):
            calls.append(max_tokens)
            raise ValueError("the model did not answer in JSON: ''")
        with mock.patch.object(request_reading, "available", return_value=True), \
                mock.patch.object(request_reading, "_candidates", return_value=[{"name": "enter-bill"}]), \
                mock.patch.object(request_reading, "_describe", return_value="enter-bill"), \
                mock.patch.object(request_reading.lessons, "known", return_value=""), \
                mock.patch.object(request_reading.study, "_call", side_effect=unreadable):
            self.assertIsNone(request_reading.read("Enter this bill: ~/work/bill.pdf"))
        self.assertEqual(calls, [800, request_reading.READ_RETRY_TOKENS])


def due_date_skill():
    """bill_skill with a field the application fills itself once the supplier is picked (a due date), its step checked,
    and a later step whose check names it."""

    skill = bill_skill()
    due = field_step(0, "textbox", "Due Date", "due_date")
    due["expect"] = [{"type": "element", "param": "due_date", "name": "Due Date"}]
    skill["steps"].insert(2, due)
    skill["steps"][-1]["expect"] = [{"type": "window", "value": "Bill due {due_date}"},
                                    {"type": "window", "value": "Bill {invoice_no}"}]
    skill["params"].append({"name": "due_date", "label": "Due Date", "default": None, "example": "15-10-2026",
                            "varies": True, "step": 0})
    return skills.renumber(skill)


REPLY = "leave the Due Date as ERPNext fills it in"


class ALeftField(unittest.TestCase):
    """
    Owner's ruling 2026-11-11 (live: ERPNext bill 09, a bill with no due date). A value the documents lack that the
    person leaves as the application fills it in is a standing decision, not data: the replay leaves that field
    untouched, the run is not helped, and the decision is kept on the control plane for this skill's later runs. With
    no decision kept, the person is asked about that one field - never the whole run handed to the model.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT
        skills.ROOT = Path(self.tmp.name)
        state.paused_skill = None
        state.reply_judgement, state.reply_secret = "j_reply0001", False
        guidance.end()
        self.asked, self.by_model, self.replayed, self.finished, self.posted = [], [], [], [], []
        self.kept_rows = []
        from decurix_agent import person_record
        person_record.clear_cache()

        def replay_run(program, params=None, start=1, ladder=None):
            self.replayed.append((program, dict(params or {})))
            return self.outcome

        self.outcome = {"outcome": skill_task.replay.COMPLETED_UNVERIFIED, "text": "ran", "step": 9, "resume_from": None}
        patches = [
            mock.patch.object(skill_task, "ask_for_query", lambda text, kind="query", engine=None, **kw: self.asked.append(text)),
            mock.patch.object(skill_task, "write_final_reply", lambda *a, **kw: None),
            mock.patch.object(skill_task, "_by_model", lambda run, **kw: self.by_model.append(kw)),
            mock.patch.object(skill_task, "_finish", lambda run, result: self.finished.append(result)),
            mock.patch.object(skill_task.replay, "run", replay_run),
            mock.patch.object(skill_task.phase_note, "write"),
            mock.patch.object(skill_task, "_engine_can_drive", return_value=True),
            mock.patch.object(skill_task, "_reads_replies", return_value=True),
            mock.patch.object(skill_task.learning, "start_checks", return_value=[]),
            mock.patch.object(skill_task.task_context, "current_files", return_value=["/home/headless/work/bill.pdf"]),
            mock.patch.object(skill_task.task_context, "current", return_value="t7455c3b2178a35ee"),
            mock.patch.object(skill_task.binding, "referenced_files", return_value=[]),
            mock.patch.object(skill_task.records, "_source_text", return_value=BILL),
            mock.patch.object(skill_task.records, "declare", lambda key, source, fields, **kw: ({"key": key}, [])),
            mock.patch.object(skill_task.progress, "emit"),
            mock.patch("decurix_agent.messaging.post_learned", lambda *a, **kw: self.posted.append((a, kw))),
            # The control plane's learned rows, as it serves them (desktopLearnedList): what the test kept.
            mock.patch("decurix_agent.person_record._get",
                       lambda path, query, fresh=False: {"learned": list(self.kept_rows)}),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def tearDown(self):
        skills.ROOT = self.saved
        state.paused_skill = None
        state.reply_judgement, state.reply_secret = "", False
        guidance.end()
        self.tmp.cleanup()

    def run_for(self, skill=None):
        skill = skill or due_date_skill()
        return {"skill": skill, "values": filled_values(), "start": 1, "checked": False, "patches": [], "asked": 0,
                "stuck": 0, "ladder": skill_task._ladder(skill), "asked_ids": [], "began": time.time(), "filled": True,
                "context": "Enter this bill: ~/work/bill.pdf"}

    def reply(self, text, reading=None):
        paused, state.paused_skill = state.paused_skill, None
        if reading is None:
            skill_task._run_reply(paused, text, None)
        else:
            with mock.patch.object(skill_task.approval, "read_choice", return_value=reading):
                skill_task._run_reply(paused, text, None)

    def keep_as_the_control_plane_does(self):
        """The row the control plane keeps from what was posted, served back with its source judgement."""

        (kind, _skill, _version, _question, text), kw = self.posted[-1]
        self.kept_rows.append({"id": "j_kept0001", "kind": kind, "task_id": "t7455c3b2178a35ee",
                               "derived_from": kw["derived_from"], "text": text,
                               "source": {"actor": {"kind": "person"}, "text": REPLY, "asked": self.asked[0]}})
        from decurix_agent import person_record
        person_record.clear_cache()

    def test_a_kept_decision_replays_leaving_the_field_untouched_and_is_no_help(self):
        """(a) A declared apprentice run with one value missing and a kept decision reaches the replay."""

        self.kept_rows.append({"id": "j_kept0001", "kind": "field_answer", "derived_from": "j_earlier01",
                               "text": json.dumps({"host": "", "label": "Due Date", "left_to_app": True, "option": REPLY,
                                                   "param": "due_date", "skill": "enter-bill"}),
                               "source": {"actor": {"kind": "person"}, "text": REPLY,
                                          "asked": "The documents for this run do not give Due Date. "
                                                   + skill_task._leave_sentence("Due Date") + " - or reply with it."}})
        run = self.run_for()
        skill_task._prepare(run)
        self.assertEqual(self.by_model, [])
        self.assertEqual(state.paused_skill["kind"], "plan")  # (an apprentice's plan first, as always)
        self.assertIn("Due Date: not in the documents - left as the application fills it in", self.asked[0])
        self.reply("yes")
        program, params = self.replayed[0]
        self.assertFalse([s for s in program["steps"] if s.get("param") == "due_date"])
        self.assertNotIn("due_date", params)
        self.assertNotIn("Bill due {due_date}", str(program["steps"]))  # nor looked for by a later step's check
        self.assertIn("Bill {invoice_no}", str(program["steps"]))
        self.assertEqual(self.by_model, [])
        self.assertFalse(skill_task.final_gate._helped(run, {"mode": "replay"}))

    def test_a_kept_row_of_another_question_or_a_pick_is_no_decision(self):
        """Only a row whose question offered leaving that very field counts; a pick's field answer never does."""

        self.kept_rows.append({"id": "j_kept0001", "kind": "field_answer", "derived_from": "j_earlier01",
                               "text": json.dumps({"host": "", "label": "Due Date", "left_to_app": True, "option": REPLY,
                                                   "param": "due_date", "skill": "enter-bill"}),
                               "source": {"actor": {"kind": "person"}, "text": REPLY, "asked": "What due date?"}})
        self.kept_rows.append({"id": "j_kept0002", "kind": "field_answer", "derived_from": "j_earlier02",
                               "text": json.dumps({"host": "erp.example", "label": "Due Date", "option": "leave"}),
                               "source": {"actor": {"kind": "person"}, "text": "leave", "asked": "Due Date?"}})
        skill_task._prepare(self.run_for())
        self.assertEqual(state.paused_skill["kind"], "lacking")
        from decurix_agent import person_record
        self.assertEqual([r["option"] for r in person_record.field_answers()], ["leave"])  # (the pick's only)

    def test_no_decision_kept_asks_about_that_field_alone_and_a_leave_is_kept_and_no_help(self):
        """(b) Asked once, naming only that field; "leave it as the app fills it" replays, is kept, is no help."""

        run = self.run_for()
        skill_task._prepare(run)
        self.assertEqual((self.by_model, self.replayed), ([], []))
        self.assertEqual(state.paused_skill["kind"], "lacking")
        self.assertEqual(len(self.asked), 1)
        question = self.asked[0]
        self.assertIn(skill_task._leave_sentence("Due Date"), question)
        for other in ("Supplier", "Item", "Qty", "Invoice"):
            self.assertNotIn(other, question)

        self.reply(REPLY, reading={"choice": "leave", "instruction": ""})
        self.assertEqual(len(self.posted), 1)
        (kind, skill, _v, _q, text), kw = self.posted[0]
        self.assertEqual((kind, skill, kw["derived_from"]), ("field_answer", "enter-bill", "j_reply0001"))
        said = json.loads(text)
        self.assertEqual((said["label"], said["param"], said["left_to_app"]), ("Due Date", "due_date", True))
        self.assertIn(said["option"], REPLY)  # (the control plane keeps it only when it stands in the reply)

        self.assertEqual(state.paused_skill["kind"], "plan")
        self.reply("yes")
        self.assertEqual(len(self.replayed), 1)
        self.assertFalse([s for s in self.replayed[0][0]["steps"] if s.get("param") == "due_date"])
        self.assertEqual(self.by_model, [])
        self.assertFalse(skill_task.final_gate._helped(run, {"mode": "replay"}))

    def test_a_plain_one_leaves_it_without_a_model_reading(self):
        run = self.run_for()
        skill_task._prepare(run)
        with mock.patch.object(skill_task.llm_client, "available", return_value=False):
            self.reply("1")
        self.assertIn("due_date", run["left_to_app"])
        self.assertEqual(state.paused_skill["kind"], "plan")
        self.assertFalse(run.get("asked_person"))

    def test_the_next_run_of_the_skill_asks_nothing(self):
        """(c) The same field missing again on the next run: no question, the replay."""

        skill_task._prepare(self.run_for())
        self.reply(REPLY, reading={"choice": "leave", "instruction": ""})
        self.keep_as_the_control_plane_does()
        self.asked.clear()
        state.paused_skill = None

        run = self.run_for()
        skill_task._prepare(run)
        self.assertEqual(state.paused_skill["kind"], "plan")  # no "lacking" question
        self.assertTrue(run["left_to_app"]["due_date"]["kept"])
        self.reply("yes")
        self.assertEqual(len(self.replayed), 1)
        self.assertFalse(skill_task.final_gate._helped(run, {"mode": "replay"}))

        # ...for that skill only: another skill with the same field is asked.
        other = due_date_skill()
        other["name"] = "enter-other-bill"
        state.paused_skill = None
        skill_task._prepare(self.run_for(other))
        self.assertEqual(state.paused_skill["kind"], "lacking")

    def test_an_answer_with_a_value_is_help_and_the_value_is_typed(self):
        """(d) The existing ruling: data the documents should have held is help."""

        run = self.run_for()
        skill_task._prepare(run)
        self.reply("15-11-2026", reading={"choice": "value", "instruction": "15-11-2026"})
        self.assertEqual(self.posted, [])  # (nothing kept: a value is this run's data)
        self.assertTrue(skill_task.final_gate._helped(run, {"mode": "replay"}))
        self.assertEqual(state.paused_skill["kind"], "plan")
        self.reply("yes")
        program, params = self.replayed[0]
        self.assertEqual(params["due_date"], "15-11-2026")
        self.assertTrue([s for s in program["steps"] if s.get("param") == "due_date"])

    def test_a_save_the_application_refuses_for_want_of_it_goes_to_the_model(self):
        """(e) The save check still stands: a required field refused at the save halts the replay, and the model takes
        over from that step, as from any halt."""

        skill_task._prepare(self.run_for())
        self.reply(REPLY, reading={"choice": "leave", "instruction": ""})
        self.outcome = {"outcome": skill_task.replay.HALTED, "step": 7, "resume_from": 7,
                        "text": "Step 7: the application refused the save: Due Date is mandatory."}
        run = state.paused_skill["run"]
        self.reply("yes")
        self.assertEqual(len(self.replayed), 1)
        self.assertEqual(len(self.by_model), 1)
        self.assertEqual(self.by_model[0]["from_step"], 7)
        self.assertTrue(run["taken_over"])
        self.assertTrue(skill_task.final_gate._helped(run, {"mode": "replay"}))
        self.assertIn("never make a value up", skill_task._left_note(run))


class AFieldLeftOutOfTheReplay(unittest.TestCase):
    """skills.expand: a value given as EMPTY_ITEM that is no list is a field no step enters."""

    def test_its_steps_and_the_checks_that_name_it_are_left_out(self):
        skill = due_date_skill()
        program, flat = skills.expand(skill, dict(filled_values(), due_date=skills.EMPTY_ITEM))
        self.assertNotIn("due_date", flat)
        self.assertFalse([s for s in program["steps"] if s.get("param") == "due_date"])
        self.assertEqual([s["n"] for s in program["steps"]], list(range(1, len(skill["steps"]))))
        self.assertEqual({p["name"]: p["step"] for p in program["params"]}["qty"],
                         next(s["n"] for s in program["steps"] if s.get("param") == "qty"))
        self.assertEqual(len(skill["steps"]), 8)  # (the skill itself is unchanged)

    def test_a_pick_left_out_takes_its_list_entry_with_it(self):
        skill = bill_skill()
        skill["steps"].insert(2, field_step(0, "combobox", "Cost Center", "cost_center"))
        skill["steps"].insert(3, field_step(0, "option", "Main - NW", kind="click"))
        skill["params"].append({"name": "cost_center", "label": "Cost Center", "default": None, "example": "Main",
                                "varies": True, "step": 0})
        skills.renumber(skill)
        program, _flat = skills.expand(skill, dict(filled_values(), cost_center=skills.EMPTY_ITEM))
        names = [(s.get("target") or {}).get("name") for s in program["steps"]]
        self.assertNotIn("Cost Center", names)
        self.assertNotIn("Main - NW", names)
        self.assertIn("Supplier", names)


TASK = "t58c3989f80a74412"


def kept_leave_row(skill="enter-bill"):
    """A leave the control plane keeps for `skill`'s due date, as it serves it (desktopLearnedList)."""

    return {"id": "j_kept0001", "kind": "field_answer", "derived_from": "j_earlier01",
            "text": json.dumps({"host": "", "label": "Due Date", "left_to_app": True, "option": REPLY,
                                "param": "due_date", "skill": skill}),
            "source": {"actor": {"kind": "person"}, "text": REPLY,
                       "asked": "The documents for this run do not give Due Date. "
                                + skill_task._leave_sentence("Due Date") + " - or reply with it."}}


class GuiDoLeavesTheFieldToo(unittest.TestCase):
    """
    Freeze 2.5 (live: ERPNext bill 10, task t58c3989f80a74412). The plan said the due date was left as the application
    fills it in, and the model's own `gui do` refused the run without it ("it needs due_date - ask the human") and with
    it empty ("a replay cannot leave a field out"). `gui do` honours the same decision: kept for the skill, or the run's
    own (handed over through guidance, its judgement read on the control plane).
    """

    def setUp(self):
        from decurix_agent.computer import skill_engine
        from decurix_agent import person_record
        self.engine, self.person_record = skill_engine, person_record
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.learned, self.judgements, self.replayed = [], [], []
        person_record.clear_cache()
        self.addCleanup(person_record.clear_cache)

        def get(path, query, fresh=False):
            if path == person_record.LEARNED_PATH:
                return {"learned": list(self.learned)}
            return {"judgements": list(self.judgements)}

        def replay_run(program, params=None, start=1, ladder=None):
            self.replayed.append((program, dict(params or {})))
            return {"outcome": skill_engine.replay.COMPLETED_UNVERIFIED, "text": "ran", "step": None, "resume_from": None}

        patches = [
            mock.patch.object(skill_engine, "REPLAYED_DIR", Path(self.tmp.name) / "replayed"),
            mock.patch.object(guidance, "RUN_FILE", Path(self.tmp.name) / "guided-run.json"),
            mock.patch("decurix_agent.task_context.current", return_value=TASK),
            mock.patch.object(person_record, "_get", get),
            mock.patch.object(skill_engine.replay, "run", replay_run),
            mock.patch.object(skill_engine.learning, "start_checks", return_value=[]),
            mock.patch.object(skill_engine, "other_system", return_value=""),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def run_gui_do(self, **given):
        return self.engine.run(due_date_skill(), params=dict(filled_values(), **given))

    def assert_replayed_without_the_field(self, result):
        self.assertEqual(result["outcome"], self.engine.replay.COMPLETED_UNVERIFIED, result.get("text"))
        program, params = self.replayed[0]
        self.assertFalse([s for s in program["steps"] if s.get("param") == "due_date"])
        self.assertNotIn("due_date", params)

    def test_a_kept_leave_replays_without_the_field_given_or_given_empty(self):
        self.learned.append(kept_leave_row())
        self.assert_replayed_without_the_field(self.run_gui_do())
        self.replayed.clear()
        self.engine._note_replayed(self.engine._note_key({"name": "enter-bill"}, None), 0)  # (a fresh request)
        self.assert_replayed_without_the_field(self.run_gui_do(due_date=""))

    def test_the_runs_own_leave_is_honoured_when_its_judgement_offered_it(self):
        guidance.begin(skill="enter-bill", task="Enter this bill", text="", values=filled_values(),
                       run={"left_to_app": {"due_date": {"label": "Due Date", "judgement": "j_reply0001"}}})
        self.addCleanup(guidance.end)
        self.judgements.append({"id": "j_reply0001", "task_id": TASK, "actor": {"kind": "person"}, "text": REPLY,
                                "asked": "The documents for this run do not give Due Date. "
                                         + skill_task._leave_sentence("Due Date") + " - or reply with it."})
        self.assert_replayed_without_the_field(self.run_gui_do())

    def test_no_decision_or_one_the_question_never_offered_still_needs_the_value(self):
        guidance.begin(skill="enter-bill", task="Enter this bill", text="", values=filled_values(),
                       run={"left_to_app": {"due_date": {"label": "Due Date", "judgement": "j_reply0001"}}})
        self.addCleanup(guidance.end)
        self.judgements.append({"id": "j_reply0001", "task_id": TASK, "actor": {"kind": "person"}, "text": REPLY,
                                "asked": "What due date should I enter?"})  # (the guidance file alone backs nothing)
        self.learned.append(kept_leave_row(skill="enter-other-bill"))
        result = self.run_gui_do()
        self.assertEqual(result["outcome"], self.engine.replay.HALTED_BEFORE_EFFECT)
        self.assertIn("due_date", result["text"])
        self.assertEqual(self.replayed, [])


class EveryHaltSaysWhy(unittest.TestCase):
    """
    Freeze 2.5 (live: ERPNext bill 10): "Replay step 7/34: click textbox "Supplier Invoice No"" was followed two minutes
    later by "The replay stopped at step 7; the model is taking it from there" and nothing else. Every halt says the
    step, what stopped it and what the screen showed - in the replay's result, and in the takeover's trace line.
    """

    SCREEN = {"active_title": "New Purchase Invoice - Chromium", "page_url": "http://erp.example/app/purchase-invoice/new",
              "elements": []}

    def setUp(self):
        from decurix_agent.computer import replay
        self.replay = replay
        self.reports = []
        patches = [
            mock.patch.object(replay, "_open_where_it_began", return_value=None),
            mock.patch.object(replay, "_fit_screen"),
            mock.patch.object(replay, "_find", return_value=({"path": 'textbox "Supplier Invoice No"', "role": "textbox",
                                                              "name": "Supplier Invoice No"}, None, {}, None)),
            mock.patch.object(replay.core, "observe", return_value=dict(self.SCREEN)),
            mock.patch.object(replay.screen_call, "remote", return_value=False),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def program(self, expect=()):
        step = field_step(1, "textbox", "Supplier Invoice No", kind="click")
        step["expect"] = list(expect)
        return {"session": "skill-enter-bill", "title": "Enter a bill", "steps": [step], "params": []}

    def halt_with(self, error):
        with mock.patch.object(self.replay.core, "act", side_effect=error):
            return self.replay._run_program(self.program(), {}, 1, self.reports.append)

    def assert_says_why(self, result):
        self.assertTrue(str(result["reason"]).strip(), result)
        self.assertIn("Supplier Invoice No", result["text"])
        self.assertIn("the screen shows", result["text"])

    def test_an_action_that_fails_with_no_message_says_its_kind_and_the_screen(self):
        result = self.halt_with(self.replay.ComputerError(""))
        self.assertEqual(result["outcome"], self.replay.HALTED_BEFORE_EFFECT)
        self.assert_says_why(result)
        self.assertIn("ComputerError", result["reason"])

    def test_an_action_done_then_lost_with_no_message_says_so_too(self):
        result = self.halt_with(self.replay.ActedError(""))
        self.assertEqual(result["outcome"], self.replay.HALTED)
        self.assert_says_why(result)

    def test_a_halt_raised_with_a_blank_reason_is_given_one(self):
        with mock.patch.object(self.replay, "_find", side_effect=self.replay._Halt("  ", "the screen shows nothing")):
            result = self.replay._run_program(self.program(), {}, 1, self.reports.append)
        self.assertTrue(result["reason"].strip())
        self.assertIn("gave no reason", result["text"])

    def test_a_result_that_never_appeared_names_it_and_the_screen(self):
        with mock.patch.object(self.replay.core, "act", return_value={"status": "done", "observation": dict(self.SCREEN)}), \
                mock.patch.object(self.replay, "_await_checks",
                                  return_value=([{"type": "window", "value": "Saved"}], dict(self.SCREEN))):
            result = self.replay._run_program(self.program(expect=[{"type": "window", "value": "Saved"}]), {}, 1,
                                              self.reports.append)
        self.assertEqual(result["outcome"], self.replay.HALTED)
        self.assert_says_why(result)

    def test_the_takeover_line_in_the_trace_carries_the_reason(self):
        emitted = []
        run = {"skill": bill_skill(), "values": filled_values(), "start": 7, "context": "Enter this bill"}
        with mock.patch.object(skill_task.progress, "emit", emitted.append), \
                mock.patch.object(skill_task, "_engine"), mock.patch.object(skill_task.phase_note, "write"), \
                mock.patch.object(skill_task.guidance, "begin"):
            skill_task._by_model(run, from_step=7, note="Stopped replaying \"Enter a bill\" at step 7 of 34 (click "
                                                         "textbox \"Supplier Invoice No\"): it was not found. Now the "
                                                         "screen shows \"New Purchase Invoice\".")
            skill_task._by_model(run, from_step=7, note="")
        self.assertIn("it was not found", emitted[0])
        self.assertIn("the screen shows", emitted[0])
        self.assertIn("gave no reason", emitted[1])
