"""The approval gate starts with a cleared environment (agent/decurix-entrypoint.sh, docs/gate-boundary.md).

The container's environment holds the worker's secrets; the gate gets only what it and the code it imports read.
These tests read the entrypoint's gate start, so they run on any host without starting anything.
"""

import re
import shlex
import unittest
from pathlib import Path

AGENT = Path(__file__).resolve().parents[1]
ENTRYPOINT = AGENT / "decurix-entrypoint.sh"

# The modules on the gate's code path whose environment reads decide what it needs. config.py is left out: it reads
# the worker's settings at import, and the gate uses only DECURIX_DIR and WORK_DIR from it, which the start line sets.
GATE_PATH = ["gate_server.py", "computer/approval.py", "computer/privacy.py", "computer/modes.py", "llm_client.py",
             "task_context.py", "logging_utils.py"]
# Fixed on the start line, or the gate's own: DECURIX_GATE_KEY is only removed, DECURIX_CALLER only set.
FIXED = {"PATH", "LANG", "HOME", "DECURIX_DIR", "DECURIX_WORK_DIR"}
THE_GATES_OWN = {"DECURIX_GATE_KEY", "DECURIX_CALLER"}


def gate_block():
    text = ENTRYPOINT.read_text(encoding="utf-8")
    start = text.index('if [ -s "$GATE_DIR/key" ]; then')
    end = text.index("\nfi\n", start)
    return text[start:end]


def code_lines(block):
    return "\n".join(line for line in block.splitlines() if not line.lstrip().startswith("#"))


class GateEnvironmentTest(unittest.TestCase):
    def setUp(self):
        self.block = code_lines(gate_block())
        found = re.search(r'^\s*GATE_ENV_PASS="([^"]*)"', self.block, re.M)
        self.assertIsNotNone(found, "no GATE_ENV_PASS list in the gate's start")
        self.passed = set(found.group(1).split())
        fixed = re.search(r"^\s*set -- (.*)$", self.block, re.M)
        self.assertIsNotNone(fixed, "no fixed environment (set -- ...) in the gate's start")
        self.fixed = {word.split("=", 1)[0] for word in shlex.split(fixed.group(1))}

    def test_the_gate_starts_with_a_cleared_environment(self):
        envs = re.findall(r"/usr/bin/env\b[^\n]*", self.block)
        self.assertEqual(len(envs), 1, envs)
        self.assertTrue(envs[0].startswith('/usr/bin/env -i "$@"'), envs[0])
        self.assertIn("decurix_agent.gate_server", self.block)
        self.assertEqual(self.fixed, FIXED)

    def test_control_plane_url_passes_and_no_secret_does(self):
        self.assertIn("CONTROL_PLANE_URL", self.passed)
        for secret in ("DECURIX_AGENT_TOKEN", "VNC_PW", "DECURIX_GATE_KEY"):
            self.assertNotIn(secret, self.passed | self.fixed)

    def test_every_variable_the_gate_reads_is_passed(self):
        read = set()
        for name in GATE_PATH:
            src = (AGENT / "decurix_agent" / name).read_text(encoding="utf-8")
            read |= set(re.findall(r"""(?:environ(?:\.get|\.pop|\.setdefault)?\s*[\[(]\s*|getenv\s*\(\s*)["']([A-Z0-9_]+)["']""",
                                   src))
        missing = read - self.passed - self.fixed - THE_GATES_OWN
        self.assertFalse(missing, f"read on the gate's path but not passed to it: {sorted(missing)}")


if __name__ == "__main__":
    unittest.main()
