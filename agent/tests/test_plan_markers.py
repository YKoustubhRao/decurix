"""The markers the agent puts in a request for soul's plan step are the ones soul looks for (plan.go)."""
import unittest
from pathlib import Path

from decurix_agent import long_task, methods

PLAN_GO = Path(__file__).resolve().parents[1] / "thebot" / "internal" / "skills" / "plan.go"


class PlanMarkers(unittest.TestCase):
    def test_the_agent_and_soul_agree_on_the_markers(self):
        go = PLAN_GO.read_text(encoding="utf-8")
        self.assertIn(f'methodMarker = "{methods.MARKER}"', go)
        self.assertIn(f'planAgreedMarker = "{long_task.PLAN_AGREED_MARKER}"', go)


if __name__ == "__main__":
    unittest.main()
