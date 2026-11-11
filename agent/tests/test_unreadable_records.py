"""
A record the checks read that is there but cannot be read is a check not made - never "nothing typed" or "no drafts"
(N-1, review round 4). Real files, no mocked readers: a recording whose events file has a torn line or whose meta is
corrupt, and a drafts log with a torn line, through skill_task._gate and through a planned stage's checks.
"""

import json
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_long_requests as base
import test_skills as ts

from decurix_agent import final_gate, guidance, long_task, skill_task, state, verify
from decurix_agent.computer import evidence, recording
from decurix_agent.tools import outbox

TASK = "t00000000000000c1"
SEEN = {"verdict": "done", "confidence": 0.95, "seen": "result.xlsx holds it", "why": ""}


def torn_recording(root, task_id, corrupt_meta=False):
    folder = root / recording.session_for_task(task_id)
    folder.mkdir(parents=True, exist_ok=True)
    (folder / "meta.json").write_text("{ not json" if corrupt_meta else json.dumps({"session": folder.name,
                                                                                   "event_count": 2}),
                                      encoding="utf-8")
    (folder / "events.jsonl").write_text(json.dumps({"kind": "type", "text": "1250.00", "outcome": "done"}) + "\n"
                                         + '{"kind": "type", "text": "99', encoding="utf-8")


def torn_drafts(folder, task_id):
    folder.mkdir(parents=True, exist_ok=True)
    (folder / f"{task_id}.jsonl").write_text(json.dumps({"id": 1, "to": ["ap@vendor.test"], "subject": "Bill"}) + "\n"
                                             + '{"id": 2, "to": ["ap@ven', encoding="utf-8")


class TheReadersSayWhatTheyCouldNotRead(unittest.TestCase):
    def setUp(self):
        import tempfile
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)

    def test_a_torn_drafts_log_raises_and_a_missing_one_is_none(self):
        torn_drafts(self.root, TASK)
        with self.assertRaises(outbox.Unreadable):
            outbox.read_log(self.root / f"{TASK}.jsonl")
        self.assertEqual(outbox.read_log(self.root / "absent.jsonl"), [])

    def test_a_torn_recording_raises_and_no_recording_is_nothing(self):
        with mock.patch.object(recording, "ROOT", self.root):
            torn_recording(self.root, TASK)
            with self.assertRaises(recording.Unreadable):
                evidence.entered(TASK)
            torn_recording(self.root, "t00000000000000c2", corrupt_meta=True)
            with self.assertRaises(recording.Unreadable):
                evidence.entered("t00000000000000c2")
            self.assertEqual(evidence.entered("t00000000000000c3"), [])


class AtTheGate(ts.TeachingBase):
    """skill_task._gate on file work, with the task's own record or drafts log torn on disk."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.judged, self.reports = [], []
        self.root = Path(self.tmp.name)
        saved = state.current_task_id
        self.addCleanup(setattr, state, "current_task_id", saved)
        state.current_task_id = TASK
        for p in (mock.patch.object(skill_task.verify, "available", lambda: True),
                  mock.patch.object(skill_task.verify, "judge",
                                    lambda *a, **k: (self.judged.append(k), dict(SEEN))[1]),
                  mock.patch.object(final_gate, "_did_screen_work", lambda: False),
                  mock.patch.object(skill_task.produced, "files", lambda report="", **kw: ["/work/result.xlsx"]),
                  mock.patch.object(skill_task.produced, "text", lambda paths, focus=(): "A2=1250.00"),
                  mock.patch.object(skill_task.produced, "missing_on_host", lambda request: ""),
                  mock.patch.object(skill_task.produced, "saved_on_host", lambda request: []),
                  mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: skill_task.produced.FileFacts()),
                  mock.patch.object(skill_task, "_engine_can_drive", lambda: False),
                  mock.patch.object(skill_task, "write_final_reply",
                                    lambda text, kind="final", engine=None, **kw: self.reports.append(
                                        dict(kw, kind=kind, text=text)))):
            p.start()
            self.addCleanup(p.stop)
        state.current_task = "enter the bill in result.xlsx"

    def assertNotChecked(self):
        self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertIs(self.reports[-1].get("checked", False), False)
        self.assertIn("could not be read", self.reports[-1]["text"])

    def test_a_torn_drafts_log_leaves_the_work_not_checked(self):
        torn_drafts(outbox.DRAFTS_DIR, TASK)
        self.assertNotChecked()

    def test_a_torn_recording_leaves_the_work_not_checked(self):
        torn_recording(recording.ROOT, TASK)
        self.assertNotChecked()

    def test_whole_records_are_judged_as_before(self):
        self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(len(self.judged), 1)
        self.assertIs(self.reports[-1].get("checked"), True)


class InAPlannedStage(base.LongRunBase):
    """A planned run whose task's recording is torn on disk: the stage checks that read it are not made."""

    def test_a_torn_recording_leaves_every_stage_not_checked(self):
        rec = mock.patch.object(recording, "ROOT", self.root / "recordings")
        rec.start()
        self.addCleanup(rec.stop)
        torn_recording(self.root / "recordings", base.TASK)
        with mock.patch.object(verify, "judge", lambda task, **k: dict(SEEN)):
            long_task.perform(base.LONG, None)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertFalse(any(r.get("checked_by") == "model" for r in steps.values()))
        final = self.finals()[-1]
        self.assertIs(final["checked"], False)
        self.assertIn("filler_values check could not be made", final["reply"])

    def test_a_torn_drafts_log_leaves_every_stage_not_checked(self):
        real_stage_drafts = _REAL_STAGE_DRAFTS
        drafts = mock.patch.object(outbox, "DRAFTS_DIR", self.root / "drafts")
        drafts.start()
        self.addCleanup(drafts.stop)
        torn_drafts(self.root / "drafts", base.TASK)
        with mock.patch.object(verify, "judge", lambda task, **k: dict(SEEN)), \
                mock.patch.object(long_task, "_stage_drafts", real_stage_drafts), \
                mock.patch.object(long_task, "_draft_ids", lambda: []):
            long_task.perform(base.LONG, None)
        steps = self.runner.status(self.run_id())["steps"]
        self.assertFalse(any(r.get("checked_by") == "model" for r in steps.values()))
        self.assertIs(self.finals()[-1]["checked"], False)


_REAL_STAGE_DRAFTS = long_task._stage_drafts

if __name__ == "__main__":
    unittest.main()
