import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import skill_task, task_context
from decurix_agent.computer import recording


# ============================================================
# GST suite T7 (2026-10-22): the first attempt's login failed and nothing was
# entered, but it had touched the screen, so the second attempt was told to
# open and correct the record the first one made - there was none.
# Independent review (2026-10-22): a word list over click labels then decided
# "kept nothing" and the retry was told "no record exists... do the task from
# its first step" - a keyboard save, an upload or an icon-only button keeps a
# record with no "Save" pressed. "Kept nothing" is said only when the attempt
# took no screen action at all; otherwise its actions are evidence and the
# engine looks in the application first.
# ============================================================

LOGIN = [
    {"kind": "click", "target": 'textbox "Username *"', "outcome": "done"},
    {"kind": "type", "target": 'textbox "Username *"', "text": "meera", "outcome": "done"},
    {"kind": "key", "keys": ["Return"], "target": 'textbox "Password *"', "outcome": "done"},
    {"kind": "click", "target": 'button "LOGIN"', "outcome": "done"},
]


class TestAnAttemptThatKeptNothing(unittest.TestCase):

    def record(self, events):
        root = Path(tempfile.mkdtemp())
        folder = root / "task-t7"
        folder.mkdir()
        (folder / "meta.json").write_text(json.dumps({"id": "task-t7", "event_count": len(events)}), encoding="utf-8")
        (folder / "events.jsonl").write_text("\n".join(json.dumps(e) for e in events), encoding="utf-8")
        run = {"skill": {"name": "gstr1-file", "title": "File GSTR-1", "steps": []}, "values": {},
               "attempt_note": "the return is not filed", "attempt_report": "DONE"}
        with mock.patch.object(recording, "ROOT", root), mock.patch.object(task_context, "current", return_value="t7"):
            skill_task._attempt_record(run)
        return run

    def test_no_screen_action_at_all_is_the_only_blank_attempt(self):
        run = self.record([])
        self.assertTrue(run["attempt_blank"])
        self.assertIn("KEPT NOTHING IN THE APPLICATION", skill_task._engine_request(run))

    def test_signing_in_is_evidence_and_the_engine_looks_before_entering_anything(self):
        run = self.record(LOGIN)
        self.assertFalse(run["attempt_blank"])
        request = skill_task._engine_request(run)
        self.assertNotIn("KEPT NOTHING", request)
        self.assertIn('click button "LOGIN"', request)
        self.assertIn("FIRST look in the application", request)
        self.assertIn("Creating a second record is the one thing you must not do", request)
        self.assertNotIn("meera", json.dumps(run["attempt_actions"]))   # what was typed is never said

    def test_a_keyboard_save_or_a_double_click_is_never_read_as_nothing_kept(self):
        for event in ({"kind": "key", "keys": ["ctrl", "s"], "target": 'textbox "Name"', "outcome": "done"},
                      {"kind": "double_click", "target": 'listitem "Invoice 12"', "outcome": "done"},
                      {"kind": "click", "target": 'button ""', "outcome": "done"}):
            run = self.record([event])
            self.assertFalse(run["attempt_blank"], event)
            self.assertNotIn("KEPT NOTHING", skill_task._engine_request(run))
        self.assertIn("key ctrl+s on textbox \"Name\"", self.record([{"kind": "key", "keys": ["ctrl", "s"],
                                                                     "target": 'textbox "Name"'}])["attempt_actions"][0])

    def test_an_action_that_did_not_go_through_is_not_counted(self):
        self.assertTrue(self.record([{"kind": "click", "target": 'button "SAVE"', "outcome": "needs_approval"}])["attempt_blank"])


if __name__ == "__main__":
    unittest.main()
