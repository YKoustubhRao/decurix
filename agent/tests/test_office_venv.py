"""
The office venv is gone (resource plan B1, CP d6d7b30): the worker libraries live in the system python3, the one the
agent, soul and worker scripts all run. Nothing in the agent may still look for /opt/office-venv - a lookup of a path
that is gone either does nothing or, worse, finds a stale copy on an old image (resource plan R1, item 1).
"""

import unittest
from pathlib import Path

AGENT = Path(__file__).resolve().parents[1]


class NothingLooksForTheGoneVenv(unittest.TestCase):
    def test_no_lookup_in_the_agent_or_its_office_command(self):
        found = []
        for path in [AGENT / "office", *sorted((AGENT / "decurix_agent").rglob("*.py"))]:
            if "thebot" in path.parts:
                continue
            text = path.read_text(encoding="utf-8")
            if "office-venv" in text or "OFFICE_VENV" in text:
                found.append(path.relative_to(AGENT).as_posix())
        self.assertEqual(found, [])

    def test_the_office_command_runs_the_system_python3(self):
        text = (AGENT / "office").read_text(encoding="utf-8")
        self.assertIn("exec python3 -m decurix_agent.tools.office", text)
        self.assertNotIn("\r\n", text)  # (a script that runs in Linux keeps LF endings)


if __name__ == "__main__":
    unittest.main()
