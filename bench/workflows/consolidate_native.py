"""
Consolidate exact passing native teacher lineages while retaining failed attempts.

A case that passed in one attempt and failed in another is flaky, not passed: this used to take the union of the
attempts' passes, so best-of-N retries read as "passed" (review 2026-11-02). "passed" now means every attempt of every
case passed; "best_of_n" keeps the old meaning, by its own name, and "flaky_cases" names the cases between the two.
"""
import argparse
import hashlib
import json
from pathlib import Path

from native_transfer import CASES


def consolidate(paths):
    passed, attempts, tries, fails = {}, [], {}, {}
    for path in paths:
        data = Path(path).read_bytes()
        report = json.loads(data)
        if not report.get("live_apps_tested"):
            raise ValueError("only actual native-app reports can qualify these cases")
        attempts.append({"path": str(path), "sha256": hashlib.sha256(data).hexdigest(),
                         "results": [{"task": row["task"], "passed": row.get("passed", False),
                                      "error": row.get("error", "")} for row in report["results"]]})
        for row in report["results"]:
            tries[row["task"]] = tries.get(row["task"], 0) + 1
            if not (row.get("passed") and row.get("artifact_verified") and row.get("original_preserved")):
                fails[row["task"]] = fails.get(row["task"], 0) + 1
                continue
            if row["task"] not in passed:
                passed[row["task"]] = dict(row, attempt_report=str(path), artifacts_root=report["root"],
                                           preservation_scope=row.get("preservation_scope") or
                                           ("teacher_file_unchanged" if row.get("source_file_bindings") else "original_contents_at_renamed_path"))
    expected = [case["id"] for case in CASES]
    completed = all(task in passed for task in expected)
    flaky = [task for task in expected if task in passed and fails.get(task)]
    return {"mode": "live_native_record_learn_holdout_consolidated", "live_apps_tested": True,
            "expected_cases": len(expected), "completed": completed, "best_of_n": completed,
            "passed": completed and not flaky, "flaky_cases": flaky,
            "case_attempts": {task: {"attempts": tries.get(task, 0), "failed": fails.get(task, 0)} for task in expected},
            "missing_cases": [task for task in expected if task not in passed], "attempt_reports": attempts,
            "results": [passed[task] for task in expected if task in passed]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("reports", type=Path, nargs="+")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    report = consolidate(args.reports)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2))
    print(json.dumps({key: report[key] for key in ("expected_cases", "completed", "passed", "flaky_cases", "missing_cases")}))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
