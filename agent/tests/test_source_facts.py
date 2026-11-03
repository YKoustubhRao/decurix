"""
A given file the task changed is a fact for the judge and the person, never reverted inside the check (review
2026-11-03, E-12): "Record today's payments in ledger.csv" is in-place work, and the gate used to put the original
back before anything judged it, because no verb of its list was in the request.
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

TASK = "t0fedcba987654321"
BEFORE = b"date,payee,amount\n2026-10-01,Alpha Supplies,100\n"


class ChangedSources(SharedBase):
    def setUp(self):
        super().setUp()
        self.put("ledger.csv", BEFORE, age=3600)
        self.put("other.csv", b"not given\n", age=3600)

    def snap(self, request):
        source_guard.take(request, TASK)

    def change(self, name, data):
        path = self.mount / name
        path.write_bytes(data)
        os.utime(path, (time.time(), time.time()))

    def test_in_place_work_worded_without_an_edit_verb_is_a_fact_and_is_not_put_back(self):
        for request in ("Record today's payments in ledger.csv.", "Post the October entries to ledger.csv.",
                        "Complete ledger.csv.", "Tick off every paid line in ledger.csv."):
            with self.subTest(request=request):
                self.put("ledger.csv", BEFORE, age=3600)
                self.snap(request)
                after = BEFORE + b"2026-10-02,Beta Traders,250\n"
                self.change("ledger.csv", after)
                facts = source_guard.changed_sources(TASK, request)
                self.assertEqual([f["name"] for f in facts], ["ledger.csv"])
                self.assertIn("Beta Traders", facts[0]["diff"])
                self.assertTrue(facts[0]["kept"])
                self.assertEqual((self.mount / "ledger.csv").read_bytes(), after)  # never put back by the check

    def test_the_judge_and_the_person_are_told(self):
        request = "Record today's payments in ledger.csv."
        self.snap(request)
        self.change("ledger.csv", BEFORE + b"2026-10-02,Beta Traders,250\n")
        facts = source_guard.changed_sources(TASK, request)
        judge = source_guard.judge_lines(facts)
        self.assertTrue(judge[0].startswith("Checked in code (not a claim): the request's file"))
        person = source_guard.report_lines(facts)
        self.assertIn("Changed your file", person[0])
        self.assertIn('say "put back ledger.csv"', person[0])

    def test_a_file_the_request_does_not_give_and_an_unchanged_one_are_not_facts(self):
        request = "Record today's payments in ledger.csv."
        self.snap(request)
        self.change("other.csv", b"changed by someone else\n")
        self.assertEqual(source_guard.changed_sources(TASK, request), [])

    def test_a_gone_file_is_a_fact(self):
        request = "Record today's payments in ledger.csv."
        self.snap(request)
        (self.mount / "ledger.csv").unlink()
        facts = source_guard.changed_sources(TASK, request)
        self.assertTrue(facts and facts[0]["gone"])

    def test_notes_that_cannot_be_read_mean_not_checked(self):
        request = "Record today's payments in ledger.csv."
        self.snap(request)
        source_guard._manifest_path(TASK).write_text("{not json", encoding="utf-8")
        with self.assertRaises(source_guard.NotChecked):
            source_guard.changed_sources(TASK, request)

    def test_putting_back_is_its_own_step_and_keeps_the_workers_version(self):
        request = "Record today's payments in ledger.csv."
        self.snap(request)
        after = BEFORE + b"2026-10-02,Beta Traders,250\n"
        self.change("ledger.csv", after)
        done, failed, kept = source_guard.restore_sources(TASK, ["ledger.csv"])
        self.assertEqual((done, failed), (["ledger.csv"], []))
        self.assertEqual((self.mount / "ledger.csv").read_bytes(), BEFORE)
        self.assertEqual(Path(kept["ledger.csv"]).read_bytes(), after)


if __name__ == "__main__":
    unittest.main()
