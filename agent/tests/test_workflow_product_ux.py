"""New PM adversaries: recovery burden, reviewable approvals and usable results."""
import json
import os
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent.workflow_runner import WorkflowRunner
from decurix_agent import workflow_task


def stage(id="work", **extra):
    return {"id":id, "instruction":"Produce a verified result", "app":"unfamiliar-app",
            "checks":[{"path":id + ".txt", "contains":["verified"]}],
            "retry_safe":True, "max_attempts":1, **extra}


def workflow(*stages, **extra):
    return {"id":"unfamiliar-inputs", "steps":list(stages or (stage(),)), **extra}


class WorkflowProductExperience(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runner = WorkflowRunner(self.root / "runs")
        self.replies = []
        self.calls = []
        message = types.ModuleType("decurix_agent.messaging")
        message.write_final_reply = lambda text, **kwargs: self.replies.append((text, kwargs))
        message.ask_for_query = lambda text, **kwargs: self.replies.append((text, kwargs))
        harness = types.ModuleType("decurix_agent.harness")
        harness.perform_task = self.execute
        import decurix_agent
        for item in (patch.dict(os.environ, {"DECURIX_DIR":str(self.root / "runtime")}),
                     patch.object(workflow_task, "runner", return_value=self.runner),
                     patch.object(workflow_task, "_active", None), patch.object(workflow_task, "_pending", None),
                     patch.object(workflow_task, "_driving", False),
                     patch.dict(sys.modules, {"decurix_agent.messaging":message,"decurix_agent.harness":harness}),
                     patch.object(decurix_agent, "harness", harness, create=True)):
            item.start()
            self.addCleanup(item.stop)
        from decurix_agent import state, guidance, task_context
        for item in (patch.object(state, "backend_ready", True, create=True),
                     patch.object(task_context, "current", return_value=""),
                     patch.object(guidance, "refused", return_value=False)):
            item.start()
            self.addCleanup(item.stop)

    def output(self, run_id, name="work"):
        path = self.root / "runs" / run_id / "artifacts" / (name + ".txt")
        path.write_text("verified", encoding="utf-8")
        return path

    def execute(self, prompt, task_file):
        run_id, step_id, _ = workflow_task._active
        self.calls.append(step_id)
        self.output(run_id, step_id)
        workflow_task.final_gate("I finished the stage", "final")

    def test_zero_and_false_are_real_required_business_inputs(self):
        for name, value in (("balance", 0), ("is_active", False)):
            with self.subTest(name=name):
                saved = self.runner.create(workflow(required_inputs=[name]), {name:value})
                self.assertEqual(saved["inputs"][name], value)

    def test_chat_does_not_reject_zero_and_false_before_runner(self):
        payload = {"spec":workflow(required_inputs=["balance","is_active"]),
                   "inputs":{"balance":0,"is_active":False}}
        workflow_task.perform("/workflow start-json " + json.dumps(payload))
        self.assertEqual(self.calls, ["work"])
        self.assertIn(": completed", self.replies[-1][0])

    def test_live_foreign_owner_observation_cannot_turn_external_action_uncertain(self):
        self.runner.create(workflow(stage(retry_safe=False)), {}, "active-external")
        self.runner.claim("active-external")
        observer = WorkflowRunner(self.runner.root)
        observed, claimed = observer.claim("active-external")
        self.assertIsNone(claimed)
        self.assertEqual(observed["status"], "running")
        self.assertEqual(self.runner.status("active-external")["status"], "running")
        self.output("active-external")
        self.runner.complete("active-external", "work")
        self.assertEqual(self.runner.claim("active-external")[0]["status"], "completed")

    def test_approval_prompt_contains_actual_action_and_copyable_scoped_command(self):
        instruction = "Publish reviewed renewal package to CRM account C-17"
        self.runner.create(workflow(stage("publish", app="CRM", instruction=instruction, approval=True)), {}, "review-123")
        workflow_task.perform("/workflow resume review-123")
        reply = self.replies[-1][0]
        self.assertIn(instruction, reply)
        self.assertIn("CRM", reply)
        self.assertIn("publish.txt", reply)
        # Asked as a question the person replies to (review 2026-11-03, F-09): no command to copy.
        self.assertEqual(self.replies[-1][1].get("kind"), "approval")
        self.assertIn("start stage publish", reply)
        self.assertNotIn("/workflow approve", reply)
        self.assertNotIn("RUN_ID STEP_ID", reply)
        self.assertEqual(self.calls, [])

    def test_completed_handoff_names_the_checked_file_in_hidden_run_directory(self):
        self.runner.create(workflow(), {}, "deliver-123")
        workflow_task.perform("/workflow resume deliver-123")
        reply = self.replies[-1][0]
        self.assertIn(str(self.runner.root / "deliver-123/artifacts/work.txt"), reply)
        self.assertIn("checked", reply.lower())

    def test_late_cancel_does_not_rewrite_verified_completion(self):
        self.runner.create(workflow(), {}, "finished")
        self.runner.claim("finished")
        self.output("finished")
        self.runner.complete("finished", "work")
        self.runner.claim("finished")
        self.assertEqual(self.runner.cancel("finished")["status"], "completed")

    def test_explicit_safe_retry_preserves_success_and_adds_only_one_attempt(self):
        self.runner.create(workflow(stage("intake"), stage("analysis", depends_on=["intake"])), {}, "fix-me")
        self.runner.claim("fix-me")
        self.output("fix-me", "intake")
        self.runner.complete("fix-me", "intake")
        original = self.runner.status("fix-me")["steps"]["intake"]
        self.runner.claim("fix-me")
        self.runner.complete("fix-me", "analysis", error="source export temporarily unavailable")
        retried = self.runner.retry("fix-me", "analysis")
        self.assertEqual(retried["status"], "pending")
        self.assertEqual(retried["steps"]["intake"], original)
        state, selected = self.runner.claim("fix-me")
        self.assertEqual(selected["id"], "analysis")
        self.assertEqual(state["steps"]["analysis"]["attempts"], 2)
        self.runner.complete("fix-me", "analysis", error="still unavailable")
        failed = self.runner.status("fix-me")
        self.assertEqual(failed["status"], "failed")
        self.assertEqual(failed["steps"]["analysis"]["attempts"], 2)

    def test_retry_never_replays_an_unreconciled_external_action(self):
        self.runner.create(workflow(stage(retry_safe=False)), {}, "external")
        self.runner.claim("external")
        self.runner.complete("external", "work", error="submission response lost")
        with self.assertRaises(ValueError):
            self.runner.retry("external", "work")
        self.assertEqual(self.runner.status("external")["steps"]["work"]["attempts"], 1)

    def test_retry_rejects_changed_checked_predecessor_before_granting_budget(self):
        self.runner.create(workflow(stage("first"), stage("second")), {}, "corrupted")
        self.runner.claim("corrupted")
        path = self.output("corrupted", "first")
        self.runner.complete("corrupted", "first")
        self.runner.claim("corrupted")
        self.runner.complete("corrupted", "second", error="missing source")
        path.write_text("different customer's data", encoding="utf-8")
        with self.assertRaises(ValueError):
            self.runner.retry("corrupted", "second")
        self.assertEqual(self.runner.status("corrupted")["steps"]["second"]["attempts"], 1)

    def test_operator_retry_cannot_remove_the_global_attempt_cap(self):
        self.runner.create(workflow(stage(max_attempts=10)), {}, "budget")
        for _ in range(10):
            self.runner.claim("budget")
            self.runner.complete("budget", "work", error="unavailable")
        with self.assertRaises(ValueError):
            self.runner.retry("budget", "work")
        self.assertEqual(self.runner.status("budget")["steps"]["work"]["attempts"], 10)


if __name__ == "__main__":
    unittest.main()
