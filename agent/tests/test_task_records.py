"""
Unit tests for how a desktop ties what it reports to the control plane's task
records: the task ID carried in a command file's name and handed to other
processes through a file, and the per-task usage totals.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import os
import tempfile
import unittest

from decurix_agent import state, task_context, usage

TASK = "t0123456789abcdef"


class TaskIDs(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = task_context.CURRENT_FILE, state.current_task_id
        task_context.CURRENT_FILE = os.path.join(self.tmp.name, "current-task")
        state.current_task_id = ""

    def tearDown(self):
        task_context.CURRENT_FILE, state.current_task_id = self.saved
        self.tmp.cleanup()

    def test_the_id_round_trips_through_a_command_file_name(self):
        name = task_context.command_filename("20260913_101500_ab12cd34", TASK)

        self.assertTrue(name.endswith(".txt"), name)
        self.assertEqual(task_context.task_id_from_filename(name), TASK)

        # The watcher renames a handled file; it must still read as a request.
        self.assertEqual(task_context.task_id_from_filename(name + ".done"), "")

    def test_a_request_without_an_id_is_saved_as_before(self):
        self.assertEqual(task_context.command_filename("20260913_101500_ab12cd34", ""), "20260913_101500_ab12cd34.txt")
        self.assertEqual(task_context.task_id_from_filename("20260913_101500_ab12cd34.txt"), "")

    def test_what_is_not_an_issued_id_never_reaches_a_file_name(self):
        for bad in ("../../etc/passwd", "t123", "T0123456789ABCDEF", "x0123456789abcdef"):
            with self.subTest(bad=bad):
                self.assertEqual(task_context.command_filename("stem", bad), "stem.txt")

    def test_another_process_reads_the_current_task_from_the_file(self):
        task_context.start(TASK)
        self.assertEqual(task_context.current(), TASK)

        # The `gui` command and the MCP server share no memory with the agent.
        state.current_task_id = ""
        self.assertEqual(task_context.current(), TASK)

    def test_a_request_without_an_id_clears_the_last_one(self):
        task_context.start(TASK)
        task_context.start("")

        state.current_task_id = ""
        self.assertEqual(task_context.current(), "")


class Usage(unittest.TestCase):

    def setUp(self):
        usage.reset()

    def test_totals_accumulate_across_calls(self):
        usage.set_model("qwen2.5-coder:3b")
        usage.add(input_tokens=545, output_tokens=12, cost_usd=0)
        usage.add(input_tokens=80, output_tokens=30, cost_usd=0)

        self.assertEqual(usage.snapshot(), {"model": "qwen2.5-coder:3b", "input_tokens": 625, "output_tokens": 42, "cost_usd": 0})

    def test_unreported_figures_stay_unknown(self):
        usage.add(cost_usd=0.041)
        usage.add(input_tokens=None, output_tokens="not a number", cost_usd=-1)

        self.assertEqual(usage.snapshot(), {"cost_usd": 0.041})

    def test_the_cached_share_of_the_input_is_kept_beside_it(self):
        # What a task really cost: the provider bills prompt-cache reads at a fraction (2026-10-28: ~80% cached).
        usage.add(input_tokens=1000, output_tokens=10, cached_input_tokens=800)
        usage.add(input_tokens=500, cached_input_tokens=None)

        self.assertEqual(usage.snapshot(), {"input_tokens": 1500, "output_tokens": 10, "cached_input_tokens": 800})

    def test_a_new_task_starts_from_nothing(self):
        usage.add(input_tokens=10)
        usage.reset()

        self.assertEqual(usage.snapshot(), {})


if __name__ == "__main__":
    unittest.main()
