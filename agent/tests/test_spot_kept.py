"""
A trusted replay's spot judgement that says "not done" stays on the skill's record when a person later confirms the run
(review round 2, B6). The spot verdict was noted on a fresh copy of the skill; the run went on with its older copy, and
the person's "done" saved that copy whole over it - the judge's dissent vanished from the skill's history (rule 4).
Now the save merges what was noted meanwhile, and the outcome says the spot check found it not done.
"""

import copy
import hashlib
import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import final_gate, skill_task, state, verify
from decurix_agent.computer import experience, replay, skills


def sampled_task():
    for i in range(500):
        task = "t" + hashlib.sha256(str(i).encode()).hexdigest()[:16]
        if int(hashlib.sha256(task.encode()).hexdigest()[:8], 16) % 5 == 0:
            return task


class TheSpotFailureIsKept(unittest.TestCase):
    def test_spot_not_done_then_pause_then_done(self):
        trusted = {"name": "enter-a-bill", "version": 2, "steps": [], "params": [],
                   "experience": {"runs": [], "steps": {}, "verdicts": [
                       {"at": 1, "mode": "replay", "ok": True, "checked_by": "screen", "helped": False, "why": "",
                        "basis": ["ledger", "screen"]}] * experience.TRUST_AFTER}}
        store = {"enter-a-bill": copy.deepcopy(trusted)}
        sent = []
        run = {"skill": copy.deepcopy(trusted), "values": {}, "patches": [], "asked_ids": [], "ladder": mock.Mock(),
               "one_off": False, "gathered": True}
        judged = {"verdict": verify.NOT_DONE, "confidence": 0.9, "seen": "the bill total is 0.00", "why": ""}
        with mock.patch.object(skills, "load", lambda name: copy.deepcopy(store[name])), \
                mock.patch.object(skills, "save", lambda s: store.__setitem__(s["name"], copy.deepcopy(s))), \
                mock.patch.object(skill_task.task_context, "current", sampled_task), \
                mock.patch.object(skill_task.verify, "available", lambda: True), \
                mock.patch.object(skill_task, "_judge", lambda run, claim, note="", purpose="": dict(judged)), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: False), \
                mock.patch.object(skill_task, "ask_for_query", lambda *a, **k: None), \
                mock.patch.object(final_gate, "_done_reading", lambda reply, seen="": {"choice": "done", "instruction": ""}), \
                mock.patch.object(skill_task, "_read_back", lambda run, text: ([], [])), \
                mock.patch.object(skill_task, "_own_run"), \
                mock.patch.object(skill_task.experience, "commit", lambda *a, **k: []), \
                mock.patch.object(skill_task.progress, "emit", lambda *a, **k: None), \
                mock.patch.object(skill_task, "write_final_reply", lambda text, **k: sent.append((text, k))):
            skill_task._finish(run, {"outcome": replay.VERIFIED, "text": "ran"})
            self.assertIsNotNone(state.paused_skill, "the run waits on the person")
            skill_task.resume("done")
        kinds = [(v["ok"], v["why"][:10]) for v in store["enter-a-bill"]["experience"]["verdicts"]]
        self.assertIn((False, "spot check"), kinds)
        self.assertEqual(kinds[-1][0], True)  # the person's confirmation, after it
        self.assertEqual(experience.stage(store["enter-a-bill"]), experience.APPRENTICE)
        outcome = sent[-1][1]["verdict"]
        self.assertTrue(any("spot check had found it not done" in e for e in outcome["evidence"]))


if __name__ == "__main__":
    unittest.main()
