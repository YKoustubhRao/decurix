"""
A task's outcome says which taught skill drove it (R7 of the plan of 2026-11-03; PLAN-P2 C5): {"name", "version"}
on the final callback, so the control plane can give a pass rate per skill version. A plain task, and a one-off mimic
of a recording (no taught skill), send none.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, messaging, skill_task, state, task_context

TASK, OTHER = "t00000000000000c1", "t00000000000000c2"


class TheSkillOnTheOutcome(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.sent = []
        for p in (mock.patch.object(messaging, "_post_querytext", lambda payload: self.sent.append(payload) or ("sent", "")),
                  mock.patch.object(messaging, "_keep_facts", lambda *a, **k: None),
                  mock.patch.object(messaging, "_final_gate", None),
                  mock.patch.object(messaging.progress, "flush", lambda: None),
                  mock.patch.object(config, "QUERYTEXT_DIR", Path(tmp.name)),
                  mock.patch.object(config, "DECURIX_DIR", Path(tmp.name)),
                  mock.patch.object(state, "current_task_id", TASK)):
            p.start()
            self.addCleanup(p.stop)

    def prepare(self, one_off=False):
        run = {"skill": {"name": "enter-a-bill", "version": 4, "steps": [], "params": []}, "values": {},
               "one_off": one_off}
        with mock.patch.object(skill_task, "_needed", side_effect=StopHere):
            try:
                skill_task._prepare(run)
            except StopHere:
                pass

    def test_a_skill_runs_final_carries_it(self):
        self.prepare()
        messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertEqual(self.sent[-1]["skill"], {"name": "enter-a-bill", "version": 4})

    def test_a_plain_task_does_not(self):
        messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertNotIn("skill", self.sent[-1])

    def test_another_task_does_not_inherit_it(self):
        self.prepare()
        with mock.patch.object(state, "current_task_id", OTHER):
            messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertNotIn("skill", self.sent[-1])

    def test_a_mimic_is_no_taught_skill(self):
        self.prepare(one_off=True)
        messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertNotIn("skill", self.sent[-1])
        self.assertIsNone(task_context.skill(TASK))


class StopHere(Exception):
    """Ends _prepare right after it noted the skill (the fake _needed raises it)."""


if __name__ == "__main__":
    unittest.main()
