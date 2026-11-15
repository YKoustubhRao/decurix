"""
Input goes only into the window it is aimed at (technique from Hermes' `_input_target_mismatch`):
- keys or typing aimed at no control go where the keyboard is, and are refused, naming both windows, when the window
  holding it is not the one the engine last looked at or brought forward (core._aimed_elsewhere);
- keys for a control go to its window once that is brought forward, and are refused, naming both, when another window
  kept the keyboard (core._keyboard_in).

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_window_mismatch.py
"""

import json
import time
import unittest
from unittest import mock

import test_action_outcome as base

from decurix_agent.computer import core
from decurix_agent.computer.errors import ComputerError

EDITOR = {"id": 11, "title": "notes.txt - Mousepad", "active": True}
DIALOG = {"id": 22, "title": "Save changes?", "active": True}


def windows(*active_first):
    """The window list with only the first one active."""

    return [dict(w, active=(i == 0)) for i, w in enumerate(active_first)]


class KeysAimedAtNoControl(base.Act):

    def note(self, window, task=base.TASK, at=None):
        core.FRONT_FILE.parent.mkdir(parents=True, exist_ok=True)
        core.FRONT_FILE.write_text(json.dumps({"task": task, "at": at or time.time(), "id": window["id"],
                                               "title": window["title"], "known": [window["id"]]}), encoding="utf-8")

    def test_a_window_that_came_forward_since_the_last_look_is_refused_naming_both(self):
        self.note(EDITOR)
        with mock.patch.object(core.screen, "windows", return_value=windows(DIALOG, EDITOR)):
            self.assertEqual(core._aimed_elsewhere(), ("Save changes?", "notes.txt - Mousepad"))

    def test_the_window_last_looked_at_is_fine(self):
        self.note(EDITOR)
        with mock.patch.object(core.screen, "windows", return_value=windows(EDITOR, DIALOG)):
            self.assertIsNone(core._aimed_elsewhere())

    def test_no_note_another_task_or_an_old_note_says_nothing(self):
        with mock.patch.object(core.screen, "windows", return_value=windows(DIALOG, EDITOR)):
            self.assertIsNone(core._aimed_elsewhere())
            self.note(EDITOR, task="tfedcba9876543210")
            self.assertIsNone(core._aimed_elsewhere())
            self.note(EDITOR, at=time.time() - core.FRONT_KEEP_SECONDS - 5)
            self.assertIsNone(core._aimed_elsewhere())

    def test_act_types_nothing_and_says_which_windows(self):
        screen = base.Screen(base.field(role="button", name="Next"))
        performed = []
        with self.assertRaises(ComputerError) as caught:
            self.act(screen, "type", text="hello", keys_from_task=True, aimed=("Save changes?", "notes.txt - Mousepad"),
                     perform=lambda *a, **k: performed.append(a))
        self.assertIn('"Save changes?"', str(caught.exception))
        self.assertIn('"notes.txt - Mousepad"', str(caught.exception))
        self.assertEqual(performed, [])
        self.assertEqual(self.recorded[-1].get("guard"), "window")

    def test_a_replay_or_a_person_is_not_asked(self):
        screen = base.Screen(base.field(role="button", name="Next"))
        self.act(screen, "type", text="hello", aimed=("Save changes?", "notes.txt - Mousepad"))


class KeysForAControl(unittest.TestCase):
    ELEMENT = {"role": "textbox", "name": "Search for", "window_title": "notes.txt - Mousepad", "ref": "e3"}

    def test_another_window_that_kept_the_keyboard_is_refused_naming_both(self):
        with mock.patch.object(core.screen, "windows", return_value=windows(DIALOG, EDITOR)), \
                mock.patch.object(core, "KEYBOARD_WAIT", 0.0):
            with self.assertRaises(ComputerError) as caught:
                core._keyboard_in(self.ELEMENT)
        self.assertIn('"notes.txt - Mousepad"', str(caught.exception))
        self.assertIn('"Save changes?"', str(caught.exception))
        self.assertIn("nothing was typed", str(caught.exception))

    def test_its_own_window_holding_the_keyboard_goes(self):
        with mock.patch.object(core.screen, "windows", return_value=windows(EDITOR, DIALOG)):
            core._keyboard_in(self.ELEMENT)

    def test_a_window_that_takes_a_moment_is_waited_for(self):
        looks = iter([windows(DIALOG, EDITOR), windows(DIALOG, EDITOR), windows(EDITOR, DIALOG)])
        with mock.patch.object(core.screen, "windows", side_effect=lambda: next(looks)), \
                mock.patch.object(core.time, "sleep", lambda s: None):
            core._keyboard_in(self.ELEMENT)

    def test_when_it_cannot_be_told_the_keys_go_as_before(self):
        with mock.patch.object(core.screen, "windows", side_effect=OSError("no display")):
            core._keyboard_in(self.ELEMENT)
        with mock.patch.object(core.screen, "windows", return_value=[dict(EDITOR, active=False)]):
            core._keyboard_in(self.ELEMENT)  # no window says it is active

    def test_typing_into_an_app_field_checks_the_keyboard_before_any_key(self):
        element = dict(base.field("", source="atspi", name="Search for"), window_title="notes.txt - Mousepad")
        with mock.patch.object(core, "_focus_window", lambda e: None), \
                mock.patch.object(core.screen, "windows", return_value=windows(DIALOG, EDITOR)), \
                mock.patch.object(core, "KEYBOARD_WAIT", 0.0), \
                mock.patch.object(core.xinput, "type_text") as typed, \
                mock.patch.object(core.xinput, "key") as pressed:
            with self.assertRaises(ComputerError):
                core._perform("type", element, None, "hello", None, None, None, None, False)
            with self.assertRaises(ComputerError):
                core._perform("key", element, None, None, ["Return"], None, None, None, False)
        typed.assert_not_called()
        pressed.assert_not_called()


if __name__ == "__main__":
    unittest.main()
