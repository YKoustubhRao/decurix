"""
The independent review of the harness (2026-10-21) and series 15's held-out H1: code blocks only on what it decides
mechanically and with certainty; who or what is in or out, which stage enters records in a system, where payment
details may come from - the model's structured reading, which code enforces; a wrong blocking finding is overruled
only by the person.

    PYTHONPATH="agent;agent/tests" python -m unittest test_harness_review
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base

from decurix_agent import long_task, payees, verify
from test_payees import EMAIL, proposal

import openpyxl

FINDING = "A figure given as a source's own is not what that source states (register.csv: 1,000.00 against 1,250.00)."
KEPT = ("\nKept register.csv total as it is: the bank statement's closing balance on page 2 is 1,000.00 - the check "
        "read the opening balance.")


class AWrongBlockingFindingIsThePersonsToOverrule(base.LongRun):
    """#6: the same blocking finding again, kept on purpose with a reason - the person decides; only they clear it."""

    def setUp(self):
        super().setUp()
        findings = lambda run_id, run_state, step, *a: [("file_facts", FINDING)] if step["id"] == "s1-register" else []
        patch = mock.patch.object(long_task, "stage_code_findings", findings)
        patch.start()
        self.addCleanup(patch.stop)
        self.views = []

        def model(system, prompt, max_tokens=4000):
            if system is long_task.FINDING_VIEW_SYSTEM:
                self.views.append(prompt)
                return {"views": [{"finding": 1, "applies": "no", "why": "page 2 gives 1,000.00 as the closing balance"}]}
            return self.model(system, prompt, max_tokens)
        patch = mock.patch.object(long_task, "_ask_json", model)
        patch.start()
        self.addCleanup(patch.stop)

    def queries(self):
        return [s for s in self.sent if s["kind"] == "query"]

    def test_kept_on_purpose_it_goes_to_the_person_and_their_keep_clears_it(self):
        self.engine.report_suffix = KEPT
        judged = []
        with mock.patch.object(verify, "judge", lambda task, **k: judged.append(task) or {
                "verdict": "done", "confidence": 0.95, "seen": "", "why": ""}):
            long_task.perform(base.LONG, None)
            # (Asked once; when nothing else can run the plan's summary repeats it, as for any open question.)
            asked = [q for q in self.queries() if q["reply"].startswith("\"Purchase register\": a check in code")]
            self.assertEqual(len(asked), 1, asked)
            question = asked[0]["reply"]
            self.assertIn("a check in code found the following, and I kept the work as it is on purpose", question)
            self.assertIn(FINDING, question)
            self.assertIn("Kept register.csv total as it is", question)
            self.assertIn("1: does not apply - page 2 gives 1,000.00", question)  # the model's view, as evidence
            self.assertNotIn(long_task.STUCK_MARK, question)
            run_id = self.run_id()
            self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "parked")
            self.assertEqual(len([p for p in self.engine.prompts if p.startswith("Stage 1 of 3")]), 2)  # no third try
            self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Keep it.", "keep": True}]}
            self.message("Keep it.")
        overrides = long_task.ledger(run_id)["overrides"]
        self.assertEqual([(o["stage"], o["check"]) for o in overrides], [("s1-register", "file_facts")])
        self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "completed")
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertIn("The person kept your work as it is against what a code check found", rerun)
        check = [t for t in judged if "THE STAGE TO CHECK: \"Purchase register\"" in t][-1]
        self.assertIn("The person kept the work as it is against these findings of a code check - BINDING", check)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_without_a_reason_kept_the_finding_is_retried_and_never_cleared(self):
        long_task.perform(base.LONG, None)
        asked = self.queries()
        self.assertTrue(asked and long_task.STUCK_MARK in asked[-1]["reply"], asked)
        self.assertFalse(long_task.ledger(self.run_id()).get("overrides"))

    def test_an_answer_that_does_not_keep_it_leaves_the_finding_blocking(self):
        self.engine.report_suffix = KEPT
        long_task.perform(base.LONG, None)
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Use your judgement.",
                                                               "hands_back": True}]}
        self.message("Use your judgement.")
        run_id = self.run_id()
        self.assertFalse(long_task.ledger(run_id).get("overrides"))  # neither code nor the judge clears it
        self.assertNotEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "completed")
        # Asked once about it: the next refusals take the ordinary way (a stuck stage, try again / skip / stop).
        self.assertEqual(len([q for q in long_task.ledger(run_id)["questions"] if q.get("kind") == "finding"]), 1)


