"""
A replay that ran to its end did the task: it saves, submits or sends at its end. Run again for the same request,
it does it a second time. Found live (2026-10-14): sent back over one value of a record the replay had saved, the
engine ran `gui do` from step 1 and a second employee record was made.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent.computer import skill_engine, skills

SKILL = {
    "name": "onboard", "title": "Onboard", "status": "ready", "params": [],
    "steps": [{"n": 1, "id": "s1", "kind": "click", "window": "", "summary": "click Onboard"},
              {"n": 2, "id": "s2", "kind": "click", "window": "", "summary": "click Submit"}],
}


class AReplayRunsOncePerRequest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for patch in (mock.patch.object(skill_engine, "REPLAYED_DIR", Path(tmp.name)),
                      mock.patch.object(skill_engine.task_context, "current", lambda: "t1"),
                      mock.patch.object(skill_engine, "other_system", lambda *a: ""),
                      mock.patch.object(skill_engine.skills, "note_run", lambda name: None)):
            patch.start()
            self.addCleanup(patch.stop)
        self.task = "t1"

    def run_skill(self, start=1, outcome="verified"):
        with mock.patch.object(skill_engine.replay, "run", return_value={"outcome": outcome, "text": "ok"}) as ran:
            result = skill_engine.run(dict(SKILL, status=skills.READY), {}, start=start)
        return result, ran

    def test_a_second_replay_of_the_same_request_is_refused(self):
        self.run_skill()
        result, ran = self.run_skill()
        ran.assert_not_called()
        self.assertEqual(result["outcome"], "halted_before_effect")
        self.assertIn("would do it a second time", result["text"])

    def test_a_replay_that_stopped_part_way_can_be_carried_on(self):
        self.run_skill(outcome="halted")
        _, ran = self.run_skill(start=2)
        ran.assert_called_once()

    def test_another_request_replays_it_again(self):
        self.run_skill()
        with mock.patch.object(skill_engine.task_context, "current", lambda: "t2"):
            _, ran = self.run_skill()
        ran.assert_called_once()


if __name__ == "__main__":
    unittest.main()
