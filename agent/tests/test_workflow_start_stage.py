"""
A workflow stage that needs the person's go-ahead starts only on their reply to its question - and, with an approval
gate, only on the gate's grant (review 2026-11-03, F-09 and the reply-file rule of E-15).

The go-ahead used to be `/workflow approve RUN STAGE`, accepted from any command file (the worker's own user can write
one) and from a button the agent listed. Starting the stage is not an approval of what it does: a consequential action
inside an approved stage still asks the gate.
"""

import json
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent import gated_command, workflow_api, workflow_task
from decurix_agent.computer import approval
from decurix_agent.workflow_runner import WorkflowRunner

RUN = "start-1"


def spec():
    return {"id": "needs-go-ahead", "steps": [
        {"id": "publish", "app": "an app", "instruction": "Publish the reviewed package", "approval": True,
         "retry_safe": True, "checks": [{"path": "publish.txt", "contains": ["published"]}]}]}


class Gate:
    def __init__(self, grants=0):
        self.grants, self.calls = grants, []

    def __call__(self, op, timeout=None, **args):
        self.calls.append((op, args))
        if op == "check" and self.grants:
            self.grants -= 1
            return {"allowed": True, "request": None}
        return {"allowed": False, "request": {"id": "r1", "fingerprint": "f1"}}


class StartStage(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.runner = WorkflowRunner(Path(self.temp.name) / "runs")
        self.finals, self.questions, self.ran, self.inside = [], [], [], []
        messaging = types.ModuleType("decurix_agent.messaging")
        messaging.write_final_reply = lambda text, **k: self.finals.append((text, k))
        messaging.ask_for_query = lambda text, **k: self.questions.append((text, k))
        harness = types.ModuleType("decurix_agent.harness")
        harness.perform_task = self.execute
        import decurix_agent
        from decurix_agent import guidance, state, task_context
        for p in (patch.object(workflow_task, "runner", return_value=self.runner),
                  patch.object(workflow_task, "_active", None), patch.object(workflow_task, "_pending", None),
                  patch.object(workflow_task, "_driving", False), patch.object(workflow_task, "_awaiting_start", None),
                  patch.dict(sys.modules, {"decurix_agent.messaging": messaging, "decurix_agent.harness": harness}),
                  patch.object(decurix_agent, "harness", harness, create=True),
                  patch.object(state, "backend_ready", True, create=True),
                  patch.object(state, "waiting_for_query", False),
                  patch.object(task_context, "current", return_value=""),
                  patch.object(guidance, "refused", return_value=False),
                  patch.object(approval, "STATE_DIR", Path(self.temp.name)),
                  patch.object(approval, "STATE_FILE", Path(self.temp.name) / "approval.json")):
            p.start()
            self.addCleanup(p.stop)
        self.runner.create(spec(), {}, RUN)

    def execute(self, prompt, task_file):
        run_id, step_id, _ = workflow_task._active
        self.ran.append(step_id)
        # Inside the started stage, a consequential press is still put to the gate.
        pay = {"role": "button", "name": "Pay now"}
        self.inside.append(approval.check("click", pay).allowed)
        root = Path(self.runner.context(self.runner.status(run_id))["work_dir"])
        (root / "publish.txt").write_text("published", encoding="utf-8")
        workflow_task.final_gate("Published.", "final")

    def ask(self):
        workflow_task.perform(f"/workflow resume {RUN}")
        self.assertEqual(len(self.questions), 1)
        return self.questions[-1]

    def test_the_stage_is_put_to_the_person_as_a_question_in_start_stage_words(self):
        with patch.object(approval, "remote", return_value=False):
            text, kwargs = self.ask()
        self.assertEqual(kwargs.get("kind"), "approval")
        self.assertIn("start stage publish", text)
        self.assertIn("Publish the reviewed package", text)
        self.assertNotIn("/workflow approve", text)
        self.assertEqual(self.ran, [])

    def test_a_command_file_saying_approve_starts_nothing(self):
        workflow_task.perform(f"/workflow approve {RUN} publish")
        self.assertEqual(self.ran, [])
        self.assertEqual(self.runner.status(RUN)["steps"]["publish"]["attempts"], 0)
        self.assertEqual(self.finals[-1][1].get("kind"), "error")

    def test_the_dashboard_lists_no_approve_command(self):
        state = self.runner.claim(RUN)[0]
        commands = [a["command"] for a in workflow_api.summary(state)["actions"]]
        self.assertFalse([c for c in commands if "approve" in c], commands)

    def test_without_a_gate_a_plain_yes_reply_starts_it_and_a_press_inside_still_asks(self):
        with patch.object(approval, "remote", return_value=False):
            self.ask()
            with patch.object(approval, "reading", return_value={"decision": "yes", "condition": ""}):
                workflow_task.answer_start("yes")
        self.assertEqual(self.ran, ["publish"])
        self.assertEqual(self.inside, [False], "starting the stage approved nothing inside it")

    def test_without_a_gate_a_qualified_reply_starts_nothing(self):
        with patch.object(approval, "remote", return_value=False):
            self.ask()
            with patch.object(approval, "reading", return_value={"decision": "yes", "condition": "after lunch"}):
                workflow_task.answer_start("yes after lunch")
        self.assertEqual(self.ran, [])
        self.assertIn("was not started", self.finals[-1][0])

    def test_with_a_gate_only_its_grant_starts_it(self):
        gate = Gate()
        with patch.object(approval, "remote", return_value=True), patch.object(approval, "gate_call", gate), \
                patch.object(approval, "reading", side_effect=AssertionError("the words do not decide")):
            self.ask()
            self.assertEqual(gate.calls[0][1]["material"]["kind"], "command")
            workflow_task.answer_start("yes")
            self.assertEqual(self.ran, [])
            self.ask_again_and_grant(gate)
        self.assertEqual(self.ran, ["publish"])

    def ask_again_and_grant(self, gate):
        self.questions.clear()
        self.ask()
        gate.grants = 1
        with patch.object(approval, "check", return_value=approval.Decision(False)):
            workflow_task.answer_start("yes")

    def test_with_a_gate_that_does_not_answer_nothing_is_asked_or_started(self):
        def down(*a, **k):
            raise OSError("no gate")

        with patch.object(approval, "remote", return_value=True), patch.object(approval, "gate_call", down):
            workflow_task.perform(f"/workflow resume {RUN}")
        self.assertEqual(self.questions, [])
        self.assertEqual(self.ran, [])
        self.assertIn(gated_command.NOT_RUN, self.finals[-1][0])


if __name__ == "__main__":
    unittest.main()
