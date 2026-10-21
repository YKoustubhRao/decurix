"""
"Ask me" in a long request (round 5 P2, 2026-10-20, Finding Y): a decision that belongs to the person is asked in the
chat, never put to a customer by email; a draft that tells one party another's business is not done; the question a
stage asks names the value; an answer about another matter is not taken for the open question's; and a stage that
needs the same thing again after an answer does not ask the identical question in a loop.

    PYTHONPATH="agent;agent/tests" python -m unittest test_long_decisions
"""

import tempfile
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import long_task, produced

P2 = base.P2_REQUEST

# The live texts.
LIVE_DRAFT = {"to": ["orders@cafebloom.example.in"], "cc": [],
              "subject": "Decision needed: credit hold orders for 19 Oct 2026",
              "body": "Hello,\n\nPlease decide whether to release or hold these:\n"
                      "1. Cafe Bloom - order WA-1810, Rs 26,432.40 with GST; outstanding Rs 38,000; limit Rs 50,000.\n"
                      "2. Harbour Hotels Group - PO HH-PO-7781, Rs 61,950.00 with GST; outstanding Rs 41,500; limit Rs 75,000.\n"}
LIVE_QUESTION = ("I could not tell where some of what I was about to type comes from, so I have not typed it. Which "
                 "document or page should I take it from?")
LIVE_ANSWER = "Release Cafe Bloom this once, hold Harbour Hotels Group until they clear their dues."
CREDIT_STAGE = {"id": "s3-ask-for-decision", "title": "Ask for decision on orders that exceed credit limit",
                "instruction": "Work out which orders, at full quantity with GST, plus what the customer already owes "
                               "take the customer over their credit limit; save credit_hold_list.xlsx; hold them and ask "
                               "the person whether to release or hold each.", "decision": True}


def _decision_plan():
    return {"long": True, "title": "Orders", "stages": [
        {"id": "credit", "title": CREDIT_STAGE["title"], "does": CREDIT_STAGE["instruction"], "decision": True,
         "drafts": 1, "outputs": [{"file": "P2_orders_19Oct/credit_hold_list.xlsx"}]},
        {"id": "replies", "title": "Customer replies", "does": "Write customer_replies.txt.", "drafts": 1,
         "outputs": [{"file": "P2_orders_19Oct/customer_replies.txt"}]}]}


class Planning(unittest.TestCase):
    def test_a_decision_stage_never_drafts_and_a_request_without_emails_gets_none(self):
        spec = long_task.spec_from_plan(_decision_plan(), request=P2)
        credit, replies = spec["steps"]
        self.assertTrue(credit["decision"])
        self.assertEqual(credit["drafts"], 0)
        self.assertEqual(replies["drafts"], 0)  # "I'll send them myself" is a file
        self.assertFalse(any(c["format"] == "drafts" for s in spec["steps"] for c in s["checks"]))

    def test_drafts_stay_where_the_request_asks_for_emails(self):
        plan = _decision_plan()
        spec = long_task.spec_from_plan(plan, request=base.LONG)  # "... AR ageing with reminder email drafts ..."
        self.assertEqual(spec["steps"][1]["drafts"], 1)
        self.assertEqual(spec["steps"][0]["drafts"], 0)  # still a decision

    def test_what_asks_for_drafts(self):
        for text in ("dispute email drafts to vendors with mismatches", "Email the customers their statements",
                     "Draft a reply to each supplier", "Put them in the Outbox", "send the invoice by email"):
            self.assertTrue(long_task.asks_for_drafts(text), text)
        for text in (P2, "2 POs that came by email", "customer_replies.txt: the reply for each customer. I'll send them myself."):
            self.assertFalse(long_task.asks_for_drafts(text), text)

    def test_the_planner_is_told_ask_me_is_the_chat(self):
        self.assertIn('"Ask me" is never an email', " ".join(long_task.PLAN_SYSTEM.split()))
        self.assertIn('"decision": true', long_task.PLAN_SYSTEM)


class DecisionCheck(unittest.TestCase):
    def test_a_draft_is_never_the_decision(self):
        problem = long_task.decision_problem(CREDIT_STAGE, {}, {"decisions": []}, [LIVE_DRAFT])
        self.assertIn("asked in the chat with HELP, never by email", problem)

    def test_not_done_until_the_person_decided(self):
        self.assertIn("they have not been asked", long_task.decision_problem(CREDIT_STAGE, {}, {"decisions": []}, []))
        self.assertEqual(long_task.decision_problem(CREDIT_STAGE, {"answer": LIVE_ANSWER}, {"decisions": []}, []), "")
        given = {"decisions": [{"stage": CREDIT_STAGE["id"], "answer": LIVE_ANSWER}]}
        self.assertEqual(long_task.decision_problem(CREDIT_STAGE, {}, given, []), "")
        self.assertEqual(long_task.decision_problem({"id": "x"}, {}, {"decisions": []}, [LIVE_DRAFT]), "")


