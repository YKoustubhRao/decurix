"""
A pick is backed by the client's documents in a folder the request names by its path, never by a file the task wrote.

Found live (2026-11-09, eval case oc-2, a return filed right): every invoice in the folder the request named wrote
"Place of Supply: Karnataka", yet the "29-Karnataka" picked from the portal's list was held as backed by nothing
given, because only attached files were read; the person was asked to confirm it and the run ended not done.
"""

import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import task_context
from decurix_agent.computer import records

OFFERED = ["29-Karnataka", "32-Kerala", "30-Goa"]
PICK = {"label": "Place of supply", "option": "29-Karnataka", "taken": "29-Karnataka", "offered": OFFERED, "by_id": False}


class TestAPickBackedByTheRequestsFolder(unittest.TestCase):

    def setUp(self):
        self.home = Path(tempfile.mkdtemp())
        self.folder = self.home / "work" / "practice" / "case-7"
        self.folder.mkdir(parents=True)
        (self.folder / "204_invoice.txt").write_text(
            "TAX INVOICE\nBILL TO\nCoastal Agro Foods LLP\nPlace of Supply: Karnataka\nTaxable Amount: 50,000.00\n",
            encoding="utf-8")
        self.request = ("File the November GSTR-1 of the client. Whatever it sent is also in ~/work/practice/case-7/. "
                        "Our apps: TaxNet http://taxnet.example.test.")
        for patcher in (mock.patch.dict(os.environ, {"HOME": str(self.home), "USERPROFILE": str(self.home)}),
                        mock.patch.object(task_context, "current", return_value="t7"),
                        mock.patch.object(task_context, "current_request", return_value=self.request),
                        mock.patch.object(task_context, "current_files", return_value=[]),
                        mock.patch.object(records, "_free_text", return_value="")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def started(self, at):
        return mock.patch.object(task_context, "since", return_value=at)

    def test_an_invoice_in_the_named_folder_backs_the_pick(self):
        with self.started(time.time() + 60):  # (the invoice was there before the task began)
            given = records.given_sources("t7")
            self.assertTrue(records.pick_backed(PICK, given))

    def test_only_the_field_the_document_states_is_backed(self):
        with self.started(time.time() + 60):
            given = records.given_sources("t7")
            other = dict(PICK, label="Supply type")
            self.assertFalse(records.pick_backed(other, given))

    def test_a_file_the_task_wrote_there_backs_nothing(self):
        with self.started(time.time() - 3600):  # (the task began before the file was written: the task made it)
            self.assertEqual(records._request_folder_files("t7"), [])
            self.assertFalse(records.pick_backed(PICK, records.given_sources("t7")))

    def test_with_the_tasks_start_unknown_nothing_in_the_folder_counts(self):
        with self.started(None):
            self.assertEqual(records._request_folder_files("t7"), [])

    def test_the_whole_home_or_work_folder_is_never_the_requests_input(self):
        with self.started(time.time() + 60), \
                mock.patch.object(task_context, "current_request", return_value="Save the result in ~/work/ please."), \
                mock.patch.object(records.config, "WORK_DIR", self.home / "work"):
            self.assertEqual([p for p in records._request_folder_files("t7") if "case-7" in p], [])


if __name__ == "__main__":
    unittest.main()
