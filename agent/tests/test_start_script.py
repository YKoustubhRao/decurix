"""
What the desktop's start script (agent/start_agent_terminal.sh) sets up that a test can read without starting it.

Review 2026-11-03 (N-F2): it ran "xhost +", which turns X access control off - anything that can reach the X server
could drive the screen around the approval gate. Only the desktop's own user may open the display.
"""

import re
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "start_agent_terminal.sh"


class TheDisplayIsTheDesktopUsersOnly(unittest.TestCase):
    def setUp(self):
        self.lines = [line for line in SCRIPT.read_text(encoding="utf-8").splitlines()
                      if not line.lstrip().startswith("#")]

    def test_access_control_is_never_turned_off(self):
        for line in self.lines:
            self.assertIsNone(re.search(r"\bxhost\s+\+(?:\s|$|>|;|&|\))", line), line)

    def test_the_desktop_user_is_let_in(self):
        self.assertTrue(any("xhost +SI:localuser:headless" in line for line in self.lines))


if __name__ == "__main__":
    unittest.main()