class Disclosure(unittest.TestCase):
    def setUp(self):
        import openpyxl
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        folder = Path(self.tmp.name)
        book = openpyxl.Workbook()
        sheet = book.active
        sheet.append(["Customer", "City", "Contact", "Email", "Credit Limit (Rs)", "Outstanding (Rs)"])
        sheet.append(["Cafe Bloom", "Pune", "Rohan Kulkarni", "orders@cafebloom.example.in", 50000, 38000])
        sheet.append(["Harbour Hotels Group", "Panaji", "Ana Gomes", "orders@harbourhotels.example.in", 75000, 41500])
        sheet.append(["Oakwood School", "Pune", "M. Joshi", "orders@oakwoodschool.example.in", 90000, 0])
        book.save(folder / "customer_master.xlsx")
        (folder / "WhatsApp Chat with Rohan Kulkarni (Cafe Bloom).txt").write_text("hi", encoding="utf-8")
        files = "\n".join(f"  {p}" for p in sorted(folder.iterdir()))
        self.request = P2 + "\n\nFiles attached to this request, saved on this desktop:\n" + files

    def test_the_live_draft_discloses_another_customer(self):
        found = produced.draft_disclosures([LIVE_DRAFT], self.request)
        self.assertEqual(len(found), 1)
        self.assertIn("names Harbour Hotels Group with figures", found[0])
        self.assertIn("orders@cafebloom.example.in", found[0])
        facts = produced.file_facts(self.request, [], drafts_made=[LIVE_DRAFT])
        self.assertEqual(facts.disclosures, found)

    def test_a_draft_about_its_own_recipient_is_fine(self):
        own = dict(LIVE_DRAFT, subject="Your order WA-1810",
                   body="Dear Rohan Kulkarni,\n\nCafe Bloom's order WA-1810 comes to Rs 26,432.40 with GST.\n")
        self.assertEqual(produced.draft_disclosures([own], self.request), [])

    def test_a_draft_to_no_party_or_without_figures_is_not_a_finding(self):
        internal = dict(LIVE_DRAFT, to=["me@ourcompany.example.in"])
        self.assertEqual(produced.draft_disclosures([internal], self.request), [])
        plain = dict(LIVE_DRAFT, body="Hello, we also supply Harbour Hotels Group and Oakwood School.")
        self.assertEqual(produced.draft_disclosures([plain], self.request), [])

    def test_no_party_list_in_the_inputs_decides_nothing(self):
        self.assertEqual(produced.draft_disclosures([LIVE_DRAFT], P2), [])


class Messages(unittest.TestCase):
    def test_an_answer_about_the_decision_is_moved_to_it(self):
        moved = long_task._about({"question": "q1", "answer": LIVE_ANSWER}, LIVE_ANSWER,
                                 [{"id": "q1", "text": LIVE_QUESTION}], [CREDIT_STAGE])
        self.assertEqual(moved["question"], "d:" + CREDIT_STAGE["id"])

    def test_an_answer_about_its_own_question_stays(self):
        question = 'I was about to type "HH-PO-7781", but the request names the PO PDF, which I could not read.'
        answer = "The PO PDF is inside the .eml from Harbour Hotels; HH-PO-7781 is right."
        kept = long_task._about({"question": "q1", "answer": answer}, answer, [{"id": "q1", "text": question}],
                                [CREDIT_STAGE])
        self.assertEqual(kept["question"], "q1")

    def test_the_classifier_is_told_an_answer_is_about_its_question(self):
        self.assertIn("is NOT that question's answer", long_task.CLASSIFY_SYSTEM)

    def test_the_same_question_again_is_seen(self):
        self.assertTrue(long_task.same_question(LIVE_QUESTION, LIVE_QUESTION + " "))
        self.assertFalse(long_task.same_question(LIVE_QUESTION, "Which bank account is the main one?"))
        text = long_task.repeated_question(LIVE_QUESTION, LIVE_ANSWER, LIVE_QUESTION)
        for want in ("took your reply", LIVE_ANSWER, "It did not settle it", "\"try again\"", "\"skip it\"", "\"stop\""):
            self.assertIn(want, text)