class APartialAnswerIsAskedAboutOnce(base.LongRun):
    """Series 15 H1: an answer that decides one of three listed items - the rest is asked once more, never assumed."""

    QUESTION = base.LongRun.H1_QUESTION
    ANSWER = base.LongRun.H1_ANSWER
    READING = {"names": ["Neha", "CL-4142"], "undecided": ["CL-4122", "CL-4153"]}

    def start(self):
        self.engine.asks["s1-register"] = self.QUESTION
        long_task.perform(base.LONG, None)
        return self.run_id()

    def queries(self):
        return [s["reply"] for s in self.sent if s["kind"] == "query"]

    def test_a_partial_answer_asks_once_naming_only_what_is_open(self):
        run_id = self.start()
        before = len(self.queries())
        self.message_verdict = {"kind": "answer", "answers": [dict(question="q1", answer=self.ANSWER, **self.READING)]}
        self.message(self.ANSWER)
        # (Asked once; when nothing else can run the plan's summary repeats it, as for any open question.)
        asked = [q for q in self.queries()[before:] if q.startswith("You decided:")]
        self.assertEqual(len(asked), 1, asked)
        open_part = asked[0].split("still open:", 1)[1]
        self.assertIn("CL-4122 Kavya Iyer", open_part)
        self.assertIn("CL-4153 Farhan Ali", open_part)
        self.assertNotIn("CL-4142", open_part)
        self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "parked")  # it waits
        # The rest of the run went on meanwhile; the answer to the follow-up joins the first one.
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q2", "answer": "Reject Kavya's cab until the receipt comes; approve Farhan's hotel at the cap.",
             "names": ["Kavya", "Farhan"]}]}
        self.message("Reject Kavya's cab until the receipt comes; approve Farhan's hotel at the cap.")
        self.assertEqual(len([q for q in self.queries()[before:] if "still open:" in q and q.startswith("You decided:")]),
                         1)  # never asked a third time
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertIn(self.ANSWER, rerun)
        self.assertIn("Then, about what that left open: Reject Kavya's cab", rerun)
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_a_full_answer_asks_nothing_more(self):
        self.start()
        before = len(self.queries())
        said = "Approve all three as the policy allows."
        self.message_verdict = {"kind": "answer", "answers": [
            {"question": "q1", "answer": said, "names": ["CL-4122", "CL-4142", "CL-4153"]}]}
        self.message(said)
        self.assertFalse([q for q in self.queries()[before:] if "still open:" in q])
        self.assertIn("Done", self.finals()[-1]["reply"])

    def test_use_your_judgement_asks_no_more_and_the_report_lists_the_assumption(self):
        self.engine.reports = {}
        run_id = self.start()
        self.message_verdict = {"kind": "answer", "answers": [dict(question="q1", answer=self.ANSWER, **self.READING)]}
        self.message(self.ANSWER)
        before = len(self.queries())
        self.engine.reports = {"s1-register": "Made register.csv.\n\nAssumed:\n- CL-4122 Kavya not reimbursed until a "
                                              "receipt comes: Rs 840 withheld\n- CL-4153 Farhan paid at the Rs 6,000 cap: "
                                              "Rs 1,400 a night withheld"}
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q2", "answer": "Use your judgement.",
                                                               "hands_back": True}]}
        self.message("Use your judgement.")
        self.assertFalse([q for q in self.queries()[before:] if "still open:" in q])
        left = long_task.ledger(run_id)["assumed_for_person"]
        self.assertEqual(left[0]["items"], ["CL-4122", "CL-4153"])
        rerun = [p for p in self.engine.prompts if p.startswith("Stage 1 of 3")][-1]
        self.assertIn("The person left these to your judgement: CL-4122, CL-4153", rerun)
        report = self.finals()[-1]["reply"]
        self.assertIn("You left CL-4122, CL-4153 to my judgement (Purchase register): check what I assumed", report)
        self.assertIn("Rs 840 withheld", report)


