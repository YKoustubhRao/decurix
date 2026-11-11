"""
A person confirming an outcome is a check, not help (final_gate._confirm_reply, research 2026-11-11): a plain `done`
to "I cannot confirm this is done" on a run nobody helped is recorded unhelped on the `person` basis, so it counts
towards trust; a confirmation that says more, or one of a run already helped (a value given, a takeover, work redone
after a send-back), stays helped. Before, every confirmation was recorded helped and so never counted.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_confirmed_not_helped.py
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import final_gate, skill_task
from decurix_agent.computer import experience


def paused_for(run):
    return {"run": run, "claim": "Exported and filed the quarter.", "result": None, "engine": "thebot", "task": "file",
            "verdict": {"verdict": "unsure", "confidence": 0.0, "seen": "the portal shows no status", "why": ""}}


def confirm(run, instruction=""):
    with mock.patch.object(skill_task, "_record_verdict") as recorded, \
            mock.patch.object(skill_task, "write_final_reply"), \
            mock.patch.object(final_gate, "_reworked", return_value=False), \
            mock.patch.object(final_gate, "_done_reading", return_value={"choice": "done", "instruction": instruction}):
        final_gate._confirm_reply(paused_for(run), "done" + (f", {instruction}" if instruction else ""))
    return recorded.call_args.kwargs


class AConfirmation(unittest.TestCase):

    def test_a_plain_confirmation_of_an_unhelped_run_counts_on_the_person_basis(self):
        said = confirm({"skill": {"name": "file-return"}, "values": {}, "asked_ids": []})
        self.assertFalse(said["helped"])
        self.assertEqual(said["basis"], ["person"])
        self.assertTrue(experience.counts({"ok": True, "helped": said["helped"], "basis": said["basis"]}))

    def test_a_confirmation_that_says_more_is_help(self):
        said = confirm({"skill": {"name": "file-return"}, "values": {}, "asked_ids": []}, instruction="use the Q3 folder")
        self.assertTrue(said["helped"])

    def test_a_confirmation_of_a_run_already_helped_stays_helped(self):
        for flag in ({"asked_person": True}, {"taken_over": True}, {"asked_ids": ["s4"]}):
            run = dict({"skill": {"name": "file-return"}, "values": {}, "asked_ids": []}, **flag)
            self.assertTrue(confirm(run)["helped"], flag)


if __name__ == "__main__":
    unittest.main()
