"""Lane: gate stops and false-done candidates.

OBSERVED: the final check's verdicts (executions.verdict, decurix.verdict/v1) by verdict and by the gate that gave
it; the approval gate's stops (interventions of kind approval, and gate-signed query notes); "done" that was never
checked; and, from eval rows, graded false done.

MODELED: false-done candidates found by a rule, never graded: a run its final check called done that a person later
marked wrong, or that was run again (a retry of the same task). A candidate is a lead to read, not a count of
false dones; it is never added to the graded figure."""

import json

from . import common
from .common import MODELED, OBSERVED, figure
from .tokens import kind_of

LANE = "gates"


def verdict_of(e):
    try:
        v = json.loads(e.get("verdict") or "")
    except ValueError:
        return None
    return v if isinstance(v, dict) else None


def run(source, origin=None):
    execs = source.executions(origin)
    groups = {"final_verdicts": {}, "by_gate": {}, "approval_stops_by_kind": {}, "candidates": []}
    done_unchecked = 0
    by_task = {}
    for e in execs:
        by_task.setdefault(e["task_id"], []).append(e)
        v = verdict_of(e)
        if v:
            key = v.get("verdict") or "?"
            groups["final_verdicts"][key] = groups["final_verdicts"].get(key, 0) + 1
            gate = v.get("by") or "?"
            g = groups["by_gate"].setdefault(gate, {})
            g[key] = g.get(key, 0) + 1
        if e["status"] == "done" and e.get("checked") != 1:
            done_unchecked += 1

    approvals = 0
    if source.has("interventions"):
        ids = {e["id"]: e for e in execs}
        for r in source.rows("SELECT execution_id, kind FROM interventions WHERE kind = 'approval'"):
            if r["execution_id"] in ids:
                approvals += 1
                k = kind_of(ids[r["execution_id"]])
                groups["approval_stops_by_kind"][k] = groups["approval_stops_by_kind"].get(k, 0) + 1
    gate_signed = 0
    if source.has("query_notes", "gate_request"):
        gate_signed = source.conn.execute("SELECT COUNT(*) FROM query_notes WHERE gate_request <> ''").fetchone()[0]

    # Candidates: a run its final check called done, then a person said wrong, or the task was run again after it.
    person_wrong = retried = 0
    for task, runs in by_task.items():
        runs.sort(key=lambda e: (e["attempt"], e["started_ns"]))
        for i, e in enumerate(runs):
            v = verdict_of(e)
            said_done = (v or {}).get("verdict") == "done" or (v is None and e.get("checked") == 1 and e["status"] == "done")
            if not said_done:
                continue
            why = []
            if e.get("person_verdict") == "wrong":
                why.append("a person marked the task wrong")
                person_wrong += 1
            if i + 1 < len(runs):
                why.append("the task was run again after it")
                retried += 1
            if why:
                groups["candidates"].append({"execution": e["id"], "task": task, "kind": kind_of(e), "why": why,
                                             "label": MODELED})

    graded = {}
    if source.has("eval_rows"):
        r = source.conn.execute("SELECT COUNT(*), COALESCE(SUM(false_done),0), COALESCE(SUM(done_unchecked_wrong),0) "
                                "FROM eval_rows WHERE invalid = 0 AND infra = 0").fetchone()
        graded = {"case_runs": r[0], "false_done": r[1], "done_unchecked_wrong": r[2]}
    figures = [
        figure("final-check verdicts", sum(groups["final_verdicts"].values()), OBSERVED, "executions.verdict"),
        figure("final check said not_done", groups["final_verdicts"].get("not_done", 0), OBSERVED, "executions.verdict"),
        figure("approval-gate stops", approvals, OBSERVED, "interventions kind approval"),
        figure("gate-signed query notes", gate_signed, OBSERVED, "query_notes.gate_request",
               "the Queries rail's record of the same stops: not added to the interventions"),
        figure("done, never checked", done_unchecked, OBSERVED, "executions status done, checked not 1"),
        figure("false-done candidates: a person said wrong", person_wrong, MODELED, "verdict done + tasks.verdict wrong",
               "a lead by rule, not a graded false done"),
        figure("false-done candidates: run again after done", retried, MODELED, "verdict done + a later attempt",
               "a retry can have other causes (a person asked for more)"),
    ]
    if graded:
        figures += [figure("eval case runs (valid)", graded["case_runs"], OBSERVED, "eval_rows"),
                    figure("eval false done (graded)", graded["false_done"], OBSERVED, "eval_rows.false_done",
                           "graded by the eval's own graders; never added to the candidates")]
    caveats = [
        "Verdicts are the desktop's final check as stored; an image from before verdicts sent only checked.",
        "Candidates are MODELED leads (a rule over records), never graded; the eval's false done is the graded figure.",
        "An approval stop is the gate holding an action for a person: a stop, not a failure.",
    ]
    population = {"origin": origin or "all", "executions": len(execs)}
    return common.lane_doc(LANE, source, population, figures, groups, caveats)
