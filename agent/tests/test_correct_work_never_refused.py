"""
Correct work is never refused (independent review, 2026-10-21; the owner's rule: code blocks only on what it decides
mechanically and with certainty - a check that guesses is evidence, never a verdict). Realistic CORRECT outputs no
benchmark ever saw must meet no blocking finding from any code check; a few clearly WRONG ones must still be blocked.
Every new check passes this in the spirit of the replay gate: if it would refuse one of these, it guesses.

Data is built in code; no fixture files.
"""

import datetime
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, produced, provenance, source_guard, stage_facts
from decurix_agent.tools import office, table_quality


def csv_file(folder, name, rows):
    path = Path(folder) / name
    path.write_text("\n".join(",".join(f'"{c}"' if "," in str(c) else str(c) for c in row) for row in rows) + "\n",
                    encoding="utf-8")
    return path


class World(unittest.TestCase):
    """A work folder of the test's own, the request's inputs in it, nothing read off any screen."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        for patch in (mock.patch.object(config, "WORK_DIR", self.dir), mock.patch.object(config, "DECURIX_DIR", self.dir),
                      mock.patch.object(long_task, "_page_notes", lambda *a: [])):
            patch.start()
            self.addCleanup(patch.stop)

    def drafts_block(self, drafts, request):
        """The blocking findings of the draft checks (provenance), and the evidence ones."""

        return long_task.draft_checks(drafts, request)

    def assertNothingBlocks(self, findings, what):
        self.assertEqual([f for f in findings if f], [], f"{what}: correct work was refused")


class CorrectWorkPasses(World):

    def test_a_travel_booking_reply_with_a_computed_trip_total(self):
        rates = csv_file(self.dir, "hotel_rates.csv", [["Hotel", "City", "Rate per night (Rs)"],
                                                      ["Seaview Inn", "Goa", "4,500"], ["Hill Lodge", "Ooty", "3,200"]])
        request = f"Draft a reply to the guest confirming 3 nights at Seaview Inn, from {rates}, with the trip total."
        draft = {"to": ["guest@example.com"], "subject": "Your stay at Seaview Inn",
                 "body": "Dear guest, your 3 nights at Seaview Inn at Rs 4,500 a night come to Rs 13,500 in total. "
                         "Check-in is on 24 Oct 2026."}
        unsourced, _fields = self.drafts_block([draft], request)
        self.assertNothingBlocks(unsourced, "a trip total of 3 x 4,500")

    def test_a_fee_reminder_with_a_due_date(self):
        fees = csv_file(self.dir, "fees.csv", [["Student", "Parent email", "Term fee (Rs)"],
                                               ["Asha Rao", "parent@example.com", "12,000"]])
        request = f"Send a polite fee reminder to each parent in {fees}; give them a week to pay."
        due = (datetime.date.today() + datetime.timedelta(days=7)).strftime("%d %b %Y")
        draft = {"to": ["parent@example.com"], "subject": "Term fee reminder",
                 "body": f"Dear parent, the term fee of Rs 12,000 for Asha Rao is due. Please pay by {due}. With 18% "
                         "GST it is shown on the invoice as Rs 14,160."}
        unsourced, _ = self.drafts_block([draft], request)
        self.assertNothingBlocks(unsourced, "a due date a week from today, and 12,000 plus 18%")

    def test_a_payroll_memo_with_fy27_and_a_phone_in_another_format(self):
        staff = csv_file(self.dir, "staff.csv", [["Name", "Phone", "Annual CTC (Rs)"],
                                                 ["Ravi Kumar", "9876543210", "6,00,000"]])
        request = f"Write the payroll memo for FY27 from {staff}."
        memo = {"to": ["hr@example.com"], "subject": "FY27 payroll",
                "body": "For FY27, Ravi Kumar's CTC is Rs 6,00,000 (Rs 50,000 a month). Reach him on +91 98765 43210 "
                        "for the 2FA set-up of the Q3 portal."}
        unsourced, _ = self.drafts_block([memo], request)
        self.assertNothingBlocks(unsourced, "FY27, 2FA, Q3, a monthly figure and a phone with its country code")

    def test_a_stock_count_with_repeated_rows_is_quiet_and_intentional_blanks_are_evidence(self):
        count = csv_file(self.dir, "stock_count.csv", [["Item", "Bin", "Qty"], ["Bolt M6", "A1", "500"],
                                                       ["Nut M6", "A2", "400"], ["Bolt M6", "A1", "500"],
                                                       ["Washer", "B1", "300"]])
        found = table_quality.inflated_totals("Total counted 1700", "", [str(count)], detail=True)
        self.assertTrue(found)
        self.assertEqual([loud for _, loud in found], [False])  # a quiet line, never the "Needs you" headline
        # A bonus column filled only for the eligible rows: the blanks are right.
        book = self.dir / "bonus.xlsx"
        office.create_workbook(str(book), [{"name": "Bonus", "rows": [
            ["Name", "Eligible", "Salary", "Bonus"], ["A", "Yes", 1000, "=C2*0.1"], ["B", "Yes", 2000, "=C3*0.1"],
            ["C", "Yes", 1500, "=C4*0.1"], ["D", "No", 1200, None], ["Total", None, "=SUM(C2:C5)", "=SUM(D2:D4)"]]}])
        # (As on the worker, where LibreOffice stores the results: here they are taken as stored.)
        with mock.patch.object(produced, "store_results", lambda paths: []):
            facts = produced.file_facts("Make the bonus sheet: 10% of salary for eligible staff.", [str(book)])
        self.assertNothingBlocks(facts.gaps + facts.file_faults, "intentional blanks")

    def test_an_expense_form_the_request_said_to_fill_in(self):
        request = ("Fill in the attached expense form with my October trips.\n\nFiles attached to this request, saved "
                   "on this desktop:\n  /home/headless/inbox/expense_form.xlsx\n")
        self.assertTrue(source_guard.may_change("expense_form.xlsx", request))
        self.assertTrue(source_guard.may_change("Onboarding_Tracker.xlsx", "The tracker is Onboarding_Tracker.xlsx in "
                                                "the folder. Update it with each joiner's employee ID."))

    def test_a_password_protected_workbook_is_not_called_broken(self):
        locked = self.dir / "salaries.xlsx"
        locked.write_bytes(b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" + b"\x00" * 512)
        self.assertNotIn("broken", [s for s, _ in produced.open_statuses([str(locked)]).values()])
        facts = produced.file_facts("Protect the salary workbook with a password.", [str(locked)])
        self.assertNothingBlocks(facts.file_faults, "a protected workbook")
        self.assertIn("password-protected", " ".join(facts.notes()))

    def test_an_onboarding_welcome_naming_a_title_and_a_place(self):
        joiners = csv_file(self.dir, "joiners.csv", [["Full name", "Personal email", "Reporting manager", "Joining date"],
                                                     ["Arjun Mehta", "arjun@example.com", "Priya Nair", "03 Nov 2026"]])
        request = f"Draft a welcome email to each joiner in {joiners}."
        draft = {"to": ["arjun@example.com"], "subject": "Welcome",
                 "body": "Dear Arjun, welcome aboard! Your reporting manager is the Engineering Lead, Priya Nair, at "
                         "the Head Office. You join on 03 Nov 2026."}
        unsourced, _ = self.drafts_block([draft], request)
        self.assertNothingBlocks(unsourced, "a title and a place before the right name")

    def test_a_request_that_reverses_an_earlier_removal(self):
        book = self.dir / "register.xlsx"
        office.create_workbook(str(book), [{"name": "Register", "rows": [
            ["Invoice No", "Vendor", "Amount"], ["INV-1001", "Apex", 12000], ["INV-1002", "Nova", 8000]]}])
        amendments = [{"n": 1, "text": "Remove INV-1002 from the register.", "removed": ["INV-1002"]},
                      {"n": 2, "text": "Actually keep INV-1002 - it was paid after all.", "restored": ["INV-1002"]}]
        self.assertNothingBlocks(stage_facts.removed_still_present(amendments, [str(book)]), "a removal reversed")


class WrongWorkIsStillBlocked(World):

    def test_an_invented_amount_in_a_draft(self):
        fees = csv_file(self.dir, "fees.csv", [["Student", "Parent email", "Term fee (Rs)"],
                                               ["Asha Rao", "parent@example.com", "12,000"]])
        request = f"Send a fee reminder to each parent in {fees}."
        draft = {"to": ["parent@example.com"], "subject": "Term fee reminder",
                 "body": "Dear parent, the term fee for Asha Rao is Rs 13,750. Please pay soon."}
        unsourced, _ = self.drafts_block([draft], request)
        self.assertTrue(unsourced)
        self.assertIn("13,750", unsourced[0])

    def test_a_typed_amount_no_source_holds(self):
        fees = csv_file(self.dir, "fees.csv", [["Student", "Term fee (Rs)"], ["Asha Rao", "12,000"]])
        found = long_task.invented_typing(["Asha Rao", "13750"], f"Enter the fee for each student in {fees}.")
        self.assertTrue(found)

    def test_an_unasked_change_to_a_given_file(self):
        self.assertFalse(source_guard.may_change("receivables.xlsx", "From receivables.xlsx write reminders.docx."))

    def test_a_file_that_no_longer_opens(self):
        book = self.dir / "register.xlsx"
        office.create_workbook(str(book), [{"name": "Register", "rows": [["Vendor", "Amount"], ["Apex", 1200]]}])
        self.assertEqual(produced.no_longer_open([str(book)]), [])
        self.assertTrue(produced.opened_ok(book))
        with zipfile.ZipFile(book) as z:
            parts = {n: z.read(n) for n in z.namelist()}
        parts["xl/workbook.xml"] = b"<not xml"
        with zipfile.ZipFile(book, "w") as z:
            for name, data in parts.items():
                z.writestr(name, data)
        found = produced.no_longer_open([str(book)])
        self.assertTrue(found)
        self.assertIn("register.xlsx cannot be opened", found[0])
        # A file never seen opening here is its own stage's to decide, not the run-wide re-check's.
        other = self.dir / "never.xlsx"
        other.write_bytes(b"PK\x03\x04 broken from the start")
        self.assertEqual(produced.no_longer_open([str(other)]), [])
        self.assertIn("broken", [s for s, _ in produced.open_statuses([str(other)]).values()])


class TheFiguresAtHand(unittest.TestCase):
    """provenance.derived: one arithmetic step from the figures at hand is the text's own work."""

    def test_one_step_explains_and_an_invented_figure_does_not(self):
        sources = provenance.Sources(["Rate Rs 4,500 per night. Fee Rs 12,000. Phone 9876543210."])
        for text in ("Total 13,500 for 3 nights", "Rs 12,000 less Rs 1,500 off is Rs 10,500", "18% GST on 4,500 is 810",
                     "half of 12,000 is 6,000 each in 2 instalments", "+91 98765 43210", "call 98765-43210"):
            self.assertEqual(sources.unsourced(text), [], text)
        self.assertEqual(sources.unsourced("Total Rs 13,750"), ["Rs 13,750"])
        self.assertEqual(sources.doubtful("FY27 and Pay by 24 Oct 2026"), ["24 Oct 2026", "FY27"])

    def test_a_value_explained_only_by_the_works_own_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            working = Path(tmp) / "working.csv"
            working.write_text("Item,Total\nTrip,\"13,750\"\n", encoding="utf-8")
            given = provenance.Sources(["Rate Rs 4,500 per night."])
            self.assertEqual(provenance.own_work_only("Total Rs 13,750", given, [str(working)]),
                             [("Rs 13,750", ["working.csv"])])


