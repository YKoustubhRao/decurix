"""PM acceptance of persisted demonstrations on unseen task inputs and UI variants."""
import importlib.util
import unittest
from pathlib import Path

_path = Path(__file__).resolve().parents[2] / "bench" / "workflows" / "transfer.py"
_spec = importlib.util.spec_from_file_location("workflow_transfer_benchmark", _path)
transfer = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(transfer)


class LearningTransferAcceptance(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.report = transfer.benchmark()

    def test_twenty_five_distinct_task_families_and_business_operations(self):
        self.assertEqual(self.report["families"], 25)
        self.assertEqual(len({case["operation"] for case in transfer.SCENARIOS}), 25)
        self.assertGreater(len({tuple(case["operations"]) for case in transfer.SCENARIOS}), 15)
        self.assertFalse(self.report["live_apps_tested"])

    def test_one_persisted_demonstration_and_skill_per_family(self):
        self.assertEqual(self.report["demonstrations"], 25)
        self.assertEqual(self.report["persisted_skills"], 25)
        self.assertTrue(all(row["skill_status"] == "ready" for row in self.report["results"]))

    def test_holdouts_and_moved_layout_use_new_values_and_correct_business_answers(self):
        cases = {case["id"]:case for case in transfer.SCENARIOS}
        for result in self.report["results"]:
            if result["variant"] not in {"holdout", "moved-layout"}: continue
            with self.subTest(task=result["task"], variant=result["variant"]):
                self.assertTrue(result["passed"], result["reason"])
                self.assertEqual(result["typed_values"], [cases[result["task"]]["holdout"]])
                self.assertNotIn(cases[result["task"]]["example"], result["typed_values"])

    def test_control_rename_updates_postconditions_after_safe_healing(self):
        for result in self.report["results"]:
            if result["variant"] == "renamed-control":
                with self.subTest(task=result["task"]): self.assertTrue(result["passed"], result["reason"])

    def test_ambiguous_controls_and_missing_values_never_type_a_guess(self):
        for result in self.report["results"]:
            if result["variant"] not in {"ambiguous-control", "missing-parameter"}: continue
            with self.subTest(task=result["task"], variant=result["variant"]):
                self.assertTrue(result["passed"], result["reason"])
                self.assertEqual(result["typed_values"], [])


if __name__ == "__main__":
    unittest.main()
