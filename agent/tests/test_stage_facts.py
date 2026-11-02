"""
Facts code decides about a long request's stage (stage_facts.py), with round 5 P1's own files (2026-10-20) when they
are on this machine, copied into a temp folder.

    PYTHONPATH="agent;agent/tests" python -m unittest test_stage_facts
"""

import shutil
import tempfile
import unittest
from pathlib import Path

import openpyxl

from decurix_agent import stage_facts

import fixture_paths  # live-run fixtures live outside the repo
from temp_case import TempDirCase

RUN = Path(r"C:\Users\Administrator\decurix-tests\wide\process\out\P1\20261020-060650")
WORKBOOK = RUN / "worker" / "P1_month_end" / "outputs" / "month_end_close_Oct2026.xlsx"
BILL = RUN / "host" / "purchase_bills" / "NS-0457.pdf"
AMENDMENT = [{"n": 1, "text": "Exclude bill NS-0457 from everything - Nova Stationers cancelled it.",
              "removed": ["NS-0457"]}]


@unittest.skipUnless(WORKBOOK.is_file() and BILL.is_file(), "round 5 P1's files are not on this machine")
class TheRoundsOwnFiles(TempDirCase):

    def setUp(self):
        super().setUp()
        self.workbook = self.dir / WORKBOOK.name
        shutil.copy(WORKBOOK, self.workbook)
        self.bill = self.dir / BILL.name
        shutil.copy(BILL, self.bill)

    def test_a_removed_bills_tax_still_in_the_return_is_found(self):
        self.assertEqual(stage_facts.removed_records(AMENDMENT), ["NS-0457"])
        found = stage_facts.removed_residue(AMENDMENT, [str(self.workbook)], [str(self.bill)])
        self.assertEqual(len(found), 2)
        self.assertIn("GSTR-3B \"Eligible ITC from October purchase bills\" CGST 43,067.40 is Purchase Register's CGST "
                      "(ITC eligible this month = Yes) 38,534.40 plus 4,533.00 of NS-0457, which was removed", found[0])
        self.assertIn("SGST 43,067.40 is Purchase Register's SGST", found[1])

    def test_the_return_recomputed_passes(self):
        wb = openpyxl.load_workbook(self.workbook)
        ws = wb["GSTR-3B"]
        for row in ws.iter_rows():
            if row[0].value == "Eligible ITC from October purchase bills":
                row[1].value, row[2].value = 38534.4, 38534.4
        wb.save(self.workbook)
        self.assertEqual(stage_facts.removed_residue(AMENDMENT, [str(self.workbook)], [str(self.bill)]), [])
        # Nothing removed: nothing to look for.
        self.assertEqual(stage_facts.removed_residue([{"text": "Add the late bill too."}], [str(self.workbook)],
                                                     [str(self.bill)]), [])


RUN2 = Path(r"C:\Users\Administrator\decurix-tests\wide\process\out\P1\20261020-063847") / "worker" / "P1_month_end" \
    / "outputs" / "month_end_close_Oct2026.xlsx"
# Round 5 P1 run 2's first pass, as its ledger carried it (s1), and the change that re-ran it.
FIRST_PASS = ["Purchase GST issue held out of ITC: SP-2291, PK-BOX billed at 18% vs item master 12%",
              "Duplicate bill BCP-7781 (Scan_resent_bill_14Oct.pdf) excluded from ITC",
              "Bank balance per books 878,193.78"]
CHANGE = [{"n": 1, "text": "Exclude bill NS-0457 from Nova Stationers from all outputs because it was cancelled."}]


@unittest.skipUnless(RUN2.is_file() and WORKBOOK.is_file(), "round 5 P1's files are not on this machine")
class AReCheckKeepsEarlierFindings(TempDirCase):
    """Finding BG: re-run for the cancelled bill, the register lost the wrong-rate bill's flag."""

    def copy(self, source, name):
        target = self.dir / name
        shutil.copy(source, target)
        return str(target)

    def test_the_dropped_flag_is_found_and_the_kept_one_passes(self):
        findings = stage_facts.earlier_findings(FIRST_PASS, CHANGE)
        self.assertEqual([ref for ref, _ in findings], ["SP-2291", "BCP-7781"])
        lost = stage_facts.lost_findings(findings, [self.copy(RUN2, "run2.xlsx")])
        self.assertEqual(len(lost), 1)
        self.assertIn("SP-2291 was flagged before", lost[0])
        self.assertIn("is not flagged now in run2.xlsx (Purchase Register)", lost[0])
        # Run 1's re-check kept it (eligible: No, "billed at 18% vs item master 12%"); the duplicate kept its note.
        self.assertEqual(stage_facts.lost_findings(findings, [self.copy(WORKBOOK, "run1.xlsx")]), [])
        # A record the change itself names is the change's to decide.
        self.assertEqual(stage_facts.earlier_findings(["NS-0457 held: cancelled by vendor"], CHANGE), [])