class PaymentDetailsTheRequestGivesInMessages(unittest.TestCase):
    """#7: a request that names messages as the source of payment details - the person approves; a message that would
    change an account on record is still a change request."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        wb = openpyxl.Workbook()
        wb.active.append(["Vendor", "Account No", "IFSC"])
        wb.active.append(["Apex Office Supplies", "000405012345", "ICIC0000004"])
        wb.active.append(["Sahyadri Print House", "60123456789", "MAHB0000101"])
        wb.save(self.dir / "vendor_master.xlsx")
        self.email = self.dir / "new vendor bank details.eml"
        self.email.write_text(EMAIL, encoding="utf-8")
        self.found = ([str(self.dir / "vendor_master.xlsx")], [str(self.email)])
        self.request = "Pay this week's bills; a new vendor's bank details are in the email they sent."
        self.addCleanup(payees.declare_message_details, self.request, False)

    def test_not_declared_it_blocks_as_before(self):
        made = [proposal(self.dir / "p.xlsx")]
        self.assertTrue(payees.mismatches(self.request, made, found=self.found)[0].startswith(
            "PAYMENT TO AN ACCOUNT FROM A MESSAGE:"))
        self.assertEqual(payees.message_payments(self.request, made, found=self.found), [])

    def test_declared_the_person_approves_it_shown_the_account_and_the_message(self):
        payees.declare_message_details(self.request, True)
        made = [proposal(self.dir / "p.xlsx")]
        self.assertEqual(payees.mismatches(self.request, made, found=self.found), [])
        asked = payees.message_payments(self.request, made, found=self.found)
        self.assertEqual(len(asked), 1)
        for words in ("FOR YOUR APPROVAL", "Shree Ganesh Packaging to 002501234567 / ICIC0000025",
                      "\"URGENT: Change of bank account for this payment run\""):
            self.assertIn(words, asked[0])
        self.assertTrue(long_task.asks_person("payee_approval"))

    def test_declared_a_change_to_an_account_on_record_is_still_a_change_request(self):
        payees.declare_message_details(self.request, True)
        wb = openpyxl.load_workbook(self.dir / "vendor_master.xlsx")
        wb.active.append(["Shree Ganesh Packaging", "50100012345678", "HDFC0000123"])
        wb.save(self.dir / "vendor_master.xlsx")
        made = [proposal(self.dir / "p.xlsx")]
        self.assertTrue(payees.mismatches(self.request, made, found=self.found)[0].startswith(
            "PAYMENT TO AN ACCOUNT FROM A MESSAGE:"))
        self.assertEqual(payees.message_payments(self.request, made, found=self.found), [])

    def test_the_planner_declares_it(self):
        plan = {"long": True, "title": "Pay", "payment_details_from_messages": True,
                "stages": [{"id": "a", "does": "x", "outputs": ["a.csv"]}, {"id": "b", "does": "y", "outputs": ["b.csv"]}]}
        self.assertTrue(long_task.spec_from_plan(plan)["payment_details_from_messages"])
        plan.pop("payment_details_from_messages")
        self.assertFalse(long_task.spec_from_plan(plan)["payment_details_from_messages"])

    def test_addresses_the_system_shows_are_known(self):
        known = payees.known_addresses("", found=([], []), documents=[],
                                       pages=["Employee EMP-2041 | Ananya Deshpande | ananya.d@hr.example.in"])
        self.assertEqual(known["ananya.d@hr.example.in"][0], "the system's pages")


class TheEntryStageIsThePlannersWord(unittest.TestCase):
    """#5: "enters" from the planner; a verb list only for a plan without it, and then as evidence."""

    REQUEST = ("Make summary.xlsx from the HR system's export (http://host.docker.internal:8702/export) and onboard "
               "nobody yet.")

    def test_the_field_decides_and_the_words_do_not(self):
        plan = {"long": True, "title": "HR", "stages": [
            {"id": "a", "does": "Create the headcount summary using the HR system's export and save as summary.xlsx.",
             "outputs": ["summary.xlsx"], "enters": None},
            {"id": "b", "does": "Enter each joiner in the HR system (http://host.docker.internal:8702).",
             "outputs": ["log.csv"], "enters": "the HR system"}]}
        steps = long_task.spec_from_plan(plan, request=self.REQUEST)["steps"]
        self.assertFalse(long_task.enters_records(steps[0]))
        self.assertEqual(long_task.enters_into_system(steps[0], self.REQUEST), "")
        self.assertTrue(long_task.enters_records(steps[1]))
        self.assertTrue(long_task.enters_into_system(steps[1], self.REQUEST))
        found = long_task.system_entry_findings("run", steps[1], [], self.REQUEST, {"t": 0}, record=([], ""))
        self.assertEqual([c for c, _ in found], ["system_entry"])  # certain: the planner said so

    def test_a_plan_without_the_field_falls_back_to_the_words_as_evidence(self):
        step = {"id": "s", "title": "Enter orders", "instruction": "Enter every order in the portal "
                "(http://host.docker.internal:8701)."}
        request = "Enter every order in our portal (http://host.docker.internal:8701) and log it."
        found = long_task.system_entry_findings("run", step, [], request, {"t": 0}, record=([], ""))
        self.assertEqual([c for c, _ in found], ["system_entry_text"])
        self.assertFalse(long_task.is_blocking("system_entry_text"))

    def test_ok_yes_and_done_are_not_claims_of_an_entry(self):
        with tempfile.TemporaryDirectory() as tmp:
            count = Path(tmp) / "stock_count.csv"
            count.write_text("Item,Counted,Result\nA4 paper,120,OK\nToner,8,Yes\nStapler,14,Done\n", encoding="utf-8")
            self.assertEqual(long_task._claimed_records([str(count)]), [])
            log = Path(tmp) / "entry_log.csv"
            log.write_text("Name,Portal ref\nRahul Sharma,100234\n", encoding="utf-8")
            self.assertEqual([c[3] for c in long_task._claimed_records([str(log)])], [["100234"]])

    def test_any_number_shape_and_a_name_in_any_order(self):
        claimed = [("100234 (Rahul Sharma)", ["100234"], ["Rahul Sharma"], ["100234"])]
        self.assertTrue(long_task._shown(["100234"], ["Rahul Sharma"], claimed, "Employees | 100234 | SHARMA/RAHUL",
                                         ["100234"]))
        self.assertTrue(long_task._name_seen("Rahul Sharma", "SHARMA/RAHUL | 100234"))
        self.assertFalse(long_task._name_seen("Rahul Sharma", "Rahul Verma"))

    def test_a_duplicate_only_when_every_field_but_the_number_matches(self):
        step = {"id": "s", "title": "Enter orders", "instruction": "Enter every order.", "enters": "the portal"}
        request = "Enter every order in our portal (http://host.docker.internal:8701)."
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / "log.csv"
            log.write_text("Customer,Status\nCafe Bloom,Entered SO-5101\n", encoding="utf-8")
            action = {"kind": "type", "outcome": "done", "summary": "type", "expect": {"url": "http://host.docker.internal:8701"}}
            two_orders = "Orders || SO-5101 | Cafe Bloom | 24 mugs || SO-5107 | Cafe Bloom | 30 notebooks"
            found = long_task.system_entry_findings("run", step, [str(log)], request, {"t": 0},
                                                    record=([action], two_orders))
            self.assertEqual([c for c, _ in found], ["system_entry_text"])  # one customer, two orders: evidence
            twice = "Orders || SO-5101 | Cafe Bloom | 24 mugs || SO-5107 | Cafe Bloom | 24 mugs"
            found = long_task.system_entry_findings("run", step, [str(log)], request, {"t": 0}, record=([action], twice))
            self.assertEqual([c for c, _ in found], ["system_entry"])
            self.assertIn("the same record twice", found[0][1])


