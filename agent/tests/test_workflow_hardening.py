"""Regression tests for concurrent claims, stale results and generic evidence."""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

from decurix_agent.workflow_runner import WorkflowRunner, validate


def spec(**extra):
    return {"id": "generic", "steps": [{"id": "produce", "instruction": "Produce evidence", "retry_safe": True,
            "checks": [{"path": "out.json", "format": "json", "json_equals": {"/total": 42}}], **extra}]}


class WorkflowHardening(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.runner = WorkflowRunner(self.temp.name)
        self.runner.create(spec(), {}, "run")

    def mutate(self, callback):
        path = Path(self.temp.name) / "run" / "checkpoint.json"
        state = json.loads(path.read_text())
        callback(state)
        path.write_text(json.dumps(state))

    def output(self, state, value=42):
        (Path(self.runner.context(state)["work_dir"]) / "out.json").write_text(json.dumps({"total": value}))

    def test_two_workers_cannot_claim_one_running_stage(self):
        barrier = threading.Barrier(2)
        results = []
        def claim():
            worker = WorkflowRunner(self.temp.name)
            barrier.wait()
            results.append(worker.claim("run"))
        workers = [threading.Thread(target=claim) for _ in range(2)]
        for worker in workers:
            worker.start()
        for worker in workers:
            worker.join(5)
            self.assertFalse(worker.is_alive())
        self.assertEqual(sum(stage is not None for _, stage in results), 1)
        self.assertEqual(self.runner.status("run")["steps"]["produce"]["attempts"], 1)

    def test_separate_live_process_prevents_duplicate_claim(self):
        code = ("import sys,time; from decurix_agent.workflow_runner import WorkflowRunner; "
                "r=WorkflowRunner(sys.argv[1]); state,step=r.claim('run'); "
                "print('claimed' if step else 'blocked',flush=True); time.sleep(30)")
        child_env = dict(os.environ)
        child_env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1]) + os.pathsep + child_env.get("PYTHONPATH", "")
        child = subprocess.Popen([sys.executable, "-c", code, self.temp.name], stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, text=True, env=child_env,
                                 creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
        try:
            self.assertEqual(child.stdout.readline().strip(), "claimed")
            state, stage = self.runner.claim("run", recover=True)
            self.assertIsNone(stage)
            self.assertEqual(state["steps"]["produce"]["attempts"], 1)
        finally:
            child.terminate()
            child.communicate(timeout=5)
        # Once that process is gone a safe stage can be recovered.
        state, stage = self.runner.claim("run")
        self.assertIsNotNone(stage)
        self.assertEqual(state["steps"]["produce"]["attempts"], 2)

    def test_current_claim_cannot_be_completed_by_another_worker(self):
        state, _ = self.runner.claim("run")
        self.output(state)
        with self.assertRaisesRegex(ValueError, "claim token"):
            WorkflowRunner(self.temp.name).complete("run", "produce")
        self.assertEqual(self.runner.complete("run", "produce")["steps"]["produce"]["status"], "completed")

    def test_resume_does_not_mark_live_non_idempotent_worker_uncertain(self):
        self.runner.create(spec(retry_safe=False), {}, "external")
        state, _ = self.runner.claim("external")
        observer = WorkflowRunner(self.temp.name)
        observed, stage = observer.claim("external", recover=True)
        self.assertIsNone(stage)
        self.assertEqual(observed["status"], "running")
        self.assertEqual(observed["steps"]["produce"]["attempts"], 1)
        self.output(state)
        self.assertEqual(self.runner.complete("external", "produce")["steps"]["produce"]["status"], "completed")

    def test_dead_non_idempotent_worker_still_requires_reconciliation(self):
        self.runner.create(spec(retry_safe=False), {}, "external")
        self.runner.claim("external")
        path = Path(self.temp.name) / "external" / "checkpoint.json"
        state = json.loads(path.read_text())
        state["steps"]["produce"]["lease"]["pid"] = 2147483647
        path.write_text(json.dumps(state))
        observed, stage = WorkflowRunner(self.temp.name).claim("external", recover=True)
        self.assertIsNone(stage)
        self.assertEqual(observed["status"], "uncertain")

    def test_live_owner_is_protected_even_after_expiration_and_explicit_recover(self):
        self.runner.claim("run")
        self.mutate(lambda s: s["steps"]["produce"]["lease"].update(expires_at=0))
        state, stage = WorkflowRunner(self.temp.name).claim("run", recover=True)
        self.assertIsNone(stage)
        self.assertEqual(state["steps"]["produce"]["attempts"], 1)

    def test_dead_owner_safe_stage_recovered_and_old_result_rejected(self):
        self.runner.claim("run")
        self.mutate(lambda s: s["steps"]["produce"]["lease"].update(pid=2147483647))
        replacement = WorkflowRunner(self.temp.name)
        state, stage = replacement.claim("run")
        self.assertIsNotNone(stage)
        self.output(state)
        with self.assertRaisesRegex(ValueError, "claim token"):
            self.runner.complete("run", "produce")
        self.assertEqual(replacement.complete("run", "produce")["steps"]["produce"]["status"], "completed")

    def test_pid_reuse_does_not_block_restart_recovery(self):
        self.runner.claim("run")
        self.mutate(lambda s: s["steps"]["produce"]["lease"].update(process_identity="previous-container-lifetime"))
        state, stage = WorkflowRunner(self.temp.name).claim("run")
        self.assertIsNotNone(stage)
        self.assertEqual(state["steps"]["produce"]["attempts"], 2)

    def test_cancel_during_execution_is_not_overwritten_by_late_result(self):
        state, _ = self.runner.claim("run")
        WorkflowRunner(self.temp.name).cancel("run")
        self.output(state)
        self.assertEqual(self.runner.complete("run", "produce")["status"], "cancelled")

    def test_json_numeric_assertion_rejects_wrong_total_despite_valid_json(self):
        state, _ = self.runner.claim("run")
        self.output(state, 43)
        state = self.runner.complete("run", "produce")
        self.assertEqual(state["steps"]["produce"]["status"], "pending")
        self.assertIn("JSON value mismatch", state["steps"]["produce"]["error"])

    def test_json_pointer_resolves_array_and_escaped_key(self):
        case = spec(checks=[{"path": "out.json", "format": "json", "json_equals": {"/a~1b/0/~0key": 42}}])
        self.runner.create(case, {}, "array")
        state, _ = self.runner.claim("array")
        (Path(self.runner.context(state)["work_dir"]) / "out.json").write_text('{"a/b":[{"~key":42}]}')
        self.assertEqual(self.runner.complete("array", "produce")["steps"]["produce"]["status"], "completed")

    def test_schema_rejects_ambiguous_or_malformed_fields(self):
        for extra in ({"max_attempts": True}, {"max_attempts": "2"}, {"retry_safe": "false"},
                      {"approval": 1}, {"depends_on": "produce"}, {"checks": "anything"},
                      {"skill": {"name": "x", "params": []}}):
            with self.subTest(extra=extra), self.assertRaises(ValueError):
                validate(spec(**extra))

    def test_checkpoint_step_identity_cannot_be_forged(self):
        self.mutate(lambda state: state["steps"].update(forged={"status": "completed", "attempts": 0, "evidence": []}))
        with self.assertRaisesRegex(ValueError, "step ids"):
            self.runner.claim("run")

    def test_csv_evidence_checks_columns_and_row_count(self):
        self.runner.create(spec(checks=[{"path": "out.csv", "format": "csv", "columns": ["id", "amount"], "min_rows": 2}]), {}, "csv")
        state, _ = self.runner.claim("csv")
        (Path(self.runner.context(state)["work_dir"]) / "out.csv").write_text("id,amount\n1,42\n")
        state = self.runner.complete("csv", "produce")
        self.assertIn("too few", state["steps"]["produce"]["error"])

    def test_html_script_or_comment_cannot_substitute_for_visible_text(self):
        self.runner.create(spec(checks=[{"path": "out.html", "format": "html", "contains": ["verified"]}]), {}, "html")
        state, _ = self.runner.claim("html")
        (Path(self.runner.context(state)["work_dir"]) / "out.html").write_text("<html><script>verified</script><!-- verified --><body>wrong</body></html>")
        self.assertIn("missing required content", self.runner.complete("html", "produce")["steps"]["produce"]["error"])


if __name__ == "__main__":
    unittest.main()
