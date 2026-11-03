"""
The run's own evidence for its bundle (R1 of the plan of 2026-11-03; PLAN-P2 C3, C8, §5.3):
- every recorded action carries soul's turn (DECURIX_TURN_ID, which soul sets on every command it runs), so a step of
  the trajectory joins the model call that chose it;
- an action carries "obs": what it was resolved against, the active window's elements kept in the recording's obs/
  folder by hash - a secret field's value withheld, an unchanged screen stored once, the size capped;
- the run's manifest (runs/<task>/run.json) names the agent's code by a hash that is stable and changes when a file
  does, and lists the parts that exist.
"""

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, state
from decurix_agent.computer import audit, recording

TASK = "t0123456789abcde0"


def observation(password="Hunter22!", extra=0):
    elements = [{"role": "textbox", "name": "User", "value": "asha", "box": [1, 2, 3, 4], "group": "g1"},
                {"role": "password text", "name": "Password", "value": password, "box": [1, 9, 3, 4], "group": "g1"},
                {"role": "button", "name": "Sign in", "states": ["focused"], "group": "g1"},
                {"role": "label", "name": "Another window", "group": "g2"}]
    elements += [{"role": "text", "name": f"row {n} " + "x" * 200, "group": "g1"} for n in range(extra)]
    return {"active_title": "Sign in - Chromium", "page_url": "http://app.example.test/login",
            "groups": [{"id": "g1", "window_title": "Sign in - Chromium"}, {"id": "g2", "window_title": "Other"}],
            "elements": elements}


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for p in (mock.patch.object(recording, "ROOT", self.root / "recordings"),
                  mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(state, "current_task_id", TASK),
                  mock.patch.object(audit.progress, "emit_action", lambda *a, **k: None)):
            p.start()
            self.addCleanup(p.stop)

    def events(self):
        return recording.load(f"task-{TASK}")[1]

    def obs_dir(self):
        return self.root / "recordings" / f"task-{TASK}" / "obs"


class TheTurn(Base):
    def test_set(self):
        with mock.patch.dict(os.environ, {"DECURIX_TURN_ID": "j42:7"}):
            audit.record({"kind": "click", "summary": "click"})
        self.assertEqual(self.events()[-1]["turn"], "j42:7")

    def test_unset_or_malformed(self):
        env = {k: v for k, v in os.environ.items() if k != "DECURIX_TURN_ID"}
        with mock.patch.dict(os.environ, env, clear=True):
            audit.record({"kind": "click", "summary": "click"})
        with mock.patch.dict(os.environ, {"DECURIX_TURN_ID": "j42:7; rm -rf"}):
            audit.record({"kind": "click", "summary": "click"})
        self.assertNotIn("turn", self.events()[0])
        self.assertNotIn("turn", self.events()[1])


class TheObservation(Base):
    def test_kept_with_no_secret_value(self):
        audit.record({"kind": "click", "summary": "click"}, observation=observation())
        name = self.events()[-1]["obs"]
        self.assertRegex(name, r"^[0-9a-f]{16}$")
        blob = (self.obs_dir() / f"{name}.json").read_text(encoding="utf-8")
        self.assertNotIn("Hunter22!", blob)
        doc = json.loads(blob)
        self.assertEqual(doc["window"], "Sign in - Chromium")
        self.assertEqual([e["name"] for e in doc["elements"]], ["User", "Password", "Sign in"])  # the active window's
        self.assertIsNone(doc["elements"][1]["value"])

    def test_an_unchanged_screen_is_stored_once(self):
        audit.record({"kind": "click", "summary": "a"}, observation=observation())
        audit.record({"kind": "click", "summary": "b"}, observation=observation())
        self.assertEqual(self.events()[0]["obs"], self.events()[1]["obs"])
        self.assertEqual(len(list(self.obs_dir().glob("*.json"))), 1)

    def test_the_size_cap(self):
        audit.record({"kind": "click", "summary": "big"}, observation=observation(extra=3000))
        blob = self.obs_dir() / f"{self.events()[-1]['obs']}.json"
        self.assertLessEqual(blob.stat().st_size, audit.MAX_OBS_BYTES)
        self.assertTrue(json.loads(blob.read_text(encoding="utf-8"))["cut"])

    def test_a_failure_drops_it_and_the_action_is_still_recorded(self):
        with mock.patch.object(audit, "_observation_doc", side_effect=RuntimeError("broken")):
            audit.record({"kind": "click", "summary": "click"}, observation=observation())
        self.assertNotIn("obs", self.events()[-1])
        self.assertEqual(self.events()[-1]["summary"], "click")


class TheManifest(Base):
    def test_the_tree_hash_is_stable_and_follows_a_file(self):
        from decurix_agent import retention
        tree = self.root / "tree"
        (tree / "computer").mkdir(parents=True)
        (tree / "a.py").write_text("x = 1\n", encoding="utf-8")
        (tree / "computer" / "b.py").write_text("y = 2\n", encoding="utf-8")
        (tree / "__pycache__").mkdir()
        (tree / "__pycache__" / "a.cpython-311.pyc").write_bytes(b"\0")
        first = retention.tree_sha(tree)
        self.assertEqual(first, retention.tree_sha(tree))
        self.assertRegex(first, r"^sha256:[0-9a-f]{64}$")
        (tree / "c.pyc").write_bytes(b"\1")
        self.assertEqual(first, retention.tree_sha(tree))  # compiled files are not the code
        (tree / "computer" / "b.py").write_text("y = 3\n", encoding="utf-8")
        self.assertNotEqual(first, retention.tree_sha(tree))
        self.assertEqual(retention.agent_sha(), retention.agent_sha())

    def test_it_lists_the_parts_that_exist_and_the_labels(self):
        from decurix_agent import retention
        audit.record({"kind": "click", "summary": "click"})
        (self.root / "computer" / "evidence").mkdir(parents=True)
        (self.root / "computer" / "evidence" / f"{TASK}.records.json").write_text("[]", encoding="utf-8")
        retention.mark_labelled(TASK, "final")
        with mock.patch.object(config, "THEBOT_SOUL_PATH", str(self.root / "no-soul")):
            data = retention.write_manifest(TASK)
        saved = json.loads((self.root / "runs" / TASK / "run.json").read_text(encoding="utf-8"))
        self.assertEqual(saved, data)
        self.assertEqual(saved["schema"], "decurix.run-manifest/v1")
        self.assertEqual(saved["parts"], {"recording": f"recordings/task-{TASK}",
                                          "ledger": f"computer/evidence/{TASK}.records.json"})
        self.assertEqual(saved["labelled_by"], ["final"])
        self.assertEqual(saved["soul_sha"], "")
        self.assertTrue(saved["agent_sha"].startswith("sha256:"))

    def test_it_never_raises(self):
        from decurix_agent import retention
        (self.root / "runs").write_text("not a folder", encoding="utf-8")
        self.assertIsNone(retention.write_manifest(TASK))


if __name__ == "__main__":
    unittest.main()