class AStatusEveryOutputAgreesOn(TempDirCase):
    """Round 5 P6 (2026-10-20), Finding BZ1: the tracker said "assess" for refunds the drafts approved."""

    def setUp(self):
        super().setUp()
        self.tracker = Path(self.tmp.name) / "support_triage_tracker.xlsx"

    def write(self, action_small, action_big):
        wb = openpyxl.Workbook()
        wb.active.append(["File", "From", "Category", "Priority", "Order No", "Customer", "Action", "Owner"])
        wb.active.append(["1015_E01.eml", "a@x.example", "Refund", "Medium", "SO-5101", "Lotus Yoga Centre", action_small,
                          "Support"])
        wb.active.append(["1022_E07.eml", "b@y.example", "Refund", "High", "SO-5107", "Harbour Hotels Group", action_big,
                          "Support Lead"])
        wb.save(self.tracker)
        return [str(self.tracker)]

    DRAFTS = [{"to": ["a@x.example"], "subject": "Your refund for SO-5101",
               "body": "Dear Nisha,\nYour refund for order SO-5101 has been approved and will be processed in 5 days."},
              {"to": ["b@y.example"], "subject": "Re: refund request SO-5107",
               "body": "Dear team,\nYour request for SO-5107 is under review; we will get back to you."}]

    def test_an_approved_refund_the_tracker_still_has_to_assess_is_found(self):
        made = self.write("Assess against policy and process refund", "For approval (over Rs 5,000)")
        found = stage_facts.status_conflicts(made, self.DRAFTS)
        self.assertEqual(len(found), 1)
        self.assertTrue(found[0].startswith("SO-5101:"))
        self.assertIn("says open (\"Assess against policy and process refund\")", found[0])
        self.assertIn("the draft to a@x.example says approved", found[0])

    def test_one_product_on_a_held_and_a_released_order_is_no_conflict(self):
        # Round 5 P2 (2026-10-20): the live dispatch plan's shape - WB-500 on Harbour Hotels' held line and on Cafe
        # Bloom's released line, both right. A product code is not an item; rows of one table never conflict.
        path = Path(self.tmp.name) / "dispatch_plan.xlsx"
        wb = openpyxl.Workbook()
        wb.active.title = "Dispatch Plan"
        wb.active.append(["Customer", "Order ID", "Customer PO / Ref", "SKU", "Decision", "Line status", "Ordered qty",
                          "Allocated qty", "Backorder qty"])
        wb.active.append(["Harbour Hotels Group", "Harbour Hotels Group|HH/PO/7781", "HH/PO/7781", "WB-500", "Hold", "Held",
                          80, 0, 0])
        wb.active.append(["Cafe Bloom", "Cafe Bloom|WA-1810", "WA-1810", "WB-500", "Release", "Released", 60, 60, 0])
        wb.active.append(["Lotus Yoga Centre", "Lotus Yoga Centre|WA-1810", "WA-1810", "USB-32", "Release", "Released",
                          25, 25, 0])
        wb.save(path)
        self.assertEqual(stage_facts.status_conflicts([str(path)], []), [])
        # The same order held in one output and released in another still disagrees.
        other = Path(self.tmp.name) / "credit_hold_decision.xlsx"
        wb = openpyxl.Workbook()
        wb.active.append(["Customer", "Order ID", "Decision"])
        wb.active.append(["Harbour Hotels Group", "Harbour Hotels Group|HH/PO/7781", "Released"])
        wb.save(other)
        found = stage_facts.status_conflicts([str(path), str(other)], [])
        self.assertEqual(len(found), 1)
        self.assertTrue(found[0].startswith("Harbour Hotels Group|HH/PO/7781:"))

    def test_the_rule_applied_where_the_record_is_made_agrees(self):
        made = self.write("Approve refund (policy, up to Rs 5,000)", "For approval (over Rs 5,000)")
        self.assertEqual(stage_facts.status_conflicts(made, self.DRAFTS), [])
        self.assertEqual(stage_facts.status_conflicts(made, []), [])


