"""Shared pieces of the forensics lanes: the read-only database, the population of executions, the join of model calls
to an execution (as the run record joins them, docs/trajectory.md), bundle files, and labelled figures.

Every figure is OBSERVED (counted from rows the control plane or the desktop kept) or MODELED (a reconstruction or an
estimate by a rule). The two are never added together: `add` refuses it. Each lane keeps its own caveats."""

import json
import sqlite3
import statistics
from pathlib import Path

OBSERVED = "OBSERVED"
MODELED = "MODELED"
SCHEMA = "decurix.forensics/v1"

# A call belongs to an execution when it was made by the execution's worker from 2 s before its start to a minute after
# its end, never into the worker's next execution (docs/trajectory.md: the task id in a header is the desktop's word).
BEFORE_NS = 2 * 10**9
AFTER_NS = 60 * 10**9


def figure(name, value, label, source, caveat=""):
    """One number a lane reports: what it is, the value, OBSERVED or MODELED, where it came from, and its caveat."""

    if label not in (OBSERVED, MODELED):
        raise ValueError(f"a figure is {OBSERVED} or {MODELED}, not {label!r}")
    out = {"name": name, "value": value, "label": label, "source": source}
    if caveat:
        out["caveat"] = caveat
    return out


def add(a, b):
    """The sum of two figures of the same label; an OBSERVED figure and a MODELED one are never added."""

    if a["label"] != b["label"]:
        raise ValueError(f"{a['name']} ({a['label']}) and {b['name']} ({b['label']}) are not added: one is modeled")
    return figure(f"{a['name']} + {b['name']}", a["value"] + b["value"], a["label"], f"{a['source']}; {b['source']}")


def median(values):
    values = list(values)
    return statistics.median(values) if values else 0


def share(part, whole):
    return round(part / whole, 4) if whole else 0.0


class Source:
    """A copy of decurix.db opened read-only (never the live file), and an optional bundles directory: the control
    plane's blob store (a folder holding sha256/) or one folder per execution id holding the bundle's files by path."""

    def __init__(self, db, bundles=None):
        path = Path(db).resolve()
        if not path.is_file():
            raise FileNotFoundError(f"no database at {path}")
        self.db_name = path.name
        self.conn = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)
        self.conn.row_factory = sqlite3.Row
        self.bundles = Path(bundles).resolve() if bundles else None

    def close(self):
        self.conn.close()

    def has(self, table, column=None):
        row = self.conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (table,)).fetchone()
        if not row:
            return False
        if column is None:
            return True
        return any(c["name"] == column for c in self.conn.execute(f"PRAGMA table_info({table})"))

    def rows(self, sql, args=()):
        return [dict(r) for r in self.conn.execute(sql, args)]

    def executions(self, origin=None):
        """Every execution with its task's type, request and person's verdict, oldest first; `origin` limits it to one
        origin (person, eval, practice, schedule, ...)."""

        cols = "e.*, t.task_type, t.request, t.verdict AS person_verdict, t.created_ns AS task_created_ns"
        sql = f"SELECT {cols} FROM executions e JOIN tasks t ON t.id = e.task_id"
        args = ()
        if origin:
            sql += " WHERE e.origin = ?"
            args = (origin,)
        return self.rows(sql + " ORDER BY e.started_ns, e.id", args)

    def calls_by_execution(self, executions):
        """{execution id: [llm_calls rows]} by the run record's join: the execution's worker, its window, never past the
        worker's next execution's start."""

        if not self.has("llm_calls"):
            return {}
        by_worker = {}
        for e in executions:
            by_worker.setdefault(e["worker_id"], []).append(e)
        out = {}
        for worker, runs in by_worker.items():
            runs.sort(key=lambda e: e["started_ns"])
            calls = self.rows("SELECT * FROM llm_calls WHERE desktop_id = ? ORDER BY at_ns", (worker,))
            for i, e in enumerate(runs):
                lo = e["started_ns"] - BEFORE_NS
                hi = (e["ended_ns"] + AFTER_NS) if e["ended_ns"] else None
                nxt = runs[i + 1]["started_ns"] if i + 1 < len(runs) else None
                if nxt is not None and (hi is None or nxt < hi):
                    hi = nxt
                out[e["id"]] = [c for c in calls if c["at_ns"] >= lo and (hi is None or c["at_ns"] < hi)]
        return out

    def bundle_files(self, exec_id):
        """{path: sha256} of an execution's pulled bundle (run_bundles), {} when none was pulled."""

        if not self.has("run_bundles"):
            return {}
        row = self.conn.execute("SELECT files FROM run_bundles WHERE exec_id = ?", (exec_id,)).fetchone()
        if not row:
            return {}
        try:
            return {f["path"]: f.get("sha256", "") for f in json.loads(row["files"] or "[]")}
        except (ValueError, TypeError, AttributeError):
            return {}

    def read_bundle_file(self, exec_id, path, sha=""):
        """The bytes of one bundle file, from the blob store by its SHA or from <bundles>/<exec id>/<path>; None when
        no bundles directory was given or the file is not there."""

        if self.bundles is None:
            return None
        if sha and (self.bundles / "sha256").is_dir():
            blob = self.bundles / "sha256" / sha[:2] / sha
            return blob.read_bytes() if blob.is_file() else None
        f = self.bundles / exec_id / Path(path)
        try:
            f.resolve().relative_to(self.bundles)
        except ValueError:
            return None
        return f.read_bytes() if f.is_file() else None


def lane_doc(lane, source, population, figures, groups, caveats):
    """A lane's JSON document: its figures, its groups, and its own caveats."""

    for f in figures:
        if f["label"] not in (OBSERVED, MODELED):
            raise ValueError(f"unlabelled figure {f['name']}")
    return {"schema": SCHEMA, "lane": lane, "db": source.db_name, "population": population,
            "figures": figures, "groups": groups, "caveats": caveats,
            "rule": "OBSERVED and MODELED figures are never added together."}


def write(out_dir, lane, doc):
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{lane}.json"
    path.write_text(json.dumps(doc, indent=1, sort_keys=False), encoding="utf-8")
    return path
