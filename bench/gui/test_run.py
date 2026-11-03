"""The benchmark process must fail when independent task checks reject a run."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import run


class BenchmarkExitTests(unittest.TestCase):
    def run_checks(self, verdicts):
        with tempfile.TemporaryDirectory() as folder, contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(run.sys, "argv", ["run.py", "--engine", "oracle",
                "--container", "isolated-test", "--tasks", "web-login,web-create-user", "--out", folder]))
            for name in ("wait_for_agent", "install_fixtures", "reset_task"):
                stack.enter_context(patch.object(run, name))
            stack.enter_context(patch.object(run, "run_oracle", return_value={"answer": "success", "seconds": 1}))
            stack.enter_context(patch.object(run, "check_task", return_value={"independent": True}))
            stack.enter_context(patch.object(run, "verdict", side_effect=verdicts))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = run.main()
            report = json.loads(next(Path(folder).glob("*.json")).read_text())
            return code, report["engines"]["oracle"]

    def test_rejected_business_result_fails_process_despite_successful_actions(self):
        code, report = self.run_checks([(True, ""), (False, "wrong saved result")])
        self.assertEqual(code, 1)
        self.assertEqual((report["passed"], report["total"]), (1, 2))
        self.assertEqual(report["tasks"][1]["why"], "wrong saved result")

    def test_all_checked_results_pass_process(self):
        code, report = self.run_checks([(True, ""), (True, "")])
        self.assertEqual(code, 0)
        self.assertEqual((report["passed"], report["total"]), (2, 2))


class AnswerKeyTests(unittest.TestCase):
    """The agent's home gets the apps' files, never the answer key; the check
    runs as root from a root-only copy."""

    def test_the_agent_copy_holds_no_answer_key(self):
        self.assertNotIn("tasks.json", run.AGENT_FILES)
        self.assertFalse(run.ORACLE.startswith("/home/"))
        commands = []
        with patch.object(run, "docker", side_effect=lambda *a, **k: commands.append((a, k))), \
                patch.object(run.subprocess, "run", side_effect=lambda cmd, **k: commands.append((tuple(cmd), k))):
            run.install_fixtures("c")
        copies = [c[0] for c in commands if c[0][:2] == ("docker", "cp")]
        for cmd in copies:
            if cmd[-1].startswith("c:" + run.REMOTE):
                self.assertNotIn("tasks.json", cmd[2], cmd)
        self.assertTrue(any(cmd[-1] == f"c:{run.ORACLE}/tasks.json" for cmd in copies))
        self.assertTrue(any("go-rwx" in " ".join(c[0]) and c[1].get("user") == "root" for c in commands))

    def test_reset_is_handed_its_start_and_check_runs_from_the_root_copy(self):
        task = {"id": "calc-sum", "start": {"calc": "budget.ods"}, "check": {"type": "answer_number", "value": 5}}
        reset = run.reset_command(task)
        self.assertTrue(reset[1].startswith(run.REMOTE))
        self.assertEqual(json.loads(reset[reset.index("--start") + 1]), {"calc": "budget.ods"})
        self.assertNotIn("answer_number", " ".join(reset))
        self.assertTrue(run.check_command(task)[1].startswith(run.ORACLE))
        calls = []
        with patch.object(run, "docker", side_effect=lambda *a, **k: calls.append((a, k)) or
                          type("P", (), {"stdout": '{"pass": true, "detail": ""}', "stderr": "", "returncode": 0})()):
            run.check_task("c", task, "5")
        self.assertTrue(all(k.get("user") == "root" for _, k in calls))


if __name__ == "__main__":
    unittest.main()
