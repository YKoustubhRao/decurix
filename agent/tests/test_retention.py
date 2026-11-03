"""
What labels a run is kept until the control plane has exported it (W0.8 of the plan of 2026-11-03; retention.py).

The notes of a run were deleted after 2 days and its recording pruned oldest-first past the disk budget, whatever the
run's state: the evidence of graded runs vanished before anyone exported it. A labelled run (its final report went out,
or a person replied on it) is now held until the control plane writes EXPORTED; then it is pruned as before.
"""

import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, messaging, retention, state
from decurix_agent.computer import evidence, recording, screen

A, B, C = "t000000000000a001", "t000000000000a002", "t000000000000a003"


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.logged = []
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(recording, "ROOT", self.root / "recordings"),
                  mock.patch.object(evidence, "NOTES_DIR", self.root / "notes"),
                  mock.patch.object(retention, "log", lambda line: self.logged.append(line)),
                  mock.patch("decurix_agent.progress.emit", lambda *a, **k: None)):
            p.start()
            self.addCleanup(p.stop)

    def export(self, task):
        (self.root / "runs" / task / "EXPORTED").write_text("e_123\nabc\n", encoding="utf-8")


class TheMarks(Base):
    def test_labelled_then_exported(self):
        self.assertFalse(retention.held(A))
        self.assertTrue(retention.mark_labelled(A, "final"))
        self.assertTrue(retention.held(A))
        self.export(A)
        self.assertTrue(retention.exported(A))
        self.assertFalse(retention.held(A))

    def test_the_reasons_go_into_the_manifest_once(self):
        retention.mark_labelled(A, "reply")
        retention.mark_labelled(A, "final")
        retention.mark_labelled(A, "final")
        data = json.loads((self.root / "runs" / A / "run.json").read_text(encoding="utf-8"))
        self.assertEqual(data["labelled_by"], ["reply", "final"])
        self.assertEqual(data["schema"], "decurix.run-manifest/v1")

    def test_an_invalid_task_marks_nothing(self):
        self.assertFalse(retention.mark_labelled("../x", "final"))
        self.assertFalse(retention.held("../x"))
        self.assertFalse((self.root / "runs").exists())

    def test_an_unwritable_folder_never_raises(self):
        (self.root / "runs").write_text("not a folder", encoding="utf-8")
        self.assertFalse(retention.mark_labelled(A, "final"))
        self.assertTrue(any("could not mark" in line for line in self.logged))

    def test_an_empty_exported_file_is_not_an_export(self):
        retention.mark_labelled(A, "final")
        (self.root / "runs" / A / "EXPORTED").write_text("", encoding="utf-8")
        self.assertTrue(retention.held(A))


class TheNotes(Base):
    def old_note(self, task):
        evidence.NOTES_DIR.mkdir(parents=True, exist_ok=True)
        path = evidence.NOTES_DIR / f"{task}.jsonl"
        path.write_text('{"text": "x"}\n', encoding="utf-8")
        then = time.time() - evidence.NOTES_KEEP_SECONDS - 60
        os.utime(path, (then, then))
        return path

    def test_held_exported_and_unlabelled(self):
        held, done, plain = self.old_note(A), self.old_note(B), self.old_note(C)
        retention.mark_labelled(A, "final")
        retention.mark_labelled(B, "final")
        self.export(B)
        evidence._prune()
        self.assertTrue(held.exists())
        self.assertFalse(done.exists())
        self.assertFalse(plain.exists())


