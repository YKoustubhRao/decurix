"""
learning_curve.py - what each run of a taught task cost, in the order the runs happened.

A worker taught a task (a recording, the lesson's questions, practice) should get cheaper with every later run of the
same kind on new inputs. This reads what the control plane kept and prints one row per run, oldest first: when it
started, minutes, tokens (in / from the cache / out), model calls by purpose, the questions put to the person, the
route the run took (the taught replay, the model, or both) and how it ended. Plus one row for the teaching itself.

Two sources:
  tasks   a worker's tasks on a control plane, in time order (a worker doing the task for real)
  evals   eval runs (their rows: the graders' verdict per case), in run and case order

    py -3 -I learning_curve.py tasks --cp http://localhost:18080 --worker erpbills --match "purchase invoice" --out DIR
    py -3 -I learning_curve.py evals --cp http://localhost:28080 --suite SUITE_NAME --out DIR

It reads only; it changes nothing. Outcome for tasks is the control plane's own (done checked / done unchecked /
asked / failed); an outside audit (a ledger read back from the application) can be merged with --audit FILE.json
({task_id: "correct" | "wrong" | "refused" | ...}).
"""

import argparse
import collections
import json
import sys
import urllib.parse
import urllib.request
from pathlib import Path

TIMEOUT = 240


def get(cp, path):
    with urllib.request.urlopen(cp.rstrip("/") + path, timeout=TIMEOUT) as r:
        return json.load(r)


def calls_of(cp, task_id):
    """{purpose: [calls, in, cached, out]} and totals, from the control plane's call record."""

    try:
        calls = get(cp, "/api/llm/calls?task=" + urllib.parse.quote(task_id)).get("calls") or []
    except Exception:  # noqa: BLE001 - a run with no record is shown as such
        return {}, None
    by = collections.defaultdict(lambda: [0, 0, 0, 0])
    for c in calls:
        b = by[c.get("purpose") or "?"]
        b[0] += 1
        b[1] += c.get("input_tokens") or 0
        b[2] += c.get("cached_input_tokens") or 0
        b[3] += c.get("output_tokens") or 0
    tot = [sum(v[i] for v in by.values()) for i in range(4)]
    return dict(by), tot


def route_of(steps_text, by):
    """How the run went: the taught replay (its "Replay step N/M" lines), the model (operator turns), or both."""

    import re
    done = [(int(a), int(b)) for a, b in re.findall(r"Replay step (\d+)/(\d+)", steps_text)]
    gui_do = len(re.findall(r"Running: gui (?:do|replay) ", steps_text))
    turns = (by.get("operator.turn") or [0])[0]
    declared = "Declared the record" in steps_text
    parts = []
    if done:
        last, total = max(done)
        parts.append("code replay %d/%d steps" % (last, total))
    if gui_do:
        parts.append("model ran gui do x%d" % gui_do)
    if turns:
        parts.append("model %d turns" % turns)
    route = " + ".join(parts) or "no screen work"
    return route + ("; record declared" if declared else "")


def task_rows(cp, worker, match, audit):
    tasks = get(cp, "/api/tasks?limit=500")
    mine = [t for t in tasks if t.get("worker_id") == worker and (not match or match.lower() in (t.get("request") or "").lower())]
    mine.sort(key=lambda t: t.get("created_at") or "")
    rows = []
    for n, t in enumerate(mine, 1):
        detail = get(cp, "/api/tasks/get?id=" + t["id"])
        steps = [s.get("text") or "" for x in detail.get("executions") or [] for s in x.get("steps") or []]
        text = "\n".join(steps)
        by, tot = calls_of(cp, t["id"])
        rows.append({
            "n": n, "task": t["id"], "input": (t.get("request") or "").split("/")[-1][:60],
            "started": t.get("created_at"), "minutes": round((t.get("duration_ns") or 0) / 6e10, 1),
            "tokens_in": tot[1] if tot else t.get("tokens"), "tokens_cached": tot[2] if tot else None,
            "tokens_out": tot[3] if tot else None, "calls": tot[0] if tot else None,
            "operator_turns": (by.get("operator.turn") or [0])[0],
            "by_purpose": {k: {"calls": v[0], "in": v[1]} for k, v in sorted(by.items(), key=lambda kv: -kv[1][1])},
            "questions": list(t.get("stepped_in") or []), "route": route_of(text, by),
            "outcome": audit.get(t["id"]) or (t.get("outcome") or {}).get("label") or t.get("status"),
        })
    return rows


