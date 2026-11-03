"""
A skill's run is done once (review 2026-11-03, D-30): a second run started while the first runs is refused, and a run
outside any task is noted like one inside it. An extra row of a list is found as that row's control, never by its name
alone (the first row's).
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import filelock
from decurix_agent.computer import skill_engine


class OneRunAtATime(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        p = mock.patch.object(skill_engine, "REPLAYED_DIR", Path(self.tmp.name))
        p.start()
        self.addCleanup(p.stop)
        q = mock.patch("decurix_agent.task_context.current", return_value="")
        q.start()
        self.addCleanup(q.stop)

    def test_a_second_run_while_one_runs_is_refused(self):
        skill = {"name": "add-a-thing", "title": "Add a thing", "steps": []}
        held = open(Path(self.tmp.name) / ".lock-no-task-add-a-thing", "a+")
        self.addCleanup(held.close)
        self.assertTrue(filelock.try_exclusive(held))
        with mock.patch.object(skill_engine, "_run") as run:
            result = skill_engine.run(skill)
        run.assert_not_called()
        self.assertIn("another run of it is going on now", result["text"])

    def test_a_run_outside_a_task_is_noted(self):
        skill_engine._note_replayed("add-a-thing", 4)
        self.assertEqual(skill_engine._replayed_to("add-a-thing"), 4)

    def test_an_old_note_outside_a_task_no_longer_counts(self):
        skill_engine._note_replayed("add-a-thing", 4)
        with mock.patch.object(skill_engine.time, "time", return_value=skill_engine.time.time() + 2 * 3600):
            self.assertEqual(skill_engine._replayed_to("add-a-thing"), 0)


if __name__ == "__main__":
    unittest.main()
