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

  live    follows one eval run and appends each case's row to a JSON list the moment the case lands (a curve page
          reads the file while the runs go on); it ends when the run does

    py -3 -I learning_curve.py live --cp http://localhost:28080 --run RUN_ID --workflow gstr1-chat --arm taught \
        --build ab3b380f --cases oc-5-january,oc-6-february,... --out curves/live.json \
        [--skill-cmd "wsl -d Decurix -- docker exec DESKTOP cat SKILL.json"] [--pilot] [--once]

Each row: workflow, n (the case's place in --cases, 1 first), case, build, arm, run, task, correct (the graders
passed), false_done, minutes, tokens in / cached / out, model_turns (operator.turn calls), q_required, q_other,
q_declined (gate approvals the case does not list, which the scripted expert declined; None on a row stored before it), questions, sendbacks (the final check's "Not done yet" send-backs), replayed_steps (the taught replay's last step
reached), model_steps (commands the model ran), trust_stage and verdict_counted (from the skill's own experience
record, which --skill-cmd prints; None without it), finished_at. A row is written once (run, case, repeat), by a
write to a temporary file then a rename, under a lock file, so two followers may share one file.
"""

import argparse
import collections
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

TIMEOUT = 240
# The skill's trust (agent/decurix_agent/computer/experience.py): a verdict counts when it succeeded, needed no person
# and an independent check passed; TRUST_AFTER such runs in a row make the skill trusted.
INDEPENDENT = {"code", "ledger", "person"}
TRUST_AFTER = 3
TERMINAL = {"done", "failed", "cancelled", "canceled", "error"}


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
                "q_declined": r.get("q_declined"),
                "questions": [(q.get("question") or "")[:90] for q in r.get("questions") or []],
                "passed": r.get("passed"), "false_done": r.get("false_done"), "observed": r.get("observed"),
                "failed_graders": [g["id"] for g in graders if g.get("result") == "fail"],
            })
    return out


def steps_of(cp, task_id):
    """The text of every step the task's executions kept, one per line ("" when none could be read)."""

    if not task_id:
        return ""
    try:
        detail = get(cp, "/api/tasks/get?id=" + urllib.parse.quote(task_id))
    except Exception:  # noqa: BLE001 - a task not read shows no steps
        return ""
    return "\n".join(s.get("text") or "" for x in detail.get("executions") or [] for s in x.get("steps") or [])


def step_counts(text):
    """(send-backs, replayed steps, model steps) from a task's steps: the final check's "Not done yet" lines, the
    highest "Replay step N/M" reached, and the commands run outside the replay ("Running: " lines)."""

    lines = text.splitlines()
    sendbacks = sum(1 for line in lines if line.startswith("Not done yet:"))
    replayed = max([int(a) for a in re.findall(r"Replay step (\d+)/\d+", text)] or [0])
    model = sum(1 for line in lines if line.startswith("Running: ") and not re.match(r"Running: gui replay ", line))
    return sendbacks, replayed, model


def verdict_counts(verdict):
    """(counts, why not) for one experience verdict, as experience.counts decides it."""

    if not verdict.get("ok"):
        return False, "not a checked success: " + (verdict.get("why") or "")[:120]
    if verdict.get("helped"):
        return False, "helped (a person stepped in, a takeover, or a save after a send-back)"
    if not INDEPENDENT & set(verdict.get("basis") or ()):
        return False, "no independent check (basis: %s)" % (", ".join(verdict.get("basis") or ()) or "none")
    return True, ""


def trust_of(skill, since):
    """(stage, counted, why) after a run that began at `since` (epoch seconds), from the skill's own JSON."""

    verdicts = (skill.get("experience") or {}).get("verdicts") or []
    streak = 0
    for v in reversed(verdicts):
        if not verdict_counts(v)[0]:
            break
        streak += 1
    stage = "trusted" if streak >= TRUST_AFTER else "apprentice"
    mine = [v for v in verdicts if (v.get("at") or 0) >= since]
    if not mine:
        return stage, False, "no verdict recorded for this run"
    counted, why = verdict_counts(mine[-1])
    return stage, counted, why


