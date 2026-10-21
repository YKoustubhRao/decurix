"""
The agent's per-request decision across the six live requests of round 5 (live_requests.json among the live-run
fixtures outside the repository - fixture_paths.py - shared with the soul's corpus tests): the app each deliverable is
bound to (long_task.stage_app), which the stage prompt hands the soul as "Work in app:". Found live (CL, round 5 P1, 2026-10-20): "a 5-slide MIS deck
in PowerPoint" held the workbook stage to PowerPoint.

    PYTHONPATH="agent;agent/tests" python -m unittest test_requests_corpus
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, workflow_task
from decurix_agent.workflow_runner import WorkflowRunner
from fixture_paths import need



class LiveRequests(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "work").mkdir()
        self.runner = WorkflowRunner(self.root / "workflows")
        for patch in (mock.patch.object(config, "DECURIX_DIR", self.root),
                      mock.patch.object(config, "WORK_DIR", self.root / "work"),
                      mock.patch.dict("os.environ", {"DECURIX_DIR": str(self.root)}),
                      mock.patch.object(workflow_task, "runner", return_value=self.runner),
                      mock.patch.object(long_task.progress, "emit", lambda *a, **k: None)):
            patch.start()
            self.addCleanup(patch.stop)
        self.addCleanup(long_task._stage_start.clear)

    def test_each_deliverable_is_bound_to_its_own_app_only(self):
        requests = json.loads(need("live_requests.json").read_text(encoding="utf-8"))["requests"]
        self.assertEqual(len(requests), 6)
        for request in requests:
            files = [d["file"] for d in request["deliverables"]]
            plan = {"long": True, "title": request["id"], "stages": [
                {"id": f"s{n}", "title": f"Make {Path(f).name}", "does": f"Make {Path(f).name}.", "outputs": [{"file": f}]}
                for n, f in enumerate(files, 1)]}
            spec = long_task.spec_from_plan(plan, (), request=request["text"])
            run_id = self.runner.create(spec, work_dir=str(self.root / "work"))["run_id"]
            long_task._update(run_id, lambda d: d.update(request=request["text"]))
            state = self.runner.status(run_id)
            for step, deliverable in zip(spec["steps"], request["deliverables"]):
                with self.subTest(request=request["id"], file=deliverable["file"]):
                    bound = long_task.stage_app(step, request["text"])
                    self.assertEqual(bound[0] if bound else "", deliverable["app"])
                    # The stage prompt says it in the lines the soul reads (atomizer.AppsFor).
                    prompt = long_task.stage_prompt(state, step)
                    if deliverable["app"]:
                        self.assertIn("\nWork in app: " + deliverable["app"] + "\n", prompt)
                    else:
                        self.assertNotIn("Work in app:", prompt)
                        self.assertIn("No app is named for what this stage hands in", prompt)


if __name__ == "__main__":
    unittest.main()
