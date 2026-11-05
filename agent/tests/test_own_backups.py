"""
Finding CA (2026-10-20, round 5 P5): the worker built MIS_Q2_analysis.xlsx, corrected it with an in-place
`office update`, and the "original once" backup left MIS_Q2_analysis.orig.xlsx - the wrong first version - beside the
deliverables. That backup is for the person's own files: a file the task made is updated without one, and a stray one
of the task's own output is removed at the end. A backup of a file that was there before the task is kept.
"""

import time
import unittest
from unittest import mock

from needs import require

require("openpyxl", "python-docx")  # every test here reads or writes workbooks or documents
import openpyxl

from temp_case import TempDirCase

from decurix_agent import source_guard, state, task_context
from decurix_agent.tools import office

TASK = "t" + "c" * 16


def workbook(path, value):
    book = openpyxl.Workbook()
    book.active["A1"] = value
    book.save(path)


class OwnBackups(TempDirCase):
    def setUp(self):
        super().setUp()
        saved = task_context.CURRENT_FILE, state.current_task_id
        self.addCleanup(lambda: (setattr(task_context, "CURRENT_FILE", saved[0]),
                                 setattr(state, "current_task_id", saved[1])))
        task_context.CURRENT_FILE = str(self.dir / "current-task")
        guard = mock.patch.object(source_guard, "manifest", lambda task_id: None)
        guard.start()
        self.addCleanup(guard.stop)
        # The person's workbook, there before the task.
        self.source = self.dir / "receivables.xlsx"
        workbook(self.source, "theirs")
        time.sleep(0.05)
        task_context.start(TASK, request="Update receivables.xlsx and make the analysis.")
        time.sleep(0.05)
        self.own = self.dir / "MIS_Q2_analysis.xlsx"
        workbook(self.own, "first, wrong")

    def test_an_in_place_update_of_a_file_the_task_made_keeps_no_backup(self):
        self.assertTrue(source_guard.made_by_task(self.own))
        office.update_workbook(str(self.own), edits=[{"sheet": "Sheet", "cell": "A1", "value": "corrected"}])
        self.assertFalse((self.dir / "MIS_Q2_analysis.orig.xlsx").exists())
        self.assertEqual(openpyxl.load_workbook(self.own).active["A1"].value, "corrected")

    def test_the_same_for_a_document(self):
        import docx
        letter = self.dir / "offer_letter.docx"
        document = docx.Document()
        document.add_paragraph("Dear {{NAME}},")
        document.save(letter)
        office.update_document(str(letter), {"{{NAME}}": "Ananya"})
        self.assertFalse((self.dir / "offer_letter.orig.docx").exists())

    def test_an_in_place_update_of_a_file_that_was_there_keeps_its_backup(self):
        self.assertFalse(source_guard.made_by_task(self.source))
        office.update_workbook(str(self.source), edits=[{"sheet": "Sheet", "cell": "A1", "value": "changed"}])
        backup = self.dir / "receivables.orig.xlsx"
        self.assertTrue(backup.exists())
        self.assertEqual(openpyxl.load_workbook(backup).active["A1"].value, "theirs")

    def test_no_task_running_keeps_the_backup(self):
        state.current_task_id = ""
        task_context.start("")
        self.assertIsNone(task_context.since())
        self.assertFalse(source_guard.made_by_task(self.own))

    def test_a_resumed_task_keeps_its_start(self):
        began = task_context.since()
        time.sleep(0.02)
        task_context.start(TASK, request="again")
        self.assertEqual(task_context.since(), began)

    def test_the_end_removes_stray_backups_of_own_outputs_only(self):
        # Stray backups as an older build left them: one of the task's own output, one of the person's file.
        own_backup = self.dir / "MIS_Q2_analysis.orig.xlsx"
        workbook(own_backup, "first, wrong")
        source_backup = self.dir / "receivables.orig.xlsx"
        import shutil
        shutil.copy2(self.source, source_backup)
        # A file that merely has ".orig." in its name with no such original beside it is not a backup.
        lone = self.dir / "notes.orig.txt"
        lone.write_text("x", encoding="utf-8")
        removed = source_guard.remove_own_backups([self.dir])
        self.assertEqual(removed, [str(own_backup)])
        self.assertFalse(own_backup.exists())
        self.assertTrue(source_backup.exists())
        self.assertTrue(lone.exists())
        self.assertTrue(self.own.exists())

    def test_a_file_the_request_brought_is_never_the_tasks(self):
        brought = self.dir / "brought.xlsx"
        workbook(brought, "attached")
        with mock.patch.object(task_context, "current_files", lambda: [str(brought)]):
            self.assertFalse(source_guard.made_by_task(brought))


if __name__ == "__main__":
    unittest.main()
