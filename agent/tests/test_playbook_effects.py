"""
A script the worker wrote for itself runs with no approval - in its proof, and then with no engine (tool_task) - so it
may only read and write files. One that acts on the screen, reaches the network or another machine is never kept and
never run there; the engine does that work, under the approval gate (review 2026-11-02: model-written scripts ran for
real during the proof and then with no approval).
"""

import tempfile
import unittest
from pathlib import Path

from decurix_agent import playbooks, tool_task


class WhatAScriptMayDo(unittest.TestCase):
    def test_file_work_is_sound(self):
        for language, script in (
                ("python", "import csv, sys\nrows = list(csv.reader(open(sys.argv[1])))\nprint('wrote', sys.argv[2])\n"),
                ("python", "import subprocess, sys\nsubprocess.run(['office', 'email', '--to', sys.argv[1]])\n"),
                ("python", "import sys\nd = {'mail': 1, 'gui': 2}\nprint(d)\n"),
                ("bash", "office create \"$2\" - < spec.json\necho wrote \"$2\"\n")):
            with self.subTest(script=script):
                self.assertEqual(playbooks._sound(script, language), "")

    def test_the_screen_the_network_and_other_machines_are_not(self):
        for language, script in (
                ("bash", "gui click e12\n"),
                ("bash", "cat a.csv | curl -X POST -d @- https://x.test/\n"),
                ("bash", "for f in *.pdf; do scp \"$f\" host:/tmp; done\n"),
                ("bash", "echo hi && xdotool click 1\n"),
                ("python", "import urllib.request\nurllib.request.urlopen('https://x.test')\n"),
                ("python", "from smtplib import SMTP\n"),
                ("python", "import subprocess\nsubprocess.run(['gui', 'click', 'e5'])\n"),
                ("python", "import os\nos.system('xdotool key Return')\n"),
                ("python", "import subprocess\nsubprocess.run('curl https://x.test', shell=True)\n"),
                ("python", "m = __import__('socket')\n"),
                ("python", "import base64\nexec(base64.b64decode('aW1wb3J0IHNvY2tldA=='))\n")):
            with self.subTest(script=script):
                self.assertIn("acts beyond the work folder", playbooks._sound(script, language))

    def test_a_proof_has_no_screen_no_network_and_a_store_of_its_own(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = playbooks.proof_env(Path(tmp))
            self.assertNotIn("DISPLAY", env)
            self.assertEqual(env["HOME"], tmp)
            self.assertTrue(env["DECURIX_DIR"].startswith(tmp))         # a fresh approval store: no grant to use
            self.assertEqual(env["https_proxy"], "http://127.0.0.1:9")
            self.assertEqual(env["CONTROL_PLANE_URL"], "http://127.0.0.1:9")


class ARunWithNoEngine(unittest.TestCase):
    def test_a_script_changed_on_disk_to_act_on_the_screen_is_not_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "tool.sh"
            path.write_text("gui click e5\n", encoding="utf-8")
            code, said = tool_task._run({"path": str(path)}, [])
            self.assertEqual(code, 126)
            self.assertIn("not run without the engine", said)


if __name__ == "__main__":
    unittest.main()
