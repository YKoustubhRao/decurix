"""Lane: rework, the same request done more than once.

OBSERVED: retries (a task's second and later executions) and what they spent (proxy and desktop-reported, apart);
the same request (whitespace and case aside) sent as a new task to the same worker within a day of an earlier one;
and the eval runner's reruns of case runs the model made no turn in.

MODELED: the retry overhead per task, the difference between the median spend of tasks run more than once and of tasks
run once. Tasks differ, so it is an estimate, never added to the observed retry spend."""

import hashlib

from . import common
from .common import MODELED, OBSERVED, figure, median
from .questions import norm

LANE = "rework"
DAY_NS = 24 * 3600 * 10**9


def run(source, origin=None):
    execs = source.executions(origin)
    calls = source.calls_by_execution(execs)
    proxy = {e["id"]: sum((c["input_tokens"] or 0) + (c["output_tokens"] or 0) for c in calls.get(e["id"], [])) for e in execs}
    by_task = {}
    for e in execs:
        by_task.setdefault(e["task_id"], []).append(e)
    retries = [e for e in execs if (e.get("attempt") or 1) > 1]
    retried_tasks = [t for t, runs in by_task.items() if len(runs) > 1]
    retry_proxy = sum(proxy[e["id"]] for e in retries)
    retry_reported = sum((e.get("input_tokens") or 0) + (e.get("output_tokens") or 0) for e in retries)
    all_proxy = sum(proxy.values())

    # The same request sent again as a new task to the same worker within a day.
    first_by_req = {}
    asked_again = []
    tasks = {}
    for e in execs:
        tasks.setdefault(e["task_id"], e)
    for t in sorted(tasks.values(), key=lambda e: e.get("task_created_ns") or 0):
        key = (t["worker_id"], norm(t["request"]))
        if not key[1]:
            continue
        prev = first_by_req.get(key)
        if prev is not None and (t.get("task_created_ns") or 0) - prev <= DAY_NS:
            asked_again.append({"sha": hashlib.sha256(key[1].encode()).hexdigest()[:12], "task": t["task_id"]})
        first_by_req[key] = t.get("task_created_ns") or 0

    once = [sum(proxy[e["id"]] for e in runs) for runs in by_task.values() if len(runs) == 1]
    more = [sum(proxy[e["id"]] for e in runs) for runs in by_task.values() if len(runs) > 1]
    groups = {"asked_again": asked_again[:100], "eval_reruns": {}}
    if source.has("eval_rows", "attempt"):
        for r in source.rows("SELECT run_id, arm, COUNT(*) AS n FROM eval_rows WHERE attempt > 1 GROUP BY run_id, arm"):
            groups["eval_reruns"][f"{r['run_id']}/{r['arm']}"] = {"reruns": r["n"], "label": OBSERVED}
    figures = [
        figure("tasks", len(by_task), OBSERVED, "executions by task"),
        figure("tasks run more than once", len(retried_tasks), OBSERVED, "executions.attempt"),
        figure("retry executions", len(retries), OBSERVED, "executions.attempt > 1"),
        figure("proxy tokens of retries", retry_proxy, OBSERVED, "llm_calls joined to retry executions"),
        figure("proxy share spent on retries", round(retry_proxy / all_proxy, 4) if all_proxy else 0, OBSERVED, "llm_calls"),
        figure("desktop-reported tokens of retries", retry_reported, OBSERVED, "executions.*_tokens",
               "the worker's own report: never added to the proxy figure"),
        figure("same request sent again within a day", len(asked_again), OBSERVED, "tasks.request, normalised"),
        figure("retry overhead per task (median, more-than-once minus once)",
               (median(more) - median(once)) if once and more else 0, MODELED, "llm_calls by task",
               "tasks run again differ from tasks run once; an estimate, not a saving"),
    ]
    caveats = [
        "A retry is a task's later execution (a person's retry, a send-back); what asked for it is not kept here.",
        "A request sent again is matched on its words; a reworded one is missed, and two real requests with the same "
        "words (this month's and next) are counted if within a day.",
        "The modeled overhead compares different tasks; read it as a size, never add it to the observed spend.",
    ]
    return common.lane_doc(LANE, source, {"origin": origin or "all", "executions": len(execs)}, figures, groups, caveats)
