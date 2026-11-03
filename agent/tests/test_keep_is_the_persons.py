"""
A "keep it" that clears a code check's finding is the person's alone (E-15, review 2026-11-03): only the reply to the
question counts (never a file dropped into the command folder, which is the worker's own), and on a desktop with an
approval gate only the gate's one-shot yes for exactly that keep. And a stage whose command the person declined ends
declined, not done (NEW-T1).
"""

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_harness_review as review
import test_long_requests as base

from decurix_agent import gated_command, long_task, verify


class _Parked(review.AWrongBlockingFindingIsThePersonsToOverrule):
    """The register stage parked on a finding the worker kept on purpose (as test_harness_review sets it up)."""

    def park(self):
        self.engine.report_suffix = review.KEPT
        with mock.patch.object(verify, "judge", lambda task, **k: {"verdict": "done", "confidence": 0.95, "seen": "",
                                                                    "why": ""}):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "parked")
        self.message_verdict = {"kind": "answer", "answers": [{"question": "q1", "answer": "Keep it.", "keep": True}]}
        return run_id


# The parent's own tests are run where they live, not again here.
for _name in [n for n in dir(review.AWrongBlockingFindingIsThePersonsToOverrule) if n.startswith("test_")]:
    setattr(_Parked, _name, None)


class AKeepFromACommandFileIsNotThePersons(_Parked):
    def test_a_keep_that_did_not_come_as_the_reply_clears_nothing(self):
        run_id = self.park()
        self.message("Keep it.", reply=False)
        self.assertFalse(long_task.ledger(run_id).get("overrides"))
        self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "parked")

    def test_the_reply_to_the_question_clears_it_where_there_is_no_gate(self):
        run_id = self.park()
        with mock.patch.object(gated_command, "gated", lambda: False):
            self.message("Keep it.", reply=True)
        self.assertEqual([o["stage"] for o in long_task.ledger(run_id)["overrides"]], ["s1-register"])


class WithAGateOnlyItsYesKeeps(_Parked):
    def setUp(self):
        super().setUp()
        self.gate = {"grant": False, "checked": []}

        def check(held):
            self.gate["checked"].append(held)
            if self.gate["grant"]:
                self.gate["grant"] = False  # one-shot
                return "allowed"
            return "asked"

        for p in (mock.patch.object(gated_command, "gated", lambda: True),
                  mock.patch.object(gated_command, "check", check)):
            p.start()
            self.addCleanup(p.stop)

    def approvals(self):
        return [s for s in self.sent if s["kind"] == "approval"]

    def test_a_keep_is_put_to_the_gate_and_kept_only_on_its_yes(self):
        run_id = self.park()
        self.message("Keep it.", reply=True)
        self.assertFalse(long_task.ledger(run_id).get("overrides"))   # not yet: the gate asks first
        self.assertIn("To keep the work of stage s1-register as it is", self.approvals()[-1]["reply"])
        self.assertIn("keep the work of stage s1-register", self.gate["checked"][-1]["request"])
        self.gate["grant"] = True                                       # the person's signed yes, at the gate
        self.message("yes", reply=True)
        self.assertEqual([o["stage"] for o in long_task.ledger(run_id)["overrides"]], ["s1-register"])
        self.assertEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "completed")

    def test_a_reply_the_gate_did_not_grant_keeps_nothing(self):
        run_id = self.park()
        self.message("Keep it.", reply=True)
        self.message("yes", reply=True)                                 # the gate holds no yes for it
        self.assertFalse(long_task.ledger(run_id).get("overrides"))
        self.assertNotEqual(self.runner.status(run_id)["steps"]["s1-register"]["status"], "completed")

    def test_a_gate_that_does_not_answer_keeps_nothing(self):
        run_id = self.park()
        with mock.patch.object(gated_command, "check", lambda held: "no gate"):
            self.message("Keep it.", reply=True)
        self.assertFalse(long_task.ledger(run_id).get("overrides"))
        self.assertEqual(self.approvals(), [])


class ADeclinedStageIsNotDone(base.LongRunBase):
    def test_a_stage_whose_command_was_declined_ends_declined_not_done(self):
        run_id_box = []
        real = long_task.stage_finished

        def finished(run_id, step_id, reply, kind, declined=None):
            run_id_box.append(run_id)
            return real(run_id, step_id, reply, kind, declined="no, do not run it" if step_id == "s1-register" else None)

        with mock.patch.object(long_task, "stage_finished", finished):
            long_task.perform(base.LONG, None)
        record = self.runner.status(run_id_box[0])["steps"]["s1-register"]
        self.assertNotEqual(record["status"], "completed")
        self.assertIn("so it is not done", str(record.get("error")))


if __name__ == "__main__":
    unittest.main()
