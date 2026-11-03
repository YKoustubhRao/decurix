"""
A workflow stage that raises lets go of the desktop and of the run (review 2026-11-03, E-04 and E-05).

- E-04: an exception while a long request's stage is verified left the run in hand: every later message was taken as
  the run's, and the run stayed "running".
- E-05: a learned-skill stage whose input binding did not resolve, reached after a query reply, raised on the reply
  thread and left the stage pointed at: the next, unrelated task's report was swallowed as that stage's.
"""

import json
import sys
import types
import unittest
from pathlib import Path
from unittest import mock
from unittest.mock import patch

import config_stub  # noqa: F401
from test_long_requests import LONG, LongRunBase
import test_workflow_task

from decurix_agent import long_task, verify, workflow_runner, workflow_task


class AVerificationThatRaises(LongRunBase):
    def test_the_run_is_let_go_and_the_person_is_told(self):
        with mock.patch.object(verify, "program_numbers", side_effect=RuntimeError("a check broke")):
            long_task.perform(LONG, None)
        self.assertIsNone(long_task._run, "the run is no longer in hand")
        self.assertFalse(long_task.takes("20261019_new.txt"), "a later message is not taken as the run's")
        self.assertIsNone(workflow_task._active)
        state = self.runner.status(self.run_id())
        self.assertNotIn("running", [record["status"] for record in state["steps"].values()])
        errors = [f for f in self.finals() if f["kind"] == "error"]
        self.assertTrue(errors and "stopped during verification" in errors[-1]["reply"], self.finals())


class AStageThatCannotBindItsInput(unittest.TestCase):
    # The workflow tests' fake harness, without running their tests again here.
    setUp = test_workflow_task.WorkflowHarnessIntegration.setUp
    execute = test_workflow_task.WorkflowHarnessIntegration.execute
    spec = test_workflow_task.WorkflowHarnessIntegration.spec

    def skill(self):
        skill = types.ModuleType("decurix_agent.skill_task")
        skill.request_text = lambda name, params: "/skill " + name
        skill.perform = lambda *args, **kwargs: None
        import decurix_agent
        patch.dict(sys.modules, {"decurix_agent.skill_task": skill}).start()
        patch.object(decurix_agent, "skill_task", skill, create=True).start()

    def test_the_run_is_refused_at_start_when_a_bound_input_is_missing(self):
        case = self.spec()
        case["steps"][1]["skill"] = {"name": "recorded-calc", "params": {"x": {"input": "not_supplied"}}}
        self.assertEqual(workflow_runner.missing_inputs(case, {}), ["not_supplied"])
        self.skill()
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": case}))
        self.assertIsNone(workflow_task._active)
        self.assertIn("not_supplied", self.reports[-1][0])
        self.assertEqual(self.reports[-1][1].get("kind"), "error")

    def test_a_binding_that_fails_after_a_reply_lets_go_and_the_next_report_reaches_the_person(self):
        case = self.spec()
        case["steps"][1]["skill"] = {"name": "recorded-calc", "params": {"x": {"input": "given"}}}
        self.skill()
        self.harness.perform_task = lambda *args: None  # stage 1 asks the person (pauses)
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": case, "inputs": {"given": "1"}}))
        run_id, step_id, _ = workflow_task._active
        # The input goes missing before stage 2 binds it (a checkpoint edited, an older run's spec).
        state = self.runner.status(run_id)
        state["inputs"].pop("given")
        self.runner._save(state)
        root = Path(self.runner.context(state)["work_dir"])
        (root / "extract.txt").write_text("checked business result")
        workflow_task.final_gate("Done after the reply", "final")
        workflow_task.engine_returned()  # the reply thread: must not raise
        self.assertIsNone(workflow_task._active)
        self.assertIn(self.runner.status(run_id)["steps"]["calculate"]["status"], ("failed", "pending"))
        self.assertIn("missing learned-skill input", self.reports[-1][0])
        self.assertFalse(workflow_task.final_gate("Your return for the month is filed.", "final"),
                         "an unrelated report is not consumed as the stage's")

    def test_a_new_request_closes_a_stage_left_pointed_at(self):
        self.harness.perform_task = lambda *args: None
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        run_id, step_id, _ = workflow_task._active
        workflow_task.drop_stale()
        self.assertIsNone(workflow_task._active)
        self.assertNotEqual(self.runner.status(run_id)["steps"][step_id]["status"], "running")
        self.assertFalse(workflow_task.final_gate("An unrelated task's report.", "final"))

    def test_a_binding_that_is_not_a_plain_input_name_is_refused(self):
        case = self.spec()
        case["steps"][0]["skill"] = {"name": "s", "params": {"x": {"input": "a", "other": 1}}}
        with self.assertRaises(ValueError):
            workflow_runner.validate(case)


if __name__ == "__main__":
    unittest.main()
