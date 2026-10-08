import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import commands, state, watchers
from decurix_agent.computer import approval, catalog, context_tracker, modes, optimizer, shadowing, skills


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
        self.orig_state_dir = modes.STATE_DIR
        self.orig_state_file = modes.STATE_FILE
        modes.STATE_DIR = Path(self.tmp.name)
        modes.STATE_FILE = modes.STATE_DIR / "worker_mode.json"

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
        modes.STATE_DIR = self.orig_state_dir
        modes.STATE_FILE = self.orig_state_file
        shadowing.SHADOWING_DIR = self.orig_shadowing_dir
        shadowing.OBSERVATIONS_FILE = self.orig_obs_file
        shadowing.METRICS_FILE = self.orig_metrics_file
        skills.ROOT = self.orig_skills_root
        self.tmp.cleanup()

    def test_default_stage_is_supervised(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(modes.get_stage(), modes.STAGE_SUPERVISED)
            self.assertTrue(modes.is_supervised_mode())
            self.assertFalse(modes.is_observation_mode())
            self.assertFalse(modes.is_autonomous_mode())

    def test_set_stage_and_persistence(self):
        modes.set_stage(modes.STAGE_OBSERVATION)
        self.assertEqual(modes.get_stage(), modes.STAGE_OBSERVATION)
        self.assertTrue(modes.is_observation_mode())

        modes.set_stage(modes.STAGE_AUTONOMOUS)
        self.assertEqual(modes.get_stage(), modes.STAGE_AUTONOMOUS)
        self.assertTrue(modes.is_autonomous_mode())

        summary = modes.stage_summary()
        self.assertEqual(summary["stage"], modes.STAGE_AUTONOMOUS)
        self.assertIn("Autonomous", summary["label"])

        with self.assertRaises(ValueError):
            modes.set_stage("invalid_stage")

    def test_env_stage_override(self):
        with patch.dict(os.environ, {"DECURIX_WORKER_STAGE": "observation"}):
            self.assertEqual(modes.get_stage(), modes.STAGE_OBSERVATION)

    def test_shadowing_predict_and_delta(self):
        pred = shadowing.predict_action("Search for annual report", screen_context={"active_window": "Search"})
        self.assertIn(pred["kind"], ("click", "type"))

        # Exact match delta
        obs = {"kind": pred["kind"], "target": pred["target"], "value": pred.get("value", "")}
        delta = shadowing.compute_delta(pred, obs)
        self.assertGreaterEqual(delta["score"], 0.8)
        self.assertTrue(delta["kind_match"])

        # Mismatched action
        diff_obs = {"kind": "open", "target": {"role": "window", "name": "other"}, "value": "xyz"}
        diff_delta = shadowing.compute_delta(pred, diff_obs)
        self.assertFalse(diff_delta["kind_match"])
        self.assertLess(diff_delta["score"], 0.5)

    def test_shadowing_record_observation_and_metrics(self):
        pred = {"kind": "click", "target": {"role": "button", "name": "Submit"}, "value": ""}
        obs = {"kind": "click", "target": {"role": "button", "name": "Submit"}, "value": ""}

        entry = shadowing.record_observation("task-1", "Click submit", pred, obs)
        self.assertEqual(entry["task_id"], "task-1")
        self.assertEqual(entry["delta"]["score"], 1.0)

        metrics = shadowing.get_metrics()
        self.assertEqual(metrics["total_observations"], 1)
        self.assertEqual(metrics["exact_matches"], 1)

        recent = shadowing.get_recent_observations()
        self.assertEqual(len(recent), 1)
        self.assertEqual(recent[0]["goal"], "Click submit")

    def test_dual_zone_is_external_action(self):
        send_btn = {"role": "button", "name": "Send message"}
        calc_btn = {"role": "button", "name": "Calculate total"}

        self.assertTrue(approval.is_external_action("click", send_btn))
        self.assertFalse(approval.is_external_action("click", calc_btn))

        # Context evaluation
        self.assertTrue(approval.is_external_action("key", None, keys=["Return"], context={"url": "https://slack.com"}))
        self.assertFalse(approval.is_external_action("key", None, keys=["Return"], context={"url": "http://localhost:3000"}))

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

    def test_external_command_reason(self):
        self.assertIsNotNone(commands.external_command_reason("curl -X POST https://api.stripe.com/v1/charges"))
        self.assertIsNotNone(commands.external_command_reason("git push origin main"))
        self.assertIsNotNone(commands.external_command_reason("mail -s 'Update' user@example.com"))
        self.assertIsNone(commands.external_command_reason("curl https://example.com/data.json"))
        self.assertIsNone(commands.external_command_reason("git status"))
        self.assertIsNone(commands.external_command_reason("ls -la"))

    def test_optimizer_execution(self):
        def fake_gui_replay():
            return {"status": "success", "steps_replayed": 3}

        # Case 1: No optimization defined -> runs GUI replay directly
        skill = {"name": "test-skill"}
        res1 = optimizer.execute_with_fallback(skill, fake_gui_replay)
        self.assertEqual(res1["route"], optimizer.ROUTE_GUI_REPLAY)
        self.assertEqual(res1["result"]["steps_replayed"], 3)

        # Case 2: Optimization defined and succeeds
        opt_skill = {
            "name": "fast-skill",
            "optimization": {
                "command": "echo 'FASTPATH_DONE'",
                "post_check": {"contains_text": "FASTPATH_DONE"},
            },
        }
        res2 = optimizer.execute_with_fallback(opt_skill, fake_gui_replay)
        self.assertEqual(res2["route"], optimizer.ROUTE_OPTIMIZED)
        self.assertFalse(res2["fallback_used"])

        # Case 3: Optimization fails post check -> cascades to GUI replay fallback
        failing_opt = {
            "name": "fail-skill",
            "optimization": {
                "command": "echo 'WRONG_OUTPUT'",
                "post_check": {"contains_text": "EXPECTED_OUTPUT"},
            },
        }
        res3 = optimizer.execute_with_fallback(failing_opt, fake_gui_replay)
        self.assertEqual(res3["route"], optimizer.ROUTE_GUI_REPLAY)
        self.assertTrue(res3["fallback_used"])

    def test_context_tracker(self):
        ctx = context_tracker.capture_context(browser_url="https://app.curalabs.io/dashboard")
        self.assertEqual(ctx["url"], "https://app.curalabs.io/dashboard")
        self.assertIn("active_window", ctx)
        self.assertIn("summary", ctx)

        self.assertEqual(context_tracker._infer_app_from_title("Google Chrome - Home"), "chromium")
        self.assertEqual(context_tracker._infer_app_from_title("Terminal - bash"), "terminal")
        self.assertEqual(context_tracker._infer_app_from_title("LibreOffice Calc"), "spreadsheet")

    def test_catalog_retrieval_and_installation(self):
        catalog_list = catalog.get_catalog()
        self.assertGreaterEqual(len(catalog_list), 20)

        finance_skills = catalog.get_catalog_by_department("finance")
        self.assertTrue(any(s["name"] == "expense-approval" for s in finance_skills))

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

