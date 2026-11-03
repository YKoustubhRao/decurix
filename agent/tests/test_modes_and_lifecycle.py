import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import state, watchers
from decurix_agent.computer import approval, catalog, modes, skills


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

        self.orig_skills_root = skills.ROOT
        skills.ROOT = Path(self.tmp.name) / "skills"
        skills.ROOT.mkdir(parents=True, exist_ok=True)

    def tearDown(self):
        modes.STATE_FILE = self.orig_state_file
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

    def test_approval_stage_awareness(self):
        btn = {"role": "button", "name": "Edit Profile"}

        # In observation mode, mutating clicks pause
        modes.set_stage(modes.STAGE_OBSERVATION)
        reason = approval.assess("click", btn)
        self.assertIsNotNone(reason)
        self.assertIn("Observation", reason)

        # Every kind that may change something pauses, not only presses and Enter (review 2026-11-03, D-28).
        field = {"role": "textbox", "name": "Notes"}
        for kind, kwargs in (("type", {"text": "x"}), ("key", {"keys": ["ctrl+s"]}), ("select", {}), ("drag", {}),
                             ("upload", {}), ("click", {})):
            with self.subTest(kind=kind):
                target = None if kind == "click" else field
                self.assertIn("Observation", approval.assess(kind, target, **kwargs) or "")
        self.assertIsNone(approval.assess("click", field), "a click that only focuses a field goes ahead")
        self.assertIsNone(approval.assess("scroll", None))

        # In supervised mode, normal non-consequential internal clicks pass
        modes.set_stage(modes.STAGE_SUPERVISED)
        self.assertIsNone(approval.assess("click", btn))

        # Consequential controls pause
        pay_btn = {"role": "button", "name": "Pay now"}
        self.assertIsNotNone(approval.assess("click", pay_btn))

    def test_the_catalog_ships_no_skill_nobody_taught(self):
        # A ready skill is one a person here demonstrated and confirmed; the
        # catalog has none of its own, so it lists nothing and installs nothing.
        self.assertEqual(catalog.get_catalog(), [])
        self.assertIsNone(catalog.get_catalog_skill("morning-brief"))
        with self.assertRaises(ValueError):
            catalog.install_catalog_skill("morning-brief")
        self.assertEqual(list(skills.ROOT.iterdir()), [])

    def test_a_catalog_entry_installs_as_a_draft_to_be_taught(self):
        entry = {"name": "example", "title": "Example", "goal": "g", "done_when": "d", "params": [], "steps": []}
        with patch.object(catalog, "CATALOG_SKILLS", [entry]):
            self.assertEqual(catalog.get_catalog(), [entry])
            installed = catalog.install_catalog_skill("example")
        self.assertEqual(installed["status"], skills.DRAFT)
        self.assertEqual(skills.load("example")["title"], "Example")


if __name__ == "__main__":
    unittest.main()

