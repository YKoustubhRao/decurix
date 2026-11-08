"""
A long request's ledger (the person's facts, decisions, amendments and questions) is never lost or overwritten
(E-23, review 2026-11-03): a ledger that cannot be read was taken for an empty one and the next update wrote the
defaults over it; a passing privacy failure replaced every record with "[held back]" for good; the run was created
before its ledger, and a stage was parked before its question was written.
"""

import json
import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_long_requests as base

from decurix_agent import long_task
from decurix_agent.computer import privacy


class AnUnreadableLedgerIsKept(base.LongRunBase):
    def test_it_is_moved_aside_and_never_overwritten(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        long_task._update(run_id, lambda d: d["decisions"].append({"stage": "s1", "question": "q", "answer": "a"}))
        path = long_task._ledger_path(run_id)
        path.write_text("{ not json", encoding="utf-8")
        with self.assertRaises(long_task.LedgerError):
            long_task.ledger(run_id)
        with self.assertRaises(long_task.LedgerError):
            long_task._update(run_id, lambda d: d["facts"].append({"text": "x"}))  # nothing is written over it
        aside = long_task._unreadable_copies(run_id)
        self.assertEqual(len(aside), 1)
        self.assertEqual(aside[0].read_text(encoding="utf-8"), "{ not json")
        self.assertFalse(path.exists())

    def test_a_passing_read_error_is_tried_again_and_never_moves_the_ledger(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        long_task._update(run_id, lambda d: d["decisions"].append({"stage": "s1", "question": "q", "answer": "a"}))
        path = long_task._ledger_path(run_id)
        real, calls = type(path).read_text, []

        def busy_twice(self_, *a, **k):
            calls.append(1)
            if self_ == path and len(calls) <= 2:
                raise PermissionError("the file is in use")
            return real(self_, *a, **k)
        with mock.patch.object(long_task, "LEDGER_IO_BACKOFF", 0), mock.patch.object(type(path), "read_text", busy_twice):
            self.assertEqual(long_task.ledger(run_id)["decisions"][-1]["answer"], "a")
        self.assertEqual(len(calls), 3)
        self.assertEqual(long_task._unreadable_copies(run_id), [])

    def test_a_read_error_that_lasts_fails_loudly_and_leaves_the_file(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        path = long_task._ledger_path(run_id)
        before = path.read_text(encoding="utf-8")

        def busy(*a, **k):
            raise PermissionError("the file is in use")
        with mock.patch.object(long_task, "LEDGER_IO_BACKOFF", 0), mock.patch.object(type(path), "read_text", busy):
            with self.assertRaises(long_task.LedgerError):
                long_task.ledger(run_id)
            with self.assertRaises(long_task.LedgerError):
                long_task._update(run_id, lambda d: d["facts"].append({"text": "x"}))
        self.assertEqual(long_task._unreadable_copies(run_id), [])
        self.assertEqual(path.read_text(encoding="utf-8"), before)
        self.assertIn("decisions", long_task.ledger(run_id))  # readable again once the file is free

    def test_a_save_error_is_tried_again_then_fails_with_the_old_file_in_place(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        path = long_task._ledger_path(run_id)
        before = path.read_text(encoding="utf-8")
        tries = []

        def busy(*a, **k):
            tries.append(1)
            raise PermissionError("the file is in use")
        with mock.patch.object(long_task, "LEDGER_IO_BACKOFF", 0), mock.patch.object(privacy, "write_scrubbed", busy):
            with self.assertRaises(long_task.LedgerError):
                long_task._update(run_id, lambda d: d["facts"].append({"text": "x"}))
        self.assertEqual(len(tries), long_task.LEDGER_IO_TRIES)
        self.assertEqual(path.read_text(encoding="utf-8"), before)
        self.assertEqual(long_task._unreadable_copies(run_id), [])
        tries.clear()
        flaky = privacy.write_scrubbed

        def busy_once(*a, **k):
            tries.append(1)
            if len(tries) == 1:
                raise PermissionError("the file is in use")
            return flaky(*a, **k)
        with mock.patch.object(long_task, "LEDGER_IO_BACKOFF", 0), mock.patch.object(privacy, "write_scrubbed", busy_once):
            long_task._update(run_id, lambda d: d["facts"].append({"text": "kept"}))
        self.assertEqual(long_task.ledger(run_id)["facts"][-1]["text"], "kept")

    def test_a_run_with_no_ledger_yet_has_the_empty_shape(self):
        self.assertEqual(long_task.ledger("no-such-run")["decisions"], [])


class APrivacyFailureSkipsTheSave(base.LongRunBase):
    def test_the_records_on_disk_are_never_replaced_by_held_back(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        long_task._update(run_id, lambda d: d["decisions"].append({"stage": "s1", "question": "Which rate?",
                                                                   "answer": "use 18%"}))
        with mock.patch.object(privacy, "withhold_known", lambda text: privacy.HELD_BACK):
            with self.assertRaises(long_task.LedgerError):
                long_task._update(run_id, lambda d: d["facts"].append({"text": "a new fact"}))
        on_disk = json.loads(long_task._ledger_path(run_id).read_text(encoding="utf-8"))
        self.assertEqual(on_disk["decisions"][-1]["answer"], "use 18%")
        self.assertNotIn(privacy.HELD_BACK, json.dumps(on_disk))


class TheLedgerComesFirst(base.LongRunBase):
    def test_the_ledger_is_written_before_the_run_exists(self):
        seen = []
        real = self.runner.create

        def create(spec, inputs=None, run_id=None, work_dir=None):
            seen.append(long_task.ledger(run_id).get("request"))
            return real(spec, inputs, run_id=run_id, work_dir=work_dir)

        with mock.patch.object(self.runner, "create", create), mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        self.assertEqual(seen, [base.LONG])

    def test_a_stage_is_parked_only_after_its_question_is_written(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        order = []

        def park(*a, **k):
            order.append(("park", len(long_task.ledger(run_id)["questions"])))

        with mock.patch.object(self.runner, "park", park), mock.patch.object(long_task, "_dispatch_next", lambda r: None), \
                mock.patch.object(long_task, "_runnable_besides", lambda *a: False), \
                mock.patch.object(long_task, "_show", lambda *a: None):
            long_task._park(run_id, "s1-register", "Which rate?", self.runner.status(run_id))
        self.assertEqual(order, [("park", 1)])


if __name__ == "__main__":
    unittest.main()
