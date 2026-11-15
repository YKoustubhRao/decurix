"""Tests of the forensics lanes (evals/forensics) against a tiny synthetic decurix.db built here from the control
plane's real schema: every Migrate step of the components the lanes read, taken from the Go source as written (raw
string literals in order), so a renamed column fails here first. No Go is run or imported."""

import json
import re
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "evals"))

from forensics import common, gates, halts, questions, rework, tokens  # noqa: E402
from forensics.lanes import main as lanes_main  # noqa: E402

# The components whose tables the lanes read, and the file holding each one's Migrate slice.
COMPONENTS = ["internal/tasks/store.go", "internal/llmrec/store.go", "internal/runs/store.go",
              "internal/querynotes/store.go", "internal/eval/store.go"]


def migrate_steps(go_file):
    """The Migrate steps of a component, in order: the raw string literals of its `var schema = []string{...}`, comment
    lines skipped (a comment may quote code in backticks)."""

    text = (ROOT / go_file).read_text(encoding="utf-8")
    start = text.index("var schema = []string{")
    body = []
    for line in text[start:].splitlines()[1:]:
        if line == "}":
            break
        if line.strip().startswith("//"):
            continue
        body.append(line)
    steps = re.findall(r"`([^`]*)`", "\n".join(body))
    if not steps:
        raise AssertionError(f"no Migrate steps found in {go_file}")
    return steps


NS = 10**9