P6_INBOX = Path(r"C:\Users\Administrator\decurix-tests\wide\process\out\P6\20261020-154917\host\inbox")


class WorkLeftUndoneInATable(TempDirCase):
    """Round 5 P6 (2026-10-20), Finding CC: an Action left as "calculate ... and process per policy", and an automatic
    reply given another email's order number."""

    def tracker(self, rows):
        path = self.dir / "support_triage_tracker.xlsx"
        wb = openpyxl.Workbook()
        wb.active.append(["File", "From", "Subject", "Category", "Order No", "Customer", "Action", "Owner"])
        for row in rows:
            wb.active.append(row)
        wb.save(path)
        return [str(path)]

    def test_an_action_left_as_an_instruction_is_found(self):
        live = "Refund requested; calculate affected units × unit price from order and process per policy"
        made = self.tracker([["1016_E03.eml", "r@cafebloom.example.in", "Lanyards", "Refund", "SO-5103", "Cafe Bloom", live,
                              "Support"],
                             ["1015_E01.eml", "n@lotus.example.in", "Refund", "Refund", "SO-5101", "Lotus Yoga Centre",
                              "Approve refund Rs 2,100 (policy, up to Rs 5,000)", "Support"]])
        found = stage_facts.deferred_actions(made)
        self.assertEqual(len(found), 1)
        self.assertIn("SO-5103 - Action says \"Refund requested; calculate affected units", found[0])

    @unittest.skipUnless((P6_INBOX / "1018_E10.eml").is_file(), "round 5 P6's inbox is not on this machine")
    def test_an_automatic_reply_given_another_emails_order_is_found(self):
        inbox = self.dir / "inbox"
        shutil.copytree(P6_INBOX, inbox)
        messages = [str(x) for x in inbox.glob("*.eml")]
        made = self.tracker([["1018_E10.eml", "anjali.k@kingsleyarchitects.example.in",
                              "Automatic reply: Your October statement", "Auto-reply", "SO-5112", "Kingsley Architects",
                              "No action", "Support"],
                             ["1019_E13.eml", "anjali.k@kingsleyarchitects.example.in", "Order", "Order status", "SO-5112",
                              "Kingsley Architects", "Share delivery update", "Support"]])
        found = stage_facts.unsupported_references(made, messages)
        self.assertEqual(len(found), 1)
        self.assertIn("1018_e10.eml is an automatic reply, and Order No says SO-5112, which its own message never "
                      "mentions", found[0])
        # Left empty, it is right.
        made = self.tracker([["1018_E10.eml", "anjali.k@kingsleyarchitects.example.in",
                              "Automatic reply: Your October statement", "Auto-reply", "", "Kingsley Architects",
                              "No action", "Support"]])
        self.assertEqual(stage_facts.unsupported_references(made, messages), [])


