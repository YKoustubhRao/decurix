"""
After a cd the shell guard cannot follow, where a recursive rm runs is unknown, and an rm of a relative path or a
pattern then waits for a person (review 2026-11-03, F-10: the guard kept the old folder and read the rm as if it ran
there, against its own rule that what cannot be read waits). The guard is a net for an honest model's mistakes, not a
boundary; this only makes its stated rule true.
"""

import os
import unittest
from pathlib import PurePosixPath
from unittest import mock

from decurix_agent import config
from decurix_agent.commands import dangerous_reason


class AFolderThatCannotBeTold(unittest.TestCase):
    def setUp(self):
        for p in (mock.patch.dict(os.environ, {"HOME": "/home/headless"}),
                  mock.patch.object(config, "WORK_DIR", PurePosixPath("/home/headless/work"))):
            p.start()
            self.addCleanup(p.stop)

    def test_a_recursive_rm_of_a_relative_path_or_pattern_waits(self):
        for command in ('cd "$TARGET" && rm -rf *', "cd - && rm -rf build", "pushd +1 && rm -rf out",
                        'pushd "$X"; rm -r data', "popd; rm -rf ./*", 'cd "$X"\ncd sub\nrm -rf tmp'):
            with self.subTest(command=command):
                self.assertIn("cannot be told", dangerous_reason(command) or "", command)

    def test_what_does_not_depend_on_the_folder_is_read_as_before(self):
        for command in ('cd "$X" && rm -rf /home/headless/work/out', 'cd "$X" && rm -f notes.txt',
                        'cd "$X" && ls', "cd build && rm -rf out", "pushd build && rm -rf out && popd",
                        'cd "$X" && rm -rf ~/work/tmp'):
            with self.subTest(command=command):
                self.assertIsNone(dangerous_reason(command), command)
        self.assertTrue(dangerous_reason('cd "$X" && rm -rf ~'))
        self.assertTrue(dangerous_reason("pushd / && rm -rf *"))


if __name__ == "__main__":
    unittest.main()
