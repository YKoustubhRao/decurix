"""
The dry hold check (decurix_agent.computer.holdcheck): against a stored skill, every press its person said needs their
OK is reported with the rule that holds it on a replay, or NOT HELD, by the functions the press itself goes through;
nothing is pressed.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_holdcheck.py
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import holdcheck


def taught(ask, confirmed=True, steps=None):
    return {"name": "a-task", "approvals": {"confirmed": confirmed, "ask": ask},
            "steps": steps if steps is not None else [
                {"n": 82, "id": "s82", "kind": "click", "summary": "click File with EVC",
                 "target": {"role": "button", "name": "File with EVC"}},
                {"n": 86, "id": "s86", "kind": "click", "summary": "click the point 54,384", "point": [54, 384]},
                {"n": 40, "id": "s40", "kind": "click", "summary": "click Save to portal", "consequential": True,
                 "target": {"role": "button", "name": "Save to portal"}}]}


class TheHoldCheck(unittest.TestCase):

    def run_on(self, skill):
        with mock.patch("decurix_agent.computer.skills.load", return_value=skill):
            return holdcheck.check(skill["name"])

    def test_every_named_step_and_point_step_is_held_by_its_place(self):
        code, lines = self.run_on(taught([{"step": 82, "name": "File with EVC"}, {"step": 86}]))
        self.assertEqual(code, 0, lines)
        text = "\n".join(lines)
        self.assertIn("step 86", text)
        self.assertIn("unidentifiable target there is held", text)
        self.assertIn("step 40", text)  # consequential while taught: held by its place too
        self.assertNotIn("NOT HELD", text)

    def test_an_entry_naming_neither_a_step_nor_a_control_is_not_held(self):
        code, lines = self.run_on(taught([{"note": "the last button"}]))
        self.assertEqual(code, 1)
        self.assertIn("NOT HELD", "\n".join(lines))

    def test_an_entry_by_name_only_is_held_by_name(self):
        code, lines = self.run_on(taught([{"name": "Verify"}], steps=[]))
        self.assertEqual(code, 0, lines)
        self.assertIn('any press of "Verify"', "\n".join(lines))

    def test_an_unconfirmed_answer_is_said_and_fails(self):
        code, lines = self.run_on(taught([{"step": 86}], confirmed=False))
        self.assertEqual(code, 2)
        self.assertIn("no confirmed approvals answer", lines[0])

    def test_a_skill_that_is_not_there(self):
        with mock.patch("decurix_agent.computer.skills.load", side_effect=FileNotFoundError("no")):
            self.assertEqual(holdcheck.check("missing")[0], 2)


if __name__ == "__main__":
    unittest.main()
