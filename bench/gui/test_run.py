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


if __name__ == "__main__":
    unittest.main()
