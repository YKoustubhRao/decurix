"""
CX (round 5 P4 on pc3, 2026-10-21, series 11): Rohit Kulkarni was onboarded twice in the portal. In ONE attempt the
worker saved him with another joiner's department (EMP-2043), saw it, could not edit it, and entered him again
(EMP-2044) - a duplicate in a live system is worse than a missing record. Not a refusal of the entry check: its tool
record shows both saves in the same attempt. A save that would enter, as new, a record this task already saved is now
refused in code; and a read-back showing a record twice is not done, the person told to delete the extra one.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths

from decurix_agent import long_task
from decurix_agent.computer import core


def typed(text, page=""):
    return {"kind": "type", "outcome": "done", "text": text, "change": page}


def opened(url):
    return {"kind": "click", "outcome": "done", "change": f"page is now {url}; 12 element(s) appeared"}


def saved(number):
    return {"kind": "click", "outcome": "done", "summary": 'click button "Submit onboarding"',
            "change": f'active window is now "Saved - Back Office - Chromium"; page is now http://h:8701/done/employee/{number}'}


ROHIT = [opened("http://h:8701/people/new"), typed("Rohit"), typed("Kulkarni"), typed("rohit.kulkarni@example.com"),
         opened("http://h:8701/people/new?step=2&t=1")]


class TheSaveGuard(unittest.TestCase):
    def test_a_second_new_record_of_the_same_one_is_refused(self):
        events = ROHIT + [saved("EMP-2043")] + ROHIT
        self.assertEqual(core.already_entered(events), ("rohit.kulkarni@example.com", "EMP-2043"))

    def test_the_first_save_and_another_record_pass(self):
        self.assertIsNone(core.already_entered(ROHIT))
        sneha = [opened("http://h:8701/people/new"), typed("Sneha"), typed("Iyer"), typed("sneha.iyer@example.com")]
        self.assertIsNone(core.already_entered(ROHIT + [saved("EMP-2043")] + sneha))

    def test_an_abandoned_form_is_not_carried_into_the_next(self):
        sneha_left = [opened("http://h:8701/people/new"), typed("Sneha"), typed("Iyer"), typed("sneha.iyer@example.com")]
        events = ROHIT + [saved("EMP-2043")] + sneha_left + [opened("http://h:8701/people/new"), typed("Arif"),
                                                              typed("Khan"), typed("arif.khan95@example.com")]
        self.assertIsNone(core.already_entered(events))

    def test_a_new_record_page_visited_earlier_does_not_make_an_existing_one_new(self):
        # Found live (2026-10-21): after a portal's "add invoice" page, saving an existing return on another page
        # (the same login and period typed again) was refused as a second record.
        login = [opened("http://h:8703/login"), typed("udupiagro"), typed("September")]
        add = [opened("http://h:8703/gstr1/b2b/add?period=092026"), typed("451")]
        ret = [opened("http://h:8703/gstr3b?period=092026"), typed("275650")]
        save = {"kind": "click", "outcome": "done", "change": "page is now http://h:8703/gstr3b?period=092026&msg=saved"}
        events = login + ret + [save] + add + login + ret
        self.assertIsNone(core.already_entered(events))

    def test_a_save_the_application_refused_is_no_save(self):
        # Found live (2026-10-25): a form posted to ".../voucher/save" answered "ledger 'IGST' does not exist"; the
        # "/save" in the address was taken for a save, and the bill was then refused as "already saved".
        bill = [opened("http://h:8704/books/voucher/new"), typed("Sales"), typed("Kasaragod Arecanut Processors")]
        failed = {"kind": "click", "outcome": "done", "change": "page is now http://h:8704/books/voucher/save; "
                  "1 element(s) appeared: alert \"ledger 'IGST' does not exist\""}
        self.assertIsNone(core.already_entered(bill + [failed] + bill))

    def test_a_second_bill_to_the_same_customer_is_another_record(self):
        # Found live (2026-10-25): voucher type and party were the "identity", so a month's second bill for one
        # customer was refused as the first one again. Two records whose own numbers differ are two records.
        def bill(no):
            return [opened("http://h:8704/books/voucher/new"), typed("Sales"), typed("Hebri Agro Industries"),
                    {"kind": "type", "outcome": "done", "text": no, "change": "", "element": {"name": "No."}}]
        ok = {"kind": "click", "outcome": "done", "change": 'page is now http://h:8704/books/voucher/12; 1 element(s) '
              'appeared: status "Sales voucher saved."'}
        self.assertIsNone(core.already_entered(bill("1190") + [ok] + bill("1194")))
        self.assertEqual(core.already_entered(bill("1190") + [ok] + bill("1190")), ("Sales Hebri Agro Industries", ""))

    def test_an_edit_form_is_never_refused(self):
        edit = [opened("http://h:8701/people/EMP-2043/edit"), typed("Rohit"), typed("Kulkarni"),
                typed("rohit.kulkarni@example.com")]
        self.assertIsNone(core.already_entered(ROHIT + [saved("EMP-2043")] + edit))

    def test_the_refusal_text_through_the_click(self):
        events = ROHIT + [saved("EMP-2043")] + ROHIT
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"):
            said = core._duplicate_record({"name": "Submit onboarding"})
        self.assertIn("Not pressed: rohit.kulkarni@example.com is already saved in this application (EMP-2043)", said)
        self.assertIn("WRONG IN THE APPLICATION", said)
        self.assertEqual(core._duplicate_record({"name": "Next"}), "")

    def test_the_live_record(self):
        path = fixture_paths.need("p4_duplicate", "events.jsonl")
        events = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
        submits = [i for i, e in enumerate(events)
                   if str(e.get("summary") or "").startswith('click button "Submit onboarding"')]
        found = [core.already_entered(events[:i]) for i in submits]
        # Ananya, Arif, Rohit, (Rohit again), Sneha: only the second Rohit is refused.
        self.assertEqual([bool(f) for f in found], [False, False, False, True, False])
        self.assertEqual(found[3][1], "EMP-2043")


TRACKER = Path(r"C:\Users\Administrator\decurix-tests\wide\process\data\P4\joiners_tracker_Nov2026.xlsx")


class AnotherRecordsValues(unittest.TestCase):
    """CY (round 5 P4 on pc3, 2026-10-21): Rohit Kulkarni's form was saved with Mohammed Arif Khan's department and job
    title. A save whose form holds another record's values from the request's own table is refused."""

    HEADER = ["Full name", "Personal email", "Mobile", "Department", "Job title", "Date of birth"]
    ROWS = [["Mohammed Arif Khan", "arif.khan95@example.com", "+91 99870 44120", "Sales", "Sales Executive", "22/07/1995"],
            ["Rohit Kulkarni", "rohit.kulkarni@example.com", "+91 98500 76231", "Operations", "Warehouse Supervisor",
             "30/01/1993"]]

    def form(self, department, title, dob="30/01/1993", mobile="9850076231"):
        return [("First name", "Rohit"), ("Last name", "Kulkarni"), ("Date of birth (DD/MM/YYYY)", dob),
                ("Personal email", "rohit.kulkarni@example.com"), ("Mobile phone", mobile), ("Department", department),
                ("Job title", title)]

    def test_another_records_values_are_named(self):
        found = core.crossed_values(self.form("Sales", "Sales Executive"), [(self.HEADER, self.ROWS)])
        self.assertEqual(found, [("Department", "Sales", "Mohammed Arif Khan", "Operations"),
                                 ("Job title", "Sales Executive", "Mohammed Arif Khan", "Warehouse Supervisor")])

    def test_its_own_values_in_any_form_pass(self):
        form = self.form("operations", "Warehouse  Supervisor", dob="1993-01-30", mobile="+91 98500 76231")
        self.assertEqual(core.crossed_values(form, [(self.HEADER, self.ROWS)]), [])

    def test_a_value_in_no_row_is_not_decided_here(self):
        self.assertEqual(core.crossed_values(self.form("Logistics", "Supervisor"), [(self.HEADER, self.ROWS)]), [])

    def test_the_refusal_text_through_the_click(self):
        import openpyxl
        events = [opened("http://h:8701/people/new")] + [
            {"kind": "type", "outcome": "done", "text": v, "element": {"name": f}}
            for f, v in self.form("Sales", "Sales Executive")]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "tracker.xlsx"
            wb = openpyxl.Workbook()
            wb.active.append(self.HEADER)
            for row in self.ROWS:
                wb.active.append(row)
            wb.save(path)
            with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                    mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"), \
                    mock.patch("decurix_agent.task_context.current_files", lambda: [str(path)]):
                said = core._crossed_record({"name": "Submit onboarding"})
        self.assertIn("Department is 'Sales' - that is Mohammed Arif Khan's; the source gives 'Operations'", said)

    def test_a_tracker_only_in_the_folder_the_request_names(self):
        # Most requests name a folder and attach nothing ("Everything is in the P4_onboarding_Nov folder").
        import openpyxl
        from decurix_agent import config
        events = [opened("http://h:8701/people/new")] + [
            {"kind": "type", "outcome": "done", "text": v, "element": {"name": f}}
            for f, v in self.form("Sales", "Sales Executive")]
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp) / "P4_onboarding_Nov"
            folder.mkdir()
            wb = openpyxl.Workbook()
            wb.active.append(self.HEADER)
            for row in self.ROWS:
                wb.active.append(row)
            wb.save(folder / "joiners_tracker_Nov2026.xlsx")
            request = "Five joiners for November. Everything is in the P4_onboarding_Nov folder in your work folder."
            with mock.patch.object(config, "WORK_DIR", Path(tmp)), \
                    mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                    mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"), \
                    mock.patch("decurix_agent.task_context.current_files", lambda: []), \
                    mock.patch("decurix_agent.task_context.current_request", lambda: request):
                said = core._crossed_record({"name": "Submit onboarding"})
        self.assertIn("Job title is 'Sales Executive' - that is Mohammed Arif Khan's", said)

    @unittest.skipUnless(TRACKER.is_file(), "round 5 P4's tracker is not on this machine")
    def test_the_live_saves(self):
        from decurix_agent import stage_facts
        path = fixture_paths.need("p4_duplicate", "events.jsonl")
        events = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
        tables = [(h, r) for _, h, r in stage_facts.tables_of([str(TRACKER)])]
        submits = [i for i, e in enumerate(events)
                   if str(e.get("summary") or "").startswith('click button "Submit onboarding"')]
        found = [core.crossed_values(core._current_form(events[:i]), tables) for i in submits]
        self.assertEqual([bool(f) for f in found], [False, False, True, False, False])  # Rohit's first save
        self.assertEqual({w for _, _, w, _ in found[2]}, {"Mohammed Arif Khan"})


class TheReadBack(unittest.TestCase):
    def test_a_record_twice_in_the_list_is_not_done(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        log = Path(tmp.name) / "portal_onboarding_log.csv"
        log.write_text("Name,Status\nRohit Kulkarni,onboarded EMP-2043\nSneha Iyer,onboarded EMP-2045\n",
                       encoding="utf-8")
        request = "Onboard them in our HR portal (http://host.docker.internal:8701) and keep a log."
        step = {"id": "s3", "title": "Onboard approved November joiners in the portal and log entries",
                "instruction": "Onboard each approved joiner in the portal."}
        action = {"kind": "click", "outcome": "done", "expect": {"active_window": "People - Chromium"}}
        listing = ("Employees | EMP-2045 | Sneha Iyer | Marketing\nEMP-2044 | Rohit Kulkarni | Operations\n"
                   "EMP-2043 | Rohit Kulkarni | Sales")
        # The live shape: both of Rohit's records were saved during the stage - each one's saved page shows it.
        saved = "Saved: EMP-2043 Rohit Kulkarni\nSaved: EMP-2044 Rohit Kulkarni\nSaved: EMP-2045 Sneha Iyer\n" + listing
        found = long_task.system_entry_findings("r", step, [str(log)], request, {"t": 0},
                                                record=([action], listing, saved))
        self.assertEqual([c for c, _ in found], ["system_entry_text" if "enters" not in step else "system_entry"])
        self.assertIn("the same record twice: Rohit Kulkarni (EMP-2043, EMP-2044)", found[0][1])
        self.assertIn("never delete a record yourself", found[0][1])
        # With the planner's "enters", it blocks.
        found = long_task.system_entry_findings("r", dict(step, enters="our HR portal"), [str(log)], request, {"t": 0},
                                                record=([action], listing, saved))
        self.assertEqual([c for c, _ in found], ["system_entry"])
        # Nothing says both were made here (a namesake already in the system): evidence, never a refusal (independent
        # review, 2026-10-21, #5c).
        found = long_task.system_entry_findings("r", dict(step, enters="our HR portal"), [str(log)], request, {"t": 0},
                                                record=([action], listing))
        self.assertEqual([c for c, _ in found], ["system_entry_text"])
        self.assertIn("more than one record for: Rohit Kulkarni (EMP-2043, EMP-2044)", found[0][1])
        once = listing.replace("\nEMP-2043 | Rohit Kulkarni | Sales", "")
        log.write_text("Name,Status\nRohit Kulkarni,onboarded EMP-2044\nSneha Iyer,onboarded EMP-2045\n", encoding="utf-8")
        self.assertEqual(long_task.system_entry_problems("r", step, [str(log)], request, {"t": 0},
                                                         record=([action], once)), [])


if __name__ == "__main__":
    unittest.main()
