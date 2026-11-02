"""
The browser extension's backend (agent/browser-agent/extension_pipeline/ext_server.py, :8092): who may call it,
where an approval may come from, and when a run counts as finished (review 2026-11-02).

It sent CORS * with no check, so any web page open in the desktop's Chromium could drive the logged-in browser
with an `approved` list naming a Pay click, and read other requests' screenshots. And a model answer that could not
be read, or a "remaining" of "abandoned", ended a run as done.

No browser, no FastAPI and no model are needed: guard.py is plain Python, and the model module is stubbed.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p "test_browser_ext_guard.py"
"""

import json
import os
import sys
import types
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parent.parent / "browser-agent"
for folder in ("extension_pipeline", "core"):
    if str(ROOT / folder) not in sys.path:
        sys.path.append(str(ROOT / folder))

# The model module talks to Ollama (and imports requests): a stand-in, so nothing here reaches a model.
if "models" not in sys.modules:
    _models = types.ModuleType("models")
    _models.DEFAULT_MODEL = "test"
    _models.MODELS = {"test": {"label": "test", "vision": False}}
    _models.chat = lambda *a, **k: ""
    _models.chat_stream = lambda *a, **k: iter(())
    sys.modules["models"] = _models

import atomizer  # noqa: E402
import ext_codegen  # noqa: E402
import guard  # noqa: E402

TOKEN = "ef" * 32
LOCAL = {"host": "127.0.0.1:8092"}


def refusal(method="GET", tok=TOKEN, **headers):
    return guard.refusal(method, {**LOCAL, **{k.replace("_", "-"): v for k, v in headers.items()}}, tok=tok)


class WhoMayCall(unittest.TestCase):

    def test_a_web_page_is_refused(self):
        for origin in ("https://evil.example", "http://127.0.0.1:8000", "null"):
            with self.subTest(origin=origin):
                self.assertEqual(refusal("POST", origin=origin, content_type="application/json")[0], 403)
        self.assertEqual(refusal(sec_fetch_site="cross-site")[0], 403, "an <img> or a no-cors GET has no Origin")
        self.assertEqual(refusal(authorization="Bearer " + TOKEN, origin="https://evil.example")[0], 403)

    def test_a_rebinding_page_is_refused_by_its_host(self):
        self.assertEqual(guard.refusal("GET", {"host": "rebound.evil.example:8092",
                                               "authorization": "Bearer " + TOKEN}, tok=TOKEN)[0], 403)
        for host in ("127.0.0.1:8092", "localhost:8092", "[::1]:8092"):
            with self.subTest(host=host):
                self.assertIsNone(guard.refusal("GET", {"host": host, "authorization": "Bearer " + TOKEN}, tok=TOKEN))

    def test_the_extension_passes_by_its_own_origin(self):
        self.assertIsNone(refusal(origin="chrome-extension://abcdefghijklmnop"))
        self.assertIsNone(refusal("POST", origin="chrome-extension://abcdefghijklmnop",
                                  content_type="application/json", content_length="2"))

    def test_soul_needs_the_token_when_the_desktop_has_one(self):
        self.assertEqual(refusal()[0], 401)
        self.assertEqual(refusal(authorization="Bearer wrong")[0], 401)
        self.assertIsNone(refusal(authorization="Bearer " + TOKEN))
        self.assertIsNone(refusal(tok=""), "a desktop created before tokens is served as before")

    def test_a_write_must_be_json(self):
        good = {"authorization": "Bearer " + TOKEN}
        self.assertEqual(refusal("POST", content_type="text/plain", content_length="5", **good)[0], 415)
        self.assertEqual(refusal("POST", content_length="5", **good)[0], 415)
        self.assertEqual(refusal("POST", content_length="-1", **good)[0], 400)
        self.assertIsNone(refusal("POST", content_type="application/json", content_length="5", **good))

    def test_the_token_is_read_from_the_environment(self):
        with mock.patch.dict(os.environ, {guard.TOKEN_ENV: TOKEN}):
            self.assertTrue(guard.has_token({"Authorization": "Bearer " + TOKEN}))
            self.assertFalse(guard.has_token({}))
        with mock.patch.dict(os.environ, {guard.TOKEN_ENV: ""}):
            self.assertFalse(guard.has_token({"Authorization": "Bearer "}), "no token is never a match")


class Approvals(unittest.TestCase):
    PAY = 'click "Pay now" on shop.example'

    def test_only_the_gates_own_stop_with_the_token_is_let_through_once(self):
        approvals = guard.Approvals()
        self.assertEqual(approvals.grant([self.PAY], trusted=True), [], "nothing was stopped: nothing to approve")
        approvals.stopped(self.PAY)
        self.assertEqual(approvals.grant([self.PAY], trusted=False), [], "a caller without the token")
        self.assertEqual(approvals.grant([self.PAY, 'click "Send" on mail.example'], trusted=True), [self.PAY])
        self.assertEqual(approvals.grant([self.PAY], trusted=True), [], "a yes is one action's, once")

    def test_a_body_that_is_not_a_list_grants_nothing(self):
        approvals = guard.Approvals()
        approvals.stopped(self.PAY)
        self.assertEqual(approvals.grant(self.PAY, trusted=True), [])


class WhenARunIsDone(unittest.TestCase):

    def test_remaining_is_done_only_as_a_whole_status(self):
        for text in ("done", "Done.", "<done>", "DONE!", "nothing left", "Task complete", "n/a", "  finished "):
            with self.subTest(text=text):
                self.assertTrue(atomizer.is_done(text))
        for text in ("", "   ", "abandoned", "incomplete", "not done yet", "nothing was found, search again",
                     "click done", "the form is incomplete", "none of the fields are filled"):
            with self.subTest(text=text):
                self.assertFalse(atomizer.is_done(text))

    def test_an_unreadable_answer_is_a_failed_step_not_done(self):
        with mock.patch.object(ext_codegen.models, "chat", return_value="not json at all"):
            with self.assertRaisesRegex(ValueError, "no usable action"):
                ext_codegen.decide_and_act("test", "buy milk", "buy milk", "https://shop.example", "<html/>")
            with self.assertRaisesRegex(ValueError, "no usable action"):
                ext_codegen.decide_stack_step("test", "buy milk", "buy milk", "https://shop.example", "<html/>")

    def test_an_invented_action_is_no_action_and_an_empty_remaining_is_not_done(self):
        invented = json.dumps({"next_instruction": "x", "remaining": "", "expectation": "", "memory": "",
                               "evaluation_previous_goal": "", "action": {"type": "teleport"}})
        with mock.patch.object(ext_codegen.models, "chat", return_value=invented):
            with self.assertRaisesRegex(ValueError, "no usable action"):
                ext_codegen.decide_and_act("test", "buy milk", "buy milk", "https://shop.example", "<html/>")
        finished = json.dumps({"next_instruction": "", "remaining": "", "expectation": "", "memory": "",
                               "evaluation_previous_goal": "", "action": {"type": "done"}})
        with mock.patch.object(ext_codegen.models, "chat", return_value=finished):
            _, remaining, _, action, *_ = ext_codegen.decide_and_act("test", "t", "t", "https://x.example", "<html/>")
        self.assertIsNone(action)
        self.assertFalse(atomizer.is_done(remaining), "an empty remaining with done disagrees: re-asked, not done")


if __name__ == "__main__":
    unittest.main()
