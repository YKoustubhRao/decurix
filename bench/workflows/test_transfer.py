"""The transfer bench runs in a home of its own: the machine's ~/.decurix and
the process's current task are as they were after a run, even a failed one."""
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent

PROBE = r"""
import json, sys
sys.path.insert(0, %r)
import transfer
from decurix_agent import state, task_context, config
from pathlib import Path
home = Path(config.DECURIX_DIR)
def tree():
    return sorted((str(p.relative_to(home)), p.stat().st_size if p.is_file() else -1) for p in home.rglob("*"))
seen = tree()  # what importing the agent makes; the run must add nothing
before = (state.current_task_id, task_context.CURRENT_FILE)
report = transfer.benchmark(transfer.SCENARIOS[:2])
try:
    with transfer.own_home(__import__("pathlib").Path(sys.argv[1])):
        raise RuntimeError("a run that fails")
except RuntimeError:
    pass
from decurix_agent.computer import skill_engine
print(json.dumps({"added": sorted(set(map(tuple, tree())) - set(map(tuple, seen))), "passed": report["passed"], "evaluations": report["evaluations"],
                  "task_before": before[0], "task_after": state.current_task_id,
                  "file_before": before[1], "file_after": task_context.CURRENT_FILE,
                  "replayed_dir": str(skill_engine.REPLAYED_DIR), "decurix_dir": str(config.DECURIX_DIR)}))
"""


class OwnHomeTests(unittest.TestCase):
    def test_a_run_leaves_the_real_home_and_task_untouched(self):
        with tempfile.TemporaryDirectory() as real, tempfile.TemporaryDirectory() as scratch:
            real = Path(real)
            home = real / "home"
            (home / ".decurix").mkdir(parents=True)
            task_file = real / "machine-current-task"
            env = dict(os.environ, HOME=str(home), USERPROFILE=str(home), DECURIX_DIR=str(home / ".decurix"),
                       DECURIX_TASK_FILE=str(task_file))
            out = subprocess.run([sys.executable, "-c", PROBE % str(HERE), scratch], env=env, cwd=str(HERE),
                                 capture_output=True, text=True, timeout=900)
            self.assertEqual(out.returncode, 0, out.stderr[-2000:])
            got = json.loads(out.stdout.strip().splitlines()[-1])
            self.assertTrue(got["passed"])
            self.assertEqual(got["evaluations"], 10)
            self.assertEqual(got["added"], [], "the run wrote into the real home")
            self.assertEqual([p.name for p in home.iterdir()], [".decurix"], "the run wrote into the real home")
            self.assertFalse(task_file.exists(), "the run wrote the machine's current-task file")
            self.assertEqual(got["task_after"], got["task_before"])
            self.assertEqual(got["file_after"], got["file_before"])
            self.assertTrue(got["replayed_dir"].startswith(got["decurix_dir"]), got)


if __name__ == "__main__":
    unittest.main()