class ComputedAmountsHold(TempDirCase):
    """Round 5 P6 (2026-10-20), Finding CF: "Approve refund Rs 1,800.00" for 60 lanyards at Rs 35 (Rs 2,100), with no
    working; and E07 filed as Refund while its own Action says "no refund requested"."""

    LIVE_E03 = ["1016_E03.eml", "Nisha Menon <nisha@lotusyogacentre.example.in>", "Lanyards - clips broken", "Refund",
                "Medium", "SO-5103", "Lotus Yoga Centre", "Approve refund Rs 1,800.00 (policy, up to Rs 5,000).", "Support"]
    LIVE_E07 = ["1017_E07.eml", "Farah Shaikh <farah@parkevents.example.in>", "Wrong caps delivered - SO-5107", "Refund",
                "Medium", "SO-5107", "Park Events Ltd", "Acknowledge wrong item delivery and arrange urgent replacement "
                "before 25 October; no refund requested.", "Support"]

    def book(self, rows, working=None):
        path = self.dir / "support_triage_tracker.xlsx"
        wb = openpyxl.Workbook()
        wb.active.title = "Triage"
        wb.active.append(["File name", "From", "Subject", "Category", "Priority", "Order No", "Customer", "Action", "Owner"])
        for row in rows:
            wb.active.append(row)
        if working:
            ws = wb.create_sheet("Working")
            ws.append(["Order No", "Item", "Qty", "Unit price", "Source", "Result"])
            for row in working:
                ws.append(row)
        wb.save(path)
        return [str(path)]

    def portal(self):
        path = self.dir / "portal_orders.csv"
        path.write_text("Order No,Customer,SKU,Qty,Unit price\nSO-5103,Lotus Yoga Centre,LAN-S,200,35\n", encoding="utf-8")
        return str(path)

    @unittest.skipUnless((P6_INBOX / "1016_E03.eml").is_file(), "round 5 P6's inbox is not on this machine")
    def test_a_computed_refund_with_no_working_is_found(self):
        made = self.book([self.LIVE_E03])
        found = stage_facts.unworked_amounts(made, [str(P6_INBOX / "1016_E03.eml")])
        self.assertEqual(len(found), 1)
        self.assertIn("SO-5103", found[0])

    def test_a_wrong_factor_and_an_amount_its_working_does_not_give_are_found(self):
        made = self.book([self.LIVE_E03], working=[["SO-5103", "LAN-S", 60, 30, "portal_orders.csv SO-5103 LAN-S", 1800]])
        found = stage_facts.working_problems(made, [self.portal()])
        self.assertTrue(any("uses a price of 30.00, but portal_orders.csv has 35.00" in f for f in found), found)
        made = self.book([self.LIVE_E03], working=[["SO-5103", "LAN-S", 60, 35, "portal_orders.csv SO-5103 LAN-S", 2100]])
        found = stage_facts.working_problems(made, [self.portal()])
        self.assertTrue(any("Action says 1,800.00, its working in" in f and "gives 2,100.00" in f for f in found), found)
        made = self.book([self.LIVE_E03], working=[["SO-5103", "LAN-S", 60, 35, "portal_orders.csv SO-5103 LAN-S", 2000]])
        self.assertTrue(any("gives Result 2,000.00, which no combination of its own factors" in f and
                            "60 x 35.00 is 2,100.00" in f
                            for f in stage_facts.working_problems(made, [self.portal()])))
        right = list(self.LIVE_E03)
        right[7] = "Approve refund Rs 2,100.00 (policy, up to Rs 5,000)."
        made = self.book([right], working=[["SO-5103", "LAN-S", 60, 35, "portal_orders.csv SO-5103 LAN-S", 2100]])
        self.assertEqual(stage_facts.working_problems(made, [self.portal()]), [])
        self.assertEqual(stage_facts.unworked_amounts(made), [])

    def test_a_row_filed_in_a_category_its_own_action_denies(self):
        found = stage_facts.self_contradictions(self.book([self.LIVE_E07]))
        self.assertEqual(len(found), 1)
        self.assertIn("SO-5107 is filed as Category \"Refund\" while the same row says \"no refund requested\"", found[0])
        right = list(self.LIVE_E07)
        right[3] = "Complaint"
        self.assertEqual(stage_facts.self_contradictions(self.book([right])), [])


LIVE_P1_S13 = fixture_paths.fixture("p1_series13_month_end_close.xlsx")
NS_0457 = [{"n": 1, "text": "Exclude bill NS-0457 from Nova Stationers from all outputs because it was cancelled.",
            "removed": ["NS-0457"]}]


class RemovedRecordsAreTheModelsReading(unittest.TestCase):
    # Harness review (2026-10-21, #4): the regex read both of these as removals.
    def test_no_word_list_reads_the_persons_words(self):
        for text in ("Don't remove INV-2026-001 after all, keep it in.", "Exclude the freight line from PO-7781's total."):
            self.assertEqual(stage_facts.removed_records([{"n": 1, "text": text}]), [], text)

    def test_a_line_item_is_not_the_record_and_a_restore_clears_it(self):
        self.assertEqual(stage_facts.removed_records([{"text": "Exclude the freight line from PO-7781's total.",
                                                       "removed_lines": ["the freight line of PO-7781"]}]), [])
        changes = [{"text": "Remove INV-2026-001.", "removed": ["INV-2026-001"]},
                   {"text": "Don't remove INV-2026-001 after all, keep it in.", "restored": ["INV-2026-001"]}]
        self.assertEqual(stage_facts.removed_records(changes[:1]), ["INV-2026-001"])
        self.assertEqual(stage_facts.removed_records(changes), [])


