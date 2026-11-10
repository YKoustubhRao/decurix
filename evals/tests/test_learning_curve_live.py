"""learning_curve.py live: each finished case of an eval run becomes one row of the curve's JSON list, once, in a file
that is always a whole JSON list; trust comes from the skill's own verdicts. Canned control-plane answers, no network.
Run: py -3 -m unittest discover -s evals/tests"""

import argparse
import importlib.util
import json
import os
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("learning_curve", os.path.join(os.path.dirname(HERE), "tools", "learning_curve.py"))
lc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lc)

NOW = time.time()
STAMP = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(NOW))
ROW = {"case_id": "oc-6-february", "repeat": 1, "task_id": "t1", "passed": True, "false_done": False, "observed": "complete",
       "minutes": 20.5, "tokens_in": 900000, "tokens_cached": 700000, "tokens_out": 40000, "q_required": 2, "q_other": 1,
       "questions": [{"question": "May I file with EVC?"}], "at": STAMP,
       "checks": {"graders": [{"id": "tn-b2b", "result": "pass"}, {"id": "reports-arn", "result": "fail"}]}}
STEPS = ("Running: gui click 10,20\nReplay step 7/9 done\nNot done yet: the ARN is not in the report. Trying again.\n"
         "Running: gui replay step 8\nRunning: office read x.pdf")


def answers(status, rows):
    def get(cp, path):
        if path.startswith("/api/evals/runs/") and path.endswith("/rows"):
            return {"rows": rows}
        if path.startswith("/api/evals/runs/"):
            return {"run": {"status": status}}
        if path.startswith("/api/llm/calls"):
            return {"calls": [{"purpose": "operator.turn", "input_tokens": 5}, {"purpose": "operator.turn"},
                              {"purpose": "judge"}]}
        if path.startswith("/api/tasks/get"):
            return {"executions": [{"steps": [{"text": t} for t in STEPS.split("\n")]}]}
        raise AssertionError(path)
    return get


def load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def args(out, **kw):
    base = dict(cp="http://cp", run="e1", workflow="gstr1-chat", arm="taught", build="ab3b380f",
                cases="oc-5-january,oc-6-february", skill_cmd="", pilot=False, freeze="freeze-2", once=False, poll=0, out=out)
    base.update(kw)
    return argparse.Namespace(**base)


class TestLiveRows(unittest.TestCase):

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.out = os.path.join(self.dir, "live.json")

    def test_a_finished_case_becomes_one_row_with_the_curve_fields(self):
        with mock.patch.object(lc, "get", answers("done", [ROW])):
            lc.follow(args(self.out))
        rows = load(self.out)
        self.assertEqual(len(rows), 1)
        r = rows[0]
        for key in ("workflow", "n", "case", "build", "arm", "run", "task", "correct", "minutes", "tokens_in",
                    "tokens_cached", "tokens_out", "model_turns", "q_required", "q_other", "sendbacks",
                    "replayed_steps", "model_steps", "trust_stage", "verdict_counted", "finished_at"):
            self.assertIn(key, r)
        self.assertEqual((r["n"], r["case"], r["correct"], r["model_turns"]), (2, "oc-6-february", True, 2))
        self.assertEqual((r["sendbacks"], r["replayed_steps"], r["model_steps"]), (1, 7, 2))
        self.assertEqual(r["failed_graders"], ["reports-arn"])
        self.assertFalse(r["graders_pass"])
        self.assertNotIn("pilot", r)
        self.assertEqual(r["freeze"], "freeze-2")

    def test_a_row_is_written_once_however_often_the_run_is_read(self):
        with mock.patch.object(lc, "get", answers("done", [ROW])):
            lc.follow(args(self.out))
            lc.follow(args(self.out))
        self.assertEqual(len(load(self.out)), 1)

    def test_it_follows_a_running_run_until_it_ends(self):
        seen = []
        states = iter([("running", []), ("running", [ROW]), ("done", [ROW])])

        def get(cp, path):
            return answers(*current)(cp, path)

        current = next(states)

        def sleep(_):
            nonlocal current
            seen.append(1)
            current = next(states)

        with mock.patch.object(lc, "get", get):
            lc.follow(args(self.out), sleep=sleep)
        self.assertEqual(len(seen), 2)
        self.assertEqual(len(load(self.out)), 1)

    def test_the_file_stays_a_whole_list_and_keeps_rows_others_wrote(self):
        with open(self.out, "w", encoding="utf-8") as f:
            json.dump([{"run": "other", "case": "ob-5-february", "repeat": 1}], f)
        added = lc.append_rows(self.out, [{"run": "e1", "case": "oc-5-january", "repeat": 1}])
        self.assertEqual(added, 1)
        self.assertEqual([r["run"] for r in load(self.out)], ["other", "e1"])
        self.assertEqual([n for n in os.listdir(self.dir) if n != "live.json"], [])  # no temp or lock file left

    def test_trust_comes_from_the_skills_verdict_for_this_run(self):
        counted = {"at": int(NOW), "ok": True, "helped": False, "basis": ["ledger", "model"]}
        helped = dict(counted, helped=True)
        skill = {"experience": {"verdicts": [counted, counted, counted]}}
        self.assertEqual(lc.trust_of(skill, NOW - 60), ("trusted", True, ""))
        stage, ok, why = lc.trust_of({"experience": {"verdicts": [counted, helped]}}, NOW - 60)
        self.assertEqual((stage, ok), ("apprentice", False))
        self.assertIn("helped", why)
        model_only = dict(counted, basis=["model"])
        self.assertIn("no independent check", lc.trust_of({"experience": {"verdicts": [model_only]}}, NOW - 60)[2])
        self.assertEqual(lc.trust_of({"experience": {"verdicts": [dict(counted, at=1)]}}, NOW - 60)[1:],
                         (False, "no verdict recorded for this run"))

    def test_pilot_rows_are_marked(self):
        with mock.patch.object(lc, "get", answers("done", [ROW])):
            lc.follow(args(self.out, pilot=True, once=True))
        self.assertTrue(load(self.out)[0]["pilot"])


if __name__ == "__main__":
    unittest.main()
