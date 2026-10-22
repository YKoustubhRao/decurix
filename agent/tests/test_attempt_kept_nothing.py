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
# ============================================================

class TestAnAttemptThatKeptNothing(unittest.TestCase):

    def run_with(self, events):
        root = Path(tempfile.mkdtemp())
        folder = root / "task-t7"
        folder.mkdir()
        (folder / "meta.json").write_text(json.dumps({"id": "task-t7", "event_count": len(events)}), encoding="utf-8")
        (folder / "events.jsonl").write_text("\n".join(json.dumps(e) for e in events), encoding="utf-8")
        with mock.patch.object(recording, "ROOT", root), mock.patch.object(task_context, "current", return_value="t7"):
            return skill_task._committed_on_screen()

    def test_signing_in_and_typing_keep_nothing(self):
        self.assertFalse(self.run_with([
            {"kind": "click", "target": 'textbox "Username *"', "outcome": "done"},
            {"kind": "type", "target": 'textbox "Username *"', "outcome": "done"},
            {"kind": "key", "keys": ["Return"], "target": 'textbox "Password *"', "outcome": "done"},
            {"kind": "click", "target": 'button "LOGIN"', "outcome": "done"},
        ]))

    def test_a_save_that_went_through_kept_something(self):
        self.assertTrue(self.run_with([{"kind": "click", "target": 'button "SAVE"', "outcome": "done"}]))
        self.assertFalse(self.run_with([{"kind": "click", "target": 'button "SAVE"', "outcome": "needs_approval"}]))


if __name__ == "__main__":
    unittest.main()
