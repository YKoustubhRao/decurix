"""
A workflow root does not grow without bound in what nothing reads again (review 2026-11-03, E-43): stale lock files and
crash-left temp files are pruned; run folders, which hold the person's outputs, are kept. And a restart's note reads
only the runs of its own task.
"""

import os
import tempfile
import time
import unittest
from pathlib import Path

import config_stub  # noqa: F401

from decurix_agent.workflow_runner import WorkflowRunner


def spec():
    return {"id": "w", "steps": [{"id": "s", "instruction": "Make it", "checks": [{"path": "s.txt"}]}]}


class Prune(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.runner = WorkflowRunner(self.tmp.name)

    def age(self, path, seconds):
        old = time.time() - seconds
        os.utime(path, (old, old))

    def test_a_stale_lock_of_a_gone_run_and_a_crash_temp_file_are_removed_and_runs_are_kept(self):
        self.runner.create(spec(), {}, "kept-run")
        root = Path(self.tmp.name)
        orphan = root / ".locks" / ("0" * 64 + ".lock")
        orphan.write_bytes(b"0")
        temp = root / "kept-run" / "checkpoint.json.abc.tmp"
        temp.write_text("{}")
        fresh = root / ".locks" / ("1" * 64 + ".lock")
        fresh.write_bytes(b"0")
        for path in (orphan, temp, *(root / ".locks").glob("*.lock")):
            if path != fresh:
                self.age(path, 3 * 24 * 3600)
        self.assertEqual(self.runner.prune(), 2)
        self.assertFalse(orphan.exists())
        self.assertFalse(temp.exists())
        self.assertTrue(fresh.exists(), "a lock younger than a day may be a run being created")
        self.assertTrue((root / "kept-run" / "checkpoint.json").exists())
        self.assertEqual(len(list((root / ".locks").glob("*.lock"))), 2, "the live run's lock stays")

    def test_the_runs_of_one_task_are_found_without_reading_the_others(self):
        a = self.runner.create(spec(), {}, "run-a")
        self.runner.bind_task(a["run_id"], "t0123456789abcdef")
        self.runner.create(spec(), {}, "run-b")
        (Path(self.tmp.name) / "broken").mkdir()
        (Path(self.tmp.name) / "broken" / "checkpoint.json").write_text("not json")
        self.assertEqual(self.runner.task_runs("t0123456789abcdef"), ["run-a"])


if __name__ == "__main__":
    unittest.main()
