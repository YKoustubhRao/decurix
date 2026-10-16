"""
What a replay does with this run's values, found on an onboarding skill run for a second joiner (2026-10-14).
"""

import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

from decurix_agent.computer import replay
from test_recordings import FakeScreen, field


class ValuesOfThisRun(unittest.TestCase):
    def test_resumed_confirmation_uses_the_earlier_supplied_value(self):
        program = {"title": "Save a customer", "params": [
            {"name": "customer_name", "step": 1, "secret": False, "default": "Acme", "example": "Acme"}],
            "steps": [
                {"n": 1, "kind": "type", "param": "customer_name", "summary": "type customer",
                 "target": {"role": "textbox", "name": "Customer"}},
                {"n": 2, "kind": "click", "summary": "click Go", "target": {"role": "button", "name": "Go"},
                 "expect": [{"type": "element", "role": "status", "name": "Saved {customer_name}"}]}]}
        for shown, expected in [("Saved Wayne", replay.VERIFIED), ("Saved Acme", replay.HALTED)]:
            with self.subTest(shown=shown):
                screen = FakeScreen([field("button", "Go")], after={"adds": [field("status", shown)]})
                with mock.patch.object(replay, "core", screen), mock.patch.object(replay, "EXPECT_SECONDS", 0):
                    result = replay.run(program, params={"customer_name": "Wayne"}, start=2, report=lambda _: None)
                self.assertEqual(result["outcome"], expected)
                self.assertEqual([kind for kind, _ in screen.acted], ["click"])

    def test_a_choice_shows_the_entry_named_within_the_value(self):
        self.assertTrue(replay._shows_value("IT", "IT department", choice=True))
        self.assertTrue(replay._shows_value("Customer Service", "Dept: Customer Service team", choice=True))
        self.assertFalse(replay._shows_value("IT", "IT department", choice=False))
        self.assertFalse(replay._shows_value("Sales", "IT department", choice=True))
        self.assertFalse(replay._shows_value("", "IT department", choice=True))

    def test_an_upload_or_choice_with_no_value_halts_rather_than_use_the_demonstrations(self):
        for step in ({"n": 1, "kind": "upload", "param": "file_to_upload", "files": ["/w/id_delgado.png"],
                      "target": {"role": "button", "name": "Proof"}},
                     {"n": 1, "kind": "select", "param": "department", "option": "Finance",
                      "target": {"role": "combobox", "name": "Department"}}):
            with mock.patch.object(replay, "_find", return_value=({"path": "p", "role": "button"}, None, {}, None)), \
                    mock.patch.object(replay.core, "act") as act:
                with self.assertRaises(replay._Halt) as halted:
                    replay._run_step(step, {})
            act.assert_not_called()
            self.assertIn("demonstration's own", halted.exception.reason)

    def test_an_upload_names_its_value_not_the_demonstrations_file(self):
        step = {"kind": "upload", "param": "file_to_upload", "files": ["/w/id_delgado.png"],
                "target": {"role": "button", "name": "Proof"}}
        self.assertIn("{file_to_upload}", replay._summary(step))
        self.assertNotIn("id_delgado", replay._summary(step))

    def test_a_scroll_whose_control_is_gone_scrolls_at_its_point(self):
        step = {"n": 40, "kind": "scroll", "point": [900, 500], "direction": "down", "amount": 3,
                "target": {"role": "row", "name": "title | Accounts Payable Specialist"}}
        missing = replay._Halt("not on the screen", missing=True)
        with mock.patch.object(replay, "_find", side_effect=missing), \
                mock.patch.object(replay.core, "act", return_value={"outcome": "done", "after": {}}) as act:
            try:
                replay._run_step(step, {})
            except replay._Halt as halt:  # a later check of the step's own result is not this test's business
                self.assertNotIn("not on the screen", halt.reason)
        act.assert_called_once()
        self.assertEqual(act.call_args.kwargs.get("point"), (900, 500))


if __name__ == "__main__":
    unittest.main()
