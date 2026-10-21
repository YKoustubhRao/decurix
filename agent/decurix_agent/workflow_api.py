"""Read-only workflow activity for people; never expose raw inputs or leases."""
from pathlib import Path

from decurix_agent.workflow_catalog import CATALOG
from decurix_agent.workflow_runner import WorkflowRunner


def summary(saved):
    run_id, status = saved["run_id"], saved["status"]
    stages = []
    actions = []
    for step in saved["spec"]["steps"]:
        record = saved["steps"][step["id"]]
        if any(not isinstance(item, dict) for item in record["evidence"]):
            raise ValueError("invalid artifact evidence")
        stages.append({"id": step["id"], "app": step.get("app", ""),
                       "status": record["status"], "attempts": record["attempts"],
                       "error": str(record.get("error", ""))[:2000],
                       "files": [{key: item.get(key) for key in ("path", "sha256", "bytes")}
                                 for item in record["evidence"]]})
        if (status == "failed" and record["status"] == "failed"
                and step.get("retry_safe") and step.get("max_attempts", 2) + record.get("additional_attempts", 0) < 10):
            actions.append({"label": "Retry failed stage", "command": f"/workflow retry {run_id} {step['id']}"})
    approval = None
    if status == "awaiting_approval":
        token = saved.get("approval_required", "")
        step = next((s for s in saved["spec"]["steps"] if token == f"{run_id}:{s['id']}"), None)
        if step:
            approval = {"stage": step["id"], "instruction": step["instruction"],
                        "files": [check["path"] for check in step["checks"]]}
            actions.append({"label": "Approve this stage", "command": f"/workflow approve {run_id} {step['id']}"})
    if status in {"pending", "awaiting_backend"}:
        actions.append({"label": "Continue workflow", "command": f"/workflow resume {run_id}"})
    if status not in {"completed", "cancelled"}:
        actions.append({"label": "Cancel workflow", "command": f"/workflow cancel {run_id}"})
    guidance = {
        "running": "A stage is running or paused. Open chat to answer its question. After a worker restart, use the saved run ID in chat to resume.",
        "uncertain": "An interrupted action may have changed another app. Check that app before starting more work; normal resume cannot resolve this state.",
        "failed": "The workflow stopped. Completed files are preserved. Only stages marked safe to retry offer a retry action.",
        "awaiting_backend": "The reasoning service is unavailable. Your work is saved; continue when it is connected.",
        "awaiting_approval": "Review the proposed stage below before approving it.",
        "awaiting_answer": "Every stage that does not need your answer is done. Answer the question in chat to continue.",
        "completed": "All required artifact checks passed. Those checks cover the workflow's stated criteria.",
        "cancelled": "No further stages will start. Changes already made are preserved.",
        "pending": "Your checkpoint is saved. Continue to run the next stage.",
    }
    return {"run_id": run_id, "title": saved["spec"].get("title", saved["spec"]["id"]),
            "status": status, "created_at": saved.get("created_at"),
            "completed_stages": sum(stage["status"] == "completed" for stage in stages),
            "total_stages": len(stages), "stages": stages, "approval": approval,
            "error": str(saved.get("error", ""))[:2000], "guidance": guidance[status], "actions": actions}


def listing(root=None, limit=100):
    from decurix_agent import config, state
    root = Path(root if root is not None else config.DECURIX_DIR / "workflows").resolve()
    entries = []
    if root.is_dir():
        for checkpoint in root.glob("*/checkpoint.json"):
            try:
                entries.append((checkpoint.stat().st_mtime, checkpoint.parent.name))
            except OSError:
                continue
    entries.sort(reverse=True)
    runs, unreadable = [], []
    runner = WorkflowRunner(root) if entries else None
    for _, run_id in entries[:limit]:
        try:
            runs.append(summary(runner.status(run_id)))
        except (OSError, ValueError, KeyError, TypeError):
            unreadable.append(run_id)
    return {"runs": runs, "unreadable_runs": unreadable, "has_more": len(entries) > limit,
            "busy": bool(state.current_task or state.waiting_for_query),
            "catalog": [{"id": spec["id"], "title": spec["title"]} for spec in CATALOG.values()]}
