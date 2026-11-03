"""
What `act` does a second time, and what it lets through at all (review 2026-11-03):

- D-06: a press retried on a control the page re-drew is judged on the page as it is now: a changed address, filled
  field or sent field refuses the retry.
- D-07: typing that pressed Enter or Tab, or typing a person approved, is never typed again.
- D-08: an upload never hands a page a file that may not leave the desktop.
- D-05: open takes a URL, a file, an app or an icon - never a program with arguments.
"""

import os
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent import config
from decurix_agent.computer import approval, audit, core, launchers, session
from decurix_agent.computer.errors import ComputerError

ALLOW = SimpleNamespace(allowed=True, approved=False)
APPROVED = SimpleNamespace(allowed=True, approved=True)


def look(field, url="https://site.example/a", value=None):
    shown = dict(field) if value is None else dict(field, value=value)
    return {"elements": [shown], "frame": None, "windows": [], "size": [1280, 720], "active_title": "Site",
            "groups": [{"id": "g", "source": "cdp", "url": url, "window_title": "Site"}]}


def common(looks, perform):
    return [patch.object(core, "observe", side_effect=lambda *a, **k: looks.pop(0) if len(looks) > 1 else looks[0]),
            patch.object(core, "_perform", side_effect=perform),
            patch.object(core, "_wait_for_settle"), patch.object(core, "RETYPE_PAUSE", 0),
            patch.object(audit, "record"), patch.object(core, "_downloads_snapshot", return_value={}),
            patch.object(core.cdp, "submit_form_fields", return_value=None)]


def run(patches, call):
    for p in patches:
        p.start()
    try:
        return call()
    finally:
        for p in reversed(patches):
            p.stop()


class AReDrawnControlIsJudgedAgain(unittest.TestCase):
    BUTTON = {"role": "button", "name": "Pay", "source": "cdp", "ref": "e3", "path": "site x > button Pay",
              "box": [10, 10, 40, 20], "states": [], "group": "g", "window_title": "Site", "handle": {"backend": 1}}

    def act(self, fresh_url, amount_after="100"):
        amount = {"role": "textbox", "name": "Amount", "source": "cdp", "ref": "e4", "path": "site x > textbox Amount",
                  "box": [10, 40, 40, 20], "states": [], "group": "g", "window_title": "Site", "value": "100"}

        def page(url, value):
            seen = look(self.BUTTON, url=url)
            seen["elements"].append(dict(amount, value=value))
            return seen

        looks = [page("https://site.example/a", "100"), page(fresh_url, amount_after), page(fresh_url, amount_after)]
        calls = []

        def perform(*a, **k):
            calls.append(1)
            if len(calls) == 1:
                raise ComputerError("Node is detached from document")
            return "cdp"

        patches = common(looks, perform) + [patch.object(approval, "check", return_value=APPROVED)]
        try:
            run(patches, lambda: core.act("click", ref="e3"))
            return calls, None
        except ComputerError as exc:
            return calls, exc

    def test_a_retry_on_a_page_whose_address_changed_is_refused(self):
        calls, raised = self.act("https://site.example/another-record")
        self.assertEqual(len(calls), 1, "pressed once, never again on another page")
        self.assertIn("changed under the action", str(raised))

    def test_a_retry_on_a_page_whose_filled_value_changed_is_refused(self):
        calls, raised = self.act("https://site.example/a", amount_after="900")
        self.assertEqual(len(calls), 1)
        self.assertIn("changed under the action", str(raised))

    def test_a_retry_on_the_same_page_goes_ahead(self):
        calls, raised = self.act("https://site.example/a")
        self.assertIsNone(raised)
        self.assertEqual(len(calls), 2)


class TypingIsNotRepeated(unittest.TestCase):
    FIELD = {"role": "textbox", "name": "Message", "source": "cdp", "ref": "e9", "path": "site x > textbox Message",
             "box": [700, 490, 400, 30], "states": [], "value": "", "group": "g", "window_title": "Site"}

    def typed(self, text, decision=ALLOW):
        looks = [look(self.FIELD), look(self.FIELD, value=""), look(self.FIELD, value="")]
        calls = []
        patches = common(looks, lambda *a, **k: calls.append(a[3]) or "cdp") + \
            [patch.object(approval, "check", return_value=decision)]
        try:
            run(patches, lambda: core.act("type", ref="e9", text=text))
        except ComputerError:
            pass
        return calls

    def test_a_line_ending_in_enter_is_typed_once(self):
        self.assertEqual(len(self.typed("a line\n")), 1)

    def test_text_with_a_tab_is_never_typed_twice(self):
        # (A Tab into a single field is refused before anything is typed; never twice either way.)
        self.assertLessEqual(len(self.typed("one\ttwo")), 1)

    def test_approved_typing_is_typed_once(self):
        self.assertEqual(len(self.typed("a value", decision=APPROVED)), 1)

    def test_a_plain_value_the_page_cleared_is_still_typed_again(self):
        self.assertEqual(len(self.typed("a value")), 2)


class AnUploadOnlySendsFilesThatMayLeave(unittest.TestCase):
    INPUT = {"role": "button", "name": "Upload", "source": "cdp", "ref": "e4", "path": "site x > button Upload",
             "box": [10, 10, 40, 20], "states": [], "group": "g", "window_title": "Site"}

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.state, self.work = root / "state", root / "work"
        for d in (self.state, self.work / ".hidden"):
            d.mkdir(parents=True)
        for f in (self.state / "x", self.work / ".hidden" / "y", self.work / "ok.pdf"):
            f.write_text("x")

    def upload(self, path):
        calls = []
        patches = common([look(self.INPUT)], lambda *a, **k: calls.append(k.get("files")) or "cdp") + \
            [patch.object(approval, "check", return_value=ALLOW),
             patch.object(config, "DECURIX_DIR", self.state), patch.object(config, "WORK_DIR", self.work)]
        try:
            run(patches, lambda: core.act("upload", ref="e4", files=[str(path)]))
            return calls, None
        except ComputerError as exc:
            return calls, exc

    def test_the_workers_own_state_is_never_uploaded(self):
        calls, raised = self.upload(self.state / "x")
        self.assertEqual(calls, [])
        self.assertIn("not uploaded", str(raised))

    def test_a_file_in_a_hidden_folder_is_never_uploaded(self):
        calls, raised = self.upload(self.work / ".hidden" / "y")
        self.assertEqual(calls, [])
        self.assertIsNotNone(raised)

    def test_a_work_file_is_uploaded(self):
        calls, raised = self.upload(self.work / "ok.pdf")
        self.assertIsNone(raised)
        self.assertEqual(len(calls), 1)


class OpenRunsNoProgram(unittest.TestCase):
    def test_a_program_on_path_with_arguments_is_not_opened(self):
        program = "py" if os.name == "nt" else "sh"
        with patch.object(session, "ensure"), patch.object(session, "launch") as launch, \
                patch.object(core.screen, "grab", return_value=None), patch.object(core.screen, "windows", return_value=[]), \
                patch.object(launchers, "entries", return_value=[]), patch.object(launchers, "by_name", return_value=None), \
                patch.object(audit, "record"):
            with self.assertRaises(ComputerError):
                core.open_target(f"{program} an-argument")
        launch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
