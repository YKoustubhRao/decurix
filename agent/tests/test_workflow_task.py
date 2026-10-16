"""Exercise the production stage dispatch and evidence gate with a fake harness."""
import json
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent.workflow_runner import WorkflowRunner
from decurix_agent import workflow_task


class WorkflowHarnessIntegration(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.runner = WorkflowRunner(self.temp.name)
        self.reports, self.prompts = [], []
        self.messaging = types.ModuleType("decurix_agent.messaging")
        self.messaging.write_final_reply = lambda reply, **kwargs: self.reports.append((reply, kwargs))
        self.queries = []
        self.messaging.ask_for_query = lambda reply, **kwargs: self.queries.append((reply, kwargs))
        self.harness = types.ModuleType("decurix_agent.harness")
        self.harness.perform_task = self.execute
        workflow_task._active = None
        workflow_task._pending = None
        self.addCleanup(setattr, workflow_task, "_active", None)
        self.addCleanup(setattr, workflow_task, "_pending", None)
        self.addCleanup(patch.stopall)
        patch.object(workflow_task, "runner", return_value=self.runner).start()
        patch.dict(sys.modules, {"decurix_agent.messaging": self.messaging, "decurix_agent.harness": self.harness}).start()
        import decurix_agent
        patch.object(decurix_agent, "harness", self.harness, create=True).start()
        from decurix_agent import state
        self.state = state
        patch.object(state, "backend_ready", True, create=True).start()
        for key, value in {"waiting_for_query": False, "paused_skill": None, "paused_replay": None,
                           "last_query_text": "", "last_query_kind": "query"}.items():
            patch.object(state, key, value, create=True).start()

    def execute(self, prompt, task_file):
        self.prompts.append(prompt)
        run_id, step_id, _ = workflow_task._active
        state = self.runner.status(run_id)
        root = Path(self.runner.context(state)["work_dir"])
        (root / (step_id + ".txt")).write_text("checked business result", encoding="utf-8")
        self.assertTrue(workflow_task.final_gate("I finished the stage", "final"))

    def spec(self):
        return {"id": "unseen-business", "steps": [
            {"id": key, "app": app, "instruction": "Create checked business result", "retry_safe": True,
             "checks": [{"path": key + ".txt", "contains": ["checked business result"]}]}
            for key, app in [("extract", "browser"), ("calculate", "excel"), ("present", "powerpoint")]]}

    def test_custom_chat_workflow_dispatches_each_stage_and_emits_only_one_final(self):
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec(), "inputs": {"customer": "Example"}}))
        self.assertEqual(len(self.prompts), 3)
        self.assertEqual(len(self.reports), 1)
        self.assertIn(": completed", self.reports[0][0])
        self.assertIn('"outputs": {"extract":', self.prompts[1])

    def test_each_stages_own_report_reaches_the_one_final_reply(self):
        # Found in review (2026-10-14): final_gate consumed every stage's report
        # and the person's one reply listed only statuses - what each stage said
        # it did was lost.
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        reply = self.reports[0][0]
        self.assertIn("What each stage reported:", reply)
        self.assertEqual(reply.count("I finished the stage"), 3)
        state = self.runner.status(workflow_task.runner().root.glob("*/checkpoint.json").__next__().parent.name)
        self.assertEqual(state["steps"]["extract"]["report"], "I finished the stage")

    def test_harness_self_claim_without_artifacts_cannot_complete(self):
        self.harness.perform_task = lambda *args: workflow_task.final_gate("All done!", "final")
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        self.assertEqual(len(self.reports), 1)
        self.assertIn(": failed", self.reports[0][0])
        self.assertIn("missing/confined artifact", self.reports[0][0])

    def test_resume_uses_completed_stages_after_restart(self):
        state = self.runner.create(self.spec())
        state, step = self.runner.claim(state["run_id"])
        root = Path(self.runner.context(state)["work_dir"])
        (root / "extract.txt").write_text("checked business result")
        self.runner.complete(state["run_id"], "extract")
        workflow_task.perform('/workflow resume ' + state["run_id"])
        self.assertEqual(len(self.prompts), 2)
        self.assertIn("stage calculate", self.prompts[0])

    def test_engine_cleanup_precedes_next_stage(self):
        busy = []
        execute = self.execute
        def worker(prompt, task_file):
            self.assertFalse(busy, "next stage recursively entered active engine")
            busy.append(True)
            try:
                execute(prompt, task_file)
            finally:
                busy.pop()
                workflow_task.engine_returned()
        self.harness.perform_task = worker
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        self.assertEqual(len(self.prompts), 3)

    def test_explicit_stop_cancels_active_run_without_retry(self):
        self.harness.perform_task = lambda *args: None
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        run_id = workflow_task._active[0]
        workflow_task.cancel_active()
        self.assertEqual(self.runner.status(run_id)["status"], "cancelled")
        self.assertFalse(workflow_task.final_gate("Stopped", "final"))

    def test_paused_stage_continues_only_after_engine_returns(self):
        self.harness.perform_task = lambda *args: None
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        run_id, step_id, _ = workflow_task._active
        state = self.runner.status(run_id)
        (Path(self.runner.context(state)["work_dir"]) / "extract.txt").write_text("checked business result")
        workflow_task.final_gate("Done after query reply", "final")
        self.assertEqual(self.reports, [])
        self.assertIsNone(workflow_task._active)
        self.harness.perform_task = self.execute
        workflow_task.engine_returned()
        self.assertEqual(len(self.prompts), 2)
        self.assertIn(": completed", self.reports[0][0])

    def test_learned_skill_stage_binds_inputs_and_uses_artifact_gate(self):
        calls = []
        skill = types.ModuleType("decurix_agent.skill_task")
        skill.request_text = lambda name, params: calls.append((name, params)) or "/skill " + name
        def replay(request, task_file):
            self.assertIn('"work_dir"', request)
            self.execute("learned skill dispatched", task_file)
        skill.perform = replay
        case = self.spec()
        case["steps"][0]["skill"] = {"name": "recorded-extract", "params": {"customer": {"input": "customer"}}}
        import decurix_agent
        with patch.dict(sys.modules, {"decurix_agent.skill_task": skill}), patch.object(decurix_agent, "skill_task", skill, create=True):
            workflow_task.perform('/workflow start-json ' + json.dumps({"spec": case, "inputs": {"customer": "Example"}}))
        self.assertEqual(calls, [("recorded-extract", {"customer": "Example"})])
        self.assertIn(": completed", self.reports[0][0])

    def test_missing_learned_skill_binding_records_failure(self):
        skill = types.ModuleType("decurix_agent.skill_task")
        case = self.spec()
        case["steps"][0]["skill"] = {"name": "recorded-extract", "params": {"customer": {"input": "customer"}}}
        import decurix_agent
        with patch.dict(sys.modules, {"decurix_agent.skill_task": skill}), patch.object(decurix_agent, "skill_task", skill, create=True):
            workflow_task.perform('/workflow start-json ' + json.dumps({"spec": case}))
        self.assertIn("missing learned-skill input", self.reports[0][0])
        self.assertIsNone(workflow_task._active)

    def test_skill_only_workflow_needs_no_model_backend(self):
        case = self.spec()
        for stage in case["steps"]:
            stage["skill"] = {"name": "learned-" + stage["id"]}
        command = '/workflow start-json ' + json.dumps({"spec": case})
        self.assertTrue(workflow_task.needs_no_backend(command))
        state = self.runner.create(case)
        self.assertTrue(workflow_task.needs_no_backend('/workflow resume ' + state["run_id"]))

    def test_mixed_workflow_executes_skill_then_defers_model_stage_without_claim(self):
        skill = types.ModuleType("decurix_agent.skill_task")
        skill.request_text = lambda name, params: "/skill " + name
        skill.perform = lambda request, task_file: self.execute("learned skill dispatched", task_file)
        case = self.spec()
        case["steps"][0]["skill"] = {"name": "recorded-extract"}
        import decurix_agent
        self.state.backend_ready = False
        with patch.dict(sys.modules, {"decurix_agent.skill_task": skill}), patch.object(decurix_agent, "skill_task", skill, create=True):
            command = '/workflow start-json ' + json.dumps({"spec": case})
            self.assertTrue(workflow_task.needs_no_backend(command))
            workflow_task.perform(command)
        checkpoint = next(Path(self.temp.name).glob("*/checkpoint.json"))
        state = self.runner.status(checkpoint.parent.name)
        self.assertEqual(state["status"], "awaiting_backend")
        self.assertEqual(state["steps"]["extract"]["status"], "completed")
        self.assertEqual(state["steps"]["calculate"]["attempts"], 0)
        self.assertEqual(len(self.prompts), 1)
        self.assertFalse(workflow_task.needs_no_backend('/workflow resume ' + state["run_id"]))
        self.state.backend_ready = True
        workflow_task.perform('/workflow resume ' + state["run_id"])
        self.assertIn(": completed", self.reports[-1][0])

    def test_explicit_workflow_cancel_at_query_never_reaches_skill_or_model(self):
        self.harness.perform_task = lambda *args: None
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        run_id = workflow_task._active[0]
        self.state.waiting_for_query = True
        self.state.paused_skill = {"mode": "run"}
        self.state.last_query_text, self.state.last_query_kind = "Which value?", "query"
        command = '/workflow cancel ' + run_id
        self.assertTrue(workflow_task.query_control(command))
        workflow_task.perform_control(command)
        self.assertEqual(self.runner.status(run_id)["status"], "cancelled")
        self.assertIsNone(workflow_task._active)
        self.assertIsNone(self.state.paused_skill)
        self.assertFalse(self.state.waiting_for_query)

    def test_status_during_query_preserves_active_stage_and_question(self):
        self.harness.perform_task = lambda *args: None
        workflow_task.perform('/workflow start-json ' + json.dumps({"spec": self.spec()}))
        active = workflow_task._active
        self.state.last_query_text, self.state.last_query_kind = "Which value?", "query"
        workflow_task.perform_control('/workflow status ' + active[0])
        self.assertEqual(workflow_task._active, active)
        self.assertEqual(len(self.reports), 0)
        self.assertIn("Which value?", self.queries[0][0])


if __name__ == "__main__":
    unittest.main()
