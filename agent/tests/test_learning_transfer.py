"""PM acceptance of persisted demonstrations on unseen task inputs and UI variants."""
import contextlib
import importlib.util
import tempfile
import unittest
from pathlib import Path
from unittest import mock

_path = Path(__file__).resolve().parents[2] / "bench" / "workflows" / "transfer.py"
_spec = importlib.util.spec_from_file_location("workflow_transfer_benchmark", _path)
transfer = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(transfer)


@contextlib.contextmanager
def _a_run_of_its_own(own):
    """
    What a bench run leaves - the skill engine's "already replayed" note, the evidence notes, the drafts' log, the task
    the process has in hand - kept in `own` and dropped with it. The bench gives each run a fixed task id (bench
    7a0063f): kept in the machine's ~/.decurix, the next suite run was refused as a second replay of the same task, and
    the gate tests after it read this run's typed values as their own (review round 3).
    """

    from decurix_agent import state, task_context
    from decurix_agent.computer import evidence, skill_engine
    from decurix_agent.tools import outbox

    saved = (state.current_task_id, state.current_task)
    try:
        with mock.patch.object(skill_engine, "NO_TASK_SECONDS", 0), \
                mock.patch.object(skill_engine, "REPLAYED_DIR", own / "replayed"), \
                mock.patch.object(evidence, "NOTES_DIR", own / "evidence"), \
                mock.patch.object(outbox, "DRAFTS_DIR", own / "drafts"), \
                mock.patch.object(task_context, "CURRENT_FILE", str(own / "current-task")):
            yield
    finally:
        state.current_task_id, state.current_task = saved


class LearningTransferAcceptance(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Each case is a request of its own with no task id: outside a task, skill_engine refuses the same skill with
        # the same values again within NO_TASK_SECONDS (review 2026-11-03, D-30). The bench's cases are not that.
        tmp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tmp.cleanup)
        with _a_run_of_its_own(Path(tmp.name)):
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


class TheBenchCanFail(unittest.TestCase):
    """A negative control: the screen says "Result ready" and the replay reports success, but the application's answer
    is wrong. The bench must score that row failed - a bench whose rows cannot fail measures nothing (review 2026-11-02
    found its old "wrong business result" variant passing on its own check, whatever the worker did)."""

    def test_a_replay_that_succeeds_on_screen_with_a_wrong_answer_is_not_passed(self):
        from decurix_agent.computer import recording, replay, skills
        case = transfer.SCENARIOS[0]
        right = transfer.evaluate
        with tempfile.TemporaryDirectory() as tmp, _a_run_of_its_own(Path(tmp)), \
                mock.patch.object(recording, "ROOT", Path(tmp) / "recordings"), \
                mock.patch.object(skills, "ROOT", Path(tmp) / "skills"), \
                mock.patch.object(transfer, "evaluate", lambda operation, raw: ("wrong", right(operation, raw))):
            result = transfer.run_case(case, Path(tmp), "holdout")
        self.assertIn(result["replay_outcome"], replay.SUCCESS, "the control needs a replay the screen calls a success")
        self.assertFalse(result["business_verified"])
        self.assertFalse(result["passed"], result)



class ABenchRunLeavesNothingBehind(unittest.TestCase):
    def test_the_task_in_hand_and_the_notes_are_as_they_were(self):
        from decurix_agent import state, task_context
        from decurix_agent.computer import skill_engine
        before = (state.current_task_id, task_context.CURRENT_FILE, skill_engine.REPLAYED_DIR)
        with tempfile.TemporaryDirectory() as tmp, _a_run_of_its_own(Path(tmp)):
            task_context.start("t00000000000000aa", request="a bench run")
        self.assertEqual((state.current_task_id, task_context.CURRENT_FILE, skill_engine.REPLAYED_DIR), before)


if __name__ == "__main__":
    unittest.main()
