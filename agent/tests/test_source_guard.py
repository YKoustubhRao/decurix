"""
Finding AB (2026-10-20, xl_overdue on a shared-folder worker): the result was right, but the person's INPUT workbook
receivables.xlsx was saved with a new column 50 s later. The person's files are noted and copied when a task starts;
a source the task changed is put back and reported in plain words; and the checks of what was made never run on a
source.
"""

import os
import sys
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from test_shared_folder import HOST, SharedBase

from decurix_agent import config, source_guard

# The live request (decurix-tests/wide/apps/appsrun.py, xl_overdue).
LIVE = ("Use Excel (the Excel app on your desktop) to open receivables.xlsx from " + HOST + ". As of 15 October 2026: "
        "add a column Days Overdue (days past the due date for invoices that are not Paid; 0 otherwise), give every "
        "overdue invoice's row a red fill, turn on filters on the header row and freeze the header row. Then add a "
        "sheet named By Customer with each customer's total overdue amount, largest first. Save it in the same folder "
        "as receivables_review.xlsx.")
TASK = "t0123456789abcdef"
ORIGINAL = b"PK\x03\x04 the person's receivables"


class Guard(SharedBase):
    def setUp(self):
        super().setUp()
        patch = mock.patch.object(config, "DECURIX_DIR", self.root / "decurix")
        patch.start()
        self.addCleanup(patch.stop)
        self.put("receivables.xlsx", ORIGINAL, age=3600)
        self.put("notes/other.docx", b"other", age=3600)
        self.notes = []
        source_guard.take(LIVE, TASK, notify=self.notes.append)

    def touch(self, name, data):
        path = self.mount / name
        path.write_bytes(data)
        os.utime(path, (time.time(), time.time()))

    def test_the_live_case_is_caught_put_back_and_said_plainly(self):
        self.put("receivables_review.xlsx", b"PK\x03\x04 the result")
        self.touch("receivables.xlsx", b"PK\x03\x04 with a Days Overdue column")
        said = source_guard.check_and_restore(TASK, LIVE)
        self.assertIn("The task changed your file " + HOST + "\\receivables.xlsx, which the request did not ask to change",
                      said)
        self.assertIn("the original is back", said)
        self.assertEqual((self.mount / "receivables.xlsx").read_bytes(), ORIGINAL)
        # The worker's version is never discarded: kept beside the original, named in the finding (one fixed name, so
        # the finding reads the same on every attempt).
        beside = self.mount / "receivables (changed by worker).xlsx"
        self.assertEqual(beside.read_bytes(), b"PK\x03\x04 with a Days Overdue column")
        self.assertIn("receivables (changed by worker).xlsx - if that change is what you wanted, use it", said)
        # The result the request asked for is not a source and is left as it is.
        self.assertEqual((self.mount / "receivables_review.xlsx").read_bytes(), b"PK\x03\x04 the result")
        self.assertEqual(source_guard.check_and_restore(TASK, LIVE), "")

    def test_an_app_saving_a_workbook_again_with_the_same_cells_is_a_note_not_a_change(self):
        import openpyxl
        path = self.mount / "monthly_revenue.xlsx"
        book = openpyxl.Workbook()
        book.active.title = "Revenue"
        for row in (["Month", "Revenue"], ["Jul", 1200], ["Aug", 1350]):
            book.active.append(row)
        book.save(path)
        os.utime(path, (time.time() - 3600, time.time() - 3600))
        request = "Use Excel to open monthly_revenue.xlsx and make a line chart; save it as revenue_chart.xlsx."
        source_guard.take(request, TASK)
        # Re-saved by an app: other bytes (a property, a size), the same cells.
        book = openpyxl.load_workbook(path)
        book.properties.creator = "Excel"
        book.active.column_dimensions["B"].width = 18
        book.save(path)
        os.utime(path, (time.time(), time.time()))
        self.assertEqual(source_guard.changed(TASK, request), [])
        self.assertEqual(source_guard.resaved(TASK, request), ["monthly_revenue.xlsx"])
        self.assertEqual(source_guard.check_and_restore(TASK, request), "")
        # Changed data is a change: a column added, half filled (the live receivables case).
        book = openpyxl.load_workbook(path)
        book.active["C1"], book.active["C2"] = "Days Overdue", "=TODAY()-B2"
        book.save(path)
        self.assertEqual(source_guard.changed(TASK, request), ["monthly_revenue.xlsx"])
        self.assertIn("which the request did not ask to change", source_guard.check_and_restore(TASK, request))
        self.assertIsNone(openpyxl.load_workbook(path).active["C1"].value)

    def test_a_file_saved_again_unchanged_is_not_a_change(self):
        self.touch("receivables.xlsx", ORIGINAL)  # new time, same bytes
        self.assertEqual(source_guard.changed(TASK, LIVE), [])

    def test_a_source_the_request_gives_counts_and_a_deleted_one_too(self):
        (self.mount / "receivables.xlsx").unlink()
        self.assertEqual(source_guard.changed(TASK, LIVE), ["receivables.xlsx"])
        source_guard.check_and_restore(TASK, LIVE)
        self.assertEqual((self.mount / "receivables.xlsx").read_bytes(), ORIGINAL)

    def test_another_file_of_the_shared_folder_is_never_judged_or_put_back(self):
        # Found live (2026-10-21, two workers on one host folder): a task was refused three times for "changing" a memo
        # another run was writing, and could not put it back. Only the sources the request gives are the task's.
        request = "Use Excel to open receivables.xlsx and save the review as receivables_review.xlsx."
        source_guard.take(request, TASK)
        self.touch("notes/other.docx", b"the person edited it meanwhile")
        self.assertEqual(source_guard.changed(TASK, request), [])
        self.assertEqual(source_guard.check_and_restore(TASK, request), "")
        self.assertEqual((self.mount / "notes" / "other.docx").read_bytes(), b"the person edited it meanwhile")
        # The named input is still guarded: restored, and said.
        self.touch("receivables.xlsx", b"PK\x03\x04 with a Days Overdue column")
        self.assertIn("which the request did not ask to change", source_guard.check_and_restore(TASK, request))
        self.assertEqual((self.mount / "receivables.xlsx").read_bytes(), ORIGINAL)

    def test_a_file_the_request_asks_to_change_may_change(self):
        for request in ("Update receivables.xlsx: add a Days Overdue column.",
                        "In receivables.xlsx, highlight the overdue rows in red.",
                        "Save the result as receivables.xlsx.",
                        "Make the review and save it into receivables.xlsx in the same folder."):
            self.assertTrue(source_guard.may_change("receivables.xlsx", request), request)
        for request in (LIVE, "Open receivables.xlsx, add a column and save it as receivables_review.xlsx.",
                        "From receivables.xlsx write reminders.docx.", "Read old_receivables.xlsx."):
            self.assertFalse(source_guard.may_change("receivables.xlsx", request), request)
        self.touch("receivables.xlsx", b"changed as asked")
        self.assertEqual(source_guard.changed(TASK, "Update receivables.xlsx: add a Days Overdue column."), [])

    def test_an_edit_asked_anywhere_in_the_request_counts(self):
        # Independent review (2026-10-21): sentence-local scope refused these, and restored the worker's right work.
        self.assertTrue(source_guard.may_change("Onboarding_Tracker.xlsx", "The tracker is Onboarding_Tracker.xlsx in the "
                                                "folder. Update it with each joiner's employee ID."))
        attached = ("Fill in the attached expense form with my October trips.\n\nFiles attached to this request, saved "
                    "on this desktop:\n  /home/headless/inbox/expense_form.xlsx\n")
        self.assertTrue(source_guard.may_change("expense_form.xlsx", attached))
        self.assertTrue(source_guard.may_change("receivables.xlsx", "Open receivables.xlsx. Then update the receivables "
                                                "workbook with a Days Overdue column."))
        # Named, but the edit is of something else, or the original is kept: not this file's change.
        self.assertFalse(source_guard.may_change("receivables.xlsx", "Read receivables.xlsx. Update the CRM with it."
                                                 " Save the result as review.xlsx."))

    def test_sources_are_not_outputs(self):
        self.assertTrue(source_guard.is_source(self.mount / "receivables.xlsx", TASK, LIVE))
        self.assertFalse(source_guard.is_source(self.mount / "receivables_review.xlsx", TASK, LIVE))
        self.assertFalse(source_guard.is_source(Path("/home/headless/work/receivables.xlsx"), TASK, LIVE))

    def test_a_file_of_the_folder_the_request_does_not_give_is_the_works_when_changed(self):
        # Found live (2026-10-22): "bring the computation workbook up to date" names the client's workbook by what it
        # is; it was neither guarded nor checked, and the final check could not see what the work had written in it.
        self.assertFalse(source_guard.is_source(self.mount / "notes" / "other.docx", TASK, LIVE))
        self.touch("notes/other.docx", b"changed by the work")
        self.assertEqual(source_guard.changed(TASK, LIVE), [])

    def test_a_file_too_large_to_copy_is_still_watched_and_the_person_is_told(self):
        with mock.patch.object(source_guard, "COPY_MAX", 4):
            notes = []
            source_guard.take(LIVE, TASK, notify=notes.append)
        self.assertIn("receivables.xlsx", " ".join(notes))
        self.touch("receivables.xlsx", b"changed")
        said = source_guard.check_and_restore(TASK, LIVE)
        self.assertIn("could not be put back: no copy was kept (too large)", said)

    def test_nothing_without_a_shared_folder(self):
        with mock.patch.dict(os.environ, {"DECURIX_SHARED_FOLDER": ""}):
            self.assertIsNone(source_guard.take(LIVE, "t2"))
            self.assertEqual(source_guard.check_and_restore("t2", LIVE), "")


from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)

from decurix_agent import skill_task, verify  # noqa: E402


class TheGateSaysYouChangedMyFile(FileWorkIsChecked):
    def test_a_changed_source_is_not_done_in_plain_words_without_a_model_and_is_never_read_as_an_output(self):
        self.verdicts = []
        said = ("The task changed your file C:\\Users\\Administrator\\Documents\\Decurix demo\\receivables.xlsx, which "
                "should have stayed as it was; I put the original back from the copy taken before the task "
                "(receivables.xlsx).")
        with mock.patch.object(skill_task.source_guard, "check_and_restore", lambda task_id, request: said), \
                mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""):
            self.assertTrue(skill_task._gate("Done: receivables_review.xlsx saved and verified.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("The task changed your file", self.asked[-1][0])
        self.assertIn("should have stayed as it was", self.asked[-1][0])

    def test_the_contract_why_says_open_never_save(self):
        import inspect
        source = inspect.getsource(skill_task._gate)
        self.assertIn("open it, never save", source)
        self.assertIn("is_source(", source)


if __name__ == "__main__":
    unittest.main()
