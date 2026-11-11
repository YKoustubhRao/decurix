"""
The part of a taught task that only collects its documents saves no record (core._phase_refusal): found live
(2026-11-09), told to collect a month's bills, the model entered all five vouchers by hand in that part, and the month
cost 60 minutes. Decided by structure - the phase and the taught steps' own controls - never by the press's words.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import skill_task
from decurix_agent.computer import core, phase

TASK = "t0123456789abcdef"


class GatherPhase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        for p in (mock.patch.object(phase, "PHASE_FILE", Path(self.tmp.name) / "phase.json"),
                  mock.patch("decurix_agent.task_context.current", lambda: TASK)):
            p.start()
            self.addCleanup(p.stop)

    def test_a_save_the_gather_steps_do_not_make_is_refused(self):
        phase.write(TASK, "gather", ["Save attachments"])
        why = core._phase_refusal({"role": "button", "name": "Save voucher"})
        self.assertIn("only collects the documents", why)

    def test_saving_the_attachments_as_taught_goes_through(self):
        phase.write(TASK, "gather", ["Save attachments"])
        self.assertIsNone(core._phase_refusal({"role": "button", "name": "Save attachments"}))

    def test_after_the_phase_and_for_another_task_saves_go_through(self):
        phase.write(TASK, "gather", [])
        self.assertTrue(core._phase_refusal({"role": "button", "name": "Save voucher"}))
        with mock.patch("decurix_agent.task_context.current", lambda: "tanother000000000"):
            self.assertIsNone(core._phase_refusal({"role": "button", "name": "Save voucher"}))
        phase.write(TASK, None)
        self.assertIsNone(core._phase_refusal({"role": "button", "name": "Save voucher"}))
        self.assertIsNone(core._phase_refusal(None))

    def test_the_gather_commits_are_the_taught_steps_before_the_per_document_block(self):
        skill = {"steps": [{"n": 1, "id": "a", "kind": "click", "target": {"role": "link", "name": "Sales tax bills"}},
                           {"n": 2, "id": "b", "kind": "click", "target": {"role": "button", "name": "Save attachments"},
                            "consequential": True},
                           {"n": 3, "id": "c", "kind": "type", "param": "voucher_number",
                            "target": {"role": "textbox", "name": "Voucher No."}},
                           {"n": 4, "id": "d", "kind": "click", "target": {"role": "button", "name": "Save voucher"},
                            "consequential": True}],
                 "loops": [{"id": "l1", "enabled": True, "blocks": [["c", "d"]]}],
                 "params": [{"name": "voucher_number", "list": True, "step": 3}]}
        self.assertEqual(skill_task._gather_commits(skill), ["Save attachments"])


if __name__ == "__main__":
    unittest.main()