class TheRecordings(Base):
    def frame(self, fill):
        return screen.Frame(4, 4, bytearray(bytes([fill]) * 48))

    def record(self, task, at, fill):
        recording.append(f"task-{task}", {"kind": "click", "actor": "agent"}, before=self.frame(fill), at=at)

    def left(self):
        return {m["id"] for m in recording.sessions()}

    def test_over_budget_with_every_run_held_nothing_is_deleted_and_it_is_said(self):
        for task in (A, B):
            retention.mark_labelled(task, "final")
        self.record(A, 1000, 1)
        self.record(B, 2000, 2)
        retention.mark_labelled(C, "final")
        with mock.patch.object(recording, "MAX_BYTES", 1):
            self.record(C, 3000, 3)
        self.assertEqual(self.left(), {f"task-{A}", f"task-{B}", f"task-{C}"})
        said = [line for line in self.logged if "kept past the disk budget" in line]
        self.assertEqual(len(said), 1)
        self.assertIn("2 recordings", said[0])

    def test_it_is_said_at_most_hourly(self):
        retention.warn_kept(2, "recordings")
        retention.warn_kept(3, "recordings")
        self.assertEqual(len([line for line in self.logged if "kept past" in line]), 1)

    def test_once_exported_a_run_is_pruned_as_before(self):
        for task in (A, B):
            retention.mark_labelled(task, "final")
        self.record(A, 1000, 1)
        self.record(B, 2000, 2)
        self.export(A)
        with mock.patch.object(recording, "MAX_BYTES", 1):
            self.record(C, 3000, 3)
        self.assertEqual(self.left(), {f"task-{B}", f"task-{C}"})

    def test_an_unlabelled_run_is_pruned_as_before(self):
        self.record(A, 1000, 1)
        with mock.patch.object(recording, "MAX_BYTES", 1):
            self.record(B, 2000, 2)
        self.assertEqual(self.left(), {f"task-{B}"})


class WhatLabelsARun(Base):
    def test_the_final_report(self):
        with mock.patch.object(messaging, "send_query", lambda *a, **k: True), \
                mock.patch.object(messaging, "_keep_facts", lambda *a, **k: None), \
                mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(config, "QUERYTEXT_DIR", self.root), \
                mock.patch.object(state, "current_task_id", A):
            messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertTrue(retention.held(A))
        self.assertEqual(retention.read_manifest(A)["labelled_by"][0], "final")


if __name__ == "__main__":
    unittest.main()


class AnExportCoversItsOwnAttempt(Base):
    """Review round 2, B8: attempts share the task's run folder; an earlier attempt's EXPORTED covered a later one."""

    def test_a_label_after_the_export_is_held_again(self):
        retention.mark_labelled(A, "final")
        self.export(A)
        self.assertFalse(retention.held(A))
        folder = self.root / "runs" / A
        later = (folder / "EXPORTED").stat().st_mtime + 5
        retention.mark_labelled(A, "final")          # attempt 2's final
        os.utime(folder / "LABELLED", (later, later))
        self.assertTrue(retention.held(A))
        self.export(A)
        os.utime(folder / "EXPORTED", (later + 5, later + 5))
        self.assertFalse(retention.held(A))

    def test_an_export_of_another_execution_does_not_cover_this_one(self):
        from decurix_agent import task_context
        task_context.set_execution(A, "e_attempt2")
        retention.mark_labelled(A, "final")
        folder = self.root / "runs" / A
        (folder / "EXPORTED").write_text("e_attempt1\nabc\n", encoding="utf-8")
        later = (folder / "LABELLED").stat().st_mtime + 5
        os.utime(folder / "EXPORTED", (later, later))
        self.assertTrue(retention.held(A))
        (folder / "EXPORTED").write_text("e_attempt2\nabc\n", encoding="utf-8")
        os.utime(folder / "EXPORTED", (later, later))
        self.assertFalse(retention.held(A))


class TheCeiling(Base):
    def test_full_of_unexported_runs_starts_no_new_work_and_says_why(self):
        retention.mark_labelled(A, "final")
        recording.append(f"task-{A}", {"kind": "click", "actor": "agent"},
                         before=screen.Frame(4, 4, bytearray(bytes([1]) * 48)), at=1000)
        with mock.patch.dict(os.environ, {"DECURIX_HELD_CEILING_MB": "0"}):
            said = retention.over_ceiling()
        self.assertIn("nothing is deleted and no new work is started", said)
        self.assertTrue(recording.sessions())  # nothing was deleted
        self.export(A)
        with mock.patch.dict(os.environ, {"DECURIX_HELD_CEILING_MB": "0"}):
            self.assertEqual(retention.over_ceiling(), "")


class TheManifestIsWrittenWhole(Base):
    """Review round 2 (mutation M22): run.json written in place could be left half written."""

    def test_a_failed_write_leaves_the_old_manifest_and_no_temporary_file(self):
        retention.mark_labelled(A, "reply")
        folder = self.root / "runs" / A
        before = (folder / "run.json").read_text(encoding="utf-8")
        with mock.patch.object(retention.os, "replace", side_effect=OSError("disk full")):
            retention.mark_labelled(A, "final")
        self.assertEqual((folder / "run.json").read_text(encoding="utf-8"), before)
        self.assertEqual([p.name for p in folder.iterdir() if p.name.endswith(".tmp")], [])