class ARemovedRecordStillInTheOutputs(unittest.TestCase):
    # Round 5 P1 on pc3 (2026-10-21, series 13): "Bill NS-0457 from Nova Stationers was cancelled - leave it out of
    # everything" - and the Purchase Register still had "08/10/2026 | Nova Stationers | NS-0457 | 66700 | 4533 | ...".

    @unittest.skipUnless(LIVE_P1_S13.is_file(), f"fixture not here: {LIVE_P1_S13}")
    def test_the_live_register_is_found(self):
        found = stage_facts.removed_still_present(NS_0457, [str(LIVE_P1_S13)])
        self.assertTrue(any(f.startswith("p1_series13_month_end_close.xlsx (Purchase Register): NS-0457 is still a row")
                            for f in found), found)

    def test_a_row_that_shows_it_cancelled_or_only_mentions_it_is_right(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "register.xlsx"
            book = openpyxl.Workbook()
            book.active.title = "Purchase Register"
            for row in (["Invoice No", "Vendor", "Taxable Value", "CGST", "Note"],
                        ["NS-0452", "Nova Stationers", 41800, 2880, "As billed"],
                        ["NS-0457", "Nova Stationers", 66700, 4533, "Cancelled - excluded from all totals"],
                        ["Note", "NS-0457 left out (cancelled by the vendor)", None, None, None]):
                book.active.append(row)
            book.save(path)
            self.assertEqual(stage_facts.removed_still_present(NS_0457, [str(path)]), [])
            self.assertEqual(stage_facts.removed_still_present([{"text": "Use the 18 Oct cash figure."}], [str(path)]), [])

    def test_a_csv_is_checked_like_a_workbook(self):
        # Review 2026-11-02: a CSV's cells are all text, and only stored numbers counted as figures - both checks of a
        # removed record found nothing in any CSV.
        with tempfile.TemporaryDirectory() as tmp:
            register = Path(tmp) / "purchase_register.csv"
            register.write_text("Invoice No,Vendor,Taxable Value,CGST,Note\n"
                                "NS-0452,Nova Stationers,41800,2880,As billed\n"
                                "NS-0457,Nova Stationers,\"66,700.00\",4533,As billed\n", encoding="utf-8")
            found = stage_facts.removed_still_present(NS_0457, [str(register)])
            self.assertTrue(found and found[0].startswith("purchase_register.csv: NS-0457 is still a row"), found)
            shown_out = Path(tmp) / "shown_out.csv"
            shown_out.write_text("Invoice No,Vendor,Taxable Value,Note\nNS-0457,Nova Stationers,66700,Cancelled\n"
                                 "NS-0457,see the vendor's mail,,\n", encoding="utf-8")
            self.assertEqual(stage_facts.removed_still_present(NS_0457, [str(shown_out)]), [])

            bill = Path(tmp) / "NS-0457.txt"
            bill.write_text("Nova Stationers\nTax invoice NS-0457\nTaxable 66,700.00\nCGST 4,533.00\n", encoding="utf-8")
            summary = Path(tmp) / "itc.csv"
            summary.write_text("Invoice No,Vendor,CGST,Claimed\nNS-0452,Nova Stationers,2880,\n"
                               "NS-0453,Nova Stationers,1000,\nTotal ITC claimed,,,8413\n", encoding="utf-8")
            residue = stage_facts.removed_residue(NS_0457, [str(summary)], [str(bill)])
            self.assertTrue(residue and "plus 4,533.00 of NS-0457, which was removed" in residue[0], residue)
            summary.write_text("Invoice No,Vendor,CGST,Claimed\nNS-0452,Nova Stationers,2880,\n"
                               "NS-0453,Nova Stationers,1000,\nTotal ITC claimed,,,3880\n", encoding="utf-8")
            self.assertEqual(stage_facts.removed_residue(NS_0457, [str(summary)], [str(bill)]), [])


if __name__ == "__main__":
    unittest.main()
