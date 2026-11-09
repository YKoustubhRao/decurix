"""
A replay's select is checked against the entry the pick took, not the words it asked for: the application's own name
for a party may extend or respell the document's (found live 2026-11-09, a replay halted at a party it had picked).
"""

import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

from decurix_agent.computer import replay
from test_recordings import FakeScreen, field


class PickingScreen(FakeScreen):
    """A combobox whose list holds `entry`: a select fills the field with it and says it took it."""

    def __init__(self, elements, entry):
        super().__init__(elements)
        self.entry = entry

    def act(self, kind, **kwargs):
        result = super().act(kind, **kwargs)
        if kind == "select":
            for element in self.elements:
                if element["path"] == kwargs.get("path"):
                    element["value"] = self.entry or kwargs.get("option")
            result = dict(result, observation=self.observation(), taken=self.entry)
        return result


PROGRAM = {"title": "Enter a bill", "params": [
    {"name": "party_name", "step": 1, "secret": False, "default": "Example Co", "example": "Example Co"}],
    "steps": [
        {"n": 1, "kind": "select", "param": "party_name", "summary": "select party",
         "target": {"role": "combobox", "name": "Party"},
         "expect": [{"type": "value", "role": "combobox", "name": "Party", "param": "party_name", "choice": True}]}]}


class SelectTakesTheEntry(unittest.TestCase):
    def run_pick(self, entry, asked="Northfield Tower"):
        screen = PickingScreen([field("combobox", "Party")], entry)
        with mock.patch.object(replay, "core", screen), mock.patch.object(replay, "EXPECT_SECONDS", 0), \
                mock.patch.object(replay, "FIND_SECONDS", 0), mock.patch.object(replay, "POLL_SECONDS", 0):
            return replay.run(PROGRAM, params={"party_name": asked}, report=lambda _: None)

    # The run's values are not read back after a save here (there is none), so a clean run ends "completed_unverified":
    # what these hold to is that the step's own check passed.
    def test_the_field_holding_the_entry_taken_passes(self):
        result = self.run_pick("Northfield Towers")
        self.assertNotEqual(result["outcome"], replay.HALTED)
        self.assertEqual(result["verified"], 1)

    def test_with_no_entry_reported_the_asked_words_are_still_expected(self):
        # The field shows the asked words: the old check holds.
        result = self.run_pick(None)
        self.assertNotEqual(result["outcome"], replay.HALTED)
        self.assertEqual(result["verified"], 1)

    def test_a_field_that_holds_neither_still_halts(self):
        screen = PickingScreen([field("combobox", "Party")], "Northfield Towers")
        original = screen.act

        def act(kind, **kwargs):
            result = original(kind, **kwargs)
            for element in screen.elements:
                element["value"] = "Someone Else Ltd"
            return dict(result, observation=screen.observation())
        screen.act = act
        with mock.patch.object(replay, "core", screen), mock.patch.object(replay, "EXPECT_SECONDS", 0), \
                mock.patch.object(replay, "FIND_SECONDS", 0), mock.patch.object(replay, "POLL_SECONDS", 0):
            result = replay.run(PROGRAM, params={"party_name": "Northfield Tower"}, report=lambda _: None)
        self.assertEqual(result["outcome"], replay.HALTED)

    def test_only_a_select_with_a_taken_entry_changes_the_values(self):
        values = {"party_name": "Northfield Tower"}
        self.assertIs(replay._held_values({"kind": "type", "param": "party_name"}, {"taken": "X"}, values), values)
        self.assertIs(replay._held_values({"kind": "select", "param": "party_name"}, {}, values), values)
        self.assertEqual(replay._held_values({"kind": "select", "param": "party_name"}, {"taken": "Northfield Towers"},
                                             values), {"party_name": "Northfield Towers"})


if __name__ == "__main__":
    unittest.main()
