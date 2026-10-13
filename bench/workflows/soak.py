"""Offline orchestration soak. Simulates apps; never claims live Office success.

Run: python bench/workflows/soak.py --cycles 20 --output bench/workflows/results/soak.json
"""
import argparse
import copy
import json
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agent"))
from decurix_agent.workflow_runner import WorkflowRunner
from decurix_agent import workflow_catalog


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cycles", type=int, default=20)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.cycles < 1:
        parser.error("cycles must be positive")
    catalog = workflow_catalog.CATALOG
    specs = list(catalog.values()) if isinstance(catalog, dict) else catalog
    started = time.monotonic()
    calls, results = {}, []
    with tempfile.TemporaryDirectory(prefix="workflow-soak-") as tmp:
        def execute(step, context):
            key = (context["run_id"], step["id"])
            calls[key] = calls.get(key, 0) + 1
            # Every stage experiences a transient app failure before producing
            # its simulated receipt. No external service or app is contacted.
            if calls[key] == 1:
                raise RuntimeError("injected transient app failure")
            receipt = {"stage": step["id"], "app": step.get("app"),
                       "upstream": sorted(context["outputs"])}
            target = Path(context["work_dir"]) / step["checks"][0]["path"]
            target.write_text(json.dumps(receipt), encoding="utf-8")
            return {"claimed_done": True}

        for cycle in range(args.cycles):
            for original in specs:
                spec = copy.deepcopy(original)
                for step in spec["steps"]:
                    step["retry_safe"], step["max_attempts"] = True, 2
                    step["checks"] = [{"path": f"{step['id']}.json", "format": "json",
                                       "contains": [step["id"]]}]
                runner = WorkflowRunner(tmp, execute)
                state = runner.create(spec, {"cycle": cycle, "source_files": ["simulated-input"],
                                             "instructions": "offline fixture receipts only"})
                run_id = state["run_id"]
                approvals = [f"{run_id}:{s['id']}" for s in spec["steps"]]
                # Recreate runner after every attempted stage to exercise loading
                # persisted checkpoints rather than relying on process memory.
                for _ in range(len(spec["steps"]) * 3 + 3):
                    runner = WorkflowRunner(tmp, execute)
                    state = runner.advance(run_id, approvals=approvals, max_steps=1)
                    if state["status"] in {"completed", "failed", "uncertain"}:
                        break
                ok = state["status"] == "completed" and all(
                    calls.get((run_id, s["id"])) == 2 for s in spec["steps"])
                results.append({"workflow": spec["id"], "cycle": cycle,
                                "status": state["status"], "passed": ok})
            if (cycle + 1) % 5 == 0:
                print(f"{cycle + 1}/{args.cycles} cycles checked", flush=True)
    report = {"mode": "offline_simulated_apps", "live_apps_tested": False,
              "elapsed_seconds": round(time.monotonic() - started, 3),
              "runs": len(results), "stage_attempts": sum(calls.values()),
              "passed": all(r["passed"] for r in results), "results": results}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({k: v for k, v in report.items() if k != "results"}, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