class ClassifiedRemovalsAndExclusions(unittest.TestCase):
    """#4 and H1: the references a change removes, and who an answer leaves out, are the model's - code keeps only
    what the person's words write."""

    def classify(self, message, reply):
        state = {"spec": {"steps": [{"id": "s1", "title": "Register"}]}, "steps": {"s1": {"status": "completed"}},
                 "run_id": "r"}
        data = {"request": "Close the month.", "questions": [], "decisions": []}
        with mock.patch.object(long_task, "privacy_withheld", lambda t: t):
            return long_task.classify(message, state, data, ask=lambda *a: reply)

    def test_only_references_the_message_writes_are_kept(self):
        found = self.classify("Exclude the freight line from PO-7781's total.",
                              {"kind": "amendment", "amendment": "Exclude the freight line from PO-7781's total.",
                               "removed": ["PO-7781", "INV-9"], "removed_lines": ["the freight line of PO-7781"]})
        self.assertEqual(found["removed"], ["PO-7781"])  # (what the model read; INV-9 it made up is dropped)
        self.assertEqual(found["removed_lines"], ["the freight line of PO-7781"])
        found = self.classify("Don't remove INV-2026-001 after all, keep it in.",
                              {"kind": "amendment", "amendment": "Keep INV-2026-001.", "removed": [],
                               "restored": ["INV-2026-001"]})
        self.assertEqual((found["removed"], found["restored"]), ([], ["INV-2026-001"]))