class RunWithDecisions(unittest.TestCase):
    """The live flow on the LongRun harness of test_long_requests."""

    setUp = base.LongRun.setUp
    restore = base.LongRun.restore
    model = base.LongRun.model
    send = base.LongRun.send
    finals = base.LongRun.finals
    message = base.LongRun.message
    run_id = base.LongRun.run_id

    def test_the_same_question_after_an_answer_does_not_loop(self):
        self.engine.asks["s1-register"] = LIVE_QUESTION
        long_task.perform(base.LONG, None)
        self.engine.asks["s1-register"] = LIVE_QUESTION  # the re-run hits the same untraced value
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": LIVE_ANSWER}]}
        self.message(LIVE_ANSWER)
        asked = [s["reply"] for s in self.sent if s["kind"] == "query"]
        self.assertIn("It did not settle it", asked[-1])
        self.assertIn(LIVE_ANSWER, asked[-1])
        self.assertNotEqual(asked[-1].strip().endswith(LIVE_QUESTION), True)
        data = long_task.ledger(self.run_id())
        self.assertEqual(data["questions"][-1]["kind"], "stuck")
        # "skip it" now leaves the stage out instead of asking a third time.
        self.message_verdict = {"kind": "answer", "answers": [{"question": data["questions"][-1]["id"], "answer": "skip it"}]}
        self.message("skip it")
        self.assertIn("you said to leave it out", self.finals()[-1]["reply"])

    def test_a_decision_stage_that_did_not_ask_is_not_done_and_a_message_can_decide_it(self):
        self.plan = {"long": True, "title": "Orders", "stages": [
            {"id": "credit", "title": CREDIT_STAGE["title"], "decision": True,
             "does": CREDIT_STAGE["instruction"].replace("credit_hold_list.xlsx", "credit_hold_list.csv"),
             "outputs": [{"file": "credit_hold_list.csv"}]},
            {"id": "other", "title": "Dispatch plan", "does": "Make the dispatch plan.", "outputs": [{"file": "plan.csv"}]}]}
        long_task.perform(P2, None)
        status = self.runner.status(self.run_id())
        credit = status["steps"]["s1-credit"]
        self.assertNotEqual(credit["status"], "completed")
        credit_prompts = [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")]
        self.assertIn("This stage is the person's decision", credit_prompts[0])
        self.assertIn("never by email", credit_prompts[0])
        # The person decides in a message: it is kept as that stage's decision, and the stage then counts.
        self.message_verdict = {"kind": "answer", "answers": [{"question": "d:s1-credit", "answer": LIVE_ANSWER}]}
        if credit["status"] == "parked":
            data = long_task.ledger(self.run_id())
            self.message_verdict["answers"] = [{"question": data["questions"][-1]["id"], "answer": LIVE_ANSWER}]
        self.message(LIVE_ANSWER)
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s1-credit"]["status"], "completed")
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 1 of 2")][-1]
        self.assertIn(f"The person decided: \"{LIVE_ANSWER}\"", prompt)


def _live_run_plan():
    return {"long": True, "title": "Orders", "stages": [
        {"id": "credit", "title": CREDIT_STAGE["title"], "decision": True,
         "does": CREDIT_STAGE["instruction"].replace("credit_hold_list.xlsx", "credit_hold_list.csv"),
         "outputs": [{"file": "credit_hold_list.csv"}]},
        {"id": "portal", "title": "Enter orders in the portal", "does": "Enter the approved orders.",
         "outputs": [{"file": "portal_log.csv"}]}]}


class TheLiveMixUp(unittest.TestCase):
    """Round 5 P2: the credit decision answered while the portal stage's grounding question was open."""

    setUp = base.LongRun.setUp
    restore = base.LongRun.restore
    model = base.LongRun.model
    send = base.LongRun.send
    finals = base.LongRun.finals
    message = base.LongRun.message
    run_id = base.LongRun.run_id

    def test_the_credit_answer_decides_the_credit_stage_not_the_portal_question(self):
        self.plan = _live_run_plan()
        self.engine.asks["s1-credit"] = ("Cafe Bloom's order WA-1810 and Harbour Hotels Group's PO HH-PO-7781 take them "
                                         "over their credit limits. Release or hold each?")
        self.engine.asks["s2-portal"] = LIVE_QUESTION
        long_task.perform(P2, None)
        data = long_task.ledger(self.run_id())
        portal_q = next(q for q in data["questions"] if q["stage"] == "s2-portal")
        # The model takes the credit answer for the portal question, as it did live.
        self.message_verdict = {"kind": "answer", "answers": [{"question": portal_q["id"], "answer": LIVE_ANSWER}]}
        self.message(LIVE_ANSWER)
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s1-credit"]["status"], "completed")
        self.assertEqual(status["steps"]["s2-portal"]["status"], "parked")
        data = long_task.ledger(self.run_id())
        self.assertEqual(next(q for q in data["questions"] if q["id"] == portal_q["id"])["status"], "open")
        self.assertTrue(any(d["stage"] == "s1-credit" and d["answer"] == LIVE_ANSWER for d in data["decisions"]))
        # The portal question is put again, not answered with the credit decision.
        self.assertIn(LIVE_QUESTION, [s["reply"] for s in self.sent if s["kind"] == "query"][-1])


# Round 5 P3 (2026-10-20): the decision stage's question went out as an approval; "yes" was a one-shot grant.
P3_ASKED = ("Please approve or reject this payment run. Approve paying the 6 proposed whole bills now for Rs 4,64,485.00 "
            "- Or reject / request changes (which bills to drop or add).")


def _p3_plan():
    return {"long": True, "title": "Procure to pay", "stages": [
        {"id": "register", "title": "Purchase register and exceptions", "does": "Build register.csv.",
         "outputs": [{"file": "register.csv"}]},
        {"id": "proposal", "title": "Payment proposal", "does": "Propose the payment run in proposal.csv.",
         "outputs": [{"file": "proposal.csv"}], "depends_on": ["register"]},
        {"id": "disputes", "title": "Dispute emails", "does": "Write disputes.csv.", "outputs": [{"file": "disputes.csv"}],
         "depends_on": ["register"]},
        {"id": "approval", "title": "Ask for approval of the payment run", "decision": True,
         "does": "Ask the person to approve the payment run in proposal.csv; record it in approval_log.csv.",
         "outputs": [{"file": "approval_log.csv"}], "depends_on": ["proposal"]}]}


class TheApprovalOfAPlanIsADecision(unittest.TestCase):
    setUp = base.LongRun.setUp
    restore = base.LongRun.restore
    model = base.LongRun.model
    send = base.LongRun.send
    finals = base.LongRun.finals
    message = base.LongRun.message
    run_id = base.LongRun.run_id

    def test_an_approval_asked_in_a_decision_stage_is_a_question_and_yes_decides_it(self):
        self.plan = _p3_plan()
        engine_perform = self.engine.perform_task
        asked = []

        def perform(prompt, task_file):
            if prompt.startswith("Stage 4 of 4") and not asked:
                asked.append(prompt)
                base.messaging.ask_for_query(P3_ASKED, kind="approval")  # as the harness's wording rule sends it
                return
            engine_perform(prompt, task_file)
        self.engine.perform_task = perform
        base.sys.modules["decurix_agent.harness"].perform_task = perform
        long_task.perform(base.LONG, None)

        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s4-approval"]["status"], "parked")
        self.assertEqual([s["kind"] for s in self.sent if s["kind"] == "approval"], [])
        q = long_task.ledger(self.run_id())["questions"][-1]
        self.assertEqual((q["kind"], q["parked"]), ("query", True))
        self.assertIn("Please approve or reject this payment run", q["text"])

        self.message_verdict = {"kind": "answer", "answers": [{"question": q["id"], "answer": "yes"}]}
        self.message("yes")
        status = self.runner.status(self.run_id())
        self.assertEqual(status["steps"]["s4-approval"]["status"], "completed")
        prompt = [p for p in self.engine.prompts if p.startswith("Stage 4 of 4")][-1]
        self.assertIn('The person decided: "yes"', prompt)

    def test_an_approval_answered_during_a_decision_stage_is_kept_as_its_decision(self):
        self.plan = _p3_plan()
        long_task.perform(base.LONG, None)  # the stage fails its check (never asked) and waits as stuck
        run_id = self.run_id()
        long_task._run = {"run_id": run_id, "task_id": base.TASK, "task_file": None, "request": base.LONG}
        long_task._new_question(run_id, "s4-approval", P3_ASKED, "approval", False)
        long_task._approval_as_decision(run_id, "yes")
        data = long_task.ledger(run_id)
        self.assertTrue(any(d["stage"] == "s4-approval" and d["answer"] == 'You approved: "yes"' for d in data["decisions"]))
        step = next(s for s in self.runner.status(run_id)["spec"]["steps"] if s["id"] == "s4-approval")
        self.assertEqual(long_task.decision_problem(step, {}, data, []), "")
        # A "no" is a decision too.
        long_task._new_question(run_id, "s2-proposal", "Approve sending?", "approval", False)
        long_task._approval_as_decision(run_id, "no")  # not a decision stage: nothing kept
        self.assertEqual(len([d for d in long_task.ledger(run_id)["decisions"] if d["stage"] == "s2-proposal"]), 0)


if __name__ == "__main__":
    unittest.main()