def read_skill(cmd):
    """The skill JSON the --skill-cmd prints (an object, or the first of a list), or None."""

    if not cmd:
        return None
    data = None
    for attempt in range(2):  # (a read while the practice desktop resets between carried cases fails once)
        try:
            data = json.loads(subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=120).stdout)
            break
        except Exception:  # noqa: BLE001 - trust unknown is shown as unknown
            if attempt == 0:
                time.sleep(15)
    if data is None:
        return None
    if isinstance(data, list):
        data = data[0] if data else None
    return data if isinstance(data, dict) else None


def _at_epoch(stamp):
    import datetime
    try:
        return datetime.datetime.fromisoformat(str(stamp).replace("Z", "+00:00")).timestamp()
    except ValueError:
        return 0.0


def live_row(cp, r, a, order, skill):
    """One curve row for an eval row `r` of the run `a.run`."""

    by, tot = calls_of(cp, r.get("task_id") or "") if r.get("task_id") else ({}, None)
    sendbacks, replayed, model = step_counts(steps_of(cp, r.get("task_id") or ""))
    finished = _at_epoch(r.get("at"))
    began = finished - 60 * (r.get("minutes") or 0) - 60  # (a minute's slack for the row being written after the task)
    stage, counted, why = trust_of(skill, began) if skill else (None, None, "")
    case = r.get("case_id")
    graders = (r.get("checks") or {}).get("graders") or []
    row = {
        "workflow": a.workflow, "n": order.index(case) + 1 if case in order else None, "case": case,
        "build": a.build, "arm": a.arm, "run": a.run, "repeat": r.get("repeat"), "task": r.get("task_id"),
        "correct": bool(r.get("passed")), "false_done": bool(r.get("false_done")), "observed": r.get("observed"),
        # The work in the application, apart from how the run ended: no grader of the case failed and one passed (a
        # grader that did not apply - "not_made", a question the run never needed - is neither). A run can do the work
        # right and still end not done, when its final check holds something it cannot back.
        "graders_pass": any(g.get("result") == "pass" for g in graders) and not any(g.get("result") == "fail" for g in graders),
        "minutes": round(r.get("minutes") or 0, 1),
        "tokens_in": r.get("tokens_in"), "tokens_cached": r.get("tokens_cached"), "tokens_out": r.get("tokens_out"),
        "model_turns": (by.get("operator.turn") or [0])[0], "calls": tot[0] if tot else r.get("calls"),
        "q_required": r.get("q_required"), "q_other": r.get("q_other"), "q_declined": r.get("q_declined"),
        "questions": [(q.get("question") or "")[:160] for q in r.get("questions") or []],
        "sendbacks": sendbacks, "replayed_steps": replayed, "model_steps": model,
        "trust_stage": stage, "verdict_counted": counted, "verdict_why": why,
        "failed_graders": [g["id"] for g in graders if g.get("result") == "fail"],
        "invalid": bool(r.get("invalid") or r.get("infra")), "finished_at": r.get("at"),
    }
    if a.pilot:
        row["pilot"] = True
    if getattr(a, "freeze", ""):
        row["freeze"] = a.freeze  # (which frozen build's curve this row belongs to, for the page to group)
    return row


def _key(row):
    return "%s|%s|%s" % (row.get("run"), row.get("case"), row.get("repeat"))