class FiguresOfEqualSizeAgree(unittest.TestCase):
    """Series 15 H2 (stock count, held-out, 2026-10-21): the report's "Figures that do not agree", all of which agreed."""

    def compare(self, figure, places):
        with tempfile.TemporaryDirectory() as tmp:
            outputs = []
            for n, (name, text, value) in enumerate(places):
                path = Path(tmp) / name
                path.write_text(text + "\n", encoding="utf-8")
                outputs.append((f"s{n}", str(path)))
            asked = {"figures": [{"figure": figure, "places": [{"file": name, "text": text, "value": value}
                                                               for name, text, value in places]}]}
            notes = []
            found, agreed = long_task.model_disagreements(outputs, ask=lambda s, p: asked, request="", notes=notes)
        return found, agreed, notes

    def test_sign_words_near_equal_figures_decide_nothing(self):
        found, agreed, notes = self.compare("FAN-EX-150 counted", [
            ("zone_C_extracted_counts.txt", "FAN-EX-150 | Exhaust fan 150mm | counted 43 | short against book", "43"),
            ("recount_list.txt", "FAN-EX-150 recount: 43, over the first count by none", "43"),
            ("memo.txt", "FAN-EX-150 was counted at 43, less than the 45 on the books.", "43")])
        self.assertEqual((found, agreed, notes), ([], 1, []))

    def test_other_units_are_not_compared(self):
        found, agreed, _ = self.compare("MCB-32A-SP quantity", [
            ("zone_C_extracted_counts.txt", "MCB-32A-SP | 12 BOX", "12"),
            ("recount_list.txt", "MCB-32A-SP | 144 PCS", "144"),
            ("item_master.txt", "MCB-32A-SP | 12 per BOX", "12")])
        self.assertEqual(found, [])

    def test_two_sign_conventions_are_a_quiet_note(self):
        found, agreed, notes = self.compare("Total shortages", [
            ("stock_recon.txt", "Total shortages: -10, -3926", "-3926"),
            ("adjustment_journal.txt", "Total shortages: 10, 3926", "3926")])
        self.assertEqual((found, agreed), ([], 1))
        self.assertIn("Total shortages: the same amount, shown with opposite signs in stock_recon.txt and "
                      "adjustment_journal.txt", notes[0])

    def test_a_different_magnitude_is_still_a_disagreement(self):
        found, _, _ = self.compare("Total shortages", [
            ("stock_recon.txt", "Total shortages: -10, -3926", "-3926"),
            ("adjustment_journal.txt", "Total shortages: 10, 3962", "3962")])
        self.assertEqual(len(found), 1)


