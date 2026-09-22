"""
Ingest: a Chrome DevTools Recorder export read into a Decurix recording, and
what happens when that recording is replayed.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import sys
import tempfile
import types
import unittest
from pathlib import Path

try:
    from decurix_agent import config  # noqa: F401
except OSError:
    import decurix_agent

    stub = types.ModuleType("decurix_agent.config")
    stub.DESKTOP_ID = "test-box"
    stub.DESKTOP_NAME = "test-box"
    stub.CONTROL_PLANE_URL = "http://127.0.0.1:9"
    stub.AGENT_HARNESS = "ollama"
    sys.modules["decurix_agent.config"] = stub
    decurix_agent.config = stub

from decurix_agent.computer import ingest, lint, recording, replay


def sel(*groups):
    return [[g] if isinstance(g, str) else g for g in groups]


LOGIN = {
    "title": "Sign in and open reports",
    "steps": [
        {"type": "setViewport", "width": 1280, "height": 720, "deviceScaleFactor": 1, "isMobile": False},
        {"type": "navigate", "url": "https://user:pw@app.test/login?next=%2Freports&token=abc123",
         "assertedEvents": [{"type": "navigation", "url": "https://app.test/login", "title": "Sign in"}]},
        {"type": "click", "target": "main", "selectors": sel('aria/Email[role="textbox"]', "#email"), "offsetX": 4, "offsetY": 9},
        {"type": "change", "value": "alice@example.com", "selectors": sel('aria/Email[role="textbox"]', "#email")},
        {"type": "change", "value": "hunter2", "selectors": sel('aria/Password[role="textbox"]', "#pw")},
        {"type": "click", "selectors": sel('aria/Sign in[role="button"]', "button.primary"),
         "assertedEvents": [{"type": "navigation", "url": "https://app.test/home", "title": "Home"}]},
        {"type": "waitForElement", "selectors": sel('aria/Reports[role="heading"]'), "visible": True},
        {"type": "hover", "selectors": sel('aria/Menu[role="button"]')},
        {"type": "scroll", "x": 0, "y": 640},
        {"type": "click", "selectors": sel("#legacy-export", "xpath///*[@id='legacy-export']")},
        {"type": "click", "selectors": sel('aria/Reports')},
        {"type": "keyDown", "key": "Control"},
        {"type": "keyDown", "key": "s"},
        {"type": "keyUp", "key": "s"},
        {"type": "keyUp", "key": "Control"},
        {"type": "keyDown", "key": "Enter"},
        {"type": "keyUp", "key": "Enter"},
        {"type": "keyDown", "key": "a"},
        {"type": "waitForExpression", "expression": "window.ready"},
        {"type": "customStep", "name": "audit"},
        {"type": "close"},
    ],
}


class Reading(unittest.TestCase):

    def setUp(self):
        self.parsed = ingest.from_chrome_recorder(LOGIN)
        self.events = self.parsed["events"]

    def test_it_is_recognised_by_its_shape(self):
        self.assertEqual(ingest.detect(LOGIN), ingest.CHROME_RECORDER)
        self.assertIsNone(ingest.detect({"steps": "nope"}))
        self.assertIsNone(ingest.detect([1, 2]))
        self.assertIsNone(ingest.detect({"steps": [{"no": "type"}]}))

    def test_the_screen_and_title_come_from_the_file(self):
        self.assertEqual((self.parsed["title"], self.parsed["screen"]), ("Sign in and open reports", [1280, 720]))

    def test_a_navigation_becomes_an_open_and_leaves_no_credentials_in_the_address(self):
        opened = self.events[0]

        self.assertEqual(opened["kind"], "open")
        self.assertNotIn("user:pw", opened["target"])
        self.assertNotIn("abc123", opened["target"])
        self.assertIn("app.test/login", opened["target"])

    def test_what_the_page_did_afterwards_becomes_what_the_step_should_leave(self):
        self.assertEqual(self.events[0]["expect"]["active_window"], "Sign in")
        self.assertEqual(self.events[0]["expect"]["url"], "https://app.test/login")

    def test_an_aria_selector_becomes_a_role_and_a_name(self):
        click = self.events[1]
        self.assertEqual((click["kind"], click["element"]["role"], click["element"]["name"]), ("click", "textbox", "Email"))

    def test_typed_text_is_kept_except_where_it_is_a_secret(self):
        email, password = self.events[2], self.events[3]

        self.assertEqual(email["text"], "alice@example.com")
        self.assertIsNone(password["text"])
        self.assertEqual(password["withheld"], {"text": "field labelled for a secret"})
        self.assertTrue(password["element"]["secret"])
        self.assertNotIn("hunter2", json.dumps(self.events))

    def test_typed_text_is_expected_to_be_in_its_field_afterwards_so_a_replay_can_check_it(self):
        email = self.events[2]
        self.assertEqual(email["expect"], {"values": [{"role": "textbox", "name": "Email", "value": "alice@example.com"}]})
        # A secret is never expected back: its value was not kept.
        self.assertNotIn("expect", self.events[3])

    def test_every_step_reads_the_way_a_recorded_one_does(self):
        lines = [e["summary"] for e in self.events]

        self.assertTrue(lines[0].startswith("open https://app.test/login"))
        self.assertIn("Email", lines[1])
        self.assertIn("alice@example.com", lines[2])
        # A secret is named as withheld, never quoted.
        self.assertIn("[text withheld", lines[3])
        self.assertNotIn("hunter2", "\n".join(lines))
        self.assertIn("Sign in", lines[4])
        # What can only be found by a selector says so, which is why it will halt.
        self.assertIn("#legacy-export", lines[5])
        self.assertIn("press ctrl+s", "\n".join(lines))

    def test_a_wait_for_an_element_is_what_the_previous_step_should_leave(self):
        sign_in = self.events[4]
        self.assertEqual(sign_in["element"]["name"], "Sign in")
        self.assertIn({"role": "heading", "name": "Reports"}, sign_in["expect"]["appeared"])

    def test_a_css_only_selector_is_kept_as_a_step_with_no_target_not_guessed(self):
        legacy = self.events[5]
        self.assertEqual((legacy["kind"], legacy["element"]), ("click", {}))
        self.assertIn("#legacy-export", legacy["imported"]["selectors"])

    def test_a_label_with_no_role_is_a_target_by_name_alone(self):
        reports = self.events[6]
        self.assertIsNone(reports["element"]["role"])
        self.assertEqual(reports["element"]["name"], "Reports")

    def test_a_chord_and_a_named_key_are_pressed_by_the_names_a_replay_uses(self):
        self.assertEqual([e["keys"] for e in self.events if e["kind"] == "key"], [["ctrl+s"], ["Return"]])

    def test_a_printable_key_is_not_a_step_because_its_text_arrives_as_a_change(self):
        self.assertNotIn(["a"], [e.get("keys") for e in self.events])

    def test_what_a_replay_does_not_take_is_listed_with_why(self):
        skipped = {(s["type"], s["step"]): s["why"] for s in self.parsed["skipped"]}

        self.assertIn(("hover", 8), skipped)
        self.assertIn(("scroll", 9), skipped)
        self.assertIn(("waitForExpression", 19), skipped)
        self.assertIn(("customStep", 20), skipped)
        self.assertIn("script", skipped[("waitForExpression", 19)])

    def test_a_secondary_button_is_a_right_click_and_a_middle_one_is_left_to_halt(self):
        parsed = ingest.from_chrome_recorder({"steps": [
            {"type": "click", "button": "secondary", "selectors": sel('aria/File[role="button"]')},
            {"type": "click", "button": "auxiliary", "selectors": sel('aria/File[role="button"]')},
        ]})
        self.assertEqual([e["kind"] for e in parsed["events"]], ["right_click", "middle_click"])

    def test_a_credential_typed_into_an_ordinary_field_is_withheld_too(self):
        parsed = ingest.from_chrome_recorder({"steps": [
            {"type": "change", "value": "ghp_" + "a" * 36, "selectors": sel('aria/Notes[role="textbox"]')},
        ]})
        event = parsed["events"][0]
        self.assertIsNone(event["text"])
        self.assertIn("GitHub token", event["withheld"]["text"])

    def test_a_file_that_is_not_a_recording_is_refused_with_words_a_person_can_act_on(self):
        for bad in ({"steps": []}, {"title": "x"}, {"steps": [{"type": "click"}] * (ingest.MAX_STEPS + 1)}):
            with self.assertRaises(ingest.IngestError):
                ingest.from_chrome_recorder(bad)


class Saving(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT
        recording.ROOT = Path(self.tmp.name)

    def tearDown(self):
        recording.ROOT = self.saved
        self.tmp.cleanup()

    def test_an_import_is_a_finished_demonstration_like_any_other(self):
        result = ingest.import_recording(LOGIN)
        meta, events = recording.load(result["session"])

        self.assertTrue(recording.valid(result["session"]))
        self.assertEqual((meta["kind"], meta["status"], meta["title"]), ("demonstration", "complete", "Sign in and open reports"))
        self.assertEqual(meta["screen"], [1280, 720])
        self.assertEqual(meta["source"]["format"], ingest.CHROME_RECORDER)
        self.assertEqual(len(events), result["imported"])
        self.assertEqual([e["i"] for e in events], list(range(len(events))))
        self.assertTrue(result["skipped"])

    def test_a_title_given_wins_over_the_files(self):
        result = ingest.import_recording(LOGIN, title="  Month-end login  ")
        self.assertEqual(recording.load(result["session"])[0]["title"], "Month-end login")

    def test_nothing_secret_reaches_the_disk(self):
        result = ingest.import_recording(LOGIN)
        raw = (recording.ROOT / result["session"] / "events.jsonl").read_text()

        self.assertNotIn("hunter2", raw)
        self.assertNotIn("abc123", raw)

    def test_a_file_of_only_steps_a_replay_cannot_take_saves_nothing(self):
        with self.assertRaises(ingest.IngestError):
            ingest.import_recording({"steps": [{"type": "hover", "selectors": sel('aria/Menu[role="button"]')}]})

        self.assertEqual(recording.sessions(), [])

    def test_an_unknown_format_is_refused(self):
        with self.assertRaises(ingest.IngestError):
            ingest.import_recording({"nope": 1})


class ReplayingAnImport(unittest.TestCase):
    """The point of importing: it compiles, is linted, and replays with no model."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        recording.ROOT = Path(self.tmp.name)
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

    def tearDown(self):
        recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved
        self.tmp.cleanup()

    def program(self, data):
        session = ingest.import_recording(data)["session"]
        meta, events = recording.load(session)
        return replay.compile_program(meta, events)

    def test_it_compiles_with_its_secret_as_a_parameter_that_must_be_given(self):
        program = self.program(LOGIN)
        secret = next(p for p in program["params"] if p["secret"])

        self.assertEqual(secret["label"], "Password")
        self.assertIsNone(secret["default"])

    def test_a_target_known_only_by_its_label_is_found_by_it_among_controls_that_can_be_acted_on(self):
        step = {"target": {"name": "Reports"}}
        heading = {"role": "heading", "name": "Reports", "path": "a", "states": []}
        link = {"role": "link", "name": "Reports", "path": "b", "states": []}

        found, _, _ = replay.resolve(step, {"elements": [heading, link]})
        self.assertEqual(found["path"], "b")

        _, _, why = replay.resolve(step, {"elements": [link, dict(link, path="c")]})
        self.assertIn("does not guess", why)

        _, _, why = replay.resolve(step, {"elements": [heading]})
        self.assertIn("is not on the screen", why)

    def test_lint_says_what_an_import_could_not_bring_across(self):
        verdict = lint.lint(self.program(LOGIN))

        self.assertEqual(verdict["grade"], lint.RISKY)
        self.assertIn("no_target", [f["code"] for f in verdict["findings"]])
        self.assertIn("secret_params", [f["code"] for f in verdict["findings"]])

    def test_a_clean_import_replays_verified_with_no_model(self):
        program = self.program({"steps": [
            {"type": "click", "selectors": sel('aria/Sign in[role="button"]'),
             "assertedEvents": [{"type": "navigation", "url": "https://app.test/home", "title": "Home"}]},
        ]})

        class Screen:
            def __init__(self):
                self.acted = []
                self.title = "Sign in - Chromium"
                self.url = "https://app.test/login"
                self.button = {"role": "button", "name": "Sign in", "path": "p", "states": []}

            def observe(self, remember=True):
                return {"elements": [self.button], "windows": [{"title": self.title}], "active_title": self.title,
                        "size": [1280, 720], "page_url": self.url}

            def active_group(self, observation):
                return "g"

            def act(self, kind, **kwargs):
                self.acted.append((kind, kwargs))
                self.title, self.url = "Home - Chromium", "https://app.test/home"
                return {"status": "done", "text": "ok", "observation": self.observe()}

        replay.core = screen = Screen()
        result = replay.run(program, report=lambda line: None)

        self.assertEqual(result["outcome"], replay.VERIFIED, result["text"])
        self.assertEqual(screen.acted[0][1]["path"], "p")
        self.assertEqual(result["model_calls"], 0)

    def test_a_field_only_a_selector_names_is_not_typed_into_whatever_has_focus(self):
        program = self.program({"steps": [
            {"type": "change", "value": "hello", "selectors": sel("#only-css")},
        ]})

        class Screen:
            def observe(self, remember=True):
                return {"elements": [], "windows": [], "active_title": "", "size": [1280, 720]}

            def active_group(self, observation):
                return "g"

            def act(self, kind, **kwargs):
                raise AssertionError("it must not type anywhere")

        replay.core = Screen()
        result = replay.run(program, report=lambda line: None)
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)

    def test_a_step_with_no_target_halts_the_replay_rather_than_clicking_a_guess(self):
        program = self.program({"steps": [{"type": "click", "selectors": sel("#only-css")}]})

        class Screen:
            def observe(self, remember=True):
                return {"elements": [{"role": "button", "name": "Go", "path": "p", "states": []}],
                        "windows": [], "active_title": "", "size": [1280, 720]}

            def active_group(self, observation):
                return "g"

            def act(self, kind, **kwargs):
                raise AssertionError("a replay must not act on a step it cannot find a target for")

        replay.core = Screen()
        result = replay.run(program, report=lambda line: None)

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("nothing to find", result["text"])

    def test_an_imported_pay_button_is_still_held_for_approval(self):
        program = self.program({"steps": [{"type": "click", "selectors": sel("aria/Pay now")}]})
        self.assertTrue(program["steps"][0]["consequential"])


if __name__ == "__main__":
    unittest.main()
