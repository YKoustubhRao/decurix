"""
A secret never reaches a file the desktop keeps, whichever path wrote it (review 2026-11-03, D-01, D-02, D-03, D-19,
E-14): every writer - a recording's events and meta, the evidence notes, the playbook store, a progress line - ends
with privacy.scrub, so a caller that forgot a helper cannot leak.

The secrets here are invented; the test only asserts their text is in no file under the folders the writers use.
"""

import json
import os
import tempfile
import unittest
from collections import deque
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401  (first: a task file of this test process's own)

import test_playbooks
from decurix_agent import playbooks, progress, task_context
from decurix_agent.computer import (audit, demonstration, evidence, ingest, ingest_workflows, privacy,
                                    recording)

SECRET = "Quill#4471"
REQUEST = f"Sign in to the portal as clerk, password {SECRET}, and download the statement"
TOKEN = "ghp_" + "a" * 36


def every_file_text(root):
    out = []
    for path in Path(root).rglob("*"):
        if path.is_file():
            out.append(path.read_bytes().decode("utf-8", "replace"))
    return "\n".join(out)


class _Isolated(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.root = root
        patches = [
            mock.patch.dict(os.environ, {"DECURIX_DIR": str(root / "state")}),
            mock.patch.object(recording, "ROOT", root / "recordings"),
            mock.patch.object(evidence, "NOTES_DIR", root / "evidence"),
            mock.patch.object(progress, "emit_action"),
            mock.patch.object(progress, "emit"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        privacy._known_cache.clear()
        self.addCleanup(privacy._known_cache.clear)
        privacy.remember_secrets(REQUEST)

    def assertNowhere(self, text, root=None):
        written = every_file_text(root or self.root)
        self.assertTrue(written, "nothing was written")
        self.assertNotIn(text, written)


class Scrub(_Isolated):
    def test_every_string_of_nested_data_is_withheld(self):
        data = {"title": REQUEST, "steps": [{"text": "token " + TOKEN}, ("plain", 3, None, True)]}
        kept = privacy.scrub(data)
        self.assertNotIn(SECRET, json.dumps(kept))
        self.assertNotIn(TOKEN, json.dumps(kept))
        self.assertEqual(kept["steps"][1], ["plain", 3, None, True])
        self.assertIn(SECRET, data["title"])  # the input is not changed

    def test_an_element_that_is_a_secret_field_loses_its_value(self):
        field = {"role": "password text", "name": "Password", "value": "x-unremembered-9"}
        kept = privacy.scrub({"element": field})
        self.assertIsNone(kept["element"]["value"])
        self.assertTrue(kept["element"]["secret"])

    def test_an_elements_handle_is_never_written(self):
        # Review 2026-11-03 (round 2): the screen layer keeps a secret field's raw value in the element's handle;
        # scrub blanked `value` and kept `handle.value`.
        field = {"role": "textbox", "name": "One-time code", "value": "[withheld]",
                 "handle": {"value": "raw-code-7731", "dom_role": "textbox"}}
        self.assertNotIn("handle", privacy.scrub({"element": field})["element"])
        recording.append("adhoc-20260101", {"kind": "type", "element": dict(field)})
        with mock.patch.object(task_context, "current", return_value="t0123456789abcdef"):
            evidence.note({"look": [dict(field)]})
        self.assertNowhere("raw-code-7731")

    def test_an_error_gives_a_placeholder_never_the_input(self):
        with mock.patch.object(privacy, "_scrub", side_effect=RuntimeError("broken")):
            self.assertEqual(privacy.scrub({"title": REQUEST}), {"held_back": privacy.HELD_BACK})
            self.assertEqual(privacy.scrub([REQUEST]), [])
            self.assertEqual(privacy.scrub(REQUEST), privacy.HELD_BACK)

    def test_text_that_cannot_be_checked_is_held_back(self):
        with mock.patch.object(privacy, "_known", side_effect=privacy._Unreadable("unreadable")):
            self.assertEqual(privacy.scrub({"t": "anything"}), {"t": privacy.HELD_BACK})

    def test_an_account_number_in_a_note_is_kept(self):
        # The card checksum alone does not withhold a number a note names as an account (withhold_known's rule).
        line = "Paid to A/c 4111111111111111 on the first"
        self.assertEqual(privacy.scrub(line), line)


class Recordings(_Isolated):
    def test_a_tasks_recording_is_not_titled_with_the_raw_request(self):
        with mock.patch.object(task_context, "current", return_value="t0123456789abcdef"), \
                mock.patch.object(task_context, "current_request", return_value=REQUEST):
            audit.record({"kind": "click", "summary": "click button Sign in", "outcome": "done"})
        meta = recording.meta(recording.session_for_task("t0123456789abcdef"))
        self.assertIn("[withheld]", meta["title"])
        self.assertNowhere(SECRET)

    def test_an_event_a_caller_did_not_withhold_is_withheld_as_it_is_written(self):
        recording.append("adhoc-20260101", {"kind": "type", "summary": f"type {SECRET} into the box", "text": SECRET})
        recording.update("adhoc-20260101", description=f"the password is {SECRET}")
        self.assertNowhere(SECRET)

    def test_a_demonstration_steps_summary_keeps_no_withheld_text(self):
        rec = demonstration.Recorder.__new__(demonstration.Recorder)
        rec._looks = deque(maxlen=20)
        secret_box = {"role": "password text", "name": "Password", "box": [10, 10, 100, 20]}
        observation = {"elements": [secret_box], "active_title": "Sign in", "size": [1280, 800]}
        action = {"kind": "type", "text": "Unremembered-77x", "t": 1.0, "t_end": 2.0}
        with mock.patch.object(demonstration.core, "_context", return_value={"url": "https://portal.test/login"}):
            entry = demonstration.Recorder._entry(rec, action, None, None, observation)
        self.assertIsNone(entry["text"])
        self.assertNotIn("Unremembered-77x", json.dumps(entry))

    def test_an_imported_workflow_demo_keeps_no_secret(self):
        data = {"workflow": "sign in", "steps": [
            {"kind": "open", "url": "https://portal.test/login?token=abc123def456"},
            {"kind": "type", "target": {"role": "textbox", "name": "Password"}, "text": "Unremembered-77x"},
            {"kind": "type", "target": "Notes", "text": TOKEN},
            {"kind": "type", "target": "User", "text": "clerk " + SECRET},
        ]}
        ingest.import_recording(data)
        self.assertNowhere("Unremembered-77x")
        self.assertNowhere("abc123def456")
        self.assertNowhere(TOKEN)
        self.assertNowhere(SECRET)

    def test_a_malformed_workflow_demo_is_refused_as_such(self):
        for data in ({"workflow": "x", "steps": [{"kind": 5}]},
                     {"workflow": "x", "steps": [{"kind": "click", "point": "here"}]},
                     {"workflow": "x", "steps": "not a list"},
                     {"workflow": "x", "steps": [{"kind": "click"}] * (ingest.MAX_STEPS + 1)}):
            with self.assertRaises(ingest.IngestError):
                ingest_workflows.from_workflow_demo(data)

    def test_a_credential_in_an_ordinary_field_is_not_kept(self):
        public = privacy.public_element({"role": "textbox", "name": "Notes", "value": TOKEN, "window_title": "Edit"})
        self.assertIsNone(public["value"])
        self.assertIn("value", public["withheld"])
        public = privacy.public_element({"role": "link", "name": "Open " + TOKEN, "window_title": "x"})
        self.assertIsNone(public["name"])


class Evidence(_Isolated):
    def test_a_look_at_a_one_time_code_field_keeps_no_code(self):
        field = {"ref": "e4", "role": "textbox", "name": "One-time code", "value": "529749", "group": "g",
                 "states": [], "source": "cdp"}
        observation = {"size": [800, 600], "active_title": "Verify", "windows": [],
                       "groups": [{"id": "g", "title": "Verify"}], "elements": [field]}
        with mock.patch.object(task_context, "current", return_value="t0123456789abcdef"):
            evidence.note_look('textbox "One-time code" value=529749', observation)
            evidence.note(f"read off the screen: password {SECRET}")
        self.assertNowhere("529749")
        self.assertNowhere(SECRET)


class Progress(unittest.TestCase):
    def setUp(self):
        self.sent = []
        for p in (mock.patch.object(progress, "_ensure_sender"),
                  mock.patch.object(progress._pending, "put_nowait", side_effect=self.sent.append),
                  mock.patch.object(task_context, "current", return_value="t0123456789abcdef")):
            p.start()
            self.addCleanup(p.stop)

    def test_a_steps_fields_are_scrubbed_and_its_pictures_kept(self):
        progress.emit_action("typed a value", {"element": {"role": "password text", "name": "Password", "value": "v-1"},
                                               "note": "token " + TOKEN}, before_png=b"\x89PNG")
        item = self.sent[0]
        self.assertIsNone(item["action"]["element"]["value"])
        self.assertNotIn(TOKEN, json.dumps(item))
        self.assertIn("before_png", item)

    def test_progress_never_raises(self):
        class Unprintable:
            def __str__(self):
                raise RuntimeError("cannot be shown")

        progress.emit(Unprintable())
        progress.emit_action(Unprintable(), {"x": 1})
        progress.emit_action("a step", {"x": 1}, before_png="not bytes")


class PlaybookStore(test_playbooks._Store):
    def test_a_kept_way_does_not_keep_the_requests_secret(self):
        privacy._known_cache.clear()
        privacy.remember_secrets(REQUEST)
        way = ['gui open "Portal"', 'gui type e3 "clerk"', "gui click e5", "gui observe", "gui click e9"]
        _, name = self.do(REQUEST, way, tokens=5000)
        self.assertTrue(name)
        kept = every_file_text(playbooks.ROOT)
        self.assertNotIn(SECRET, kept)
        self.assertNotIn("quill", kept.lower())  # nor the secret's letters, in the playbook's name or shape


class RecordingLock(unittest.TestCase):
    def test_a_held_session_lock_is_waited_for_only_so_long(self):
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(recording, "ROOT", Path(tmp)), \
                mock.patch.object(recording, "LOCK_WAIT_SECONDS", 0.1), \
                mock.patch.object(recording.filelock, "try_exclusive", return_value=False):
            with self.assertRaises(TimeoutError):
                recording.append("adhoc-20260101", {"kind": "click"})


if __name__ == "__main__":
    unittest.main()