class TheJudgeSeesWhatMatters(unittest.TestCase):
    """produced.sample_table: a long table is shown by its header, the rows findings name, its totals and a sample."""

    def test_the_rows_a_finding_names_and_the_totals_are_shown(self):
        lines = ["A1='Order' B1='Amount'"] + [f"A{i}='SO-{5000 + i}' B{i}={i * 10}" for i in range(2, 2002)]
        lines.append("A2002='Total' B2002=20030000")
        body = "\n".join(lines)
        shown = produced.sample_table(body, 7000, produced.focus_tokens(["SO-6500 has no working"]))
        self.assertLessEqual(len(shown), 7000)
        self.assertIn("A1='Order'", shown)
        self.assertIn("SO-6500", shown)
        self.assertIn("'Total'", shown)
        self.assertIn("line(s) not shown", shown)
        # An even sample of the rest - not only the first rows.
        self.assertTrue(any(f"SO-{n}" in shown for n in range(6600, 7000)))



class EveryStageCheckOnASyntheticStage(unittest.TestCase):
    """Every check of long_task.stage_code_findings at once, on a correct synthetic stage (stage_check_harness)."""

    def test_a_welcome_draft_naming_a_title_and_a_place_is_never_refused(self):
        from stage_check_harness import run_checks
        tracker = "Name,Email,Joining date,Manager\nPriya Nair,priya.nair@example.com,02 Nov 2026,Priya Nair\n"
        draft = {"id": "d1", "to": ["priya.nair@example.com"], "subject": "Welcome aboard",
                 "body": "Dear Priya, welcome! You join on 02 Nov 2026 and report to our Engineering Lead, Priya Nair, "
                         "at the Head Office."}
        found = run_checks({"title": "Welcome drafts", "does": "Draft a welcome email to each joiner in joiners.csv.",
                            "drafts": 1}, {"joiners.csv": tracker},
                           request="Draft a welcome email to each joiner in joiners.csv.", drafts=[draft])
        self.assertEqual([f for _, f, blocks in found if blocks], [], found)

    def test_a_stock_count_marked_ok_is_no_claim_of_an_entry(self):
        from stage_check_harness import run_checks
        found = run_checks({"title": "Stock count", "does": "Count the stock and mark each line.", "enters": None},
                           {"stock_count.csv": "Item,Counted,Result\nA4 paper,120,OK\nToner,8,Yes\n"},
                           request="Count the stock in our store system (http://host.docker.internal:8703).",
                           tool_record=([], ""))
        self.assertEqual([f for _, f, blocks in found if blocks], [], found)


if __name__ == "__main__":
    unittest.main()
