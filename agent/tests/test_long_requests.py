"""
A long request carried to the end (long_task.py, docs/long-requests.md): a
plan of stages run by the workflow runner, a ledger carried between them,
questions that park only what depends on them, messages read mid-run for what
they are, and a cross-check of figures across outputs before the report.

    PYTHONPATH="agent;agent/tests" python -m unittest test_long_requests
"""

import json
import sys
import tempfile
import time
import types
import unittest
from decimal import Decimal
from pathlib import Path
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

from decurix_agent import config, long_task, messaging, state, task_context, verify, workflow_task
from decurix_agent.workflow_runner import WorkflowRunner
# Imported now, not first inside a test: LongRun patches sys.modules with patch.dict, which drops every module first
# imported while it is in place - and a module imported twice is two modules (found: the next test file's mocks then
# patched a copy the code no longer used, and the suite hung waiting on a real model server).
from decurix_agent import llm_client, procedures, produced, skill_task, watchers  # noqa: F401
from decurix_agent.computer import approval, evidence  # noqa: F401
from decurix_agent.tools import office, outbox  # noqa: F401

TASK = "t0123456789abcdef"
OTHER_TASK = "tfedcba9876543210"

LONG = ("Close October: reconcile the bank, build the purchase register workbook, a GST summary, AR ageing with "
        "reminder email drafts, a 5-slide deck and a Word memo of exceptions.")


# Round 5's P2 request, as the person wrote it (decurix-tests/wide/process/requests.json).
P2_REQUEST = """Morning! Today's orders please. Attached: 4 WhatsApp chat exports, 2 POs that came by email (.eml, the PO PDF is inside), our price list, today's stock and the customer master.

Rules:
- Prices from our price list, 5% off the rate on any line of 50 or more units of one SKU in an order. The portal still shows the old catalogue prices - overwrite them with ours.
- Customers change their minds in the chats - go by what they finally want.
- Some items are short: allocate first come, first served by the time the customer asked for that item. Part-ship what we have, the rest is backorder.
- If an order (full quantity, with GST) plus what the customer already owes takes them over their credit limit, hold it and ask me.

Please:
1. Enter every approved order in the Northwind Back Office portal (http://host.docker.internal:8701, user clerk, password Northwind#2026): one order per customer PO or chat, with only what we ship now.
2. A tax invoice PDF for each order (CGST+SGST for Maharashtra customers, IGST for other states).
3. dispatch_plan.xlsx: every order line with ordered, allocated and backordered quantities.
4. purchase_requisition.xlsx: what we need to buy for the backorders, per SKU.
5. customer_replies.txt: the reply for each customer (what's coming, invoice total, what's on backorder). I'll send them myself.
Save everything in P2_orders_19Oct in your work folder."""


class SyncThread:
    def __init__(self, target=None, args=(), daemon=None, name=None):
        self.target, self.args = target, args

    def start(self):
        self.target(*self.args)


class Detection(unittest.TestCase):

    def setUp(self):
        patch = mock.patch.dict("os.environ", {long_task.ENABLED: "1"})
        patch.start()
        self.addCleanup(patch.stop)

    def test_off_unless_switched_on(self):
        with mock.patch.dict("os.environ", {long_task.ENABLED: ""}), tempfile.TemporaryDirectory() as root, \
                mock.patch.object(config, "DECURIX_DIR", Path(root)):
            self.assertFalse(long_task.looks_long(LONG))
            (Path(root) / "long-requests").write_text("on\n", encoding="utf-8")
            self.assertTrue(long_task.looks_long(LONG))

    def test_several_deliverables_are_long(self):
        self.assertTrue(long_task.looks_long(LONG))

    def test_short_requests_are_not(self):
        for text in ("Make a workbook of these invoices and email it to bob@example.com",
                     "Create an invoice PDF for order 12 and draft an email with a summary",
                     "what is the kernel version?", "/workflow list", "stop"):
            self.assertFalse(long_task.looks_long(text), text)

    def test_three_kinds_with_many_files(self):
        files = "\n".join(f"  /home/headless/work/inbox/f{n}.pdf" for n in range(6))
        text = "Build the dispatch plan workbook, the requisition and a reply to each customer.\n\n" \
               "Files attached to this request, saved on this desktop:\n" + files
        self.assertTrue(long_task.looks_long(text))
        self.assertFalse(long_task.looks_long(text.split("\n\n")[0]))

    def test_can_be_switched_off(self):
        with mock.patch.dict("os.environ", {long_task.ENABLED: "0"}):
            self.assertFalse(long_task.looks_long(LONG))


class PlanToSpec(unittest.TestCase):

    def test_plan_becomes_a_planned_spec(self):
        plan = {"long": True, "title": "October close", "stages": [
            {"id": "bank", "title": "Bank reconciliation", "does": "Reconcile.", "inputs": ["/in/bank.csv", "x.pdf"],
             "outputs": [{"file": "~/work/close/bank.xlsx", "must_contain": ["Difference"]}], "depends_on": []},
            {"id": "mail", "title": "Reminders", "does": "Draft reminders.", "drafts": 3, "depends_on": ["bank", "nope"]},
            {"id": "portal", "title": "Enter orders", "does": "Enter them.", "depends_on": ["mail"]}]}
        spec = long_task.spec_from_plan(plan, ["/in/bank.csv"], request='Add a column "Difference" to the reconciliation, and draft reminder emails to the customers.')
        self.assertTrue(spec["planned"])
        bank, mail, portal = spec["steps"]
        self.assertEqual(bank["checks"], [{"path": "close/bank.xlsx", "format": "xlsx", "contains": ["Difference"]}])
        self.assertEqual(bank["inputs"], ["/in/bank.csv"])
        self.assertEqual(mail["checks"], [{"path": "outbox", "format": "drafts", "min_count": 3}])
        self.assertEqual(mail["depends_on"], [bank["id"]])
        # A stage that makes no file or draft writes a log of what it did, so it has something to check.
        self.assertEqual(portal["checks"][0]["format"], "csv")
        self.assertIn(portal["checks"][0]["path"], portal["instruction"])

    def test_a_word_the_planner_invented_is_never_a_code_check(self):
        # Round 5, Finding P: "Orders" - the planner's own word - failed a correct workbook.
        plan = {"stages": [{"id": "a", "does": "x", "outputs": [{"file": "order_workings.xlsx",
                                                                 "must_contain": ["Orders", "Lines", "Status"]}]},
                           {"id": "b", "does": "y", "outputs": ["b.csv"]}]}
        spec = long_task.spec_from_plan(plan, request=P2_REQUEST)
        self.assertEqual(spec["steps"][0]["checks"], [{"path": "order_workings.xlsx", "format": "xlsx"}])

    def test_one_file_per_item_is_a_pattern_not_a_placeholder(self):
        plan = {"stages": [{"id": "a", "does": "x", "outputs": [{"file": "P2/invoice_WA-xxxx.pdf"},
                                                                {"file": "P2/inv_*.pdf", "count": 4}]},
                           {"id": "b", "does": "y", "outputs": ["b.csv"]}]}
        checks = long_task.spec_from_plan(plan)["steps"][0]["checks"]
        self.assertEqual(checks, [{"path": "P2/invoice_WA-*.pdf", "format": "pdf", "min_count": 1},
                                  {"path": "P2/inv_*.pdf", "format": "pdf", "min_count": 4}])

    def test_the_planner_is_told_to_give_a_gating_decision_its_own_early_stage(self):
        rules = " ".join(long_task.PLAN_SYSTEM.split())
        self.assertIn("gets its own early stage", rules)
        self.assertIn("only work that comes out the same whatever the answer", rules)
        # The planner says which stages a decision affects; with none named, nothing is redone.
        self.assertIn("A decision stage's \"affects\" lists the ids of the stages", rules)

    def test_a_deliverable_keeps_the_requests_own_scope_words(self):
        # Round 5, Finding AF: the stage "Finalize approved allocations and create dispatch plan ... and customer
        # replies" left the held customer out of both.
        plan = {"stages": [
            {"id": "decide", "does": "Work out the credit holds and ask.", "decision": True,
             "outputs": [{"file": "P2_orders_19Oct/order_working.xlsx"}]},
            {"id": "plans", "does": "Finalize approved allocations and create the dispatch plan and replies for approved orders.",
             "outputs": [{"file": "P2_orders_19Oct/dispatch_plan.xlsx"}, {"file": "P2_orders_19Oct/customer_replies.txt"}],
             "depends_on": ["decide"]}]}
        steps = long_task.spec_from_plan(plan, request=P2_REQUEST)["steps"]
        text = steps[1]["instruction"]
        self.assertIn("\"dispatch_plan.xlsx: every order line with ordered, allocated and backordered quantities.\"", text)
        self.assertIn("\"customer_replies.txt: the reply for each customer (what's coming, invoice total, what's on backorder).\"", text)
        self.assertIn("with any decision about it shown", text)
        self.assertNotIn("The request's own words", steps[0]["instruction"])  # a working file the request does not name

    def test_the_planner_is_told_to_keep_scope_words_and_not_narrow(self):
        rules = " ".join(long_task.PLAN_SYSTEM.split())
        for words in ("Copy each deliverable's scope words word for word", "\"every line\", \"each customer\"",
                      "never narrow a deliverable to a subset the request does not name for it",
                      "still covers all of them, showing the decision"):
            self.assertIn(words, rules)

    def test_one_stage_or_not_long_is_no_plan(self):
        self.assertIsNone(long_task.spec_from_plan({"long": False}))
        self.assertIsNone(long_task.spec_from_plan({"stages": [{"id": "a", "does": "x"}]}))

    def test_paths_outside_the_work_folder_are_dropped(self):
        plan = {"stages": [{"id": "a", "does": "x", "outputs": ["/etc/passwd.txt", "../up.csv", "ok.csv"]},
                           {"id": "b", "does": "y", "outputs": ["b.docx"]}]}
        spec = long_task.spec_from_plan(plan)
        self.assertEqual([c["path"] for c in spec["steps"][0]["checks"]], ["ok.csv"])


class RunnerPlannedRuns(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name) / "work"
        self.r = WorkflowRunner(Path(self.tmp.name) / "runs")
        self.spec = {"id": "p", "planned": True, "steps": [
            {"id": "a", "instruction": "a", "checks": [{"path": "a.csv", "format": "csv"}], "retry_safe": True},
            {"id": "b", "instruction": "b", "checks": [{"path": "b.csv", "format": "csv"}], "retry_safe": True},
            {"id": "c", "instruction": "c", "checks": [{"path": "c.csv", "format": "csv"}], "retry_safe": True,
             "depends_on": ["a"]}]}

    def finish(self, run_id, step_id):
        (self.work / f"{step_id}.csv").write_text("x,y\n1,2\n", encoding="utf-8")
        return self.r.complete(run_id, step_id)

    def test_a_parked_stage_holds_only_what_depends_on_it(self):
        run = self.r.create(self.spec, work_dir=str(self.work))
        _, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "a")
        self.r.park(run["run_id"], "a", "Which account?")
        state, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "b")
        self.finish(run["run_id"], "b")
        state, step = self.r.claim(run["run_id"])
        self.assertIsNone(step)
        self.assertEqual(state["status"], "awaiting_answer")
        self.assertEqual(state["steps"]["a"]["attempts"], 0)
        self.r.unpark(run["run_id"], "a", "The current account")
        _, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "a")
        self.finish(run["run_id"], "a")
        _, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "c")

    def test_an_approval_stage_that_changes_no_file_completes_once_decided(self):
        # Round 5 P3 (2026-10-20): "Ask for approval of the payment run" hands in the files two earlier stages made;
        # approved as proposed, it changed none and failed twice with "artifact was already present and unchanged".
        spec = {"id": "p3", "planned": True, "steps": [
            {"id": "s1", "instruction": "register", "checks": [{"path": "register.csv", "format": "csv"}],
             "retry_safe": True},
            {"id": "s3", "instruction": "proposal", "checks": [{"path": "proposal.csv", "format": "csv"}],
             "retry_safe": True, "depends_on": ["s1"]},
            {"id": "s4", "instruction": "ask for approval", "decision": True, "retry_safe": True,
             "checks": [{"path": "proposal.csv", "format": "csv"}, {"path": "register.csv", "format": "csv"}],
             "depends_on": ["s1", "s3"]},
            {"id": "s5", "instruction": "own file", "checks": [{"path": "kept.csv", "format": "csv"}],
             "retry_safe": True}]}
        (self.work).mkdir(parents=True, exist_ok=True)
        (self.work / "kept.csv").write_text("x,y\n1,2\n", encoding="utf-8")
        run = self.r.create(spec, work_dir=str(self.work))
        rid = run["run_id"]
        for sid, name in (("s1", "register"), ("s3", "proposal")):
            _, step = self.r.claim(rid)
            self.assertEqual(step["id"], sid)
            (self.work / f"{name}.csv").write_text("x,y\n1,2\n", encoding="utf-8")
            self.r.complete(rid, sid)
        _, step = self.r.claim(rid)
        self.assertEqual(step["id"], "s4")
        self.r.park(rid, "s4", "Approve the payment run?")
        _, step = self.r.claim(rid)  # s5: a stage handing in a file it found, unchanged, still fails
        self.assertEqual(step["id"], "s5")
        state = self.r.complete(rid, "s5")
        self.assertEqual(state["steps"]["s5"]["error"], "artifact was already present and unchanged before this step")
        self.r.unpark(rid, "s4", "Approved as proposed.")
        _, step = self.r.claim(rid)
        self.assertEqual(step["id"], "s4")
        state = self.r.complete(rid, "s4")
        self.assertEqual(state["steps"]["s4"]["status"], "completed")

    def test_a_retry_after_asking_may_leave_one_of_its_files_as_it_was(self):
        # Round 5 P4 (2026-10-20), Finding BH2: the stage made two workbooks, asked, and the retry fixed one; the
        # baseline taken again after it asked held its own first files, and the stage failed as "unchanged".
        spec = {"id": "p4", "planned": True, "steps": [
            {"id": "s1", "instruction": "review", "retry_safe": True,
             "checks": [{"path": "completeness_review.csv", "format": "csv"}, {"path": "pay_working.csv", "format": "csv"}]}]}
        run = self.r.create(spec, work_dir=str(self.work))
        rid = run["run_id"]
        self.r.claim(rid)
        self.work.mkdir(parents=True, exist_ok=True)
        (self.work / "completeness_review.csv").write_text("Name,Complete\nArif,Yes\n", encoding="utf-8")
        (self.work / "pay_working.csv").write_text("Name,CTC\nArif,600000\n", encoding="utf-8")
        self.r.park(rid, "s1", "Is Arif's ID name a mismatch?")
        self.r.unpark(rid, "s1", "try again")
        self.r.claim(rid)
        (self.work / "completeness_review.csv").write_text("Name,Complete,Note\nArif,No,Mohd. vs Mohammed\n",
                                                           encoding="utf-8")
        state = self.r.complete(rid, "s1")
        self.assertEqual(state["steps"]["s1"]["status"], "completed", state["steps"]["s1"].get("error"))

    def test_outputs_go_to_the_given_work_folder(self):
        run = self.r.create(self.spec, work_dir=str(self.work))
        self.assertEqual(self.r.context(run)["work_dir"], str(self.work.resolve()))

    def test_a_failed_stage_does_not_stop_independent_ones(self):
        run = self.r.create(self.spec, work_dir=str(self.work))
        for _ in range(2):
            _, step = self.r.claim(run["run_id"])
            self.r.complete(run["run_id"], "a", error="broken")
        state, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "b")
        self.finish(run["run_id"], "b")
        state, step = self.r.claim(run["run_id"])
        self.assertIsNone(step)
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["c"]["status"], "pending")

    def test_reopen_and_a_later_change_to_an_earlier_file(self):
        run = self.r.create(self.spec, work_dir=str(self.work))
        self.r.claim(run["run_id"])
        self.finish(run["run_id"], "a")
        # A later stage adds to a's file: it is checked again, not taken as tampering.
        (self.work / "a.csv").write_text("x,y\n1,2\n3,4\n", encoding="utf-8")
        state, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "b")
        self.assertEqual(state["steps"]["a"]["status"], "completed")
        self.assertTrue(state["steps"]["a"].get("changed_later"))
        self.finish(run["run_id"], "b")
        self.r.reopen(run["run_id"], "a", "the person changed the request")
        _, step = self.r.claim(run["run_id"])
        self.assertEqual(step["id"], "a")
        # Re-checked and unchanged is fine: a re-check may find nothing to change.
        done = self.r.complete(run["run_id"], "a")
        self.assertEqual(done["steps"]["a"]["status"], "completed")

    def test_a_sequential_run_is_unchanged(self):
        spec = dict(self.spec)
        spec.pop("planned")
        run = self.r.create(spec, work_dir=str(self.work))
        self.r.claim(run["run_id"])
        with self.assertRaises(ValueError):
            self.r.park(run["run_id"], "a", "q")

    def test_a_pattern_checks_every_file_it_matches(self):
        from decurix_agent.workflow_runner import validate
        spec = {"id": "p", "planned": True, "steps": [{"id": "inv", "instruction": "x", "retry_safe": True,
                                                       "checks": [{"path": "out/inv_*.csv", "format": "csv", "min_count": 2}]}]}
        unplanned = dict(spec, planned=False)
        with self.assertRaises(ValueError):
            validate(unplanned)
        run = self.r.create(spec, work_dir=str(self.work))
        (self.work / "out").mkdir()
        (self.work / "out" / "inv_1.csv").write_text("a,b\n1,2\n", encoding="utf-8")
        self.r.claim(run["run_id"])
        state = self.r.complete(run["run_id"], "inv")
        self.assertIn("at least 2 file(s) matching out/inv_*.csv, found 1", state["steps"]["inv"]["error"])
        (self.work / "out" / "inv_2.csv").write_text("a,b\n3,4\n", encoding="utf-8")
        self.r.claim(run["run_id"])
        state = self.r.complete(run["run_id"], "inv")
        self.assertEqual([e["path"] for e in state["steps"]["inv"]["evidence"]], ["out/inv_1.csv", "out/inv_2.csv"])

    def test_drafts_checks_are_for_planned_runs_and_count_drafts(self):
        from decurix_agent.workflow_runner import validate
        drafts = {"id": "d", "steps": [{"id": "m", "instruction": "x", "retry_safe": True,
                                        "checks": [{"path": "outbox", "format": "drafts", "min_count": 2}]}]}
        with self.assertRaises(ValueError):
            validate(drafts)
        drafts["planned"] = True
        run = self.r.create(drafts, work_dir=str(self.work))
        self.r.claim(run["run_id"])
        state = self.r.complete(run["run_id"], "m", result={"drafts": [{"id": "1", "to": ["a@x.com"], "subject": "s"}]})
        self.assertIn("at least 2", state["steps"]["m"]["error"])
        self.r.claim(run["run_id"])
        state = self.r.complete(run["run_id"], "m", result={"drafts": [
            {"id": "1", "to": ["a@x.com"], "subject": "s"}, {"id": "2", "to": ["b@x.com"], "subject": "t"}]})
        self.assertEqual(state["steps"]["m"]["status"], "completed")
        self.assertEqual(state["steps"]["m"]["evidence"][0]["bytes"], 2)


class Ledger(unittest.TestCase):

    def test_carry_forward_and_assumed_sections(self):
        report = ("Built the register.\n\nCarry forward:\n- Eligible ITC: 45,000.00\n- Duplicate bill B-7 excluded\n"
                  "Assumed:\n- Rounding to the rupee\n\nChecked by reading the file back.")
        self.assertEqual(long_task.carried(report), [("fact", "Eligible ITC: 45,000.00"),
                                                     ("fact", "Duplicate bill B-7 excluded"),
                                                     ("assumed", "Rounding to the rupee")])