class ACrossCheckDisagreementIsAdvisory(base.LongRun):
    """H2: a disagreement the cross-check could not settle is under "Please check" - never the headline."""

    def test_the_run_is_done_and_the_figures_are_under_please_check(self):
        disagreement = {"figure": "total shortages", "values": [
            {"stage": "s1-register", "file": "register.csv", "text": "Total: 1,000.00"},
            {"stage": "s2-bank", "file": "bank.csv", "text": "Total: 1,100.00"}]}
        with mock.patch.object(long_task, "code_disagreements", lambda outputs, read=None: ([disagreement], 0)), \
                mock.patch.object(long_task, "odd_one_out", lambda run_state, d: None):
            long_task.perform(base.LONG, None)
        final = self.finals()[-1]
        self.assertEqual(final["kind"], "final")
        self.assertTrue(final["reply"].startswith("Done - every stage was made and checked."), final["reply"][:200])
        please = final["reply"].split(long_task.PLEASE_CHECK, 1)[1]
        self.assertIn("Figures that may not agree - total shortages", please)
        self.assertNotIn("Figures that may not agree", final["reply"].split("Needs you:", 1)[-1].split(
            long_task.PLEASE_CHECK)[0] if "Needs you:" in final["reply"] else "")


def load_tests(loader, tests, pattern):
    """Only this module's own tests: two classes here build on test_long_requests.LongRun, whose tests are run there."""

    suite = unittest.TestSuite()
    for cls in (AWrongBlockingFindingIsThePersonsToOverrule, APartialAnswerIsAskedAboutOnce,
                PaymentDetailsTheRequestGivesInMessages, TheEntryStageIsThePlannersWord, ClassifiedRemovalsAndExclusions,
                FiguresOfEqualSizeAgree, ACrossCheckDisagreementIsAdvisory):
        for name in loader.getTestCaseNames(cls):
            if name in cls.__dict__:
                suite.addTest(cls(name))
    return suite


if __name__ == "__main__":
    unittest.main()


class AnItemDecidedEarlierIsNotAskedAgain(unittest.TestCase):
    """Series 16 H1: CL-4142 was decided by the first answer; the stage's next question listed it again, and the
    follow-up for what an answer leaves open must not put it to the person a third time."""

    def test_only_items_no_earlier_answer_decided_are_still_open(self):
        first = {"question": "CL-4142 has no receipt: approve or reject?", "answer": base.LongRun.H1_ANSWER,
                 "names": ["Neha", "CL-4142"]}
        data = {"decisions": [first], "questions": []}

        class Runner:
            def status(self, run_id):
                return {"steps": {"s1": {"status": "parked"}}}

        answer = {"answer": "Yes - that was my answer to your question.",
                  "undecided": ["CL-4122 Kavya Raghavan", "CL-4142 Neha Joshi", "CL-4145 Neha Joshi flight"]}
        with mock.patch.object(long_task, "ledger", lambda run_id: data), \
                mock.patch.object(long_task, "_runner", lambda: Runner()):
            left = long_task._rest_to_ask("run", "s1", {"id": "q2", "kind": "query", "text": "1. ... 2. ... 3. ..."},
                                          answer)
        self.assertEqual(left, ["CL-4122 Kavya Raghavan", "CL-4145 Neha Joshi flight"])