def eval_rows(cp, suite):
    runs = get(cp, "/api/evals/runs?limit=200")
    runs = runs if isinstance(runs, list) else runs.get("runs") or []
    out = []
    for run in sorted(runs, key=lambda r: r.get("created_at") or ""):
        full = get(cp, "/api/evals/runs/" + run["id"]).get("run") or {}
        if suite and full.get("suite_name") != suite:
            continue
        rows = get(cp, "/api/evals/runs/%s/rows" % run["id"]).get("rows") or []
        for r in rows:
            by, tot = calls_of(cp, r.get("task_id") or "") if r.get("task_id") else ({}, None)
            graders = (r.get("checks") or {}).get("graders") or []
            out.append({
                "run": run["id"], "status": full.get("status"), "state": "carried" if full.get("carry_state") else "snapshot",
                "from_worker": full.get("from_worker") or "", "tree": (full.get("arms") or [{}])[0].get("agent_tree_sha", "")[:10],
                "arm": r.get("arm"), "repeat": r.get("repeat"), "case": r.get("case_id"), "minutes": round(r.get("minutes") or 0, 1),
                "tokens_in": r.get("tokens_in"), "tokens_cached": r.get("tokens_cached"), "tokens_out": r.get("tokens_out"),
                "operator_turns": (by.get("operator.turn") or [0])[0], "q_required": r.get("q_required"), "q_other": r.get("q_other"),
                "questions": [(q.get("question") or "")[:90] for q in r.get("questions") or []],
                "passed": r.get("passed"), "false_done": r.get("false_done"), "observed": r.get("observed"),
                "failed_graders": [g["id"] for g in graders if g.get("result") == "fail"],
            })
    return out


def fmt_tok(n):
    if n is None:
        return "-"
    return "%.2fM" % (n / 1e6) if n >= 1e6 else "%.1fk" % (n / 1e3)


def md_tasks(title, rows, notes=()):
    lines = [f"## {title}", ""] + [f"- {n}" for n in notes] + ["",
             "| n | input | started (UTC) | min | tokens total | in (cached) | out | turns | questions | route | outcome |",
             "|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        total = (r["tokens_in"] or 0) + (r["tokens_out"] or 0)
        lines.append("| %s | %s | %s | %s | %s | %s (%s) | %s | %s | %d | %s | %s |" % (
            r["n"], r["input"], (r["started"] or "")[5:16].replace("T", " "), r["minutes"], fmt_tok(total), fmt_tok(r["tokens_in"]),
            fmt_tok(r["tokens_cached"]), fmt_tok(r["tokens_out"]), r["operator_turns"], len(r["questions"]), r["route"], r["outcome"]))
    return "\n".join(lines) + "\n"


def md_evals(title, rows, notes=()):
    lines = [f"## {title}", ""] + [f"- {n}" for n in notes] + ["",
             "| run | arm | starts from | state | case | rep | min | tokens total | in (cached) | turns | q (req/other) | result | failed graders |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        res = "FALSE DONE" if r["false_done"] else ("PASS" if r["passed"] else "fail (%s)" % r["observed"])
        total = (r["tokens_in"] or 0) + (r["tokens_out"] or 0)
        lines.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s (%s) | %s | %s/%s | %s | %s |" % (
            r["run"][:9], r["arm"], r["from_worker"] or "empty desktop", r["state"], r["case"], r["repeat"], r["minutes"],
            fmt_tok(total), fmt_tok(r["tokens_in"]), fmt_tok(r["tokens_cached"]), r["operator_turns"], r["q_required"], r["q_other"], res,
            ", ".join(r["failed_graders"])))
    return "\n".join(lines) + "\n"


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    t = sub.add_parser("tasks")
    t.add_argument("--cp", required=True)
    t.add_argument("--worker", required=True)
    t.add_argument("--match", default="")
    t.add_argument("--audit", default="")
    t.add_argument("--title", default="")
    t.add_argument("--out", required=True)
    e = sub.add_parser("evals")
    e.add_argument("--cp", required=True)
    e.add_argument("--suite", required=True)
    e.add_argument("--title", default="")
    e.add_argument("--out", required=True)
    a = p.parse_args(argv)
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    if a.cmd == "tasks":
        audit = json.load(open(a.audit, encoding="utf-8")) if a.audit else {}
        rows = task_rows(a.cp, a.worker, a.match, audit)
        name = a.title or a.worker
        (out / f"{name}.json").write_text(json.dumps(rows, indent=1), encoding="utf-8")
        (out / f"{name}.md").write_text(md_tasks(name, rows), encoding="utf-8")
    else:
        rows = eval_rows(a.cp, a.suite)
        name = a.title or a.suite
        (out / f"{name}.json").write_text(json.dumps(rows, indent=1), encoding="utf-8")
        (out / f"{name}.md").write_text(md_evals(name, rows), encoding="utf-8")
    print(f"{len(rows)} row(s) -> {out / name}.md")


if __name__ == "__main__":
    sys.stdout.reconfigure(encoding="utf-8")
    main()
