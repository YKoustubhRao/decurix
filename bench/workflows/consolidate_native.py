"""Consolidate exact passing native teacher lineages while retaining failed attempts."""
import argparse
import hashlib
import json
from pathlib import Path

from native_transfer import CASES


def consolidate(paths):
    passed, attempts = {}, []
    for path in paths:
        data = Path(path).read_bytes()
        report = json.loads(data)
        if not report.get("live_apps_tested"):
            raise ValueError("only actual native-app reports can qualify these cases")
        attempts.append({"path": str(path), "sha256": hashlib.sha256(data).hexdigest(),
                         "results": [{"task": row["task"], "passed": row.get("passed", False),
                                      "error": row.get("error", "")} for row in report["results"]]})
        for row in report["results"]:
            if row.get("passed") and row.get("artifact_verified") and row.get("original_preserved"):
                passed[row["task"]] = dict(row, attempt_report=str(path), artifacts_root=report["root"],
                                           preservation_scope=row.get("preservation_scope") or
                                           ("teacher_file_unchanged" if row.get("source_file_bindings") else "original_contents_at_renamed_path"))
    expected = [case["id"] for case in CASES]
    completed = all(task in passed for task in expected)
    return {"mode": "live_native_record_learn_holdout_consolidated", "live_apps_tested": True,
            "expected_cases": len(expected), "completed": completed, "passed": completed,
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
    print(json.dumps({key: report[key] for key in ("expected_cases", "completed", "passed", "missing_cases")}))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