class CrossCheck(unittest.TestCase):

    def test_figures_compare_at_their_precision(self):
        self.assertTrue(long_task.same_figure(long_task.parse_number("1,23,450"), long_task.parse_number("123450.40")))
        self.assertTrue(long_task.same_figure(long_task.parse_number("12.3 lakh"), long_task.parse_number("12,34,000")))
        self.assertFalse(long_task.same_figure(long_task.parse_number("45,000.00"), long_task.parse_number("45,900.00")))
        self.assertEqual(long_task.parse_number("(1,200)")[0], Decimal(-1200))

    def test_code_finds_a_total_that_differs(self):
        rows = {"/w/book.xlsx": [("Net GST payable", ["12500.0"]), ("Item", ["3"])],
                "/w/deck.pptx": [("Net GST payable", ["12,900.00"])],
                "/w/memo.docx": [("Net GST payable", ["12,500"])]}
        found, agreed = long_task.code_disagreements(
            [("gst", "/w/book.xlsx"), ("deck", "/w/deck.pptx"), ("memo", "/w/memo.docx")], read=lambda p: rows[p])
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0]["figure"], "net gst payable")
        run_state = {"spec": {"steps": [{"id": "gst"}, {"id": "deck", "depends_on": ["gst"]},
                                        {"id": "memo", "depends_on": ["gst"]}]}}
        # Two of three agree: the deck is the odd one out.
        self.assertEqual(long_task.odd_one_out(run_state, found[0]), "deck")

    def test_two_way_disagreement_goes_to_the_stage_built_on_the_other(self):
        run_state = {"spec": {"steps": [{"id": "book"}, {"id": "deck", "depends_on": ["book"]}, {"id": "x"}]}}
        d = {"figure": "total", "values": [{"stage": "book", "file": "a", "text": "Total: 100"},
                                           {"stage": "deck", "file": "b", "text": "Total: 110"}]}
        self.assertEqual(long_task.odd_one_out(run_state, d), "deck")
        d["values"][1]["stage"] = "x"
        self.assertIsNone(long_task.odd_one_out(run_state, d))

    def test_the_model_only_points_and_code_checks_each_place(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        a, b = Path(tmp.name) / "a.txt", Path(tmp.name) / "b.txt"
        a.write_text("Total revenue: 10,000.00\n", encoding="utf-8")
        b.write_text("Revenue for the quarter was 10,500.00\n", encoding="utf-8")
        ask = lambda system, prompt: {"figures": [
            {"figure": "revenue", "places": [{"file": "a.txt", "text": "Total revenue: 10,000.00"},
                                             {"file": "b.txt", "text": "was 10,500.00"}]},
            {"figure": "invented", "places": [{"file": "a.txt", "text": "Total cost: 5"},
                                              {"file": "b.txt", "text": "cost 6"}]}]}
        found, agreed = long_task.model_disagreements([("s1", str(a)), ("s2", str(b))], ask=ask)
        self.assertEqual([d["figure"] for d in found], ["revenue"])


class FakeEngine:
    """The engine of one stage: writes what the stage hands in, or asks."""

    def __init__(self, case):
        self.case, self.prompts, self.asks = case, [], {}

    def perform_task(self, prompt, task_file):
        self.prompts.append(prompt)
        run_id, step_id, _ = workflow_task._active
        if self.asks.get(step_id):
            question = self.asks.pop(step_id)
            messaging.ask_for_query(question, kind="query")
            return
        work = Path(config.WORK_DIR)
        made = []
        for check in next(s for s in workflow_task.runner().status(run_id)["spec"]["steps"]
                          if s["id"] == step_id)["checks"]:
            path = work / check["path"]
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(f"Stage,Total\n{step_id},\"{self.case.figures.get(step_id, '1,000.00')}\"\n", encoding="utf-8")
            made.append(check["path"])
        once = getattr(self, "once", {}).pop(step_id, "")
        if getattr(self, "reports", {}).get(step_id):
            messaging.write_final_reply(self.reports.pop(step_id), kind="final")
            return
        messaging.write_final_reply(f"Made {', '.join(made)}. {getattr(self, 'report_suffix', '')}{once}\n\nCarry forward:\n"
                                    f"- {step_id} total: 1,000.00", kind="final")

    def continue_after_query(self, reply):
        pass


class LongRunBase(unittest.TestCase):
    """A planned run on a fake engine and model; no tests of its own, so other modules' classes build on it."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        for name in ("command", "querytext", "work"):
            (root / name).mkdir()
        self.root = root
        self.sent, self.emitted, self.answered = [], [], []
        self.figures = {}
        self.engine = FakeEngine(self)
        harness = types.ModuleType("decurix_agent.harness")
        harness.perform_task = self.engine.perform_task
        harness.continue_after_query = self.engine.continue_after_query
        harness.cancel_running = lambda: False
        harness.ask_model = lambda system, prompt: "{}"
        import decurix_agent
        self.runner = WorkflowRunner(root / "workflows")
        self.classified = []
        saved = (state.current_task, state.current_task_id, state.waiting_for_query, state.backend_ready,
                 state.paused_skill, state.paused_replay, state.last_query_text, state.last_query_kind)
        self.addCleanup(self.restore, saved)
        state.current_task, state.waiting_for_query, state.backend_ready = None, False, True
        state.paused_skill = state.paused_replay = None
        patches = [
            mock.patch.dict(sys.modules, {"decurix_agent.harness": harness}),
            mock.patch.object(decurix_agent, "harness", harness, create=True),
            mock.patch.object(config, "DECURIX_DIR", root),
            mock.patch.object(config, "QUERYTEXT_DIR", root / "querytext"),
            mock.patch.object(config, "COMMAND_DIR", root / "command"),
            mock.patch.object(config, "WORK_DIR", root / "work"),
            # The secrets a message gives are remembered under the agent's folder: this test's, never the machine's.
            mock.patch.dict("os.environ", {"DECURIX_DIR": str(root)}),
            mock.patch.object(workflow_task, "runner", return_value=self.runner),
            mock.patch.object(task_context, "CURRENT_FILE", str(root / "current-task")),
            mock.patch.object(task_context, "_active_path", lambda: root / "active-task.json"),
            mock.patch.object(messaging, "send_query", self.send),
            mock.patch.object(messaging, "_final_gate", None),
            mock.patch.object(messaging, "_query_gate", None),
            mock.patch.object(long_task.progress, "emit", self.emitted.append),
            mock.patch.object(long_task, "_draft_ids", lambda: []),
            mock.patch.object(long_task, "_stage_drafts", lambda began, scope=None: []),
            mock.patch.object(long_task, "threading", types.SimpleNamespace(Thread=SyncThread)),
            mock.patch.object(verify, "available", lambda: True),
            mock.patch.object(verify, "judge", lambda *a, **k: {"verdict": "done", "confidence": 0.95,
                                                                "seen": "the files hold it", "why": ""}),
            mock.patch.object(long_task, "_ask_json", self.model),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        workflow_task._active = workflow_task._pending = None
        workflow_task._driving = False
        long_task._run = None
        long_task._deferred.clear()
        self.addCleanup(setattr, long_task, "_run", None)
        task_context.start(TASK, request=LONG)
        self.plan = {"long": True, "title": "October close", "stages": [
            {"id": "register", "title": "Purchase register", "does": "Build the register.",
             "outputs": [{"file": "register.csv"}]},
            {"id": "bank", "title": "Bank reconciliation", "does": "Reconcile the bank.",
             "outputs": [{"file": "bank.csv"}]},
            {"id": "memo", "title": "Memo", "does": "Write the memo.", "outputs": [{"file": "memo.md"}],
             "depends_on": ["register"]}]}
        self.message_verdict = None
        self.route_reply = {"for": "PERSON"}
        self.routed = []

    def restore(self, saved):
        (state.current_task, state.current_task_id, state.waiting_for_query, state.backend_ready,
         state.paused_skill, state.paused_replay, state.last_query_text, state.last_query_kind) = saved
        workflow_task._active = workflow_task._pending = None
        workflow_task._driving = False

    def model(self, system, prompt, max_tokens=4000):
        if system is long_task.PLAN_SYSTEM:
            return self.plan
        if system is long_task.CLASSIFY_SYSTEM:
            self.classified.append(prompt)
            return self.message_verdict
        if system is long_task.CROSS_SYSTEM:
            return {"figures": []}
        if system is long_task.ROUTE_SYSTEM:
            self.routed.append(prompt)
            return self.route_reply
        raise AssertionError("unexpected model call")

    def send(self, filename, content, kind="", reply="", **kwargs):
        self.sent.append({"kind": kind, "reply": reply, **kwargs})
        return True

    def finals(self):
        # The run's own reports: sent on its own task (Finding BO), or on no task named; never a reply on another task.
        return [s for s in self.sent if s["kind"] in ("final", "error") and s.get("task_id") in (None, TASK)]

    def message(self, text, task_id=TASK):
        path = self.root / "command" / f"20261019_101500_{len(self.classified) + len(self.sent):08d}.{task_id}.txt"
        path.write_text(text, encoding="utf-8")
        state.processed_commands.add(path.name)
        long_task.take_message(text, path)
        return path

    def run_id(self):
        return long_task._run["run_id"] if long_task._run else next(self.runner.root.glob("*/checkpoint.json")).parent.name


class LongRun(LongRunBase):

    def test_stages_run_one_by_one_and_the_report_is_plain(self):
        self.assertTrue(long_task.perform(LONG, None))
        self.assertEqual(len(self.engine.prompts), 3)
        # Each stage is told its own part, and later ones what earlier ones found.
        self.assertIn("Stage 1 of 3", self.engine.prompts[0])
        self.assertIn("Do only this stage", self.engine.prompts[0])
        self.assertIn("register total: 1,000.00", self.engine.prompts[2])
        # Round 5 P6 (BZ1): a later stage keeps what earlier outputs record about an item in step with what it decides.
        self.assertIn("update that output too, so the outputs never disagree", self.engine.prompts[1])
        # Round 5 P6 (CC): every stage applies the rules as soon as the figures are known, and records only evidenced refs.
        self.assertIn("never an instruction to calculate or check later", self.engine.prompts[0])
        self.assertIn("Record a reference for an item only when its own source gives it", self.engine.prompts[0])
        self.assertNotIn("update that output too", self.engine.prompts[0])
        self.assertIn("applied in the stage that first records the item", " ".join(long_task.PLAN_SYSTEM.split()))
        finals = self.finals()
        self.assertEqual(len(finals), 1)
        report = finals[0]["reply"]
        self.assertTrue(finals[0]["checked"])
        self.assertEqual(finals[0]["kind"], "final")
        self.assertIn("Done - every stage was made and checked.", report)
        self.assertIn("register.csv", report)
        self.assertIn("Cost:", report)
        self.assertIn("model tokens", report)
        # Progress shows the plan with ticks.
        self.assertTrue(any(line.startswith("Plan for") and "[x] 1." in line for line in self.emitted))
        data = long_task.ledger(self.run_id())
        self.assertEqual(set(data["costs"]), {s for s in self.runner.status(self.run_id())["steps"]})
        self.assertFalse(long_task.active())

    # ---- Round 5 P2 (2026-10-20), Finding AS: an entry stage ticked done with two approved orders never entered ----

    P2_ENTRY = ("Enter every approved order in the Northwind Back Office portal (http://host.docker.internal:8701): one "
                "order per customer PO or chat, with only what we ship now. Write a CSV log of the orders entered, one "
                "row per record with the portal reference.")
    P2_DISPATCH = [
        ["Customer", "Customer PO / Ref", "SKU", "Decision", "Line status", "Ordered qty", "Allocated qty", "Backorder qty"],
        ["Harbour Hotels Group", "HH/PO/7781", "WB-500", "Hold", "On hold", 80, 0, 80],
        ["Lotus Yoga Centre", "WA-1810", "USB-32", "Release", "Allocated in full", 25, 25, 0],
        ["Maple Leaf Bakery", "WA-1810", "USB-32", "Release", "Allocated in full", 20, 20, 0],
        ["Cafe Bloom", "WA-1810", "MUG-12", "Release", "Allocated in full", 24, 24, 0],
        ["Cafe Bloom", "WA-1810", "NB-A5", "Release", "Allocated in full", 30, 30, 0],
        ["Oakwood School", "OS/PUR/2026/118", "USB-32", "Release", "Backorder", 30, 0, 30],
        ["Oakwood School", "OS/PUR/2026/118", "NB-A5", "Release", "Allocated in full", 200, 200, 0],
        ["Shaw Fitness Gym", "WA-1910", "WB-500", "Release", "Allocated in full", 40, 40, 0],
        ["Shaw Fitness Gym", "WA-1910", "CAP-B", "Release", "Allocated in full", 40, 40, 0]]
    P2_LOG = ("portal_reference,customer,customer_po,order_date,entered_status\n"
              "SO-5101,Lotus Yoga Centre,WA-1810,2026-10-18,Entered\n"
              "SO-5102,Maple Leaf Bakery,WA-1810,2026-10-18,Entered\n"
              "SO-5103,Cafe Bloom,WA-1810,2026-10-18,Entered\n")

    def p2_run(self):
        import openpyxl
        spec = {"id": "p2", "planned": True, "steps": [
            {"id": "s3-allocate-and-plan", "title": "Allocate and plan", "instruction": "Allocate stock across every order line.",
             "checks": [{"path": "P2_orders_19Oct/dispatch_plan.xlsx", "format": "xlsx"}], "retry_safe": True},
            {"id": "s4-enter-approved-orders", "title": "Enter approved orders", "instruction": self.P2_ENTRY,
             "checks": [{"path": "P2_orders_19Oct/portal_order_entry_log.csv", "format": "csv"}], "retry_safe": True,
             "depends_on": ["s3-allocate-and-plan"]}]}
        work = Path(config.WORK_DIR)
        run = self.runner.create(spec, work_dir=str(work))
        rid = run["run_id"]
        self.runner.claim(rid)
        (work / "P2_orders_19Oct").mkdir(parents=True, exist_ok=True)
        wb = openpyxl.Workbook()
        for row in self.P2_DISPATCH:
            wb.active.append(row)
        wb.save(work / "P2_orders_19Oct" / "dispatch_plan.xlsx")
        self.runner.complete(rid, "s3-allocate-and-plan")
        return rid, work / "P2_orders_19Oct" / "portal_order_entry_log.csv"

    def test_a_removal_recheck_recomputes_what_was_built_on_it(self):
        # Round 5 P1 (2026-10-20), Finding BB: the cancelled bill left the register, its ITC stayed in the return.
        rid, _ = self.p2_run()
        long_task._update(rid, lambda d: d["amendments"].append(
            {"n": 1, "text": "Exclude bill NS-0457 from everything.", "at": 0, "affects": [], "removed": ["NS-0457"]}))
        self.runner.reopen(rid, "s3-allocate-and-plan", "the person changed the request")
        run_state = self.runner.status(rid)
        prompt = long_task.stage_prompt(run_state, run_state["spec"]["steps"][0])
        self.assertIn("Removing NS-0457: take it out and recompute every figure built on it in every output", prompt)
        # Finding BG: what the stage found before stays found.
        long_task._update(rid, lambda d: d["facts"].append(
            {"stage": "s3-allocate-and-plan", "kind": "fact",
             "text": "Purchase GST issue held out of ITC: SP-2291, PK-BOX billed at 18% vs item master 12%"}))
        prompt = long_task.stage_prompt(self.runner.status(rid), run_state["spec"]["steps"][0])
        self.assertIn("Earlier findings of this stage - still true unless the change says otherwise", prompt)
        self.assertIn("- Purchase GST issue held out of ITC: SP-2291", prompt)
        # Drafts to "every" party are one per party, never a count of 1.
        step = dict(run_state["spec"]["steps"][0], drafts=1,
                    instruction="Draft reminders to every customer with anything over 30 days.")
        self.assertIn("for EACH one the stage's rule covers", long_task.stage_prompt(run_state, step))

    # Round 5 P6 (2026-10-20), Finding BI: the request as the person wrote it.
    P6_REQUEST = (
        "Can you triage the support inbox? The 22 emails are in P6_support_inbox/inbox (.eml) in your work folder, with "
        "our refund policy PDF.\n\n1. support_triage_tracker.xlsx: one row per email.\n2. Reply drafts (Outbox) for every "
        "email that needs one, in the customer's language. Refunds up to Rs 5,000: approve them as per the policy. Over "
        "5,000: don't promise anything, list them for my approval.\n3. manager_summary.docx: a short summary for my "
        "manager.\nSave the files in P6_support_inbox/outputs. Drafts only - I'll review and send.")

    def test_each_stage_is_judged_against_its_own_task_whatever_finished_last(self):
        # Round 5 P5 (2026-10-20), Finding BJ: stage 1's retry, after stage 3 had finished, was judged against stage 3's
        # deck - the "Stages already done" list above its instruction read as its task.
        long_task.perform(LONG, None)
        run_state = self.runner.status(self.run_id())
        seen = []
        with mock.patch.object(verify, "judge", lambda task, **k: seen.append(task) or {"verdict": "done"}):
            for step in reversed(run_state["spec"]["steps"]):  # the last stage first, then the earlier ones
                long_task._judge(run_state, step, "Made it.", [], [])
        self.assertEqual(len(seen), 3)
        for task, step in zip(seen, reversed(run_state["spec"]["steps"])):
            self.assertTrue(task.startswith(f"THE STAGE TO CHECK: \"{step['title']}\" - it hands in "
                                            f"{', '.join(step['outputs'])}."), task[:200])
            self.assertNotIn("Stages already done", task)
            others = [s["title"] for s in run_state["spec"]["steps"] if s["id"] != step["id"]]
            head = task.split("Context from the other stages")[0]
            self.assertFalse([t for t in others if f"\"{t}\"" in head])

    # Round 5 P4 (2026-10-20), Finding BL: the person's answer, word for word, and the stage it narrowed.
    P4_ANSWER = ("Skip Priya for now - I'll chase her for the ID. Don't onboard her or make her letter yet. Arif is fine - "
                 "Mohd. is just short for Mohammed. Use the name exactly as in the tracker and go ahead with him.")

    def test_the_persons_answer_binds_the_check_of_a_later_stage(self):
        self.plan["stages"] = [
            {"id": "review", "title": "Review joiners tracker and papers completeness", "does": "Check whose papers are "
             "complete and ask about any doubt.", "outputs": [{"file": "completeness_review.csv"}]},
            {"id": "documents", "title": "Create offer letters and payroll workbook for everyone whose papers are complete",
             "does": "For everyone whose papers are complete, create an offer letter and a payroll row.",
             "outputs": [{"file": "payroll_input.csv"}], "depends_on": ["review"]}]
        self.engine.asks["s1-review"] = "Priya Nair's ID scan is missing and Arif's ID says Mohd. Arif Khan - how to go on?"
        long_task.perform(LONG, None)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": self.P4_ANSWER,
                                                               "excluded": ["Priya"], "names": ["Priya", "Arif"]}]}
        self.message(self.P4_ANSWER)
        run_state = self.runner.status(self.run_id())
        step = run_state["spec"]["steps"][1]
        # The worker's own task carries the answer and is told to name what it left out.
        prompt = long_task.stage_prompt(run_state, step)
        self.assertIn(self.P4_ANSWER, prompt)
        self.assertIn("left out: you said", prompt)
        # The check gets it as binding, in the part that says what is checked - never as "for reference only".
        seen = []
        with mock.patch.object(verify, "judge", lambda task, **k: seen.append(task) or {"verdict": "done"}):
            long_task._judge(run_state, step, "Made 4 letters; Priya Nair left out: you said to skip her until her ID "
                             "arrives.", [], [])
        head, _, context = seen[0].partition("Context from the other stages")
        self.assertIn("BINDING", head)
        self.assertIn(self.P4_ANSWER, head)
        self.assertIn("Left out by the person, so correctly missing: Priya.", head)
        self.assertIn("The answer addresses: Priya, Arif. It leaves out: Priya.", head)
        self.assertNotIn(self.P4_ANSWER, context)
        self.assertNotIn("did not pass its check", head)

    LIVE_REFUSAL = {"verdict": "not_done", "confidence": 0.95,
                    "seen": "The saved results have offer letters and payroll rows for four joiners, but no offer letter or "
                            "payroll row for Priya Nair.",
                    "why": "The tracker shows Priya Nair with Docs status 'Received', so for 'everyone whose papers are "
                           "complete' she should be included; the worker instead omitted her based on a later "
                           "instruction, making the saved result incomplete for this request's source scope."}

    def p4_run(self, judge):
        self.plan["stages"] = [
            {"id": "review", "title": "Review joiners tracker and papers completeness", "does": "Check whose papers are "
             "complete and ask about any doubt.", "outputs": [{"file": "completeness_review.csv"}]},
            {"id": "documents", "title": "Create offer letters and payroll workbook for everyone whose papers are complete",
             "does": "For everyone whose papers are complete, create an offer letter and a payroll row.",
             "outputs": [{"file": "payroll_input.csv"}], "depends_on": ["review"]}]
        self.engine.asks["s1-review"] = "Priya Nair's ID scan is missing and Arif's ID says Mohd. Arif Khan - how to go on?"
        restated = {"stages": [{"id": "s2-documents", "title": "Create offer letters and payroll workbook for everyone "
                                "whose papers are complete - except Priya Nair",
                                "does": "For everyone whose papers are complete, create an offer letter and a payroll row "
                                        "- except Priya Nair (you said: Skip Priya for now - I'll chase her for the ID); "
                                        "Mohammed Arif Khan is complete (you said Mohd. is just short for Mohammed)."}],
                    "summary": "Priya Nair is left out of the letters and payroll until her ID arrives."}

        def model(system, prompt, max_tokens=4000):
            if system is long_task.RESCOPE_SYSTEM:
                return restated
            if system is long_task.ROUTE_SYSTEM:
                return {"for": "PERSON"}
            return self.model(system, prompt, max_tokens)
        seen = []

        def judging(task, **k):
            seen.append(task)
            return judge(task)
        with mock.patch.object(long_task, "_ask_json", model), mock.patch.object(verify, "judge", judging):
            long_task.perform(LONG, None)
            self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": self.P4_ANSWER,
                                                                   "excluded": ["Priya"], "names": ["Priya", "Arif"]}]}
            self.message(self.P4_ANSWER)
        return seen

    def test_the_answer_restates_the_plan_and_the_check_judges_against_it(self):
        def judge(task):
            if "THE STAGE TO CHECK: \"Create offer letters" in task and "left out by the person's own answer" not in task:
                return dict(self.LIVE_REFUSAL)
            return {"verdict": "done", "confidence": 0.95, "seen": "four letters", "why": ""}
        seen = self.p4_run(judge)
        step = self.runner.status(self.run_id())["spec"]["steps"][1]
        self.assertIn("except Priya Nair (you said: Skip Priya for now - I'll chase her for the ID)", step["instruction"])
        self.assertEqual(step["title"], "Create offer letters and payroll workbook for everyone whose papers are complete "
                                        "- except Priya Nair")
        self.assertTrue(any(e.startswith("Plan for") and "Plan updated after your answer: Priya Nair is left out" in e
                            for e in self.emitted))
        documents = [t for t in seen if "THE STAGE TO CHECK: \"Create offer letters" in t]
        self.assertEqual(len(documents), 2)  # refused on Priya, judged once more with the note, accepted
        self.assertIn("except Priya Nair", documents[0])
        self.assertIn("Priya was left out by the person's own answer", documents[1])
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_a_check_that_keeps_counting_the_excluded_person_never_fails_the_stage(self):
        def judge(task):
            if "THE STAGE TO CHECK: \"Create offer letters" in task:
                return dict(self.LIVE_REFUSAL)
            return {"verdict": "done", "confidence": 0.95, "seen": "", "why": ""}
        self.p4_run(judge)
        record = self.runner.status(self.run_id())["steps"]["s2-documents"]
        self.assertEqual(record["status"], "completed")
        self.assertEqual(record.get("checked_by"), "code only")
        self.assertIn("whom the person left out", record.get("check_note", ""))

    def test_the_retry_prompt_never_pushes_the_excluded_person_back_in(self):
        rid, _ = self.p2_run()
        long_task._update(rid, lambda d: d["decisions"].append(
            {"stage": "s3-allocate-and-plan", "question": "Priya's ID is missing - how to go on?", "answer": self.P4_ANSWER,
             "excluded": ["Priya"]}))
        self.runner.claim(rid)
        self.runner.complete(rid, "s4-enter-approved-orders", error="The check found: " + self.LIVE_REFUSAL["seen"])
        run_state = self.runner.status(rid)
        prompt = long_task.stage_prompt(run_state, run_state["spec"]["steps"][1])
        self.assertIn("Priya was left out by the person's own answer", prompt)
        self.assertIn("Do not add them; keep the rest as it is.", prompt)

    def test_output_that_ignores_a_binding_answer_is_still_refused(self):
        # The negative side, in code: a stage that hands in a record the person excluded is not done when the judge
        # says so on the binding answer (the judge sees the answer; the check reports what it found).
        data = {"decisions": [{"stage": "s1", "question": "Priya's ID is missing - how to go on?", "answer": self.P4_ANSWER,
                               "excluded": ["Priya"]},
                              {"stage": "s2", "question": "\"Docs\" did not pass its check after 2 attempt(s).",
                               "answer": "try again"}],
                "amendments": [{"n": 1, "text": "Exclude bill NS-0457.", "removed": ["NS-0457"]}]}
        text = long_task.binding_text(data)
        self.assertIn("Output that follows them is right; output that ignores them is wrong", text)
        self.assertIn("Exclude bill NS-0457", text)
        self.assertIn("Left out by the person, so correctly missing: Priya, NS-0457.", text)
        self.assertNotIn("try again", text)
        self.assertEqual(long_task.binding_text({"decisions": [], "amendments": []}), "")
        long_task.perform(LONG, None)
        run_state = self.runner.status(self.run_id())
        long_task._update(self.run_id(), lambda d: d["decisions"].extend(data["decisions"][:1]))
        step = run_state["spec"]["steps"][0]
        with mock.patch.object(verify, "judge", lambda task, **k: {
                "verdict": verify.NOT_DONE if "BINDING" in task and "Priya Nair" in k.get("claim", "") else verify.DONE,
                "confidence": 0.9, "seen": "Priya Nair has a letter though the person said to skip her", "why": ""}):
            verdict = long_task._judge(run_state, step, "Made 5 letters including Priya Nair.", [], [])
        self.assertEqual(verdict["verdict"], verify.NOT_DONE)
        # The safety net never overrides a refusal for including the excluded person - only one for her absence.
        ledger_now = long_task.ledger(self.run_id())
        self.assertEqual(long_task.refused_for_excluded(
            {"seen": "Priya Nair has a letter though the person said to skip her", "why": ""}, ledger_now), [])
        self.assertEqual(long_task.refused_for_excluded(self.LIVE_REFUSAL, ledger_now), ["Priya"])

    # Round 5 P3 (2026-10-20), Finding CD: the person's answer to the approval stage, word for word.
    P3_ANSWER = ("Approved as proposed. I'll release the payments from the bank portal myself. No, don't change anything. "
                 "That email isn't from their real domain - treat it as suspicious and keep the account in our vendor "
                 "master.")
    P3_KEEP = ("Treat the email received last night as suspicious and keep the bank account from the vendor master; do "
               "not change anything in the payment run.")

    def p3_plan(self):
        self.plan["stages"] = [
            {"id": "match-bills", "title": "Build AP register", "does": "Match the bills.", "outputs": [{"file": "ap.csv"}]},
            {"id": "proposal", "title": "Payment proposal", "does": "Make the payment proposal.",
             "outputs": [{"file": "proposal.csv"}], "depends_on": ["match-bills"]},
            {"id": "disputes", "title": "Dispute list", "does": "List the disputes.", "outputs": [{"file": "disputes.csv"}],
             "depends_on": ["match-bills"]},
            {"id": "approval", "title": "Ask for approval of the payment run", "does": "Ask the person to approve the run.",
             "decision": True, "outputs": [{"file": "approval.csv"}], "depends_on": ["proposal"]}]
        self.engine.asks["s4-approval"] = "Please approve the payment run of Rs 4,28,716.30 for 5 bills, or hold it."

    def test_an_approval_answer_that_keeps_everything_reopens_nothing(self):
        self.p3_plan()
        long_task.perform(LONG + " Then ask me to approve the payment run.", None)
        prompts_before = len(self.engine.prompts)
        self.message_verdict = {"kind": "both", "answers": [{"question": "q1", "answer": self.P3_ANSWER}],
                                "amendment": self.P3_KEEP, "keeps_done": True,
                                "affects": ["s1-match-bills", "s2-proposal", "s3-disputes"]}
        self.message(self.P3_ANSWER)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertEqual({k: v["status"] for k, v in steps.items()},
                         {"s1-match-bills": "completed", "s2-proposal": "completed", "s3-disputes": "completed",
                          "s4-approval": "completed"})
        self.assertFalse([k for k, v in steps.items() if v.get("reopened")])
        rerun = [p for p in self.engine.prompts[prompts_before:] if not p.startswith("Stage 4 of 4")]
        self.assertEqual(rerun, [])  # no finished stage ran again
        self.assertIn("Done", self.finals()[-1]["reply"])
        self.assertTrue(long_task.ledger(self.run_id())["amendments"][-1].get("no_change"))

    def test_the_persons_own_words_decide_and_a_paraphrase_reopens_nothing(self):
        # Whether words change what is done is the model's reading of the person's own words ("keeps_done"), never a
        # keep-clause word list (harness audit, 2026-10-21): a paraphrase no list matched reopened finished work (CD).
        self.assertIn("keeps_done", long_task.CLASSIFY_SYSTEM)
        self.p3_plan()
        long_task.perform(LONG + " Then ask me to approve the payment run.", None)
        before = len(self.engine.prompts)
        self.message_verdict = {"kind": "both", "answers": [{"question": "q1", "answer": self.P3_ANSWER}],
                                "amendment": "Do not change the bank details of Shree Ganesh; use the master's account.",
                                "keeps_done": True, "affects": ["s1-match-bills", "s2-proposal", "s3-disputes"]}
        self.message(self.P3_ANSWER)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertFalse([k for k, v in steps.items() if v.get("reopened")])
        self.assertEqual([p for p in self.engine.prompts[before:] if not p.startswith("Stage 4 of 4")], [])
        self.assertIn("Done", self.finals()[-1]["reply"])
        self.assertTrue(long_task.ledger(self.run_id())["amendments"][-1].get("no_change"))

    def test_an_approval_with_a_real_change_reopens_the_proposal(self):
        self.p3_plan()
        long_task.perform(LONG + " Then ask me to approve the payment run.", None)
        said = "Approved - but pay APX-5607 first"
        self.message_verdict = {"kind": "both", "answers": [{"question": "q1", "answer": said}],
                                "amendment": "Pay APX-5607 first in the payment run.", "affects": ["s2-proposal"]}
        self.message(said)
        self.assertFalse(long_task.ledger(self.run_id())["amendments"][-1].get("no_change"))
        self.assertTrue(any("Stage 2 of 4" in p for p in self.engine.prompts[-6:]))

    def test_a_change_not_known_to_keep_what_is_done_reopens_it(self):
        # Fails safe: a change the model did not read as keeping what is done (or no reading at all) is re-checked.
        self.p3_plan()
        long_task.perform(LONG + " Then ask me to approve the payment run.", None)
        receipt = long_task.amend(self.run_id(), "Approved - but pay APX-5607 first", ["s2-proposal"])
        self.assertIn("re-checking Payment proposal", receipt)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertEqual([k for k, v in steps.items() if v.get("reopened")], ["s2-proposal"])
        receipt = long_task.amend(self.run_id(), "Approved as proposed.", ["s3-disputes"], keeps=True)
        self.assertIn("nothing already done changes", receipt)

    def test_a_real_change_still_reopens_what_it_changes(self):
        self.p3_plan()
        long_task.perform(LONG + " Then ask me to approve the payment run.", None)
        self.message_verdict = {"kind": "amendment", "answers": [], "amendment": "Remove bill APX-5607 from the run.",
                                "affects": ["s2-proposal"]}
        receipt = long_task.amend(self.run_id(), "Remove bill APX-5607 from the run.", ["s2-proposal"])
        self.assertIn("re-checking Payment proposal", receipt)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertEqual(steps["s1-match-bills"]["status"], "completed")
        self.assertEqual(steps["s3-disputes"]["status"], "completed")
        self.assertIn("Remove bill APX-5607", steps["s2-proposal"].get("reopened", ""))

    def test_a_run_ending_releases_its_files_our_apps_still_hold(self):
        # BT2 (2026-10-20): a kept Word still held the memo of a run stopped hours before.
        import io
        import json as _json
        import urllib.parse
        memo = r"C:\Users\me\Documents\Decurix demo\process\P1\Exceptions_memo_Oct2026.docx"
        asked = []

        def urlopen(request, timeout=None):
            url = getattr(request, "full_url", request)
            if "/api/host-files/release" not in str(url):
                return mock.MagicMock(__enter__=lambda s: io.BytesIO(b"{}"), __exit__=lambda *a: False)
            asked.append(urllib.parse.parse_qs(urllib.parse.urlparse(url).query)["path"][0])
            body = _json.dumps({"released": ["WINWORD.EXE 26456"], "held": []}).encode()
            return mock.MagicMock(__enter__=lambda s: io.BytesIO(body), __exit__=lambda *a: False)
        with mock.patch("urllib.request.urlopen", urlopen):
            long_task.perform(LONG + " Save the memo as " + memo + ".", None)
        self.assertEqual(asked, [memo])
        self.assertIn("Released on your computer (an app of mine still had them open): Exceptions_memo_Oct2026.docx",
                      self.finals()[-1]["reply"])
        # A document left open with unsaved changes is the person's to decide: never released.
        data = {"request": "Save the memo as " + memo + "."}
        with mock.patch("urllib.request.urlopen", urlopen):
            lines = long_task._release_host_files(self.runner.status(self.run_id()), data,
                                                  ["Still open in the apps on your computer (not closed, so nothing "
                                                   "unsaved is lost): Exceptions_memo_Oct2026.docx"])
        self.assertEqual(lines, [])

    def test_a_list_for_my_approval_is_a_deliverable_not_a_blocking_decision(self):
        self.plan["stages"] = [
            {"id": "triage", "title": "Triage tracker", "does": "Build the triage tracker.",
             "outputs": [{"file": "support_triage_tracker.csv"}]},
            {"id": "refund-approval-list", "title": "Ask for approval list for refunds over Rs 5,000",
             "does": "List refunds over Rs 5,000 and ask for the person's decision.", "decision": True,
             "for_approval": True,
             "outputs": [{"file": "refunds_over_5000_for_approval.csv"}], "depends_on": ["triage"]}]
        spec = long_task.spec_from_plan(self.plan, [], request=self.P6_REQUEST)
        approval = spec["steps"][1]
        self.assertFalse(approval["decision"])
        self.assertTrue(approval["for_approval"])
        self.assertIn("promise nothing about them anywhere", approval["instruction"])
        # A decision the planner does not mark for approval (P2's "hold it and ask me") stays a blocking one.
        del self.plan["stages"][1]["for_approval"]
        self.assertTrue(long_task.spec_from_plan(self.plan, [], request=P2_REQUEST)["steps"][1]["decision"])
        self.plan["stages"][1]["for_approval"] = True
        long_task.perform(self.P6_REQUEST, None)
        report = self.finals()[-1]["reply"]
        self.assertIn("For your approval, nothing promised: Ask for approval list for refunds over Rs 5,000", report)

    # Round 5 P4 (2026-10-20), Finding BP2: what reached the person, word for word (as far as the coordinator saw it).
    LIVE_STUCK = ("I'm blocked by LibreOffice Writer's \"Search for formatting\" dialog, which opened on top of Find and "
                  "Replace and is not responding to gui clicks or normal close actions, so I can't continue editing the "
                  "offer letter.")

    def test_own_app_trouble_goes_back_once_then_to_the_person_as_what_failed(self):
        self.plan["stages"] = [
            {"id": "letters", "title": "Offer letters", "does": "Make the offer letters.", "outputs": [{"file": "letters.csv"}]},
            {"id": "payroll", "title": "Payroll", "does": "Make the payroll workbook.", "outputs": [{"file": "payroll.csv"}]}]
        engine_perform, stuck = self.engine.perform_task, {"times": 2}

        def perform(prompt, task_file):
            if prompt.startswith("Stage 1 of 2") and stuck["times"]:
                stuck["times"] -= 1
                self.engine.prompts.append(prompt)
                messaging.ask_for_query(self.LIVE_STUCK, kind="query")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = perform
        self.route_reply = {"for": "OWN_TOOLS"}
        long_task.perform(LONG, None)
        retry = [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")]
        self.assertGreaterEqual(len(retry), 2)
        self.assertIn("You asked the person about your own app or screen trouble", retry[1])
        self.assertIn("kill the app and open the file again through the shell", retry[1])
        # Asked again about the same trouble after recovering once: it reaches the person, said as what failed.
        asked = [s for s in self.sent if s["kind"] == "query"]
        self.assertTrue(asked, "the second time it reaches the person")
        self.assertIn("A problem with my own tools that I could not get past", asked[0]["reply"])

    # Round 5 P4 (2026-10-20), Finding BP2b: the next live wording, which the phrase list missed.
    LIVE_MEANS = ("LibreOffice Writer is not reliably accepting replacement text in this document: typing \"Ananya\" into "
                  "the Replace field is being changed by the app back to \"Ananya Deshpande\", so I cannot verify correct "
                  "placeholder replacement on screen. I can finish immediately if you allow me to create the offer letters "
                  "with office update from the same template and then verify each .docx with office read, while keeping "
                  "the payroll_input_Nov2026.xlsx already created and verified.")
    PERSON_QUESTIONS = ("Priya Nair's ID scan is missing - how should I go on?",
                        "The portal form needs a Mobile phone for each joiner, which is not in the tracker. Please provide it.",
                        "Arif's ID says Mohd. Arif Khan - should I go ahead and onboard him?",
                        "Three refunds are over Rs 5,000. Should I approve them?",
                        "The portal shows a login page with a captcha - only you can pass it.")

    def ask_in_stage_one(self, question, verdicts):
        """Run a 2-stage plan whose first stage asks `question` once; the main brain answers from `verdicts`."""
        self.plan["stages"] = [
            {"id": "letters", "title": "Offer letters", "does": "Make the offer letters.", "outputs": [{"file": "letters.csv"}]},
            {"id": "payroll", "title": "Payroll", "does": "Make the payroll workbook.", "outputs": [{"file": "payroll.csv"}]}]
        self.engine.asks["s1-letters"] = question
        asked_model = []

        def model(system, prompt, max_tokens=4000):
            if system is long_task.ROUTE_SYSTEM:
                asked_model.append(prompt)
                return verdicts
            return self.model(system, prompt, max_tokens)
        with mock.patch.object(long_task, "_ask_json", model):
            long_task.perform(LONG, None)
        return asked_model, [s for s in self.sent if s["kind"] == "query"], \
            [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")]

    def test_leave_to_use_another_tool_is_the_workers_own_means(self):
        asked_model, to_person, prompts = self.ask_in_stage_one(self.LIVE_MEANS, {"for": "OWN_TOOLS", "other_means": True})
        self.assertEqual(len(asked_model), 1)  # one reading, with the request as evidence
        self.assertIn(self.LIVE_MEANS[:60], asked_model[0])
        self.assertEqual(to_person, [])
        self.assertIn("The result is what the person asked for; the means are yours unless the request itself names "
                      "them", prompts[1])

    def test_every_question_is_read_once_by_the_main_brain(self):
        question = "The template's fields are greyed out in this editor view; can I fill them another way?"
        asked_model, to_person, prompts = self.ask_in_stage_one(question, {"for": "OWN_TOOLS", "other_means": True})
        self.assertEqual(len(asked_model), 1)
        self.assertIn(question, asked_model[0])
        self.assertIn("The request:", asked_model[0])
        self.assertEqual(to_person, [])
        self.assertIn("the means are yours", prompts[1])

    def test_the_persons_questions_still_reach_the_person(self):
        asked_model, to_person, _prompts = self.ask_in_stage_one(self.PERSON_QUESTIONS[0], {"for": "PERSON"})
        self.assertEqual(len(asked_model), 1)
        self.assertTrue(to_person and self.PERSON_QUESTIONS[0] in to_person[0]["reply"])
        self.assertNotIn("could not get past", to_person[0]["reply"])
        # A model that cannot answer leaves the question with the person.
        with mock.patch.object(long_task, "_ask_json", side_effect=ValueError("no JSON")):
            self.assertEqual(long_task.route_question(self.run_id(), "anything", LONG), ("PERSON", "", False))

    # Round 5 P6 (2026-10-20), Finding BW: the request as written, and the stage's question word for word (its end).
    P6_FULL = ("Can you triage the support inbox? The 22 emails are in P6_support_inbox/inbox (.eml) in your work folder, "
               "with our refund policy PDF. Order details are in the Back Office portal (http://host.docker.internal:8701, "
               "user clerk, password Northwind#2026, Orders).\n\n2. Reply drafts (Outbox) for every email that needs one.")
    LIVE_LEAVE = ("I can draft the three refund approvals up to Rs 5,000 now, but for the other emails the request says to "
                  "\"share delivery update\", \"send tax invoice copy\", or give a resolution/replacement status, and this "
                  "stage's only approved source is the tracker, which does not contain those facts. Please confirm whether "
                  "I should draft only the refund replies now and log the rest as \"NEEDS YOU\", or allow me to use the "
                  "Back Office portal to gather those facts for the remaining drafts.")

    def test_leave_to_use_a_source_the_request_gives_is_sent_back(self):
        self.plan["stages"] = [
            {"id": "triage", "title": "Triage tracker", "does": "Build the triage tracker.",
             "outputs": [{"file": "support_triage_tracker.csv"}]},
            {"id": "replies", "title": "Reply drafts", "does": "Using support_triage_tracker.csv, prepare the replies.",
             "outputs": [{"file": "reply_drafts_log.csv"}], "depends_on": ["triage"]}]
        self.engine.asks["s2-replies"] = self.LIVE_LEAVE
        asked_model = []

        portal = long_task.request_systems(self.P6_FULL)[0]

        def model(system, prompt, max_tokens=4000):
            if system is long_task.ROUTE_SYSTEM:
                asked_model.append(prompt)
                return {"for": "SOURCE", "pointer": "the Back Office portal (http://host.docker.internal:8701"}
            return self.model(system, prompt, max_tokens)
        with mock.patch.object(long_task, "_ask_json", model):
            long_task.perform(self.P6_FULL, None)
        self.assertEqual([s for s in self.sent if s["kind"] == "query"], [])  # never the person
        self.assertEqual(len(asked_model), 1)
        self.assertIn("Order details are in the Back Office portal", portal)
        stage_two = [p for p in self.engine.prompts if p.startswith("Stage 2 of 2")]
        self.assertIn("Systems the request gives - this stage may use them", stage_two[0])
        self.assertIn("Order details are in the Back Office portal", stage_two[0])
        self.assertIn("The request itself gives you it - Order details are in the Back Office portal", stage_two[1])
        self.assertIn("A stage's instruction naming one source never rules out the others", stage_two[1])

    def test_leave_to_use_a_source_the_request_does_not_give_is_a_real_question(self):
        # The model may say SOURCE; code keeps it only for a system or file the request gives, word for word.
        with mock.patch.object(long_task, "_ask_json", lambda *a, **k: {"for": "SOURCE", "pointer": "the CRM"}), \
                mock.patch.object(long_task, "_charge", lambda *a: None):
            self.assertEqual(long_task.route_question("r", "May I use the CRM to look up the delivery dates?",
                                                      self.P6_FULL), ("PERSON", "", False))
        self.assertEqual(long_task.request_systems("Make the register from the attached bills."), [])

    def test_use_your_judgement_never_decides_a_reserved_approval(self):
        self.plan["stages"] = [
            {"id": "triage", "title": "Triage tracker", "does": "Build the triage tracker.",
             "outputs": [{"file": "support_triage_tracker.csv"}]},
            {"id": "refunds", "title": "Refunds for approval", "does": "Ask the person about refunds over Rs 5,000.",
             "decision": True, "outputs": [{"file": "refunds.csv"}], "depends_on": ["triage"]},
            {"id": "summary", "title": "Manager summary", "does": "Write the summary.",
             "outputs": [{"file": "manager_summary.md"}]}]
        self.engine.asks["s2-refunds"] = ("Three refunds are over Rs 5,000 (Harbour 35,960; Northstar 6,900; Oakwood "
                                          "7,600). Should I approve them?")
        long_task.perform(self.P6_REQUEST, None)
        # The hand-back is the model's reading (classify's hands_back); a decision stage's question stays the person's.
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": "Use your judgement and carry on.", "hands_back": True}]}
        self.message("Use your judgement and carry on.")
        data = long_task.ledger(self.run_id())
        self.assertIn("Harbour 35,960", " ".join(data.get("kept_for_person") or []))
        report = self.finals()[-1]["reply"]
        self.assertIn("Kept for your approval (you left it to me, but the request keeps it for you): Three refunds", report)

    def test_drafts_the_facts_support_are_made_and_the_rest_need_you(self):
        # Round 5 P6: the drafts stage stopped every reply for delivery dates the portal does not hold.
        self.plan["stages"] = [
            {"id": "triage", "title": "Triage tracker", "does": "Build the triage tracker.",
             "outputs": [{"file": "support_triage_tracker.csv"}]},
            {"id": "replies", "title": "Reply drafts", "does": "Draft a reply for every email that needs one.",
             "outputs": [{"file": "replies_log.csv"}], "drafts": 2, "depends_on": ["triage"]}]
        self.engine.once = {"s2-replies": "\nNEEDS YOU: Harbour Hotels Group (THIRD delay on PO HH/PO/7745) - the "
                                          "delivery date and LR number are not in the portal"}
        long_task.perform(self.P6_REQUEST, None)
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 2 of 2")][0]
        self.assertIn("draft every one the facts support now", prompt)
        # CC3: 19 drafts took 3.7M tokens one turn at a time - made by one script from a file, the portal read once.
        self.assertIn("make them all with ONE script that runs office email for each row", prompt)
        self.assertIn("Read what you need from a system (a portal) once", prompt)
        kept = long_task.ledger(self.run_id()).get("needs_you") or []
        self.assertEqual(kept, ["(Reply drafts) Harbour Hotels Group (THIRD delay on PO HH/PO/7745) - the delivery date "
                                "and LR number are not in the portal"])
        # ... and the report's "Needs you" names it.
        run_state = self.runner.status(self.run_id())
        long_task._reported.discard(self.run_id())
        long_task.final_report(run_state)
        self.assertIn(kept[0], self.finals()[-1]["reply"])

    def test_a_retry_that_changes_nothing_is_asked_again_in_plain_words(self):
        # Round 5 P4, Finding BH2: after "try again" the retry left the file as it was, and the run ended.
        rid, log_file = self.p2_run()
        log_file.write_text(self.P2_LOG, encoding="utf-8")
        self.runner.claim(rid)
        self.runner.complete(rid, "s4-enter-approved-orders", error="first try")  # attempt 1 used
        self.runner.claim(rid)
        self.runner.park(rid, "s4-enter-approved-orders", "\"Enter\" did not pass its check after 2 attempt(s). What the "
                         "check found: Arif's name mismatch (Mohd. vs Mohammed) is not recorded.\n\nHow should I go on?")
        self.runner.unpark(rid, "s4-enter-approved-orders", "try again")
        state, step = self.runner.claim(rid)
        found = long_task._unchanged_after_retry(state, step, [])
        self.assertEqual(found, "the retry left portal_order_entry_log.csv unchanged, so this is still not done: Arif's "
                                "name mismatch (Mohd. vs Mohammed) is not recorded.")
        self.assertEqual(long_task.MAX_STUCK_ASKS, 3)
        state["steps"][step["id"]]["attempts"] = 1
        step = dict(step, inputs=["/w/inbox/up-1/Offer_letter_template.docx", "/w/inbox/up-1/ID_Arif_Khan.jpg"])
        self.assertIn("never a hand-written zip/XML script", long_task.stage_prompt(state, step))

    def test_a_stage_is_held_only_to_its_own_files(self):
        # Round 5 P1 (2026-10-20), Finding BA: the memo stage carried the request's sentence naming both the deck and
        # the memo, and was refused twice for the deck - stage 4's file.
        from decurix_agent import produced
        folder = "Documents/Decurix demo/process/P1"
        memo_stage = ("Write the exceptions memo as a Word document. The request's own words for what this stage hands "
                      "in (they decide its scope: every item they name, held or not, with the decision shown): \"Save "
                      "the deck as MIS_Oct2026.pptx and the memo as Exceptions_memo_Oct2026.docx in Documents\\Decurix "
                      "demo\\process\\P1 on my PC. The figures in the deck and the memo must match the workbook.\"")
        work = Path(config.WORK_DIR)
        (work / folder).mkdir(parents=True)
        memo = work / folder / "Exceptions_memo_Oct2026.docx"
        memo.write_bytes(b"PK")
        own = [f"{folder}/Exceptions_memo_Oct2026.docx"]
        with mock.patch.object(produced, "_host_paths", lambda text: []):
            self.assertEqual(produced.missing_requested(memo_stage, [str(memo)], root=work, only=own), "")
            self.assertIn("MIS_Oct2026.pptx", produced.missing_requested(memo_stage, [str(memo)], root=work))
            # The deck stage is still held to its deck.
            deck_stage = memo_stage.replace("Write the exceptions memo as a Word document.", "Create the 5-slide MIS deck.")
            self.assertIn("MIS_Oct2026.pptx was not made",
                          produced.missing_requested(deck_stage, [], root=work, only=[f"{folder}/MIS_Oct2026.pptx"]))
            facts = produced.file_facts(LONG, [str(memo)], root=work, named_in=memo_stage, own_outputs=own)
        self.assertEqual(facts.missing_files, "")

    def test_every_approved_record_must_be_in_the_entry_log(self):
        rid, log_file = self.p2_run()
        log_file.write_text(self.P2_LOG, encoding="utf-8")
        run_state = self.runner.status(rid)
        step = run_state["spec"]["steps"][1]
        found = long_task.coverage_problems(run_state, step, [str(log_file)])
        self.assertEqual(len(found), 1)
        self.assertIn("its log has 3 of 5", found[0])
        self.assertIn("Oakwood School", found[0])
        self.assertIn("Shaw Fitness Gym", found[0])
        self.assertNotIn("Harbour", found[0])  # held: never entered
        log_file.write_text(self.P2_LOG + "SO-5104,Oakwood School,OS/PUR/2026/118,2026-10-19,Entered\n"
                            "SO-5105,Shaw Fitness Gym,WA-1910,2026-10-19,Entered\n", encoding="utf-8")
        self.assertEqual(long_task.coverage_problems(run_state, step, [str(log_file)]), [])
        # Without "every" in the stage, nothing is decided.
        quiet = dict(step, instruction="Enter the orders in the portal and log them.")
        log_file.write_text(self.P2_LOG, encoding="utf-8")
        self.assertEqual(long_task.coverage_problems(run_state, quiet, [str(log_file)]), [])

    # Round 5 P3 (2026-10-20), Finding AV: the live stage 3 instruction, as the planner wrote it.
    P3_PROPOSAL = (
        "Using AP_register.xlsx and the vendor details captured from the source files, create payment_proposal.xlsx "
        "listing the bills to pay in this run, in order, with amount, bank account and the total. Apply the payment "
        "selection and ordering rules exactly: MSME vendors first (they must be paid within 45 days of their invoice), "
        "then everyone else by due date (invoice date + their payment terms). Whole bills only, in that order. Never go "
        "over the cash available, Rs 5,00,000. Use the payable amounts established from the matching rules: Match every "
        "bill to its PO and GRN, line by line. Price: up to 2% above the PO rate is fine, pay the bill rate. More than 2% "
        "above: pay the PO rate and dispute the difference. Pay only the quantity we actually received. Include the bills "
        "to pay in this run in order, with amount, bank account and the total, and do not include any payment that would "
        "cause the run to go over the cash available, Rs 5,00,000; because Whole bills only, in that order, a bill that "
        "would take the total over the limit is left out and no later bill is included ahead of it. The request's own "
        "words for what this stage hands in (they decide its scope: every item they name, held or not, with the decision "
        "shown): \"payment_proposal.xlsx: the bills to pay in this run, in order, with amount, bank account and the "
        "total.\"")

    def test_a_selection_is_never_held_to_every_record(self):
        import openpyxl
        spec = {"id": "p3", "planned": True, "steps": [
            {"id": "s1-build-ap-files", "title": "AP files", "instruction": "Match every bill.", "retry_safe": True,
             "checks": [{"path": "P3_payment_run/outputs/AP_register.xlsx", "format": "xlsx"}]},
            {"id": "s3-build-payment-proposal", "title": "Payment proposal", "instruction": self.P3_PROPOSAL,
             "retry_safe": True, "depends_on": ["s1-build-ap-files"],
             "checks": [{"path": "P3_payment_run/outputs/payment_proposal.xlsx", "format": "xlsx"}]}]}
        work = Path(config.WORK_DIR)
        out = work / "P3_payment_run" / "outputs"
        out.mkdir(parents=True, exist_ok=True)
        rid = self.runner.create(spec, work_dir=str(work))["run_id"]
        self.runner.claim(rid)
        vendors = ["Gujarat Pantry Supplies", "Shree Ganesh Packaging", "Sahyadri Print House", "Kaveri IT Peripherals",
                   "Apex Office Supplies", "Bharat Cleaning Products"]
        wb = openpyxl.Workbook()
        wb.active.append(["Vendor", "Bill No", "Match status", "Amount payable"])
        for n, vendor in enumerate(vendors):
            wb.active.append([vendor, f"B-{n}", "Matched", 50000 + n])
        wb.save(out / "AP_register.xlsx")
        self.runner.complete(rid, "s1-build-ap-files")
        run_state = self.runner.status(rid)
        step = run_state["spec"]["steps"][1]
        self.assertFalse(long_task.covers_every(step))
        for shape in ("paid only", "all with status"):
            wb = openpyxl.Workbook()
            wb.active.append(["Serve order", "Vendor", "Amount payable", "Account No", "IFSC", "Status"])
            for n, vendor in enumerate(vendors[:4] if shape == "paid only" else vendors, 1):
                wb.active.append([n, vendor, 50000, "", "", "Pay in this run" if n <= 4 else "NOT IN THIS RUN"])
            wb.save(out / "payment_proposal.xlsx")
            self.assertEqual(long_task.coverage_problems(run_state, step, [str(out / "payment_proposal.xlsx")]), [], shape)
        # The P2 entry stage is still held to every approved order.
        self.assertTrue(long_task.covers_every({"instruction": self.P2_ENTRY, "checks": []}))

    def test_an_answer_part_way_never_narrows_the_stage(self):
        rid, _ = self.p2_run()
        self.runner.claim(rid)
        self.runner.park(rid, "s4-enter-approved-orders", "The quantity box changed while typing Cafe Bloom's order.")
        self.runner.unpark(rid, "s4-enter-approved-orders", "Use your judgement and carry on.")
        run_state = self.runner.status(rid)
        prompt = long_task.stage_prompt(run_state, run_state["spec"]["steps"][1])
        self.assertIn("This stage is NOT finished", prompt)
        self.assertIn("An answer about one record never narrows the stage to that record.", prompt)
        self.assertIn("open it in the application (its list or detail view) and compare every value you entered", prompt)
        self.assertTrue(long_task.APP_ENTRY.search(self.P2_ENTRY))
        self.assertFalse(long_task.APP_ENTRY.search("Create dispatch_plan.xlsx covering every order line."))

    def test_a_wrong_record_saved_in_an_application_heads_the_report(self):
        self.engine.once = {"s2-bank": "\nWRONG IN THE APPLICATION: SO-5103 Cafe Bloom - MUG-12 quantity 60 instead of "
                                       "24, the NB-A5 line missing - the portal cannot edit orders: correct it there by hand"}
        long_task.perform(LONG, None)
        report = self.finals()[0]["reply"]
        self.assertTrue(report.startswith("WARNING - WRONG IN THE APPLICATION - Bank reconciliation: SO-5103 Cafe Bloom"),
                        report[:300])
        self.assertIn("Not everything is done.", report)

    def test_the_report_fits_the_chat_and_the_whole_of_it_is_a_file(self):
        # Round 5 P3 (2026-10-20): an 18 KB report was refused by the control plane; the person never heard the end.
        long_words = "Matched every line to its source and carried the figures forward, line by line. " * 60
        self.engine.report_suffix = long_words
        with mock.patch.object(long_task, "_payment_warnings", lambda run_state, request: []):
            long_task.perform(LONG, None)
        final = self.finals()[0]
        report = final["reply"]
        self.assertLessEqual(len(report), long_task.REPORT_MESSAGE_CHARS + 200)
        self.assertIn("Full report: ", report)
        self.assertIn("Cost:", report)
        saved = Path(report.split("Full report: ", 1)[1].splitlines()[0])
        whole = saved.read_text(encoding="utf-8")
        self.assertIn("It reported:", whole)
        self.assertIn("Done - every stage was made and checked.", whole)
        self.assertNotIn("It reported:", report)
        self.assertEqual(state.current_task, None)  # cleared once reported

    def test_a_payment_to_an_account_not_on_record_heads_the_report(self):
        # Found live (round 5, 2026-10-20): a payment run paid a vendor to the account in a lookalike email.
        found = ["PAYMENT TO AN ACCOUNT FROM A MESSAGE: proposal.xlsx row 2 pays A Vendor to 002501234567."]
        with mock.patch.object(long_task, "_payment_warnings", lambda run_state, request: found):
            long_task.perform(LONG, None)
        report = self.finals()[0]["reply"]
        self.assertTrue(report.startswith("WARNING - PAYMENT TO AN ACCOUNT FROM A MESSAGE"))
        self.assertNotIn("Done - every stage was made and checked.", report)
        self.assertNotIn("Where money goes", " ".join(self.engine.prompts[0].split()))  # taskRules' (one home)

    def test_no_plan_means_the_ordinary_path(self):
        self.plan = {"long": False}
        self.assertFalse(long_task.perform(LONG, None))
        self.assertEqual(self.engine.prompts, [])

    def stuck_bank(self):
        verdicts = iter([{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""},
                         {"verdict": "not_done", "confidence": 0.9, "seen": "bank total wrong", "why": "recompute"},
                         {"verdict": "not_done", "confidence": 0.9, "seen": "still wrong", "why": "recompute"}]
                        + [{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}] * 5)
        patch = mock.patch.object(verify, "judge", lambda *a, **k: next(verdicts))
        patch.start()
        self.addCleanup(patch.stop)
        long_task.perform(LONG, None)

    def test_use_your_judgement_to_a_stuck_stage_is_a_new_attempt_with_guidance(self):
        # Round 5 P6 (2026-10-20), Finding BY: the person's own words to "how should I go on?" re-ran the stage as a
        # "decision" with nothing to apply; it changed nothing and the same question came back 40 s later, twice.
        self.stuck_bank()
        before = len(self.engine.prompts)
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": "Use your judgement and carry on.", "hands_back": True}]}
        self.message("Use your judgement and carry on.")
        rerun = [p for p in self.engine.prompts[before:] if p.startswith("Stage 2 of 3")]
        self.assertEqual(len(rerun), 1)  # a new attempt ran
        self.assertIn("This stage is NOT finished: its previous attempts did not pass the check. What the check found: "
                      "still wrong", rerun[0])
        self.assertIn("The person said: \"Use your judgement and carry on.\" - take it as guidance", rerun[0])
        self.assertNotIn("This decides it", rerun[0])
        self.assertNotIn("you left it to my judgement", rerun[0])
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s2-bank"]["status"], "completed")

    def test_a_persons_own_words_for_skip_and_stop_are_understood(self):
        for words in ("Never mind, not needed.", "Leave it.", "skip it"):
            self.assertTrue(long_task.SKIP_RE.match(words), words)
        for words in ("Cancel", "halt the whole thing", "stop"):
            self.assertTrue(long_task.STOP_RE.match(words), words)
        self.assertFalse(long_task.STOP_RE.match("Use your judgement and carry on."))
        self.stuck_bank()
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Never mind, not needed."}]}
        self.message("Never mind, not needed.")
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s2-bank"]["status"], "failed")
        self.assertIn("you said to leave it out", self.finals()[-1]["reply"])

    def test_a_stage_that_fails_its_checks_asks_instead_of_ending_the_run(self):
        # Round 5, Finding P: a stage out of attempts ended the whole run ("Done: nothing").
        self.stuck_bank()
        bank_prompts = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]
        self.assertEqual(len(bank_prompts), 2)
        self.assertIn("bank total wrong", bank_prompts[1])
        self.assertEqual(self.finals(), [])  # nothing ended
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s2-bank"]["status"], "parked")
        # The stages that do not need it went on.
        self.assertEqual(status["steps"]["s1-register"]["status"], "completed")
        self.assertEqual(status["steps"]["s3-memo"]["status"], "completed")
        asked = [s for s in self.sent if s["kind"] == "query"][-1]["reply"]
        self.assertIn("\"Bank reconciliation\" did not pass its check after 2 attempt(s). What the check found: still wrong", asked)
        self.assertIn("\"try again\"", asked)
        self.assertIn("\"skip it\"", asked)
        self.assertTrue(state.waiting_for_query)

    def test_skip_it_leaves_the_stage_out_and_the_report_says_so(self):
        self.stuck_bank()
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "skip it"}]}
        self.message("skip it")
        report = self.finals()[-1]["reply"]
        self.assertIn("Not done:", report)
        self.assertIn("Bank reconciliation: could not be done", report)
        self.assertIn("you said to leave it out", report)
        self.assertEqual(self.finals()[-1]["kind"], "error")

    def test_try_again_reruns_the_stage_with_the_guidance_leading(self):
        self.stuck_bank()
        self.message_verdict = {"kind": "answer",
                                "answers": [{"question": "q1", "answer": "try again, use the closing balance on page 3"}]}
        self.message("try again, use the closing balance on page 3")
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")][-1]
        self.assertIn("The person said: \"try again, use the closing balance on page 3\" - take it as guidance", prompt.split("Do only this stage")[0])
        self.assertIn("Done - every stage was made and checked.", self.finals()[-1]["reply"])

    def test_a_question_parks_only_what_depends_on_it(self):
        self.engine.asks["s1-register"] = "Bill B-7 charges 18% where the item master says 12% - which is right?"
        long_task.perform(LONG, None)
        # The register asked; the bank reconciliation (independent) was done meanwhile; the memo (needs the register) waits.
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s1-register"]["status"], "parked")
        self.assertEqual(status["steps"]["s2-bank"]["status"], "completed")
        self.assertEqual(status["steps"]["s3-memo"]["status"], "pending")
        posted = [s for s in self.sent if s["kind"] == "query"]
        self.assertTrue(any("carrying on with the parts that do not depend" in s["reply"] for s in posted))
        # When nothing else can run, it asks once, with everything that is waiting.
        self.assertTrue(state.waiting_for_query)
        self.assertIn("Everything that does not depend on your answer is done", posted[-1]["reply"])
        self.assertEqual(self.finals(), [])
        # The answer resumes just the parked stage, then what depends on it.
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Flag it, keep 12%"}]}
        self.message("Flag it, keep 12%")
        prompts = self.engine.prompts
        self.assertIn("The person decided: \"Flag it, keep 12%\"", prompts[-2])
        self.assertIn("Stage 3 of 3", prompts[-1])
        self.assertIn("Flag it, keep 12%", prompts[-1])
        self.assertEqual(len(self.finals()), 1)
        self.assertIn("Your answers I used", self.finals()[0]["reply"])

    def test_an_amendment_is_folded_in_receipted_and_rechecked(self):
        # The register is done; then, while the bank stage runs, the person changes the task.
        engine_perform = self.engine.perform_task

        def bank_with_message(prompt, task_file):
            if "Bank reconciliation." in prompt.split("\n")[0] and not self.classified:
                self.message_verdict = {"kind": "amendment", "amendment": "Leave bill NS-0457 out of everything",
                                        "affects": ["s1-register"]}
                self.message("Bill NS-0457 was cancelled - leave it out of everything.", task_id=OTHER_TASK)
            engine_perform(prompt, task_file)
        self.engine.perform_task = bank_with_message
        sys.modules["decurix_agent.harness"].perform_task = bank_with_message
        long_task.perform(LONG, None)
        # A receipt at once, on the message's own task.
        receipts = [s for s in self.sent if s.get("task_id") == OTHER_TASK]
        self.assertEqual(len(receipts), 1)
        self.assertIn("Noted: Leave bill NS-0457 out of everything", receipts[0]["reply"])
        self.assertIn("re-checking Purchase register", receipts[0]["reply"])
        # DF (round 5 P1, 2026-10-21): the run goes on after the receipt - the chat keeps showing the desktop at work.
        self.assertTrue(receipts[0].get("still_working"))
        # The register was re-checked against it, and the stages after it read it.
        register_prompts = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")]
        self.assertEqual(len(register_prompts), 2)
        self.assertIn("Re-check them because", register_prompts[1])
        self.assertIn("NS-0457", self.engine.prompts[-1])
        report = self.finals()[0]["reply"]
        self.assertIn("Changes you made while I worked:", report)
        self.assertIn("NS-0457", report)
        self.assertIn("re-checked: Purchase register", report)

    def test_an_unclear_message_applies_nothing_and_asks(self):
        self.engine.asks["s1-register"] = "Which bank account is the main one?"
        long_task.perform(LONG, None)
        run_id = self.run_id()
        self.message_verdict = {"kind": "unclear", "ask": "Do you mean HDFC is the main account, or a change to the task?"}
        self.message("HDFC")
        status = self.runner.status(run_id)
        data = long_task.ledger(run_id)
        self.assertEqual(status["steps"]["s1-register"]["status"], "parked")
        self.assertEqual(data["amendments"], [])
        self.assertEqual(data["decisions"], [])
        self.assertEqual(data["confirming"]["message"], "HDFC")
        self.assertIn("Do you mean HDFC", [s for s in self.sent if s["kind"] == "query"][-1]["reply"])
        # The reply to the confirmation is read with the earlier message.
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "HDFC is the main account"}]}
        self.message("yes, the answer")
        self.assertIn("Earlier the person wrote: \"HDFC\"", self.classified[-1])
        self.assertEqual(long_task.ledger(run_id)["decisions"][0]["answer"], "HDFC is the main account")

    def test_no_model_never_guesses_an_answer(self):
        data = {"questions": [{"id": "q1", "text": "Which?", "status": "open"}], "request": LONG}
        run_state = {"spec": {"steps": []}, "steps": {}}
        verdict = long_task.classify("the second one", run_state, data, ask=lambda *a: (_ for _ in ()).throw(ValueError("down")))
        self.assertEqual(verdict["kind"], "unclear")
        verdict = long_task.classify("the second one", run_state, {"questions": [], "request": LONG},
                                     ask=lambda *a: (_ for _ in ()).throw(ValueError("down")))
        self.assertEqual(verdict["kind"], "amendment")

    def test_an_amendment_is_never_the_answer_and_the_answer_leads_the_rerun(self):
        # Only stages that need the answer are left: the question parks its stage and the run asks once.
        self.plan["stages"] = self.plan["stages"][:1] + [dict(self.plan["stages"][2], depends_on=["register"])]
        self.engine.asks["s1-register"] = "Which rate is right for B-7?"
        long_task.perform(LONG, None)
        self.assertTrue(state.waiting_for_query)
        asked = [s for s in self.sent if s["kind"] == "query"][-1]["reply"]
        self.assertNotIn("Everything that does not depend on your answer is done", asked)  # nothing was done yet
        self.assertIn("Which rate is right for B-7?", asked)
        with mock.patch("decurix_agent.watchers.answer_waiting") as answer:
            self.message_verdict = {"kind": "amendment", "amendment": "Also exclude bill B-9", "affects": []}
            self.message("Also leave out B-9")
            asked = [s for s in self.sent if s["kind"] == "query"][-1]["reply"]
            self.assertIn("Noted: Also exclude bill B-9", asked)
            self.assertIn("Still waiting on your answer:", asked)
            self.assertTrue(state.waiting_for_query)
            self.assertEqual(self.runner.status(self.run_id())["steps"]["s1-register"]["status"], "parked")
            # The answer, with a change in the same message: the stage runs again with the answer as the decision,
            # the change in its own place - never the change as the answer.
            self.message_verdict = {"kind": "both", "answers": [{"question": "q1", "answer": "12%"}],
                                    "amendment": "Exclude B-11 too", "affects": []}
            self.message("12% - and exclude B-11 too")
            answer.assert_not_called()
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")][-1]
        head = rerun.split("Do only this stage")[0]
        self.assertIn("The person decided: \"12%\"", head)
        self.assertIn("(You had asked: \"Which rate is right for B-7?\")", head)
        self.assertNotIn("Exclude B-11", head)
        self.assertIn("Exclude B-11 too", rerun)
        self.assertIn("Also exclude bill B-9", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_an_answer_drops_the_stale_rejection_and_uses_no_attempt(self):
        # Round 5, Finding P: the first attempt failed a check; the second asked; the answer re-ran it behind the stale
        # "not accepted" note, with no attempt left.
        verdicts = iter([{"verdict": "not_done", "confidence": 0.9, "seen": "status column wrong", "why": ""}]
                        + [{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}] * 5)
        self.plan["stages"] = self.plan["stages"][:1] + [dict(self.plan["stages"][2], depends_on=["register"])]
        asks = iter([None, "Release which held order?"])
        engine_perform = self.engine.perform_task

        def first_fails_then_asks(prompt, task_file):
            question = next(asks, None) if prompt.startswith("Stage 1 of 2") else None
            if question:
                self.engine.prompts.append(prompt)
                messaging.ask_for_query(question, kind="query")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = first_fails_then_asks
        with mock.patch.object(verify, "judge", lambda *a, **k: next(verdicts)):
            long_task.perform(LONG, None)
            record = self.runner.status(self.run_id())["steps"]["s1-register"]
            self.assertEqual((record["status"], record["attempts"]), ("parked", 1))
            self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Release Cafe Bloom this once"}]}
            self.message("Release Cafe Bloom this once")
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")][-1]
        self.assertIn("The person decided: \"Release Cafe Bloom this once\"", rerun)
        self.assertNotIn("status column wrong", rerun)
        self.assertNotIn("was not accepted", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_an_approval_reply_goes_to_the_gate_unread(self):
        long_task._run = {"run_id": "x", "task_id": TASK, "task_file": None, "request": LONG}
        (self.runner.root / "x").mkdir()
        state.waiting_for_query, state.last_query_kind = True, "approval"
        with mock.patch("decurix_agent.watchers.answer_waiting") as answer, \
                mock.patch.object(long_task, "classify") as classify:
            self.message("yes")
            answer.assert_called_once()
            classify.assert_not_called()

    def test_stop_stops_and_reports_where_it_stands(self):
        self.engine.asks["s1-register"] = "Which rate?"
        long_task.perform(LONG, None)
        self.message("stop")
        report = self.finals()[-1]["reply"]
        self.assertIn("Stopped, as you said", report)
        self.assertIn("Bank reconciliation", report)
        self.assertIn("Memo: not started because it needs: Purchase register", report)
        self.assertEqual(self.runner.status(self.run_id())["status"], "cancelled")
        self.assertFalse(long_task.active())

    def test_an_unrelated_request_waits_for_the_run(self):
        self.engine.asks["s1-register"] = "Which rate?"
        long_task.perform(LONG, None)
        state.waiting_for_query = False
        self.message_verdict = {"kind": "unrelated"}
        path = self.message("What's the weather in Pune?", task_id=OTHER_TASK)
        self.assertTrue(path.exists())
        self.assertNotIn(path.name, state.processed_commands)
        self.assertFalse(long_task.takes(path.name))

    def test_figures_that_disagree_go_back_to_their_stage(self):
        self.figures = {"s1-register": "1,000.00", "s2-bank": "5.00", "s3-memo": "1,200.00"}
        rows = {}

        def read(path):
            stage = Path(path).stem
            figure = rows.get(stage) or {"register": "1,000.00", "memo": "1,200.00", "bank": "5"}[stage]
            return [("Total payable", [figure])] if stage != "bank" else [("Fees", [figure])]

        attempts = []

        def recorded(*a, **k):
            attempts.append(1)
            if len(attempts) > 3:
                rows["memo"] = "1,000.00"  # the memo stage corrected its figure
            return {"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}
        with mock.patch.object(long_task, "_rows", read), mock.patch.object(verify, "judge", recorded):
            long_task.perform(LONG, None)
        memo_prompts = [p for p in self.engine.prompts if p.startswith("Stage 3 of 3")]
        self.assertEqual(len(memo_prompts), 2)
        self.assertIn("does not agree across the outputs", memo_prompts[1])
        report = self.finals()[0]["reply"]
        self.assertIn("Cross-check: 1 figure(s) that appear in several outputs agree.", report)

    def test_a_no_at_an_approval_is_that_stages_and_the_rest_goes_on(self):
        from decurix_agent import guidance

        def bank_refused(prompt, task_file):
            if "Bank reconciliation." in prompt.split("\n")[0]:
                with mock.patch.object(guidance, "refused", side_effect=["no, do not send it", None, None]):
                    messaging.write_final_reply("Stopped before sending, as you said.", kind="final")
                return
            self.engine.perform_task.__func__(self.engine, prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = bank_refused
        long_task.perform(LONG, None)
        bank = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]
        self.assertEqual(bank, [])  # asked once, never sent back to ask again
        report = self.finals()[-1]["reply"]
        self.assertIn("Bank reconciliation: could not be done after 1 attempt(s) - you said no at its approval", report)
        self.assertIn("and it did not finish another way", report)
        self.assertIn("Memo", report.split("Not done:")[0])

    def test_a_refused_command_does_not_fail_a_stage_that_finished_another_way(self):
        from decurix_agent import guidance
        engine_perform = self.engine.perform_task

        def bank_other_way(prompt, task_file):
            if "Bank reconciliation." in prompt.split("\n")[0]:
                with mock.patch.object(guidance, "refused", side_effect=["no, don't send anything", None, None]):
                    engine_perform(prompt, task_file)  # the engine carries on and makes the file
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = bank_other_way
        long_task.perform(LONG, None)
        report = self.finals()[-1]["reply"]
        self.assertIn("Done - every stage was made and checked.", report)
        self.assertIn("You declined one of its commands (\"no, don't send anything\"); it was done another way.", report)
        self.assertEqual(len([p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]), 1)

    def test_the_stage_prompt_carries_the_requests_rules_and_every_ask(self):
        request = P2_REQUEST + "\n\nFiles attached to this request, saved on this desktop:\n  /home/headless/work/a.eml\n  /home/headless/work/chat.txt"
        self.plan["stages"][0]["inputs"] = ["/home/headless/work/chat.txt"]
        with mock.patch.object(workflow_task, "_dispatch"):
            long_task.perform(request, None)
        run_state = self.runner.status(self.run_id())
        prompt = long_task.stage_prompt(run_state, run_state["spec"]["steps"][1])
        first = long_task.stage_prompt(run_state, run_state["spec"]["steps"][0])
        for rule in ("5% off the rate on any line of 50 or more units", "allocate first come, first served",
                     "takes them over their credit limit, hold it and ask me", "I'll send them myself",
                     "3. dispatch_plan.xlsx: every order line", "1. Enter every approved order"):
            self.assertIn(rule, prompt)
        self.assertIn("The whole request, for its rules and context", prompt)
        self.assertTrue(prompt.startswith("Stage 2 of 3"))
        # A stage that names its inputs gets those as its files, and the request's other inputs as sources it may read
        # (Finding BM: a stage given only some asked the person for a column of another); one that names none gets
        # every attached file.
        self.assertIn("chat.txt", first.split("The request's own input files")[0])
        self.assertNotIn("a.eml", first.split("The request's own input files")[0])
        self.assertIn("a.eml", first.split("The request's own input files")[1])
        self.assertIn("a.eml", prompt)

    def test_work_done_while_a_decision_was_open_is_rechecked_against_it(self):
        # Round 5, Finding Z: a stage that did not depend on the decision (the planner's slip) ran while it was open and
        # stayed wrong once it was given. The register finished before the question: it is left alone.
        self.plan["stages"] = [
            {"id": "register", "title": "Purchase register", "does": "Build the register.", "outputs": [{"file": "register.csv"}]},
            {"id": "holds", "title": "Credit holds", "does": "Work out the holds and ask.", "outputs": [{"file": "holds.csv"}],
             "depends_on": ["register"]},
            {"id": "replies", "title": "Replies", "does": "Write the replies.", "outputs": [{"file": "replies.csv"}],
             "depends_on": ["register"]}]
        self.figures = {"s3-replies": "Cafe Bloom on hold"}
        self.engine.asks["s2-holds"] = "Cafe Bloom and Harbour Hotels Group are over their limits - release or hold?"
        long_task.perform(LONG, None)
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s3-replies"]["status"], "completed")  # ran while the question was open
        before = len(self.engine.prompts)
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues.",
             "names": ["Cafe Bloom", "Harbour Hotels Group"]}]}
        self.message("Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues.")
        rerun = self.engine.prompts[before:]
        replies = [p for p in rerun if p.startswith("Stage 3 of 3")]
        self.assertEqual(len(replies), 1)
        self.assertIn("The person decided: \"Release Cafe Bloom this once", replies[0].split("Do only this stage")[0])
        self.assertIn("its outputs name Cafe Bloom", replies[0])
        self.assertFalse([p for p in rerun if p.startswith("Stage 1 of 3")])  # finished before the question
        self.assertTrue(any("Re-checking against your decision: Replies" in e for e in self.emitted))

    # Series 15 H1 (2026-10-21), held-out expense audit: the approval question and the person's answer, word for word.
    H1_QUESTION = ("Three claims need your decision before I go on:\n1. CL-4122 Kavya Iyer - cab from the client site, Rs "
                   "840, no receipt attached; the policy needs a receipt over Rs 500. I assumed it is not reimbursed until "
                   "a receipt comes.\n2. CL-4142 Neha Gupta - airport taxi, Rs 1,850, no receipt; the policy's prepaid "
                   "fare is lower.\n3. CL-4153 Farhan Ali - hotel at Rs 7,400 a night over the Rs 6,000 cap.\nFor each, "
                   "choose one: approve Kavya's claim, approve it in part, or reject it, and the same for the others.")
    H1_ANSWER = ("Approve Neha's airport taxi without the receipt, but pay only Rs 1,200 - that is the prepaid taxi fare "
                 "from Delhi airport to her hotel. Note it as approved by me.")

    def test_who_is_left_out_is_the_classifiers_reading_never_a_word_list(self):
        # "without", "only" in an approval left Neha out, and her correct draft was refused (series 15 H1).
        approval = {"decisions": [{"stage": "s2", "question": self.H1_QUESTION, "answer": self.H1_ANSWER}]}
        self.assertEqual(long_task.excluded_names(approval), [])
        read = long_task._reading({"names": ["Neha", "CL-4142"], "excluded": [], "undecided": ["CL-4122", "CL-4153"]},
                                  self.H1_ANSWER, question=self.H1_QUESTION)
        self.assertEqual(read, {"names": ["Neha", "CL-4142"], "undecided": ["CL-4122", "CL-4153"]})
        # A name neither the message nor the question writes is never kept.
        self.assertEqual(long_task._reading({"excluded": ["Rahul"]}, self.H1_ANSWER, question=self.H1_QUESTION), {})
        # "Skip Priya for now" still leaves Priya out - by the reading, and a later answer can put her back.
        skip = {"decisions": [{"stage": "s1", "question": "Priya's ID is missing - how to go on?", "answer": self.P4_ANSWER,
                               "excluded": ["Priya"]}]}
        self.assertEqual(long_task.excluded_names(skip), ["Priya"])
        skip["decisions"].append({"stage": "s1", "question": "And Priya?", "answer": "Her ID came - include Priya.",
                                  "restored": ["Priya"]})
        self.assertEqual(long_task.excluded_names(skip), [])

    def test_the_binding_text_says_what_an_answer_addresses_and_what_it_leaves_undecided(self):
        data = {"decisions": [{"stage": "s2", "question": self.H1_QUESTION, "answer": self.H1_ANSWER,
                               "names": ["Neha", "CL-4142"], "undecided": ["CL-4122", "CL-4153"]}], "amendments": []}
        text = long_task.binding_text(data)
        self.assertIn("The answer addresses: Neha, CL-4142.", text)
        self.assertIn("It does not decide: CL-4122, CL-4153 - nothing about them is the person's decision", text)
        self.assertIn("3. CL-4153 Farhan Ali", text)  # the whole question, never its first 300 characters
        self.assertIn("An item an answer does not address is not decided by it.", text)
        self.assertNotIn("correctly missing", text)  # nobody was left out
        self.assertNotIn("It leaves out", text)

    def test_the_planner_puts_work_the_decision_changes_after_it_and_carries_the_rules(self):
        rules = " ".join(long_task.PLAN_SYSTEM.split())
        for words in ("Every stage whose result can change with the person's answer runs after the decision stage "
                      "and depends on it",
                      "Copy each rule of the request word for word into every stage it applies to"):
            self.assertIn(words, rules)

    def test_stop_while_a_stage_runs_is_reported_once_when_the_stage_ends(self):
        # Round 5, Finding AA: a stop was read and nothing more reached the chat.
        engine_perform = self.engine.perform_task
        cancelled = []
        sys.modules["decurix_agent.harness"].cancel_running = lambda: cancelled.append(1) or True

        def bank_stopped(prompt, task_file):
            if prompt.startswith("Stage 2 of 3"):
                self.message("stop")
            engine_perform(prompt, task_file)  # the cancelled job still reports, as soul does
        sys.modules["decurix_agent.harness"].perform_task = bank_stopped
        with mock.patch.object(long_task, "STOP_GRACE_SECONDS", 5):
            long_task.perform(LONG, None)
        self.assertEqual(cancelled, [1])
        self.assertTrue(any(e.startswith("Stopping the request in hand") for e in self.emitted))
        finals = self.finals()
        self.assertEqual(len(finals), 1)
        self.assertIn("Stopped, as you said", finals[0]["reply"])
        self.assertIn("Memo: not started", finals[0]["reply"])
        self.assertEqual(self.runner.status(self.run_id())["status"], "cancelled")
        self.assertFalse(long_task.active())
        self.assertFalse(state.waiting_for_query)

    def test_stop_reports_in_bounded_time_when_the_stage_does_not_end(self):
        long_task.perform(LONG, None)  # a finished run gives the run and its ledger; then pretend a stage is running
        run_id = self.run_id()
        self.sent.clear()
        long_task._reported.discard(run_id)
        self.runner.reopen(run_id, "s2-bank", "a test")
        long_task._run = {"run_id": run_id, "task_id": TASK, "task_file": None, "request": LONG}
        workflow_task._active = (run_id, "s2-bank", None)
        workflow_task._driving = True
        sys.modules["decurix_agent.harness"].cancel_running = lambda: True  # the job never comes back
        with mock.patch.object(long_task, "STOP_GRACE_SECONDS", 0.2):
            long_task.stop("stop")
            self.assertEqual(self.finals(), [])  # not before the grace
            deadline = time.time() + 5
            while not self.finals() and time.time() < deadline:
                time.sleep(0.05)
        self.assertEqual(len(self.finals()), 1)
        self.assertIn("Stopped, as you said", self.finals()[0]["reply"])
        # The stage's engine ends late: nothing more is said.
        workflow_task._driving = False
        long_task.final_report(self.runner.status(run_id))
        self.assertEqual(len(self.finals()), 1)

    def test_stop_with_a_question_parked_and_a_stage_running(self):
        self.engine.asks["s1-register"] = "Which rate?"
        engine_perform = self.engine.perform_task
        sys.modules["decurix_agent.harness"].cancel_running = lambda: True

        def bank_stopped(prompt, task_file):
            if prompt.startswith("Stage 2 of 3"):
                state.waiting_for_query = False
                self.message("stop")
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = bank_stopped
        long_task.perform(LONG, None)
        finals = self.finals()
        self.assertEqual(len(finals), 1)
        self.assertIn("Stopped, as you said", finals[0]["reply"])
        self.assertIn("Purchase register: waiting for your answer", finals[0]["reply"])
        self.assertEqual(self.runner.status(self.run_id())["status"], "cancelled")

    def test_a_report_that_cannot_be_made_still_says_it_stopped(self):
        self.engine.asks["s1-register"] = "Which rate?"
        long_task.perform(LONG, None)
        with mock.patch.object(long_task, "ledger", side_effect=RuntimeError("ledger unreadable")):
            long_task.stop("stop")
        reply = self.finals()[-1]["reply"]
        self.assertIn("Stopped, as you said. Nothing more is started.", reply)
        self.assertIn("ledger unreadable", reply)
        self.assertFalse(long_task.active())

    # ---- Finding AE: a request of its own, sent while one is in hand ----

    NEW_P2 = P2_REQUEST + ("\n\nFiles attached to this request, saved on this desktop:\n"
                           "  /home/headless/work/inbox/up-3e6255a18c5b/price_list.xlsx\n"
                           "  /home/headless/work/inbox/up-3e6255a18c5b/stock_today.xlsx")

    def parked_run(self):
        self.engine.asks["s1-register"] = "Which rate is right for B-7?"
        long_task.perform(LONG, None)
        return self.run_id()

    def queries(self):
        return [s for s in self.sent if s["kind"] == "query"]

    def test_a_new_request_while_a_question_is_parked_is_never_its_answer(self):
        run_id = self.parked_run()
        before = self.runner.status(run_id)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Use the new files instead"}]}
        self.message(self.NEW_P2, task_id=OTHER_TASK)
        self.assertEqual(self.classified, [])  # not even put to the model as a possible answer
        data = long_task.ledger(run_id)
        self.assertEqual(data["decisions"], [])
        self.assertEqual([q["status"] for q in data["questions"]], ["open"])
        self.assertEqual(self.runner.status(run_id)["steps"], before["steps"])
        self.assertEqual(data["pending_request"]["why"], "it brought files of its own")
        asked = [s for s in self.sent if s.get("task_id") == OTHER_TASK][-1]
        self.assertEqual(asked["kind"], "query")  # a real question: the chat waits for the person
        self.assertIn("You have a request in progress:", asked["reply"])
        self.assertIn("1 of 3 stages done", asked["reply"])  # the bank stage went on
        self.assertIn("waiting on your answer to: \"Which rate is right for B-7?\"", asked["reply"])
        self.assertIn("finish that one first and then do this new one", asked["reply"])
        self.assertIn("or is this meant as your answer to my question?", asked["reply"])

    def test_finish_first_queues_the_new_request_behind_the_run(self):
        run_id = self.parked_run()
        self.message(self.NEW_P2, task_id=OTHER_TASK)
        self.message("Finish that one first, then do the new one.", task_id=OTHER_TASK)
        held = [p for p in Path(config.COMMAND_DIR).glob("*.txt")]
        names = [p.name for p in held if p.read_text(encoding="utf-8") == self.NEW_P2]
        self.assertEqual(len(names), 1)
        self.assertIn(OTHER_TASK, names[0])  # it runs under its own task
        self.assertFalse(long_task.takes(names[0]))  # left for when the run is over
        self.assertNotIn("pending_request", long_task.ledger(run_id))
        self.assertTrue(any("will start as soon as this one is finished" in e for e in self.emitted))
        self.assertEqual(self.runner.status(run_id)["status"], "awaiting_answer")

    def test_drop_that_one_stops_the_run_and_queues_the_new_one(self):
        run_id = self.parked_run()
        self.message(self.NEW_P2, task_id=OTHER_TASK)
        self.message("Drop that one and do this one instead.", task_id=OTHER_TASK)
        self.assertEqual(self.runner.status(run_id)["status"], "cancelled")
        self.assertIn("Stopped, as you said", self.finals()[-1]["reply"])
        self.assertTrue([p for p in Path(config.COMMAND_DIR).glob("*.txt") if p.read_text(encoding="utf-8") == self.NEW_P2])

    def test_meant_as_the_answer_is_read_as_the_message_it_was(self):
        run_id = self.parked_run()
        long_reply = "Use 12% for B-7, and this is a long note. " * 20  # long: taken for a request of its own
        self.message(long_reply)
        self.assertIn("pending_request", long_task.ledger(run_id))
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "12% for B-7"}]}
        self.message("It was my answer to your question.")
        self.assertEqual(long_task.ledger(run_id)["decisions"][0]["answer"], "12% for B-7")
        self.assertIn("Use 12% for B-7", self.classified[-1])

    def test_the_same_request_sent_again_is_a_request_not_an_answer(self):
        self.assertEqual(long_task.new_request_reason(
            "Bill NS-0457 was cancelled - leave it out of the register, the GST summary, the deck and the memo.",
            {"request": LONG}), "")  # a change that names the outputs it touches is still a change
        self.assertEqual(long_task.new_request_reason(P2_REQUEST, {"request": P2_REQUEST}),
                         "it is the request in hand sent again")
        self.assertEqual(long_task.new_request_reason("Release Cafe Bloom this once, hold Harbour Hotels Group.",
                                                      {"request": P2_REQUEST}), "")

    def test_an_answer_or_change_question_on_its_own_task_is_a_question(self):
        self.parked_run()
        self.message_verdict = {"kind": "unclear", "ask": "Is this your answer to my question, or a change to the task?"}
        self.message("HDFC", task_id=OTHER_TASK)
        asked = [s for s in self.sent if s.get("task_id") == OTHER_TASK][-1]
        self.assertEqual(asked["kind"], "query")
        self.assertEqual(asked["reply"], "Is this your answer to my question, or a change to the task?")

    def test_a_secret_the_request_gave_is_kept_out_of_the_ledger_and_the_models_view(self):
        from decurix_agent.computer import privacy
        privacy.remember_secrets(P2_REQUEST)
        run_id = self.parked_run()
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": "For the portal type user clerk and password Northwind#2026"}]}
        self.message("12%. The portal password is Northwind#2026 if you need it.")
        text = (self.runner.root / run_id / "ledger.json").read_text(encoding="utf-8")
        self.assertNotIn("Northwind#2026", text)
        self.assertIn("[withheld]", text)
        self.assertNotIn("Northwind#2026", self.classified[-1])
        self.assertNotIn("Northwind#2026", json.dumps(self.runner.status(run_id)))

    # ---- Findings AH and AI ----

    def test_a_retry_says_the_stage_is_not_finished_and_to_do_all_that_is_left(self):
        # Round 5, AI: "portal_entry_log.csv missing" and "Correct it where it is - do not start over" made the model
        # write the CSV of the 3 orders it had entered and stop; two orders were never entered.
        verdicts = iter([{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""},
                         {"verdict": "not_done", "confidence": 0.9, "seen": "portal_entry_log.csv missing", "why": ""}]
                        + [{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}] * 5)
        engine_perform = self.engine.perform_task

        def entry(prompt, task_file):
            if prompt.startswith("Stage 2 of 3") and len([p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]) == 0:
                self.engine.prompts.append(prompt)
                messaging.write_final_reply("Entered Cafe Bloom, Lotus Yoga Centre and Maple Leaf Bakery in the portal.",
                                            kind="final")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = entry
        with mock.patch.object(verify, "judge", lambda *a, **k: next(verdicts)):
            long_task.perform(LONG, None)
        retry = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")][1]
        self.assertIn("This stage is NOT finished", retry)
        self.assertIn("do ALL that is left, not only what the check named", retry)
        self.assertIn("never redo or enter anything twice", retry)
        self.assertIn("Entered Cafe Bloom, Lotus Yoga Centre and Maple Leaf Bakery", retry)  # what was done
        self.assertNotIn("Correct it where it is - do not start over", retry)

    def test_a_stage_that_ran_out_of_steps_carries_on_without_asking(self):
        out = ("I ran out of steps (79 commands) before this was finished, and stopped rather than start over.\n\n"
               "Entered 3 of 5 orders.\n\nTell me how to go on, or reply `stop`.")
        self.engine.asks["s2-bank"] = out
        long_task.perform(LONG, None)
        self.assertEqual([s for s in self.sent if s["kind"] == "query"], [])  # nobody was asked
        bank = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]
        self.assertEqual(len(bank), 2)
        self.assertIn("its previous session ran out of room before the end", bank[1])
        self.assertIn("Done - every stage was made and checked.", self.finals()[-1]["reply"])
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s2-bank"]["attempts"], 1)  # no attempt used

    def test_running_out_of_room_again_and_again_is_put_to_the_person(self):
        out = "I ran out of steps (79 commands) before this was finished, and stopped rather than start over."
        engine_perform = self.engine.perform_task

        def always_out(prompt, task_file):
            if prompt.startswith("Stage 2 of 3"):
                self.engine.prompts.append(prompt)
                messaging.ask_for_query(out, kind="query")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = always_out
        long_task.perform(LONG, None)
        self.assertEqual(len([p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]),
                         long_task.MAX_CONTINUATIONS + 1)
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s2-bank"]["status"], "parked")
        self.assertIn("I ran out of steps", [s for s in self.sent if s["kind"] == "query"][-1]["reply"])

    def test_a_job_that_hit_its_time_limit_carries_on(self):
        engine_perform = self.engine.perform_task
        said = []

        def timed_out_once(prompt, task_file):
            if prompt.startswith("Stage 2 of 3") and not said:
                said.append(1)
                self.engine.prompts.append(prompt)
                messaging.write_final_reply("I stopped before finishing - ran out of time (the job limit is 25m0s).\n\n"
                                            "Entered 3 of 5 orders.", kind="final")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = timed_out_once
        long_task.perform(LONG, None)
        bank = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")]
        self.assertEqual(len(bank), 2)
        self.assertIn("ran out of time", bank[1])
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_an_ordering_rule_is_applied_by_one_script_on_the_per_line_attribute(self):
        # Round 5, AH: the allocation gave an item added later in a chat its order's first time, twice.
        live = ("Using the extracted orders, today's stock, the price list, and the customer master, create a planning "
                "workbook that allocates short items first come, first served by the time the customer asked for that "
                "item; marks part-ship quantities as allocated now and the rest as backorder.")
        self.assertIn("first come, first served by the time the customer asked for that item",
                      long_task.ordering_rule(live))
        self.assertEqual(long_task.ordering_rule("Write the memo of exceptions."), "")
        self.plan["stages"][1]["does"] = live
        long_task.perform(LONG, None)
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")][0]
        self.assertIn("apply it in one script over a table that records, per item, the attribute it orders by as its "
                      "source gives it", prompt)
        self.assertIn("show that attribute and the order served", prompt)

    def test_the_planner_records_per_line_what_a_later_rule_orders_by(self):
        rules = " ".join(long_task.PLAN_SYSTEM.split())
        self.assertIn("records that attribute for every line, in the rule's own words", rules)
        self.assertIn("as the source gives it for that line", rules)

    # ---- Finding AJ: the live texts of round 5's P2 run on pc2 (chat entries 66-68) ----

    LIVE_Q = ("I found two extraction ambiguities that need your decision. 1) Daniel Shaw's chat says '70 water bottles "
              "WB-500 and 40 navy caps CAP-B', but my text extraction also sees a stray '40' near WB-500 in one parse; I "
              "would record WB-500 as 70 and CAP-B as 40 unless you want otherwise. Reply with confirmation or corrections.")
    LIVE_Q_SOURCE = ("Nisha Menon's chat says 'make it 50 totes' and later 'actually 40' - I would record TOTE-C as 40 "
                     "unless you want otherwise.")

    def test_use_your_judgement_takes_the_stated_default_and_never_reasks(self):
        self.engine.asks["s1-register"] = self.LIVE_Q_SOURCE
        long_task.perform(LONG, None)
        run_id = self.run_id()
        asked_before = len([s for s in self.sent if s["kind"] == "query"])
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": "Use your judgement and carry on.", "hands_back": True}]}
        self.message("Use your judgement and carry on.")
        self.assertEqual(len([s for s in self.sent if s["kind"] == "query"]), asked_before)  # nothing asked again
        decision = long_task.ledger(run_id)["decisions"][0]["answer"]
        self.assertIn("you said to use my judgement, so I go with what I proposed", decision)
        self.assertIn("I would record TOTE-C as 40 unless you want otherwise", decision)
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertIn("The person decided: \"Use your judgement and carry on.", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_a_question_about_its_own_tooling_goes_back_to_the_stage_once(self):
        self.engine.asks["s1-register"] = self.LIVE_Q
        self.route_reply = {"for": "OWN_READING"}
        long_task.perform(LONG, None)
        self.assertEqual([s for s in self.sent if s["kind"] == "query"], [])  # the person was not asked
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertIn("You asked the person about your own reading of a source", rerun)
        self.assertIn("read the source again", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_asked_about_its_own_tooling_twice_the_person_is_asked(self):
        engine_perform = self.engine.perform_task

        def always(prompt, task_file):
            if prompt.startswith("Stage 1 of 3"):
                self.engine.prompts.append(prompt)
                messaging.ask_for_query(self.LIVE_Q, kind="query")
                return
            engine_perform(prompt, task_file)
        sys.modules["decurix_agent.harness"].perform_task = always
        self.route_reply = {"for": "OWN_READING"}
        long_task.perform(LONG, None)
        self.assertEqual(len([p for p in self.engine.prompts if p.startswith("Stage 1 of 3")]), 2)
        self.assertEqual(self.runner.status(self.run_id())["steps"]["s1-register"]["status"], "parked")

    def test_the_person_never_sees_internal_ids(self):
        self.engine.asks["s1-register"] = "Which rate is right for B-7?"
        long_task.perform(LONG, None)
        self.message_verdict = {"kind": "unclear", "ask": "Do you want me to confirm q1, or change s2-bank?"}
        self.message("HDFC")
        asked = [s for s in self.sent if s["kind"] == "query"][-1]["reply"]
        self.assertNotIn("q1", asked)
        self.assertNotIn("s2-bank", asked)
        self.assertIn("my question (\"Which rate is right for B-7?...\")", asked)
        self.assertIn("\"Bank reconciliation\"", asked)

    # ---- Finding AK, and Z tightened (round 5's P2 run on pc2) ----

    LIVE_AK = ("The portal is showing unit prices like 4.2 for MUG-12, while our approved prices from the workbook are in "
               "rupees like 149 and 175.75. Before I enter orders, I need one decision from you: should I type our rupee "
               "prices exactly as given despite the portal's field apparently using a different scale, or should I stop "
               "and let you confirm how this portal expects unit price values?")
    OVERWRITE = "The portal still shows the old catalogue prices - overwrite them with ours."

    def test_the_request_settles_a_question_only_by_its_own_words(self):
        with mock.patch.object(long_task, "_charge", lambda *a: None):
            with mock.patch.object(long_task, "_ask_json", lambda *a, **k: {"for": "SETTLED", "pointer": self.OVERWRITE}):
                self.assertEqual(long_task.route_question("r", self.LIVE_AK, P2_REQUEST)[:2], ("SETTLED", self.OVERWRITE))
            with mock.patch.object(long_task, "_ask_json",
                                   lambda *a, **k: {"for": "SETTLED", "pointer": "Overwrite the portal prices."}):
                self.assertEqual(long_task.route_question("r", self.LIVE_AK, P2_REQUEST)[0], "PERSON")  # not its words

    def test_a_question_the_request_settles_goes_back_to_the_stage(self):
        settled = []

        def model(system, prompt, max_tokens=4000):
            if system is long_task.ROUTE_SYSTEM:
                settled.append(prompt)
                return {"for": "SETTLED", "pointer": self.OVERWRITE}
            return self.model(system, prompt, max_tokens)
        self.engine.asks["s2-bank"] = self.LIVE_AK
        with mock.patch.object(long_task, "_ask_json", model):
            long_task.perform(P2_REQUEST, None)
        self.assertEqual([s for s in self.sent if s["kind"] == "query"], [])  # the person was not asked
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 2 of 3")][-1]
        self.assertIn("The request already says: \"" + self.OVERWRITE + "\"", rerun)
        self.assertIn("Follow it and carry on", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_a_deliverable_made_before_the_decision_is_rechecked_after_it(self):
        # The live shape: the stage that worked out the holds also made dispatch_plan.xlsx; the decision came after.
        self.plan["stages"] = [
            {"id": "read", "title": "Read orders", "does": "Read the orders.", "outputs": [{"file": "orders_working.csv"}]},
            {"id": "allocate", "title": "Allocate and check credit", "does": "Allocate and work out the holds.",
             "outputs": [{"file": "dispatch_plan.csv"}], "depends_on": ["read"]},
            {"id": "decide", "title": "Credit hold decision", "does": "Ask which held orders to release.",
             "decision": True, "affects": ["allocate"], "outputs": [{"file": "credit_hold_decision.csv"}],
             "depends_on": ["allocate"]}]
        request = LONG + " Make dispatch_plan.csv: every order line with ordered and allocated quantities."
        self.engine.asks["s3-decide"] = "Release Cafe Bloom or keep it on hold?"
        long_task.perform(request, None)
        steps = {s["id"]: s for s in self.runner.status(self.run_id())["spec"]["steps"]}
        self.assertTrue(steps["s2-allocate"].get("before_decision"))
        self.assertFalse(steps["s1-read"].get("before_decision"))  # the decision does not affect it
        before = len(self.engine.prompts)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Release Cafe Bloom this once."}]}
        self.message("Release Cafe Bloom this once.")
        rerun = self.engine.prompts[before:]
        allocate = [p for p in rerun if p.startswith("Stage 2 of 3")]
        self.assertEqual(len(allocate), 1)
        self.assertIn("can change with the decision it was made before", allocate[0])
        self.assertIn("The person decided: \"Release Cafe Bloom this once.", allocate[0])
        self.assertFalse([p for p in rerun if p.startswith("Stage 1 of 3")])

    def test_invented_accounts_are_refused_and_the_retry_leads_with_the_master(self):
        # Round 5 P3 (2026-10-20), Finding AT: the proposal gave the rows not paid in this run made-up accounts
        # (11112222333344 / ICIC0001111) that are in no input; the check refused it, the stage got stuck.
        import os
        work = Path(config.WORK_DIR)
        master = work / "vendor_master.csv"
        master.write_text("Vendor,Account No,IFSC\nApex Office Supplies,000405012345,ICIC0000004\n"
                          "Bharat Cleaning Products,917020034567812,UTIB0000234\n", encoding="utf-8")
        os.utime(master, (1577836800, 1577836800))
        self.plan["stages"] = [self.plan["stages"][2], {"id": "proposal", "title": "Build payment proposal",
                                "does": "Make the payment proposal of the bills to pay in this run, with amount and bank account.",
                                "outputs": [{"file": "payment_proposal.csv"}]}]
        prompts = []
        others = self.engine.perform_task

        def engine(prompt, task_file):
            if "proposal" not in workflow_task._active[1]:
                return others(prompt, task_file)
            prompts.append(prompt)
            invented = len(prompts) < 3
            rows = (["Apex Office Supplies,107970.00,11112222333344,ICIC0001111,Not in this run",
                     "Bharat Cleaning Products,75520.00,99887766554433,SBIN0000456,Not in this run"] if invented else
                    ["Apex Office Supplies,107970.00,,,Not in this run",
                     "Bharat Cleaning Products,75520.00,,,Not in this run"])
            (work / "payment_proposal.csv").write_text("Vendor,Amount payable,Account No,IFSC,Status\n" + "\n".join(rows)
                                                       + "\n", encoding="utf-8")
            messaging.write_final_reply("Made payment_proposal.csv with accounts for every vendor.", kind="final")
        sys.modules["decurix_agent.harness"].perform_task = engine
        long_task.perform(LONG + " Then make payment_proposal.csv for this week's payment run.", None)
        asked = [s for s in self.sent if s["kind"] == "query"]
        self.assertTrue(asked and "PAYMENT TO AN ACCOUNT ON NO RECORD" in asked[-1]["reply"], asked)
        self.assertIn("11112222333344", asked[-1]["reply"])
        self.assertNotIn("Where money goes", prompts[0])  # (the operator's taskRules say it, once)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "try again"}]}
        self.message("try again")
        self.assertEqual(len(prompts), 3)
        lead = prompts[2].split("This stage:")[0]
        self.assertIn("Take them ONLY from", lead)
        self.assertIn("vendor_master.csv", lead)
        self.assertIn("leave them empty where a row has none", lead)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_an_allocation_made_before_the_decision_is_redone_first_after_it(self):
        # Round 5 P2 (2026-10-20), Finding AW - the live plan shape: stage 1 extracted AND allocated into a working file
        # the request does not name, stage 2 asked about the credit holds, stage 3 entered orders from stage 1's file.
        self.plan["stages"] = [
            {"id": "extract", "title": "Extract orders and allocate",
             "does": "Extract today's orders, apply pricing, allocate stock and prepare decision support.",
             "outputs": [{"file": "order_workings.csv"}]},
            {"id": "decide", "title": "Credit hold decision", "does": "Ask which held orders to release.",
             "decision": True, "affects": ["extract"], "outputs": [{"file": "credit_hold_decision.csv"}],
             "depends_on": ["extract"]},
            {"id": "enter", "title": "Enter approved orders",
             "does": "Enter every approved order in the portal from the allocation in order_workings.csv; log them.",
             "outputs": [{"file": "portal_log.csv"}], "depends_on": ["decide"]}]
        self.engine.asks["s2-decide"] = "Release Cafe Bloom or keep it on hold? Harbour Hotels is over its limit too."
        long_task.perform(P2_REQUEST, None)
        steps = {s["id"]: s for s in self.runner.status(self.run_id())["spec"]["steps"]}
        self.assertTrue(steps["s1-extract"].get("before_decision"))
        before = len(self.engine.prompts)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1",
                                                              "answer": "Release Cafe Bloom; hold Harbour Hotels."}]}
        self.message("Release Cafe Bloom; hold Harbour Hotels.")
        rerun = self.engine.prompts[before:]
        order = [p.split(":")[0] for p in rerun]
        self.assertEqual(order[0], "Stage 1 of 3 of a longer request a person made")  # the computation first
        self.assertTrue(any(p.startswith("Stage 3 of 3") for p in rerun[1:]))
        first = rerun[0]
        self.assertIn("Redo it from the sources with the decision applied, by the request's own rules", first)

    def test_a_credit_decision_leaves_the_pricing_stage_alone(self):
        # Round 5 P2 (2026-10-20), Finding CG: the live plan's stage 1 extracted and priced the orders into a working
        # workbook later stages add to; the decision reopened it, and its re-check failed it for a later stage's sheet.
        self.plan["stages"] = [
            {"id": "extract-orders", "title": "Extract every order and price it in a working workbook",
             "does": "Extract every order line and price it from the price list into the working workbook.",
             "outputs": [{"file": "order_working.csv"}]},
            {"id": "credit-hold-decision", "title": "Credit hold decision", "does": "Ask which held orders to release.",
             "decision": True, "outputs": [{"file": "credit_hold_decision.csv"}], "depends_on": ["extract-orders"]},
            {"id": "allocate-and-plan", "title": "Allocate and plan", "does": "Allocate stock from order_working.csv.",
             "outputs": [{"file": "dispatch_plan.csv"}], "depends_on": ["extract-orders", "credit-hold-decision"]}]
        self.engine.asks["s2-credit-hold-decision"] = "Release Cafe Bloom or keep it on hold? Harbour is over too."
        long_task.perform(P2_REQUEST, None)
        steps = {s["id"]: s for s in self.runner.status(self.run_id())["spec"]["steps"]}
        self.assertFalse(steps["s1-extract-orders"].get("before_decision"))
        before = len(self.engine.prompts)
        answer = "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues."
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": answer}]}
        self.message(answer)
        rerun = self.engine.prompts[before:]
        self.assertFalse([p for p in rerun if p.startswith("Stage 1 of 3")])  # pricing untouched
        status = self.runner.status(self.run_id())["steps"]
        self.assertEqual(status["s1-extract-orders"]["status"], "completed")
        self.assertFalse(status["s1-extract-orders"].get("reopened"))
        self.assertEqual(status["s2-credit-hold-decision"]["status"], "completed")
        allocate = [p for p in rerun if p.startswith("Stage 3 of 3")]
        self.assertTrue(allocate and answer in allocate[0])  # the stage after carries the decision

    def test_an_answered_decision_stage_is_judged_on_its_file_not_on_asking(self):
        # Round 5 P2 on pc3 (2026-10-20), Finding CM: after the person answered, the decision stage was refused twice -
        # "The stage also required asking the user in chat ... no chat question from this run is shown" - and the
        # extraction stage before it was reopened for the "dispatch" in dispatch_plan.xlsx.
        self.plan["stages"] = [
            {"id": "extract", "title": "Extract every order line and pricing basis into dispatch_plan.xlsx",
             "does": "Extract every order line and pricing basis into dispatch_plan.xlsx.",
             "outputs": [{"file": "dispatch_plan.csv"}]},
            {"id": "credit-hold", "title": "List orders to hold for credit approval and ask the user",
             "does": "List every order over its credit limit in credit_hold_review.csv with the figures, and ask the "
                     "user in chat which held orders to release or keep on hold.",
             "decision": True, "outputs": [{"file": "credit_hold_review.csv"}], "depends_on": ["extract"]},
            {"id": "allocate", "title": "Allocate stock and complete dispatch_plan.xlsx",
             "does": "Allocate stock and complete dispatch_plan.csv.", "outputs": [{"file": "dispatch_plan.csv"}],
             "depends_on": ["credit-hold"]}]
        self.engine.asks["s2-credit-hold"] = "Release Cafe Bloom or keep it on hold? Harbour is over too."
        judged = []

        def judge(task, **kwargs):
            judged.append(task)
            return {"verdict": "done", "confidence": 0.95, "seen": "the file follows the answer", "why": ""}
        with mock.patch.object(verify, "judge", judge):
            long_task.perform(P2_REQUEST, None)
            steps = {s["id"]: s for s in self.runner.status(self.run_id())["spec"]["steps"]}
            self.assertFalse(steps["s1-extract"].get("before_decision"))  # a file's name is no work
            before, checks = len(self.engine.prompts), len(judged)
            answer = "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues."
            self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": answer}]}
            self.message(answer)
        rerun = self.engine.prompts[before:]
        self.assertFalse([p for p in rerun if p.startswith("Stage 1 of 3")])
        decision = [p for p in rerun if p.startswith("Stage 2 of 3")]
        self.assertTrue(decision)
        self.assertIn("Record the decision in this stage's file", decision[0])
        self.assertIn("now shows what they decided, in their words", decision[0])
        check = next(t for t in judged[checks:] if "List orders to hold for credit approval" in t)
        self.assertIn("THE STAGE TO CHECK: \"List orders to hold for credit approval\"", check)
        self.assertNotIn("ask the user", check.split("BINDING")[0].lower())
        self.assertIn("the person answered \"" + answer, check)
        self.assertIn("not that a question was asked", check)
        status = self.runner.status(self.run_id())["steps"]
        self.assertEqual(status["s2-credit-hold"]["status"], "completed")
        self.assertFalse(status["s1-extract"].get("reopened"))

    def test_only_work_the_decision_affects_is_redone_after_it_and_never_on_a_retry(self):
        # CM: the planner says which stages a decision affects ("affects"); no word list reads the stage's text or its
        # file names. With "affects" missing, nothing is redone. "Try again" decides nothing.
        plan = {"long": True, "title": "Orders", "stages": [
            {"id": "allocate", "title": "Allocate stock and complete dispatch_plan.xlsx",
             "does": "Allocate stock and complete dispatch_plan.xlsx.", "outputs": [{"file": "dispatch_plan.xlsx"}]},
            {"id": "credit-hold", "title": "List orders to hold for credit approval and ask the user",
             "does": "Ask which held orders to release.", "decision": True,
             "outputs": [{"file": "credit_hold_review.xlsx"}], "depends_on": ["allocate"]}]}
        self.assertFalse(long_task.spec_from_plan(plan, [], request=P2_REQUEST)["steps"][0].get("before_decision"))
        plan["stages"][1]["affects"] = ["allocate"]
        spec = long_task.spec_from_plan(plan, [], request=P2_REQUEST)
        self.assertTrue(spec["steps"][0].get("before_decision"))
        self.assertEqual(spec["steps"][1]["affects"], ["s1-allocate"])
        rid = self.runner.create(spec, work_dir=str(config.WORK_DIR))["run_id"]
        self.runner.claim(rid)
        import openpyxl
        book = openpyxl.Workbook()
        book.active.append(["Customer", "SKU", "Ordered", "Allocated"])
        book.active.append(["Cafe Bloom", "MUG-12", 24, 24])
        book.save(Path(config.WORK_DIR) / "dispatch_plan.xlsx")
        self.runner.complete(rid, "s1-allocate")
        self.assertEqual(self.runner.status(rid)["steps"]["s1-allocate"]["status"], "completed")
        stuck = {"id": "q2", "stage": "s2-credit-hold", "kind": "stuck", "at": 0,
                 "text": "\"List orders to hold\" did not pass its check after 2 attempt(s). How should I go on?"}
        long_task._recheck_after_decision(rid, stuck, "try again")
        self.assertEqual(self.runner.status(rid)["steps"]["s1-allocate"]["status"], "completed")
        long_task._recheck_after_decision(rid, {"id": "q1", "stage": "s2-credit-hold", "at": 0, "text": "Release?"},
                                          "Release Cafe Bloom this once, hold Harbour Hotels Group.")
        self.assertNotEqual(self.runner.status(rid)["steps"]["s1-allocate"]["status"], "completed")

    def test_a_restatement_never_adds_a_word_nobody_said(self):
        # Round 5 P2 on pc3 (2026-10-20), Finding CN: after "hold Harbour Hotels Group until they clear their dues", the
        # judge required "every line of held order HH/PO/7781 to show allocated quantity 0 and full ordered quantity as
        # backorder"; the code fact requires a held line with 0 backordered. Each attempt failed one of them.
        answer = "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues."
        question = "Release Cafe Bloom or keep it on hold? Harbour is over too."
        old = "Allocate stock and complete dispatch_plan.xlsx."
        invented = ("Allocate stock and complete dispatch_plan.xlsx; every line of held order HH/PO/7781 shows allocated "
                    "quantity 0 and full ordered quantity as backorder.")
        self.assertIn("backorder", long_task.restatement_adds(invented, old, question + " " + answer))
        kept = ("Allocate stock and complete dispatch_plan.xlsx - except Harbour Hotels Group's (you said: hold Harbour "
                "Hotels Group until they clear their dues); Cafe Bloom released (you said: release Cafe Bloom this once).")
        self.assertEqual(long_task.restatement_adds(kept, old, question + " " + answer), [])
        rescope = " ".join(long_task.RESCOPE_SYSTEM.split())
        self.assertIn("Never add a rule about how to handle it", rescope)
        self.assertIn("a restatement with any other word is not taken", rescope)
        # A restatement that invents one is not taken: the stage keeps its own text.
        rid, _ = self.p2_run()
        long_task._update(rid, lambda d: d.update(request=P2_REQUEST))
        step_id = "s4-enter-approved-orders"
        with mock.patch.object(long_task, "_ask_json", lambda *a, **k: {"stages": [
                {"id": step_id, "title": "Enter approved orders",
                 "does": "Enter every approved order; the held order's full ordered quantity goes as backorder."}]}):
            self.assertEqual(long_task.rescope_after_answer(rid, question, answer), [])
        self.assertEqual(self.runner.status(rid)["spec"]["steps"][1]["instruction"], self.P2_ENTRY)

    def test_figures_with_rows_the_input_repeats_are_loud_never_a_failure(self):
        # Harness audit (2026-10-21, amended): a total that includes exact duplicate rows of an input is a domain-free
        # fact, but a repeat may be real - it fails no stage, heads the report and comes first under "Needs you".
        seen = ("A figure may include rows the input repeats: 3.64 crore is the Net Amount (Rs) total including the rows "
                "sales_export_Aug2026.csv repeats (exact copies of earlier rows); without them it is 34,040,974.")
        judged = []
        with mock.patch.object(long_task, "repeated_rows", lambda made, reply, request="": [seen] if made else []), \
                mock.patch.object(verify, "judge", lambda task, **k: judged.append(task) or {
                    "verdict": "done", "confidence": 0.9, "seen": "", "why": ""}):
            self.assertTrue(long_task.perform(LONG, None))
        report = self.finals()[-1]["reply"]
        self.assertTrue(report.startswith("Done - but 3 figures include rows the input repeats (see Needs you)."), report)
        needs = report.split("Needs you:")[1]
        self.assertIn("Please check this figure: (Purchase register) " + seen, needs)
        self.assertNotIn(seen, report.split(long_task.PLEASE_CHECK)[-1] if long_task.PLEASE_CHECK in report else "")
        self.assertEqual(len(self.engine.prompts), 3)  # nothing was sent back
        # The judge saw it as evidence, not as a verdict.
        self.assertIn("What code checks noticed in this stage's files - evidence", judged[0])
        self.assertIn("sales_export_Aug2026.csv repeats", judged[0])

    def test_an_advisory_check_never_fails_a_stage_and_the_person_sees_it(self):
        # PM decision (2026-10-20): new code checks refused correct live work seven times in a day. Since the harness
        # audit (2026-10-21) only domain-free invariants block; every other check is evidence, under "Please check".
        self.assertFalse(long_task.is_blocking("status_conflicts"))
        self.assertFalse(long_task.is_blocking("a check nobody listed"))
        self.assertTrue(long_task.is_blocking("file_facts"))
        seen = "register.xlsx says SO-5101 is open while the draft says approved"
        with mock.patch.object(long_task, "_status_conflicts", lambda *a: [seen] if a[2] else []):
            self.assertTrue(long_task.perform(LONG, None))
        report = self.finals()[-1]["reply"]
        self.assertIn("Done - every stage was made and checked.", report)
        self.assertIn(long_task.PLEASE_CHECK, report)
        self.assertIn("(Purchase register) " + seen, report)
        self.assertEqual(len(self.engine.prompts), 3)  # nothing was sent back

    def test_a_blocking_check_still_fails_the_stage(self):
        with mock.patch.object(long_task, "app_emptied",
                               lambda made: ["register.xlsx: saved from the app, it keeps 0 of the 40 words"] if made else []):
            long_task.perform(LONG, None)
        self.assertGreater(len(self.engine.prompts), 3)  # sent back
        self.assertIn("it keeps 0 of the 40 words", " ".join(self.engine.prompts[3:]))
        self.assertNotIn(long_task.PLEASE_CHECK, " ".join(s["reply"] for s in self.finals()))

    # Round 5 P2 on pc3 (2026-10-20), after CM: the live plan's stage 1, word for word from its checkpoint.
    LIVE_EXTRACT = (
        "Read the 4 WhatsApp chat exports, the 2 POs that came by email (.eml, the PO PDF is inside), the price list, "
        "today's stock and the customer master, using the copies in P2_orders_19Oct. Work out every order per customer "
        "PO or chat. For chats, customers change their minds in the chats - go by what they finally want. Record every "
        "order line for every order with the customer, source order identifier, SKU, final ordered quantity, the day "
        "they ordered, and for each line the time the customer asked for that item so later allocation can apply "
        "allocate first come, first served by the time the customer asked for that item. Also record for each chat "
        "order that Order date = the day they ordered (for a chat, the day of their first message for that order) and "
        "that for chat orders use WA-<ddmm of that day> as the customer PO, e.g. WA-1810. Price every line from our "
        "price list, 5% off the rate on any line of 50 or more units of one SKU in an order. The portal still shows the "
        "old catalogue prices - overwrite them with ours. Include the customer master details needed later, including "
        "what the customer already owes, their credit limit, and state for GST treatment. Save a working workbook with "
        "the extracted orders and priced lines.")

    def test_extraction_and_pricing_are_never_redone_after_a_decision(self):
        # The live plan: the decision affects only the dispatch plan, which comes after it - nothing before is redone.
        plan = {"long": True, "title": "Orders", "stages": [
            {"id": "extract-orders", "title": "Extract every order and price it", "does": self.LIVE_EXTRACT,
             "outputs": [{"file": "P2_orders_19Oct/order_working.xlsx"}]},
            {"id": "credit-hold-decision", "title": "List orders to hold if they take customers over credit limit and "
             "ask for decision", "does": "List every order over its credit limit and ask which to release or hold.",
             "decision": True, "affects": ["dispatch-plan"], "outputs": [{"file": "P2_orders_19Oct/credit_hold_review.xlsx"}],
             "depends_on": ["extract-orders"]},
            {"id": "dispatch-plan", "title": "Create dispatch plan for every order line after credit decision",
             "does": "Allocate stock first come, first served.", "outputs": [{"file": "P2_orders_19Oct/dispatch_plan.xlsx"}],
             "depends_on": ["extract-orders", "credit-hold-decision"]}]}
        spec = long_task.spec_from_plan(plan, [], request=P2_REQUEST)
        self.assertFalse(spec["steps"][0].get("before_decision"))

    def test_a_recheck_after_a_decision_refuses_only_on_what_was_decided(self):
        rid, _ = self.p2_run()
        answer = "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues."
        record = {"reopened": long_task.DECISION_RECHECK + " (what it hands in can change with the decision it was made "
                              "before), and the person has since decided: \"" + answer + "\"", "answer": answer}
        step = {"id": "s3-allocate-and-plan", "title": "Extract every order and price it"}
        # With no reading of what the decision names, a refusal stands: nothing is softened on a guess.
        no_reading = {"verdict": verify.NOT_DONE, "seen": "WA-1810 shared", "why": ""}
        self.assertEqual(long_task._recheck_refusal(rid, step, record, no_reading)["verdict"], verify.NOT_DONE)
        long_task._update(rid, lambda d: d["decisions"].append(
            {"stage": "s2", "question": "Release?", "answer": answer, "names": ["Cafe Bloom", "Harbour Hotels Group"]}))
        live = {"verdict": verify.NOT_DONE, "seen": "multiple different WhatsApp customers on 18/10/2026 are all given "
                "the same customer PO WA-1810", "why": "the file collapses separate customers' 18/10 chat orders"}
        kept = long_task._recheck_refusal(rid, step, record, live)
        self.assertEqual(kept["verdict"], verify.UNSURE)
        advisories = long_task.ledger(rid)["advisories"]
        self.assertEqual(advisories[-1]["check"], "recheck_after_decision")
        self.assertIn("WA-1810", " ".join(long_task.please_check(long_task.ledger(rid))))
        about = {"verdict": verify.NOT_DONE, "seen": "Harbour Hotels Group's lines are still allocated", "why": ""}
        self.assertEqual(long_task._recheck_refusal(rid, step, record, about)["verdict"], verify.NOT_DONE)
        # A first check (no decision re-check) is never softened.
        self.assertEqual(long_task._recheck_refusal(rid, step, {"reopened": ""}, live)["verdict"], verify.NOT_DONE)

    # Round 5 P6 on pc3 (2026-10-21), Finding CU: the live email and the live refusal.
    E18 = ("Your invoice for SO-5114 charges the compact umbrellas at Rs 329 each, but the agreed rate on our PO NSL/4502 "
           "is Rs 299. Please correct and reissue.")
    CU_REFUSAL = ("An email asking for money back must be categorized as Refund ... 1019_E18.eml as Complaint ... instead "
                  "of a Refund")

    def test_a_retry_keeps_a_right_answer_the_check_misread_and_the_judge_sees_why(self):
        refused = []

        def judge(task, **kwargs):
            if "THE STAGE TO CHECK: \"Purchase register\"" in task and not refused:
                refused.append(task)
                return {"verdict": verify.NOT_DONE, "confidence": 0.9, "seen": self.CU_REFUSAL, "why": ""}
            return {"verdict": "done", "confidence": 0.95, "seen": "", "why": ""}
        with mock.patch.object(verify, "judge", judge):
            long_task.perform(LONG, None)
        retry = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][1]
        self.assertIn("The check is not the authority - the sources are.", retry)
        self.assertIn("Kept <item> as <value>: <the source's own words>", retry)
        self.assertIn("correct the notes, not the answer", retry)
        # The working is asked for generically, never as a business rule of the harness's own.
        self.assertIn("Record how you computed each figure - its inputs, where each was read, the result", retry)
        # The second check sees the kept answer and its quote as binding.
        rid = self.run_id()
        run_state = self.runner.status(rid)
        reply = ("Updated the tracker.\nKept 1019_E18.eml as Complaint: \"" + self.E18 + "\" - the check's reading "
                 "(an email asking for money back) does not match the source.")
        seen = []
        with mock.patch.object(verify, "judge", lambda task, **k: seen.append(task) or {"verdict": "done"}):
            long_task._judge(run_state, run_state["spec"]["steps"][0], reply, [], [])
        self.assertIn("The worker kept these answers against the previous check, quoting the source - BINDING", seen[0])
        self.assertIn("Kept 1019_E18.eml as Complaint: \"Your invoice for SO-5114", seen[0])
        self.assertIn("never on the worker's working notes alone", seen[0])
        self.assertIn("The worker's working notes are not a source", " ".join(verify.JUDGE_SYSTEM.split()))

    # Round 5 P1 on pc3 (2026-10-21), Finding DB: the stage's own report, as the parked question quoted it.
    DB_DONE = ("Done. - Checked what was already in place in P1_month_end/outputs/month_end_close_Oct2026.xlsx. - Updated "
               "the Bank Rec sheet with the reconciling items. - Recalculated and read the saved workbook back.")

    def db_engine(self, report):
        self.engine.reports = {"s1-register": report}

    def test_a_stage_that_says_done_goes_to_its_check_never_parked(self):
        self.db_engine(self.DB_DONE)
        verdicts = [{"verdict": verify.CANNOT, "confidence": 0.6, "seen": "", "why": "could not confirm"},
                    {"verdict": "done", "confidence": 0.9, "seen": "the Bank Rec sheet", "why": ""}]
        notes = []

        def judge(task, **kwargs):
            if "THE STAGE TO CHECK: \"Purchase register\"" in task and verdicts:
                notes.append(task)
                return verdicts.pop(0)
            return {"verdict": "done", "confidence": 0.95, "seen": "", "why": ""}
        with mock.patch.object(verify, "judge", judge):
            self.assertTrue(long_task.perform(LONG, None))
        self.assertFalse([s for s in self.sent if "could not be finished" in str(s.get("reply"))])
        self.assertIn("The worker's report says this stage is DONE and names no obstacle", notes[-1])
        self.assertIn("Done - every stage was made and checked.", self.finals()[-1]["reply"])

    def test_try_again_to_a_stopped_stage_is_a_new_attempt_never_a_question_back(self):
        self.db_engine("I could not finish: the bank statement CSV would not open.")
        calls = []

        def judge(task, **kwargs):
            if "THE STAGE TO CHECK: \"Purchase register\"" in task and not calls:
                calls.append(task)
                return {"verdict": verify.CANNOT, "confidence": 0.8, "seen": "", "why": "the worker stopped"}
            return {"verdict": "done", "confidence": 0.95, "seen": "", "why": ""}
        with mock.patch.object(verify, "judge", judge):
            long_task.perform(LONG, None)
            asked = [s for s in self.sent if s["kind"] == "query"]
            self.assertTrue(asked and "could not be finished" in asked[-1]["reply"])
            self.assertEqual(long_task.ledger(self.run_id())["questions"][-1]["kind"], "stuck")
            self.db_engine(self.DB_DONE)
            before = len(self.engine.prompts)
            self.message_verdict = {"kind": "unclear", "answers": [], "amendment": "", "affects": [],
                                    "ask": "Do you want me to retry the bank reconciliation stage, or were you referring "
                                           "to a different part of the month-end close?"}
            self.message("try again")
        self.assertFalse(self.classified)  # read in code: never put to the classifier, never asked back
        retry = [p for p in self.engine.prompts[before:] if p.startswith("Stage 1 of 3")]
        self.assertTrue(retry)
        self.assertIn("its previous attempt stopped before the end. What it reported: I could not finish", retry[0])
        self.assertIn("The person said: \"try again\"", retry[0])

    def test_the_run_is_still_working_until_it_is_over_or_waits_on_the_person(self):
        # DF: running while a stage runs or is to run; not when only parked stages are left, nor when it is over.
        self.engine.asks["s2-bank"] = "Which bank account is the main one?"
        long_task.perform(LONG, None)
        rid = self.run_id()
        statuses = {k: v["status"] for k, v in self.runner.status(rid)["steps"].items()}
        self.assertEqual(statuses["s2-bank"], "parked")
        self.assertFalse(long_task.run_still_working(rid))  # waiting on the person: not running
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "The HDFC one."}]}
        self.message("The HDFC one.")
        self.assertFalse(long_task.run_still_working(rid))  # over
        self.assertFalse(long_task.run_still_working("another-run"))

    def test_without_asking_keeps_the_work_and_drops_the_asking(self):
        self.assertEqual(long_task.without_asking("List orders to hold for credit approval and ask the user"),
                         "List orders to hold for credit approval")
        self.assertEqual(long_task.without_asking("List every order over its limit with the figures. The stage also "
                                                  "asks the user in chat which held orders to release."),
                         "List every order over its limit with the figures.")
        self.assertEqual(long_task.without_asking("Ask the person to approve the payment run."),
                         "Record what the person decided.")

    def test_a_rechecked_stage_is_judged_on_its_own_sheets(self):
        import openpyxl
        rid, _ = self.p2_run()
        book = Path(config.WORK_DIR) / "P2_orders_19Oct" / "order_working.xlsx"
        wb = openpyxl.Workbook()
        wb.active.title = "Order lines"
        wb.active.append(["Customer", "SKU", "Qty"])
        wb.create_sheet("Order summary").append(["Customer", "Total"])
        wb.save(book)
        long_task._snapshot_sheets(rid, "s3-allocate-and-plan", [str(book)])
        wb = openpyxl.load_workbook(book)
        wb.create_sheet("Credit hold review").append(["Customer", "Decision"])
        wb.save(book)
        self.assertEqual(long_task.sheets_added_later(rid, "s3-allocate-and-plan", [str(book)]),
                         {"order_working.xlsx": ["Credit hold review"]})
        seen = []
        run_state = self.runner.status(rid)
        with mock.patch.object(verify, "judge", lambda task, **k: seen.append(task) or {"verdict": "done"}):
            long_task._judge(run_state, run_state["spec"]["steps"][0], "Made it.", [str(book)], [])
        self.assertIn("In order_working.xlsx, the sheet(s) Credit hold review were added by later stages - not this "
                      "stage's to check", seen[0])

    def test_a_stage_that_only_reads_the_sources_is_not_redone(self):
        self.plan["stages"] = [
            {"id": "read", "title": "Read orders", "does": "Read the chats and POs and list every order line.",
             "outputs": [{"file": "order_lines.csv"}]},
            {"id": "decide", "title": "Credit hold decision", "does": "Ask which held orders to release.",
             "decision": True, "outputs": [{"file": "credit_hold_decision.csv"}], "depends_on": ["read"]},
            {"id": "allocate", "title": "Allocate", "does": "Allocate stock from order_lines.csv.",
             "outputs": [{"file": "dispatch_plan.csv"}], "depends_on": ["decide"]}]
        spec = long_task.spec_from_plan(self.plan, [], request=LONG)
        self.assertFalse(spec["steps"][0].get("before_decision"))  # no "affects": nothing is redone

    def test_the_planner_makes_answer_dependent_deliverables_after_the_decision(self):
        rules = " ".join(long_task.PLAN_SYSTEM.split())
        self.assertIn("the stage that works out what needs deciding makes only that", rules)
        self.assertIn("is made in a stage after the decision stage, never in one before it", rules)

    def test_a_broken_stage_machinery_ends_the_task_with_a_report(self):
        with mock.patch.object(workflow_task, "_dispatch", side_effect=RuntimeError("checkpoint unreadable")):
            long_task.perform(LONG, None)
        self.assertEqual(self.finals()[-1]["kind"], "error")
        self.assertIn("checkpoint unreadable", self.finals()[-1]["reply"])
        self.assertFalse(long_task.active())

    def test_a_restart_resumes_the_run(self):
        self.engine.asks["s2-bank"] = "Which?"
        self.plan["stages"][1]["depends_on"] = ["register"]
        self.plan["stages"][2]["depends_on"] = ["bank"]
        with mock.patch.object(workflow_task, "_dispatch"):
            long_task.perform(LONG, None)
        run_id = self.run_id()
        # The process dies with the first stage claimed; a new one starts.
        self.runner.claim(run_id)
        long_task._run = None
        workflow_task._active = None
        self.engine.asks.clear()
        state.current_task = None
        with mock.patch("decurix_agent.workflow_runner._alive", return_value=False):
            self.assertTrue(long_task.resume_interrupted({"task_id": TASK, "request": LONG}))
            # Not carried on silently (Finding BO): the person is asked, and nothing runs until they say.
            self.assertEqual(self.engine.prompts, [])
            asked = [s["reply"] for s in self.sent if s["kind"] == "query"][-1]
            self.assertIn("This desktop restarted in the middle of", asked)
            self.assertIn("\"carry on\"", asked)
            self.message("carry on")
        self.assertIn("interrupted by a restart", self.engine.prompts[0])
        self.assertEqual(len(self.finals()), 1)
        self.assertIn("Done", self.finals()[0]["reply"])


class WatcherRoutes(unittest.TestCase):
    """The watcher's side: a long request is planned, a short one is not, and a mid-run message is read."""

    def setUp(self):
        from test_watcher_restart import Base, FakeThread
        self.base = Base("run")
        self.base.setUp()
        # Its patches (threading.Thread among them) are its cleanups: they must be undone, or every later test's
        # threads never start (found: the next file's fake server never answered and the suite hung).
        self.addCleanup(self.base.doCleanups)
        self.addCleanup(self.base.tearDown)
        self.FakeThread = FakeThread
        from decurix_agent import watchers
        self.watchers = watchers
        self.addCleanup(setattr, long_task, "_run", None)
        long_task._deferred.clear()
        patch = mock.patch.dict("os.environ", {long_task.ENABLED: "1"})
        patch.start()
        self.addCleanup(patch.stop)

    def drop(self, text, task_id=TASK):
        path = self.base.command_dir / f"20261019_101500_ab12cd34.{task_id}.txt"
        path.write_text(text, encoding="utf-8")
        return path

    def test_a_long_request_is_planned_and_a_short_one_is_not(self):
        self.drop(LONG)
        self.watchers._pass()
        self.assertIs(self.FakeThread.started[-1][0], self.watchers._perform_long)
        state.current_task = None
        state.processed_commands.clear()
        self.FakeThread.started = []
        path = self.base.command_dir / f"20261019_101501_ab12cd35.{OTHER_TASK}.txt"
        path.write_text("what is the kernel version?", encoding="utf-8")
        with mock.patch.object(long_task, "draft_plan") as plan:
            self.watchers._pass()
            plan.assert_not_called()
        self.assertIsNot(self.FakeThread.started[-1][0], self.watchers._perform_long)

    def test_a_message_while_a_plan_runs_is_read_not_queued(self):
        long_task._run = {"run_id": "r", "task_id": TASK, "task_file": None, "request": LONG}
        state.current_task = "stage 2 of 6"
        self.drop("Bill NS-0457 was cancelled - leave it out of everything.", task_id=OTHER_TASK)
        self.watchers._pass()
        self.assertIs(self.FakeThread.started[-1][0], long_task.take_message)

    def test_a_message_left_for_later_waits_as_before(self):
        long_task._run = {"run_id": "r", "task_id": TASK, "task_file": None, "request": LONG}
        state.current_task = "stage 2 of 6"
        path = self.drop("What's the weather?", task_id=OTHER_TASK)
        long_task._deferred.add(path.name)
        self.watchers._pass()
        self.assertEqual(self.FakeThread.started, [])
        self.assertTrue(path.exists())


if __name__ == "__main__":
    unittest.main()
