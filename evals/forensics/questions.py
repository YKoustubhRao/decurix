"""Lane: questions per run. Interventions (the questions and approvals each execution put to a person) per
execution, by kind and by kind of work; the Queries rail's notes; questions asked again on the same worker; and,
when the database holds eval rows, the graded questions per eval case run. Each source is OBSERVED and kept apart.

No question's text is written out: a repeated question is shown by a short hash and its length only."""

import hashlib
import re

from . import common
from .common import OBSERVED, figure, median
from .tokens import kind_of

LANE = "questions"


def norm(text):
    return re.sub(r"\s+", " ", (text or "").strip().lower())


def run(source, origin=None):
    execs = source.executions(origin)
    ids = {e["id"] for e in execs}
    inter = [r for r in source.rows("SELECT * FROM interventions")] if source.has("interventions") else []
    inter = [r for r in inter if r["execution_id"] in ids]
    per_exec = {e["id"]: {"query": 0, "approval": 0, "secret": 0} for e in execs}
    for r in inter:
        c = per_exec[r["execution_id"]]
        c[r["kind"] if r["kind"] in ("query", "approval") else "query"] += 1
        c["secret"] += 1 if r.get("secret") else 0
    groups = {"by_kind": {}, "query_notes": {}, "eval": {}}
    for e in execs:
        k = groups["by_kind"].setdefault(kind_of(e), {"runs": 0, "questions": 0, "approvals": 0})
        k["runs"] += 1
        k["questions"] += per_exec[e["id"]]["query"]
        k["approvals"] += per_exec[e["id"]]["approval"]
    for k in groups["by_kind"].values():
        k["questions_per_run"] = round(k["questions"] / k["runs"], 3) if k["runs"] else 0
        k["label"] = OBSERVED

    # The same question (whitespace and case aside) asked on the same worker in more than one execution.
    by_worker_q = {}
    worker_of = {e["id"]: e["worker_id"] for e in execs}
    for r in inter:
        if r["kind"] != "query":
            continue
        key = (worker_of[r["execution_id"]], norm(r["request"]))
        by_worker_q.setdefault(key, set()).add(r["execution_id"])
    repeated = [{"sha": hashlib.sha256(q.encode()).hexdigest()[:12], "chars": len(q), "executions": len(ex)}
                for (_, q), ex in by_worker_q.items() if len(ex) > 1]
    repeated.sort(key=lambda r: -r["executions"])
    groups["asked_again"] = repeated[:50]

    if source.has("query_notes"):
        for r in source.rows("SELECT kind, resolved_ns, gate_request FROM query_notes"):
            g = groups["query_notes"].setdefault(r["kind"] or "(none)", {"open": 0, "resolved": 0, "gate_signed": 0, "label": OBSERVED})
            g["resolved" if r["resolved_ns"] else "open"] += 1
            g["gate_signed"] += 1 if r["gate_request"] else 0

    if source.has("eval_rows"):
        valid = " WHERE invalid = 0 AND infra = 0"
        for r in source.rows("SELECT run_id, arm, COUNT(*) AS n, AVG(q_required) AS req, AVG(q_other) AS other"
                             + (", AVG(q_declined) AS declined" if source.has("eval_rows", "q_declined") else "")
                             + " FROM eval_rows" + valid + " GROUP BY run_id, arm"):
            groups["eval"][f"{r['run_id']}/{r['arm']}"] = {"case_runs": r["n"], "required_per_run": round(r["req"] or 0, 3),
                                                          "other_per_run": round(r["other"] or 0, 3),
                                                          "declined_per_run": round(r.get("declined") or 0, 3), "label": OBSERVED}

    qs = [c["query"] for c in per_exec.values()]
    figures = [
        figure("executions", len(execs), OBSERVED, "executions"),
        figure("questions per execution (mean)", round(sum(qs) / len(qs), 3) if qs else 0, OBSERVED, "interventions kind query"),
        figure("questions per execution (median)", median(qs), OBSERVED, "interventions kind query"),
        figure("approvals per execution (mean)", round(sum(c["approval"] for c in per_exec.values()) / len(qs), 3) if qs else 0,
               OBSERVED, "interventions kind approval"),
        figure("secret questions", sum(c["secret"] for c in per_exec.values()), OBSERVED, "interventions.secret"),
        figure("questions asked again on the same worker", len(repeated), OBSERVED, "interventions, text normalised",
               "the same words; a question reworded is not caught"),
    ]
    caveats = [
        "Interventions are the questions the control plane recorded; a question the worker answered itself is not one.",
        "Query notes are the Queries rail's records, another source: never added to the interventions.",
        "Eval figures are graded by the eval (required or other by the case's facts) and cover valid rows only.",
        "No question text is written: repeated questions are shown by hash and length.",
    ]
    return common.lane_doc(LANE, source, {"origin": origin or "all", "executions": len(execs)}, figures, groups, caveats)
