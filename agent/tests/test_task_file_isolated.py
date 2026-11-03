"""
A test run never shares the desktop's machine-wide task file with another run (config_stub).

Two suites on one machine shared /tmp/decurix-current-task, and one run's task, request or stage changed the other's
outcome (review 2026-11-03: test_apprentice, test_lessons and test_draft_proof).
"""

import os
import subprocess
import sys
import unittest

import config_stub

from decurix_agent import task_context


class TheTaskFileIsThisProcesss(unittest.TestCase):
    def test_the_task_file_is_not_the_machine_wide_default(self):
        self.assertNotEqual(task_context.CURRENT_FILE, "/tmp/decurix-current-task")
        self.assertEqual(task_context.CURRENT_FILE, config_stub.TASK_FILE)
        self.assertEqual(os.environ.get("DECURIX_TASK_FILE"), config_stub.TASK_FILE)

    def test_another_test_process_gets_another_task_file(self):
        here = os.path.dirname(os.path.abspath(__file__))
        agent = os.path.dirname(here)
        env = dict(os.environ, PYTHONPATH=os.pathsep.join([agent, here]))
        env.pop("DECURIX_TASK_FILE", None)
        other = subprocess.run([sys.executable, "-c", "import config_stub; print(config_stub.TASK_FILE)"],
                               env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(other.returncode, 0, other.stderr)
        self.assertNotEqual(other.stdout.strip(), config_stub.TASK_FILE)

    def test_a_stage_set_here_is_kept_in_this_processs_file(self):
        self.addCleanup(task_context.set_stage, "")
        task_context.set_stage("run-a:s1", 2)
        self.assertEqual(task_context.current_stage(), ("run-a:s1", 2))
        self.assertTrue(os.path.exists(config_stub.TASK_FILE + ".stage"))


if __name__ == "__main__":
    unittest.main()
