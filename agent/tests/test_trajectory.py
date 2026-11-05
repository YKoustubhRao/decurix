"""
decurix.trajectory/v1's Python validator (decurix_agent/trajectory.py) against the documents the control plane's
runs.Build makes from its fixtures (internal/runs/testdata, written by its golden tests).
"""

import copy
import json
import unittest
from pathlib import Path

from decurix_agent import trajectory

TESTDATA = Path(__file__).resolve().parents[2] / "internal" / "runs" / "testdata"
EXAMPLES = ("hosted.json", "local.json", "stopped.json")


def load(name):
    return json.loads((TESTDATA / name).read_text(encoding="utf-8"))


class TrajectoryExamples(unittest.TestCase):
    def setUp(self):
        missing = [n for n in EXAMPLES if not (TESTDATA / n).exists()]
        if missing:
            self.skipTest(f"no {', '.join(missing)} under {TESTDATA} (go test ./internal/runs/ -run Golden -update)")

    def test_every_example_validates(self):
        for name in EXAMPLES:
            with self.subTest(name):
                self.assertEqual(trajectory.validate(load(name)), [])

    def test_a_broken_copy_is_said_wrong(self):
        doc = load("hosted.json")
        no_run = copy.deepcopy(doc)
        del no_run["run_id"]
        self.assertIn("run_id is missing", trajectory.validate(no_run))

        bad_label = copy.deepcopy(doc)
        bad_label["steps"][0]["label"] = "fine"
        self.assertTrue(any("label 'fine'" in p for p in trajectory.validate(bad_label)))

        bad_gate = copy.deepcopy(doc)
        bad_gate["steps"][0]["gate"]["decision"] = "maybe"
        self.assertTrue(any("gate decision 'maybe'" in p for p in trajectory.validate(bad_gate)))

        bad_origin = copy.deepcopy(doc)
        bad_origin["origin"]["kind"] = "somewhere"
        self.assertTrue(any("origin kind" in p for p in trajectory.validate(bad_origin)))

        bad_verdict = copy.deepcopy(doc)
        bad_verdict["verdict"]["verdict"] = "great"
        self.assertTrue(any(p.startswith("verdict:") for p in trajectory.validate(bad_verdict)))

        bad_run = copy.deepcopy(doc)
        bad_run["labels"]["run"] = "great"
        self.assertTrue(any("labels run 'great'" in p for p in trajectory.validate(bad_run)))

        bad_call = copy.deepcopy(doc)
        bad_call["steps"][0]["call"] = 99
        self.assertTrue(any("does not point into calls" in p for p in trajectory.validate(bad_call)))

    def test_step_labels_are_the_steps_own(self):
        self.assertEqual(load("hosted.json")["labels"]["run"], "done")
        self.assertEqual(load("stopped.json")["labels"]["run"], "stopped")
        doc = load("local.json")
        for label in ("failed", "refused"):
            doc["steps"][0]["label"] = label
            self.assertEqual(trajectory.validate(doc), [])

    def test_unknown_keys_pass(self):
        doc = load("local.json")
        doc["a_v1_1_field"] = {"anything": True}
        doc["steps"][0]["another"] = 1
        self.assertEqual(trajectory.validate(doc), [])

    def test_an_old_run_has_a_null_verdict(self):
        doc = load("local.json")
        doc["verdict"] = None
        self.assertEqual(trajectory.validate(doc), [])
        del doc["verdict"]
        self.assertTrue(any("verdict is missing" in p for p in trajectory.validate(doc)))


class TrajectoryShapes(unittest.TestCase):
    def test_not_an_object(self):
        self.assertEqual(trajectory.validate([]), ["a trajectory is a JSON object"])

    def test_a_count_is_never_a_bool(self):
        self.assertFalse(trajectory._is(True, int))
        self.assertTrue(trajectory._is(3, int))


if __name__ == "__main__":
    unittest.main()