def build_db(path):
    db = sqlite3.connect(path)
    for f in COMPONENTS:
        for step in migrate_steps(f):
            db.executescript(step)
    t0 = 1_700_000_000 * NS

    def task(tid, worker, request, task_type="computer", verdict=""):
        db.execute("INSERT INTO tasks (id, worker_id, request, task_type, status, verdict, created_ns, updated_ns) "
                   "VALUES (?,?,?,?,?,?,?,?)", (tid, worker, request, task_type, "done", verdict, t0, t0))

    def execution(eid, tid, attempt, worker, start_s, end_s, checked, verdict=None, skill="", status="done",
                  tin=None, tout=None, origin="person"):
        db.execute("INSERT INTO executions (id, task_id, attempt, worker_id, status, started_ns, ended_ns, checked, verdict, "
                   "skill_name, input_tokens, output_tokens, origin) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                   (eid, tid, attempt, worker, status, t0 + start_s * NS, t0 + end_s * NS, checked,
                    json.dumps({"schema": "decurix.verdict/v1", "verdict": verdict, "by": "final_gate"}) if verdict else "",
                    skill, tin, tout, origin))

    def call(cid, worker, at_s, purpose, tin, cached, tout, task=""):
        db.execute("INSERT INTO llm_calls (id, at_ns, ms, provider, status, desktop_id, task_id, purpose, messages, prefix_sha, "
                   "prompt_sha, input_tokens, cached_input_tokens, output_tokens) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                   (cid, t0 + at_s * NS, 10, "azure", 200, worker, task, purpose, "[]", "p", "q", tin, cached, tout))

    # Worker w1: a skill run (checked, done), then the same task run again; a plain task checked not_done.
    task("t1", "w1", "Enter the bill in the folder.", verdict="wrong")
    execution("e1", "t1", 1, "w1", 0, 100, 1, "done", skill="enter-bill", tin=999, tout=1)
    execution("e2", "t1", 2, "w1", 200, 300, 1, "done", skill="enter-bill")
    task("t2", "w1", "Reconcile the statement.")
    execution("e3", "t2", 1, "w1", 400, 500, 1, "not_done", tin=50, tout=5)
    # The same request again, as a new task, within a day; never checked.
    task("t3", "w1", "  enter the bill in the folder. ")
    execution("e4", "t3", 1, "w1", 600, 700, None)
    # An eval run on another worker.
    task("t4", "w2", "Do it.")
    execution("e5", "t4", 1, "w2", 0, 50, 1, "done", origin="eval")

    call("c1", "w1", 10, "operator.turn", 1000, 800, 50, task="t1")
    call("c2", "w1", 20, "review", 400, 0, 10, task="t1")
    call("c3", "w1", 150, "operator.turn", 99999, 0, 1, task="t1")  # e1's: within a minute of its end, before e2 began
    call("c4", "w1", 250, "operator.turn", 300, 300, 20, task="t9")    # e2's, whatever its header said
    call("c5", "w1", 450, "operator.turn", 200, 100, 10, task="t2")
    call("c6", "w2", 10, "operator.turn", 70, 0, 7, task="t4")
    call("c7", "w9", 10, "operator.turn", 5, 0, 5)                      # another worker's

    db.execute("INSERT INTO interventions (id, execution_id, kind, reason, request, requested_ns) VALUES (?,?,?,?,?,?)",
               ("i1", "e1", "query", "", "Which cost centre?", t0))
    db.execute("INSERT INTO interventions (id, execution_id, kind, reason, request, requested_ns) VALUES (?,?,?,?,?,?)",
               ("i2", "e4", "query", "", "which  cost centre?", t0))
    db.execute("INSERT INTO interventions (id, execution_id, kind, reason, request, requested_ns, secret) VALUES (?,?,?,?,?,?,?)",
               ("i3", "e3", "approval", "", "Approval needed before I click Save", t0, 0))
    db.execute("INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns, kind, gate_request) "
               "VALUES ('n1','w1','w1','f','x',1,'approval','g1')")

    trace = "\n".join(json.dumps(x) for x in [
        {"kind": "call", "purpose": "operator.turn"},
        {"kind": "command", "command": {"text": "gui do enter-bill --from 3",
                                        "output_head": 'halted: Replayed "Enter bill": step 7 of 12 (click "Save") stopped\nTo carry on'}},
        {"kind": "command", "command": {"text": "gui do enter-bill",
                                        "output_head": 'halted: Replayed "Enter bill": step 9 of 12 (click "Post") stopped'}},
        {"kind": "command", "command": {"text": "gui replay s1", "output_head": "verified: Replayed s1: all 4 steps ran"}},
        {"kind": "command", "command": {"text": "ls", "output_head": "halted: not a replay"}},
    ]).encode()
    db.execute("INSERT INTO run_bundles (exec_id, task_id, worker_id, pulled_ns, manifest_sha, total_bytes, files) "
               "VALUES (?,?,?,?,?,?,?)", ("e1", "t1", "c-1", t0, "m", len(trace),
                                          json.dumps([{"path": ".decurix/ai/traces/t1.jsonl", "sha256": "ab" * 32, "bytes": len(trace)}])))
    db.execute("INSERT INTO eval_runs (id, created_ns, suite_name, suite_sha, arms, repeats, status) VALUES "
               "('r1', 1, 's', 'x', '[]', 1, 'done')")
    for arm, attempt, invalid, fd, qr in (("a", 1, 1, 0, 0), ("a", 2, 0, 1, 2), ("b", 1, 0, 0, 1)):
        db.execute("INSERT INTO eval_rows (run_id, arm, repeat, case_id, observed, passed, false_done, done_unchecked_wrong, "
                   "q_required, at_ns, invalid, invalid_why, attempt) VALUES ('r1',?,1,'c','complete',0,?,0,?,1,?,?,?)",
                   (arm, fd, qr, invalid, "no_turn" if invalid else "", attempt))
    db.commit()
    db.close()
    return trace


def fig(doc, name):
    for f in doc["figures"]:
        if f["name"] == name:
            return f
    raise AssertionError(f"no figure {name!r} in {[f['name'] for f in doc['figures']]}")


class ForensicsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.db = Path(cls.tmp.name) / "decurix-copy.db"
        cls.trace = build_db(cls.db)
        # The blob store holds the trace under its (fixture) SHA, as the control plane's blobs/ does.
        blob = Path(cls.tmp.name) / "blobs" / "sha256" / "ab" / ("ab" * 32)
        blob.parent.mkdir(parents=True)
        blob.write_bytes(cls.trace)
        cls.blobs = blob.parents[2]

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def source(self, bundles=None):
        s = common.Source(self.db, bundles)
        self.addCleanup(s.close)
        return s

    def test_the_database_is_opened_read_only(self):
        s = self.source()
        with self.assertRaises(sqlite3.OperationalError):
            s.conn.execute("DELETE FROM tasks")

    def test_observed_and_modeled_are_never_added(self):
        a = common.figure("x", 1, common.OBSERVED, "s")
        b = common.figure("y", 2, common.MODELED, "s")
        with self.assertRaises(ValueError):
            common.add(a, b)
        self.assertEqual(common.add(a, common.figure("z", 3, common.OBSERVED, "s"))["value"], 4)
        with self.assertRaises(ValueError):
            common.figure("w", 1, "GUESSED", "s")

    def test_tokens_per_checked_task_by_kind_and_purpose(self):
        doc = tokens.run(self.source())
        self.assertEqual(fig(doc, "checked executions")["value"], 4)
        # e1 gets c1, c2 and c3 (a minute after its end, before e2 began); e2 gets c4, whatever its header said; e3 gets
        # c5; e5 gets c6; c7 is another worker's.
        e1 = 1000 + 50 + 400 + 10 + 99999 + 1
        e2 = 300 + 20
        self.assertEqual(doc["groups"]["by_kind"]["enter-bill"]["proxy_tokens_total"], e1 + e2)
        self.assertEqual(fig(doc, "proxy calls")["value"], 6)
        purpose = doc["groups"]["by_purpose"]
        self.assertEqual(purpose["review"]["calls"], 1)
        self.assertEqual(purpose["operator.turn"]["cached"], 800 + 300 + 100)
        # The desktop's own report is kept apart, never added to the proxy's figure.
        self.assertEqual(fig(doc, "desktop-reported tokens, all checked executions")["value"], 999 + 1 + 55)
        self.assertTrue(all(f["label"] == common.OBSERVED for f in doc["figures"]))
        self.assertTrue(doc["caveats"])
        # One origin only.
        self.assertEqual(fig(tokens.run(self.source(), "eval"), "checked executions")["value"], 1)

    def test_questions_per_run(self):
        doc = questions.run(self.source())
        self.assertEqual(fig(doc, "questions asked again on the same worker")["value"], 1)
        self.assertNotIn("cost centre", json.dumps(doc), "a question's text is written out")
        self.assertEqual(doc["groups"]["query_notes"]["approval"]["gate_signed"], 1)
        self.assertEqual(doc["groups"]["eval"]["r1/a"]["required_per_run"], 2)  # the invalid row is left out

    def test_gate_stops_and_false_done_candidates(self):
        doc = gates.run(self.source())
        self.assertEqual(doc["groups"]["final_verdicts"], {"done": 3, "not_done": 1})
        self.assertEqual(fig(doc, "approval-gate stops")["value"], 1)
        self.assertEqual(fig(doc, "done, never checked")["value"], 1)
        # e1 said done, then a person said wrong and the task ran again; e2 said done and a person said wrong.
        cands = {c["execution"]: c["why"] for c in doc["groups"]["candidates"]}
        self.assertEqual(set(cands), {"e1", "e2"})
        self.assertEqual(fig(doc, "false-done candidates: a person said wrong")["label"], common.MODELED)
        self.assertEqual(fig(doc, "eval false done (graded)")["value"], 1)
        self.assertEqual(fig(doc, "eval false done (graded)")["label"], common.OBSERVED)

    def test_replay_halts_need_the_bundles(self):
        doc = halts.run(self.source())
        self.assertIn("not measured", doc["caveats"][0])
        doc = halts.run(self.source(self.blobs))
        self.assertEqual(doc["groups"]["outcomes"], {"halted": 2, "verified": 1})
        # Two halts at different steps of different controls are one cause.
        self.assertEqual(list(doc["groups"]["halts_by_cause"].values()), [2])
        self.assertEqual(fig(doc, "replays that halted or needed a person")["value"], 2)

    def test_replay_halts_from_per_run_folders(self):
        folder = Path(self.tmp.name) / "runs"
        f = folder / "e1" / ".decurix" / "ai" / "traces" / "t1.jsonl"
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_bytes(self.trace)
        doc = halts.run(self.source(folder))
        self.assertEqual(fig(doc, "replays seen")["value"], 3)

    def test_rework(self):
        doc = rework.run(self.source())
        self.assertEqual(fig(doc, "tasks run more than once")["value"], 1)
        self.assertEqual(fig(doc, "proxy tokens of retries")["value"], 300 + 20)
        self.assertEqual(fig(doc, "same request sent again within a day")["value"], 1)
        self.assertEqual(fig(doc, "retry overhead per task (median, more-than-once minus once)")["label"], common.MODELED)
        self.assertEqual(doc["groups"]["eval_reruns"]["r1/a"]["reruns"], 1)

    def test_every_lane_writes_its_own_json(self):
        out = Path(self.tmp.name) / "out"
        lanes_main(["--db", str(self.db), "--bundles", str(self.blobs), "--out", str(out)])
        for lane in ("tokens", "questions", "gates", "halts", "rework"):
            doc = json.loads((out / f"{lane}.json").read_text(encoding="utf-8"))
            self.assertEqual((doc["schema"], doc["lane"]), ("decurix.forensics/v1", lane))
            self.assertTrue(doc["caveats"])
            for f in doc["figures"]:
                self.assertIn(f["label"], ("OBSERVED", "MODELED"))


if __name__ == "__main__":
    unittest.main()
