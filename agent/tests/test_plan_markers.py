"""The words and fields the agent and soul share for soul's plan step are the same on both sides (plan.go, main.go)."""
import unittest
from pathlib import Path

from decurix_agent import methods, thebot_harness

THEBOT = Path(__file__).resolve().parents[1] / "thebot"


class PlanMarkers(unittest.TestCase):
    def test_the_agent_and_soul_agree_on_the_method_marker(self):
        go = (THEBOT / "internal" / "skills" / "plan.go").read_text(encoding="utf-8")
        self.assertIn(f'methodMarker = "{methods.MARKER}"', go)

    def test_the_plan_is_agreed_by_a_field_of_the_job_soul_reads(self):
        # soul reads "the plan is agreed" only from the job body (a marker in the text could come from a pasted
        # message); the agent sends that field (thebot_harness._run_thebot).
        go = (THEBOT / "cmd" / "soul" / "main.go").read_text(encoding="utf-8")
        self.assertIn('json:"planAgreed"', go)
        source = Path(thebot_harness.__file__).read_text(encoding="utf-8")
        self.assertIn('"planAgreed": _plan_agreed()', source)


if __name__ == "__main__":
    unittest.main()
