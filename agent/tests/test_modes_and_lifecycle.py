import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import state, watchers
from decurix_agent.computer import approval, catalog, modes, shadowing, skills


class WatcherDispatch(unittest.TestCase):
    def setUp(self):
        self.waiting = state.waiting_for_query
        self.backend_ready = state.backend_ready
        self.paused_replay = state.paused_replay
        self.paused_skill = state.paused_skill
        state.backend_ready = False

    def tearDown(self):
        state.waiting_for_query = self.waiting
        state.backend_ready = self.backend_ready
        state.paused_replay = self.paused_replay
        state.paused_skill = self.paused_skill

    def test_learn_answer_does_not_wait_for_the_model(self):
        state.waiting_for_query = True
        state.paused_skill = {"mode": "learn"}
        self.assertTrue(watchers._can_run_without_backend("yes"))

    def test_an_agent_answer_still_waits_for_its_model(self):
        state.waiting_for_query = True
        state.paused_skill = None
        state.paused_replay = None
        self.assertFalse(watchers._can_run_without_backend("yes"))

    def test_a_new_learn_request_never_needs_the_model(self):
        state.waiting_for_query = False
        self.assertTrue(watchers._can_run_without_backend("/learn demo-20260923T010203-abcdef"))


class TestModesAndLifecycle(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.orig_state_file = modes.STATE_FILE
        modes.STATE_FILE = Path(self.tmp.name) / "worker_mode.json"

        self.orig_shadowing_dir = shadowing.SHADOWING_DIR
        self.orig_obs_file = shadowing.OBSERVATIONS_FILE
        self.orig_metrics_file = shadowing.METRICS_FILE
        shadowing.SHADOWING_DIR = Path(self.tmp.name) / "shadowing"
        shadowing.OBSERVATIONS_FILE = shadowing.SHADOWING_DIR / "observations.jsonl"
        shadowing.METRICS_FILE = shadowing.SHADOWING_DIR / "metrics.json"

        self.orig_skills_root = skills.ROOT
        skills.ROOT = Path(self.tmp.name) / "skills"
        skills.ROOT.mkdir(parents=True, exist_ok=True)

    def tearDown(self):
        modes.STATE_FILE = self.orig_state_file
        shadowing.SHADOWING_DIR = self.orig_shadowing_dir
        shadowing.OBSERVATIONS_FILE = self.orig_obs_file
        shadowing.METRICS_FILE = self.orig_metrics_file
        skills.ROOT = self.orig_skills_root
        self.tmp.cleanup()

    def test_default_stage_is_supervised(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(modes.get_stage(), modes.STAGE_SUPERVISED)

    def test_set_stage_and_persistence(self):
        modes.set_stage(modes.STAGE_OBSERVATION)
        self.assertEqual(modes.get_stage(), modes.STAGE_OBSERVATION)

        modes.set_stage(modes.STAGE_AUTONOMOUS)
        self.assertEqual(modes.get_stage(), modes.STAGE_AUTONOMOUS)

        summary = modes.stage_summary()
        self.assertEqual(summary["stage"], modes.STAGE_AUTONOMOUS)
        self.assertIn("Autonomous", summary["label"])

        with self.assertRaises(ValueError):
            modes.set_stage("invalid_stage")

    def test_env_stage_override(self):
        with patch.dict(os.environ, {"DECURIX_WORKER_STAGE": "observation"}):
            self.assertEqual(modes.get_stage(), modes.STAGE_OBSERVATION)

    def test_shadowing_reads_nothing_recorded(self):
        self.assertEqual(shadowing.get_metrics()["total_observations"], 0)
        self.assertEqual(shadowing.get_recent_observations(), [])

    def test_approval_stage_awareness(self):
        btn = {"role": "button", "name": "Edit Profile"}

        # In observation mode, mutating clicks pause
        modes.set_stage(modes.STAGE_OBSERVATION)
        reason = approval.assess("click", btn)
        self.assertIsNotNone(reason)
        self.assertIn("Observation", reason)

        # In supervised mode, normal non-consequential internal clicks pass
        modes.set_stage(modes.STAGE_SUPERVISED)
        self.assertIsNone(approval.assess("click", btn))

        # Consequential controls pause
        pay_btn = {"role": "button", "name": "Pay now"}
        self.assertIsNotNone(approval.assess("click", pay_btn))

    def test_catalog_retrieval_and_installation(self):
        catalog_list = catalog.get_catalog()
        self.assertGreaterEqual(len(catalog_list), 20)

        item = catalog.get_catalog_skill("morning-brief")
        self.assertIsNotNone(item)
        self.assertEqual(item["department"], "executive_assistant")

        # Install skill into Decurix ready store
        installed = catalog.install_catalog_skill("morning-brief")
        self.assertEqual(installed["name"], "morning-brief")
        self.assertEqual(installed["status"], skills.READY)

        # Verify it is loaded by Decurix skills system
        loaded = skills.load("morning-brief")
        self.assertEqual(loaded["name"], "morning-brief")
        self.assertEqual(loaded["title"], item["title"])


if __name__ == "__main__":
    unittest.main()

