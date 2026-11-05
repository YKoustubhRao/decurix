"""
A /replay that is not VERIFIED never goes out as a plain final (review 2026-11-03: lane C handoff item 2, D-09/D-25).
A COMPLETED_UNVERIFIED run is checked on the screen (skill_task.check_unchecked); when that check cannot be made, the
report goes out checked=False with "Not checked" first.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import replay_task, skill_task

PROGRAM = {"session": "s1", "steps": [], "params": []}


class NotVerified(unittest.TestCase):
    def run_with(self, outcome, check=None):
        finals = []
        result = {"outcome": outcome, "step": 3, "resume_from": None, "text": "Replayed the recording."}
        patches = [mock.patch.object(replay_task.replay, "run", return_value=result),
                   mock.patch.object(replay_task, "write_final_reply", side_effect=lambda t, **k: finals.append((t, k)))]
        if check is not None:
            patches.append(mock.patch.object(skill_task, "check_unchecked", side_effect=check))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        replay_task._run(PROGRAM, {}, 1)
        return finals

    def test_an_unverified_run_goes_to_the_screen_check(self):
        called = []
        finals = self.run_with(replay_task.replay.COMPLETED_UNVERIFIED, check=lambda *a, **k: called.append(a))
        self.assertEqual(len(called), 1)
        self.assertEqual(finals, [])

    def test_when_the_check_cannot_be_made_it_goes_out_not_checked(self):
        def broken(*a, **k):
            raise RuntimeError("the judge broke")

        finals = self.run_with(replay_task.replay.COMPLETED_UNVERIFIED, check=broken)
        text, kwargs = finals[-1]
        self.assertTrue(text.startswith("Not checked"), text)
        self.assertFalse(kwargs.get("checked"))
        self.assertTrue(kwargs.get("gated"))

    def test_a_verified_run_still_goes_to_the_final_gate(self):
        text, kwargs = self.run_with(replay_task.replay.VERIFIED)[-1]
        self.assertEqual(kwargs.get("kind"), "final")
        self.assertFalse(kwargs.get("gated"), "the final gate still sees it")


if __name__ == "__main__":
    unittest.main()