class _Lock:
    """An exclusive lock file beside the curve file; a lock older than `stale` seconds is taken for a dead holder's."""

    def __init__(self, path, stale=120):
        self.path, self.stale = str(path) + ".lock", stale

    def __enter__(self):
        deadline = time.time() + 2 * self.stale
        while True:
            try:
                os.close(os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY))
                return self
            except FileExistsError:
                try:
                    if time.time() - os.path.getmtime(self.path) > self.stale:
                        os.remove(self.path)
                        continue
                except OSError:
                    continue
                if time.time() > deadline:
                    raise TimeoutError("the curve file stays locked: " + self.path)
                time.sleep(0.2)

    def __exit__(self, *exc):
        try:
            os.remove(self.path)
        except OSError:
            pass


def append_rows(path, rows):
    """Add the rows not yet in the JSON list at `path` (by run, case, repeat); returns how many were added. The file is
    always a whole JSON list: written to a temporary file in its folder, then renamed over it."""

    path = Path(path)
    with _Lock(path):
        try:
            have = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            have = []
        if not isinstance(have, list):
            have = []
        seen = {_key(r) for r in have}
        new = [r for r in rows if _key(r) not in seen]
        if not new:
            return 0
        tmp = path.with_name(path.name + ".%d.tmp" % os.getpid())
        tmp.write_text(json.dumps(have + new, indent=1), encoding="utf-8")
        os.replace(tmp, path)
        return len(new)


def follow(a, sleep=time.sleep):
    """Append each finished case of run `a.run` as it lands; return when the run ends (or after one pass, --once)."""

    order = [c for c in (a.cases or "").split(",") if c]
    while True:
        full = get(a.cp, "/api/evals/runs/" + a.run).get("run") or {}
        rows = get(a.cp, "/api/evals/runs/%s/rows" % a.run).get("rows") or []
        try:
            have = {_key(r) for r in json.loads(Path(a.out).read_text(encoding="utf-8"))}
        except (OSError, ValueError, TypeError):
            have = set()
        fresh = [r for r in rows if "%s|%s|%s" % (a.run, r.get("case_id"), r.get("repeat")) not in have]
        if fresh:
            skill = read_skill(a.skill_cmd)
            added = append_rows(a.out, [live_row(a.cp, r, a, order, skill) for r in fresh])
            if added:
                print(f"{added} row(s) from {a.run} -> {a.out}", flush=True)
        if a.once or str(full.get("status") or "").lower() in TERMINAL:
            return
        sleep(a.poll)


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
             "| run | arm | starts from | state | case | rep | min | tokens total | in (cached) | turns | q (req/other/declined) | result | failed graders |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        res = "FALSE DONE" if r["false_done"] else ("PASS" if r["passed"] else "fail (%s)" % r["observed"])
        total = (r["tokens_in"] or 0) + (r["tokens_out"] or 0)
        lines.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s (%s) | %s | %s/%s/%s | %s | %s |" % (
            r["run"][:9], r["arm"], r["from_worker"] or "empty desktop", r["state"], r["case"], r["repeat"], r["minutes"],
            fmt_tok(total), fmt_tok(r["tokens_in"]), fmt_tok(r["tokens_cached"]), r["operator_turns"], r["q_required"], r["q_other"],
            r.get("q_declined") or 0, res,
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
    v = sub.add_parser("live")
    v.add_argument("--cp", required=True)
    v.add_argument("--run", required=True)
    v.add_argument("--workflow", required=True)
    v.add_argument("--arm", required=True, choices=["taught", "untaught"])
    v.add_argument("--build", required=True)
    v.add_argument("--cases", default="", help="the run's case ids in curve order (n = place in this list)")
    v.add_argument("--skill-cmd", default="", help="a command printing the taught skill's JSON, for its trust")
    v.add_argument("--pilot", action="store_true")
    v.add_argument("--freeze", default="", help="the curve's freeze label (e.g. freeze-2), kept on each row")
    v.add_argument("--once", action="store_true", help="one pass, then return (backfill)")
    v.add_argument("--poll", type=float, default=60)
    v.add_argument("--out", required=True, help="the curve's JSON list (created when missing)")
    a = p.parse_args(argv)
    if a.cmd == "live":
        follow(a)
        return
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
