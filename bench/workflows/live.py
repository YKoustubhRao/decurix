"""Run selected workflow examples through a desktop and verify disk artifacts.

Inputs JSON maps each catalog id to its source_files/instructions/recipient.
Requires a running control plane, desktop, Docker and configured model.
"""
import argparse
import json
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agent"))
from decurix_agent.workflow_catalog import CATALOG


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--desktop", required=True)
    p.add_argument("--inputs", type=Path, required=True)
    p.add_argument("--only", help="Comma-separated example IDs; defaults to keys in inputs JSON")
    p.add_argument("--base", default="http://127.0.0.1:8080")
    p.add_argument("--timeout", type=int, default=7200, help="Seconds per workflow")
    p.add_argument("--output", type=Path, default=Path("bench/workflows/results/live.json"))
    args = p.parse_args()
    inputs = json.loads(args.inputs.read_text(encoding="utf-8"))
    selected = args.only.split(",") if args.only else list(inputs)
    if not selected or any(key not in CATALOG for key in selected):
        p.error("Select valid workflow example IDs")
    for key in selected:
        if not inputs.get(key, {}).get("source_files") or not inputs.get(key, {}).get("instructions"):
            p.error(f"{key} needs source_files and instructions")

    def api(path, data=None):
        body = urllib.parse.urlencode(data).encode() if data is not None else None
        with urllib.request.urlopen(args.base.rstrip("/") + path, data=body, timeout=30) as r:
            return json.load(r)

    chat_path = "/api/desktops/chat?" + urllib.parse.urlencode({"id": args.desktop})
    results = []
    for key in selected:
        before = api(chat_path)
        if before.get("running"):
            raise RuntimeError("Desktop already working; use an idle test desktop")
        seen = {e.get("id") for e in before.get("entries", [])}
        api("/api/desktops/send", {"id": args.desktop,
            "content": f"/workflow start {key} " + json.dumps(inputs[key])})
        deadline = time.monotonic() + args.timeout
        outcome = {"workflow": key, "passed": False}
        while time.monotonic() < deadline:
            thread = api(chat_path)
            entries = [e for e in thread.get("entries", []) if e.get("id") not in seen and e.get("role") != "user"]
            if entries and not thread.get("running"):
                reply = entries[-1].get("reply") or entries[-1].get("content") or ""
                match = re.search(r"Workflow ([\w-]+): completed\.", reply)
                if match:
                    run_id = match.group(1)
                    # A second process re-reads every artifact rather than trust
                    # the chat or the model's report - but with the run's own
                    # check code (artifact_evidence), so it proves the files are
                    # still what the run's checks saw, not that those checks are
                    # right. It is not an independent check, and the report says
                    # so (review 2026-11-02): ground truth for a workflow lives
                    # in a checker outside the repository.
                    code = ("import json; from decurix_agent import config; "
                            "from decurix_agent.workflow_runner import WorkflowRunner,artifact_evidence; "
                            "r=WorkflowRunner(config.DECURIX_DIR/'workflows'); "
                            f"s=r.status({run_id!r}); "
                            "assert s['status']=='completed'; "
                            "assert all(artifact_evidence(t,{},r.context(s))==s['steps'][t['id']]['evidence'] for t in s['spec']['steps']); "
                            "print(json.dumps({'run_id':s['run_id'],'verified_stages':len(s['steps'])}))")
                    checked = subprocess.run(["docker", "exec", "-u", "headless", "-e", "PYTHONPATH=/opt",
                        f"desktop_{args.desktop}", "python3", "-c", code], capture_output=True, text=True, timeout=90)
                    outcome.update(passed=checked.returncode == 0, run_id=run_id,
                                   verification=checked.stdout.strip(), error=checked.stderr[-1000:])
                else:
                    outcome["error"] = reply[:1000] or "No completed workflow report"
                break
            time.sleep(4)
        else:
            outcome["error"] = "Timed out; inspect the worker before resuming"
        results.append(outcome)
        print(json.dumps(outcome), flush=True)
        if not outcome["passed"]:
            break  # Do not feed a new workflow into a pending human query.
    report = {"mode": "live_desktop", "passed": len(results) == len(selected) and all(r["passed"] for r in results),
              "verification": "the run's own artifact checks re-read in a second process; not an independent checker",
              "results": results}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2), encoding="utf-8")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
