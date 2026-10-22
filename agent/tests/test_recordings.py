"""
Unit tests for recordings: turning raw input into actions, what a recording
never keeps, which text a replay must not check, the on-disk store, and
compiling and running a replay against a scripted screen. None of it needs a
display; the screen-facing half is exercised on a live desktop.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import io
import json
import os
import tempfile
import threading
import time
import unittest
import zipfile
from datetime import date
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401 - first: stubs decurix_agent.config on a machine with no desktop

from decurix_agent import record_task, replay_task, state
from decurix_agent.computer import approval, core, privacy, processing, recording, replay, screen, volatility
from decurix_agent.computer.errors import ComputerError


def press(t, x, y, button=1):
    return [{"type": "press", "t": t, "button": button, "x": x, "y": y}]


def click(t, x, y, button=1):
    return press(t, x, y, button) + [{"type": "release", "t": t + 0.08, "button": button, "x": x, "y": y}]


def keys(t, *pairs):
    """pairs of (key, char), each pressed and let go."""
    events = []
    for key, char in pairs:
        events.append({"type": "key_down", "t": t, "key": key, "char": char})
        events.append({"type": "key_up", "t": t + 0.02, "key": key})
        t += 0.1
    return events


def text(t, word):
    return keys(t, *[(c if c != " " else "space", c) for c in word])


class Merging(unittest.TestCase):

    def test_clicks_double_clicks_and_right_clicks(self):
        self.assertEqual([a["kind"] for a in processing.merge(click(0, 10, 10))], ["click"])
        self.assertEqual([a["kind"] for a in processing.merge(click(0, 10, 10) + click(0.2, 12, 11))], ["double_click"])
        self.assertEqual([a["kind"] for a in processing.merge(click(0, 10, 10) + click(1.5, 10, 10))], ["click", "click"])
        self.assertEqual([a["kind"] for a in processing.merge(click(0, 10, 10, button=3))], ["right_click"])

    def test_a_long_press_and_move_is_a_drag(self):
        raw = press(0, 10, 10) + [{"type": "release", "t": 0.5, "button": 1, "x": 200, "y": 40}]
        (action,) = processing.merge(raw)
        self.assertEqual((action["kind"], action["point"], action["end"]), ("drag", [10, 10], [200, 40]))

    def test_wheel_clicks_in_a_row_are_one_scroll(self):
        raw = press(0, 5, 5, 5) + press(0.1, 5, 5, 5) + press(0.2, 5, 5, 5) + press(2, 5, 5, 4)
        actions = processing.merge(raw)
        self.assertEqual([(a["direction"], a["amount"]) for a in actions], [("down", 3), ("up", 1)])

    def test_typing_with_shift_and_backspace_is_one_text(self):
        raw = [{"type": "key_down", "t": 0, "key": "Shift_L", "char": None},
               {"type": "key_down", "t": 0.05, "key": "h", "char": "H"},
               {"type": "key_up", "t": 0.06, "key": "h"},
               {"type": "key_up", "t": 0.07, "key": "Shift_L"}]
        raw += text(0.1, "elx") + keys(0.5, ("BackSpace", None)) + text(0.7, "lo")
        (action,) = processing.merge(raw)
        self.assertEqual((action["kind"], action["text"]), ("type", "Hello"))

    def test_shortcuts_are_keys_not_text(self):
        raw = [{"type": "key_down", "t": 0, "key": "Control_L", "char": None},
               {"type": "key_down", "t": 0.1, "key": "s", "char": "s"},
               {"type": "key_up", "t": 0.2, "key": "s"},
               {"type": "key_up", "t": 0.3, "key": "Control_L"},
               {"type": "key_down", "t": 1, "key": "Shift_L", "char": None},
               {"type": "key_down", "t": 1.1, "key": "Tab", "char": None},
               {"type": "key_up", "t": 1.2, "key": "Tab"},
               {"type": "key_up", "t": 1.3, "key": "Shift_L"}]
        self.assertEqual([a["keys"] for a in processing.merge(raw)], [["ctrl+s"], ["shift+Tab"]])

    def test_return_and_clicks_end_a_run_of_typing(self):
        raw = text(0, "ab") + keys(0.3, ("Return", None)) + text(0.5, "c") + click(1, 50, 50) + text(1.5, "d")
        actions = processing.merge(raw)
        self.assertEqual([(a["kind"], a.get("text") or a.get("keys")) for a in actions],
                         [("type", "ab"), ("key", ["Return"]), ("type", "c"), ("click", None), ("type", "d")])

    def test_text_typed_and_deleted_again_is_nothing(self):
        self.assertEqual(processing.merge(text(0, "a") + keys(0.2, ("BackSpace", None))), [])

    def test_the_frame_of_the_first_raw_event_is_carried(self):
        raw = click(0, 1, 1)
        raw[0]["frame"] = "first"
        self.assertEqual(processing.merge(raw)[0]["frame"], "first")

    def test_idle_waits_for_every_button_and_key(self):
        self.assertFalse(processing.idle(press(0, 1, 1)))
        self.assertFalse(processing.idle([{"type": "key_down", "t": 0, "key": "a", "char": "a"}]))
        self.assertTrue(processing.idle(click(0, 1, 1) + text(1, "ok")))


def field(role, name, **extra):
    base = {"role": role, "name": name, "value": "", "states": [], "box": [10, 10, 100, 20],
            "path": f"site x > {role} \"{name}\"", "source": "cdp", "group": "g", "window_title": "Shop - Chromium"}
    base.update(extra)
    return base


class Privacy(unittest.TestCase):

    def test_secret_fields_are_recognised_however_the_toolkit_says_so(self):
        self.assertEqual(privacy.secret_field(field("textbox", "", handle={"raw_role": "password text"})), "password field")
        self.assertEqual(privacy.secret_field(field("textbox", "Login", handle={"input_type": "password"})), "password field")
        self.assertIsNotNone(privacy.secret_field(field("textbox", "Number", handle={"autocomplete": "cc-number"})))
        self.assertIsNotNone(privacy.secret_field(field("textbox", "Password")))
        self.assertIsNone(privacy.secret_field(field("link", "Forgot password?")))
        self.assertIsNone(privacy.secret_field(field("textbox", "Customer name")))

    def test_credentials_are_recognised_in_any_field(self):
        self.assertEqual(privacy.credential_in("key sk-ant-api03-abcdefghijklmnopqrstuv"), "API key")
        self.assertEqual(privacy.credential_in("4111 1111 1111 1111"), "card number")
        self.assertIsNone(privacy.credential_in("order 12345 for Acme"))

    def test_withheld_text_is_named_never_quoted(self):
        password = field("textbox", "Password")
        self.assertEqual(privacy.withhold_text(password, "hunter2"), (None, "field labelled for a secret"))
        self.assertEqual(privacy.withhold_text(field("textbox", "Name"), "Kim"), ("Kim", None))

        line = approval.describe_action("type", password, "hunter2")
        self.assertNotIn("hunter2", line)
        self.assertIn("withheld", line)

    def test_a_recorded_element_keeps_no_handle_no_secret_and_no_half_string(self):
        public = privacy.public_element(field("textbox", "x" * 600, value="hunter2", handle={"input_type": "password"}))
        self.assertNotIn("handle", public)
        self.assertIsNone(public["value"])
        self.assertIsNone(public["name"])
        self.assertEqual(public["withheld"]["value"], "password field")
        self.assertEqual(public["withheld"]["name"], "longer than 512 characters")

    def test_urls_are_reduced_by_structure(self):
        self.assertEqual(
            privacy.reduce_url("https://kim:pw@shop.example/cart?token=abc&page=2#access_token=zz"),
            "https://shop.example/cart?token=&page=2",
        )
        self.assertEqual(privacy.reduce_url("https://shop.example/cart"), "https://shop.example/cart")

    def test_a_masked_frame_paints_over_the_box_only(self):
        frame = screen.Frame(4, 2, bytearray(b"\xff" * 24))
        masked = frame.masked([[1, 0, 2, 1]])
        self.assertEqual(bytes(masked.rgb[:12]), b"\xff\xff\xff" + b"\x00" * 6 + b"\xff\xff\xff")
        self.assertEqual(bytes(masked.rgb[12:]), b"\xff" * 12)
        self.assertEqual(bytes(frame.rgb), b"\xff" * 24)


class Volatility(unittest.TestCase):

    def test_moments_and_counts_are_volatile(self):
        today = date(2026, 9, 14)
        self.assertEqual(volatility.classify("10:42"), "clock_time")
        self.assertEqual(volatility.classify("updated 3 min ago"), "relative_time")
        self.assertEqual(volatility.classify("Showing 1 to 10 of 56 entries"), "count")
        self.assertEqual(volatility.classify("Inbox (3)"), "counter")
        self.assertEqual(volatility.classify("Due 2026-09-20", today), "near_date")
        self.assertIsNone(volatility.classify("Born 1980-02-03", today))
        self.assertIsNone(volatility.classify("Invoices"))

    def test_only_a_moment(self):
        self.assertTrue(volatility.only_a_moment("10:42"))
        self.assertTrue(volatility.only_a_moment("Mon 14 Sep, 10:42"))
        self.assertFalse(volatility.only_a_moment("Saved at 10:42"))
        self.assertFalse(volatility.only_a_moment("Invoices"))
        self.assertEqual(volatility.without_counters("Inbox (3) - Mail"), "Inbox - Mail")

    def test_a_clock_ticking_is_not_a_change(self):
        clock = {"role": "text", "name": "10:41", "value": "10:41", "states": [], "path": "panel > clock"}
        before = {"elements": [clock, field("textbox", "Password", value="")], "windows": [], "active_title": "A"}
        after = {"elements": [dict(clock, name="10:42", value="10:42"), field("textbox", "Password", value="•••")],
                 "windows": [], "active_title": "A"}

        change = core.compare(before, after)
        summary = core.summarize(change)

        self.assertNotIn("10:4", summary)
        self.assertIn('textbox "Password" value changed', summary)
        self.assertEqual(core.expectations(change)["values"], [])

    def test_a_forms_answer_is_read_back_whole(self):
        # OrangeHRM after Save on a form with a badly typed date: a close button
        # and two words of text. Only the button used to be named.
        save = field("button", "Save")
        before = {"elements": [save], "windows": [], "active_title": "A"}
        appeared = [
            {"role": "button", "name": "\u00d7", "value": "", "states": [], "path": "form > button x"},
            {"role": "text", "name": "Should be a valid date in yyyy-dd-mm format", "value": "", "states": [], "path": "form > text d"},
            {"role": "text", "name": "Required", "value": "", "states": [], "path": "form > text r"},
        ]
        change = core.compare(before, dict(before, elements=[save] + appeared))
        summary = core.summarize(change)
        self.assertIn("Should be a valid date in yyyy-dd-mm format", summary)
        self.assertIn('"Required"', summary)
        self.assertNotIn("more)", summary)
        self.assertLess(summary.index("valid date"), summary.index("\u00d7"))

    def test_a_new_page_is_cut_to_its_messages_and_controls(self):
        before = {"elements": [], "windows": [], "active_title": "A"}
        many = [{"role": "text", "name": f"row {i}", "value": "", "states": [], "path": f"t {i}"} for i in range(40)]
        many.append({"role": "alert", "name": "Record created", "value": "", "states": [], "path": "alert"})
        summary = core.summarize(core.compare(before, dict(before, elements=many)))
        self.assertIn('alert "Record created"', summary)
        self.assertNotIn("row 39", summary)


class Store(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, recording.MAX_BYTES
        recording.ROOT = Path(self.tmp.name)

    def tearDown(self):
        recording.ROOT, recording.MAX_BYTES = self.saved
        self.tmp.cleanup()

    def frame(self, fill):
        return screen.Frame(4, 4, bytearray(bytes([fill]) * 48))

    def test_events_are_numbered_on_the_session_clock_and_frames_stored_once(self):
        a, b = self.frame(1), self.frame(2)
        first = recording.append("task-t0123456789abcdef", {"kind": "click", "actor": "agent"}, before=a, after=b,
                                 defaults={"title": "pay invoice"})
        second = recording.append("task-t0123456789abcdef", {"kind": "type", "actor": "human"}, before=b, after=a)

        self.assertEqual((first["i"], second["i"]), (0, 1))
        self.assertEqual(first["after"], second["before"])
        self.assertEqual(len(list((recording.ROOT / "task-t0123456789abcdef" / "frames").glob("*.png"))), 2)

        meta, events = recording.load("task-t0123456789abcdef")
        self.assertEqual((meta["title"], meta["event_count"], meta["actors"], meta["screen"]),
                         ("pay invoice", 2, ["agent", "human"], [4, 4]))
        self.assertEqual([e["kind"] for e in events], ["click", "type"])

    def test_session_ids_cannot_leave_the_store(self):
        for bad in ("../etc", "task-../x", "demo-a/b", ""):
            with self.assertRaises(ValueError):
                recording.load(bad)
        recording.append("adhoc-20260914", {"kind": "click"}, before=self.frame(3))
        self.assertIsNone(recording.frame_file("adhoc-20260914", "../meta.json"))

    def test_export_is_the_recording_layout(self):
        recording.append("demo-20260914T101500-abc123", {"kind": "click"}, before=self.frame(1))
        buffer = io.BytesIO()
        recording.export("demo-20260914T101500-abc123", buffer, extra={"program.json": b"{}"})

        names = zipfile.ZipFile(io.BytesIO(buffer.getvalue())).namelist()
        prefix = "demo-20260914T101500-abc123/"
        for expected in ("README.txt", "meta.json", "events.jsonl", "program.json"):
            self.assertIn(prefix + expected, names)
        self.assertTrue(any(n.startswith(prefix + "frames/") for n in names))

    def test_pruning_never_touches_a_demonstration(self):
        recording.append("demo-20260101T000000-aaaaaa", {"kind": "click"}, before=self.frame(1), at=1000)
        recording.append("task-t00000000000000aa", {"kind": "click"}, before=self.frame(2), at=2000)
        recording.MAX_BYTES = 1
        recording.append("task-t00000000000000bb", {"kind": "click"}, before=self.frame(3), at=3000)

        left = {m["id"] for m in recording.sessions()}
        self.assertEqual(left, {"demo-20260101T000000-aaaaaa", "task-t00000000000000bb"})

    def test_the_old_audit_trail_is_imported_without_its_secrets(self):
        audit = Path(self.tmp.name) / "audit"
        (audit / "frames").mkdir(parents=True)
        shot = audit / "frames" / "x-before.png"
        shot.write_bytes(self.frame(9).png())
        entry = {"kind": "type", "text": "hunter2", "element": field("textbox", "Password"), "task_id": "t0123456789abcdef",
                 "time": "2026-09-13T10:00:00+0000", "summary": 'type "hunter2" into textbox', "before_frame": str(shot)}
        (audit / "actions.jsonl").write_text(json.dumps(entry) + "\n")

        self.assertEqual(recording.import_legacy_audit(audit), 1)

        _, events = recording.load("task-t0123456789abcdef")
        self.assertIsNone(events[0]["text"])
        self.assertNotIn("hunter2", json.dumps(events))
        self.assertTrue(events[0]["before"].startswith("frames/"))
        self.assertFalse((audit / "actions.jsonl").exists())


def event(i, kind, element=None, **extra):
    base = {"i": i, "kind": kind, "outcome": "done", "actor": "human", "element": element,
            "window": "Shop - Chromium", "url": "http://shop.test/", "screen": [1280, 720]}
    base.update(extra)
    return base


META = {"id": "demo-20260914T101500-abc123", "title": "Pay an invoice", "kind": "demonstration",
        "started_at": 1789380000, "actors": ["human"], "screen": [1280, 720]}

EVENTS = [
    event(0, "click", field("link", "Invoices"), expect={"url": "http://shop.test/invoices/1043",
                                                          "appeared": [{"role": "heading", "name": "Invoice INV-1043"},
                                                                       {"role": "dialog", "name": "Invoice"}]}),
    event(1, "type", field("textbox", "Customer name"), text="Acme",
          expect={"values": [{"role": "textbox", "name": "Customer name", "value": "Acme"}]}),
    event(2, "type", field("textbox", "Password", secret="password field"), text=None, withheld={"text": "password field"}),
    event(3, "click", field("button", "Pay now"), outcome="needs_approval"),
    event(4, "click", field("button", "Pay now"), approved=True, expect={"appeared": [{"role": "alert", "name": "Paid"}]}),
    event(5, "middle_click", None, point=[5, 5]),
]


class FakeScreen:
    """core, as a replay sees it: a scripted list of observations and a log of actions."""

    def __init__(self, elements, after=None, approval_on=None):
        self.elements = elements
        self.after = after or {}
        self.approval_on = approval_on
        self.acted = []

    def observation(self):
        return {"elements": self.elements, "windows": [{"title": "Shop - Chromium"}], "active_title": "Shop - Chromium",
                "size": [1280, 720], "page_url": self.after.get("url", "http://shop.test/")}

    def observe(self, remember=True):
        return self.observation()

    def active_group(self, observation):
        return "g"

    def act(self, kind, **kwargs):
        if self.approval_on and kwargs.get("path") == self.approval_on:
            return {"status": "needs_approval", "text": "NEEDS_APPROVAL: Approval needed before I click", "observation": None}
        self.acted.append((kind, kwargs))
        if kind == "type":
            for element in self.elements:
                if element["path"] == kwargs.get("path"):
                    element["value"] = kwargs["text"]
        for element in self.after.get("adds", []):
            if element not in self.elements:
                self.elements.append(element)
        return {"status": "done", "text": "ok", "observation": self.observation()}


class Replay(unittest.TestCase):

    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = 0
        replay.POLL_SECONDS = 0

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved

    def run_with(self, fake, program, **kwargs):
        replay.core = fake
        return replay.run(program, report=lambda line: None, **kwargs)

    def test_compiling_keeps_what_ran_and_names_its_parameters(self):
        program = replay.compile_program(META, EVENTS)

        self.assertEqual([s["kind"] for s in program["steps"]], ["click", "type", "type", "click", "middle_click"])
        self.assertEqual([(p["name"], p["default"], p["secret"]) for p in program["params"]],
                         [("customer_name", "Acme", False), ("password", None, True)])
        self.assertTrue(program["steps"][3]["consequential"])
        # The invoice number and its heading will differ next time; the page shape and the dialog will not.
        self.assertEqual(program["steps"][0]["expect"], [
            {"type": "url", "value": "http://shop.test/invoices/*"},
            {"type": "element", "role": "dialog", "name": "Invoice"},
        ])

    def test_window_titles_and_urls_are_compared_by_their_stable_parts(self):
        self.assertEqual(replay._stable_title("Untitled 2 - LibreOffice Calc"), "LibreOffice Calc")
        self.assertTrue(replay.title_matches("LibreOffice Calc", "Untitled 3 - LibreOffice Calc"))
        self.assertFalse(replay.title_matches("LibreOffice Writer", "Untitled 3 - LibreOffice Calc"))
        self.assertTrue(replay.url_matches("http://shop.test/invoices/*", "http://shop.test/invoices/1044/"))
        self.assertFalse(replay.url_matches("http://shop.test/invoices/*", "http://shop.test/login"))

    def test_a_target_is_found_by_path_then_role_and_name_and_never_guessed(self):
        replay.core = FakeScreen([])
        step = {"target": {"role": "button", "name": "Save", "path": "old path"}}

        found, _, _ = replay.resolve(step, {"elements": [field("button", "Save")]})
        self.assertEqual(found["name"], "Save")

        two = [field("button", "Save"), field("button", "Save", path="elsewhere")]
        found, _, why = replay.resolve(step, {"elements": two})
        self.assertIsNone(found)
        self.assertIn("2 elements match", why)

        _, point, why = replay.resolve({"point": [5, 5], "screen": [800, 600]}, {"elements": [], "size": [1280, 720]})
        self.assertIsNone(point)
        self.assertIn("different size", why)

    def test_a_run_with_every_result_seen_is_verified(self):
        program = replay.compile_program(META, EVENTS[:2])
        fake = FakeScreen([field("link", "Invoices"), field("textbox", "Customer name")],
                          after={"url": "http://shop.test/invoices/1044", "adds": [field("dialog", "Invoice")]})

        result = self.run_with(fake, program, params={"customer_name": "Globex"})

        self.assertEqual(result["outcome"], replay.VERIFIED, result["text"])
        self.assertEqual(fake.acted[1][1]["text"], "Globex")
        self.assertEqual(program["steps"][1]["expect"],
                         [{"type": "value", "role": "textbox", "name": "Customer name", "param": "customer_name"}])

    def test_a_field_that_did_not_take_the_text_halts(self):
        program = replay.compile_program(META, EVENTS[1:2])
        fake = FakeScreen([field("textbox", "Customer name")])
        fake.act = lambda kind, **kwargs: {"status": "done", "text": "ok", "observation": fake.observation()}

        result = self.run_with(fake, program)

        self.assertEqual((result["outcome"], result["resume_from"]), (replay.HALTED, 2))
        self.assertIn("holding {customer_name}", result["text"])

    def test_a_missing_secret_halts_before_anything_happens(self):
        fake = FakeScreen([field("link", "Invoices")])
        result = self.run_with(fake, replay.compile_program(META, EVENTS))

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(result["missing"], ["password"])
        self.assertEqual(fake.acted, [])

    def test_a_target_that_is_not_there_halts_the_run(self):
        result = self.run_with(FakeScreen([]), replay.compile_program(META, EVENTS[:2]))
        self.assertEqual((result["outcome"], result["step"], result["resume_from"]), (replay.HALTED_BEFORE_EFFECT, 1, 1))
        self.assertIn("is not on the screen", result["text"])

    def test_a_consequential_step_whose_result_never_appears_needs_reconciling(self):
        program = replay.compile_program(META, [EVENTS[4]])
        result = self.run_with(FakeScreen([field("button", "Pay now")]), program)

        self.assertEqual(result["outcome"], replay.RECONCILIATION_REQUIRED)
        self.assertIsNone(result["resume_from"])
        self.assertIn("will not retry", result["text"])

    def test_an_approval_pause_resumes_at_the_same_step(self):
        pay = field("button", "Pay now")
        program = replay.compile_program(META, [EVENTS[1], EVENTS[4]])
        result = self.run_with(FakeScreen([field("textbox", "Customer name"), pay], approval_on=pay["path"]), program)

        self.assertEqual((result["outcome"], result["resume_from"]), (replay.NEEDS_APPROVAL, 2))
        self.assertFalse(result["text"].startswith("NEEDS_APPROVAL"))

    def test_an_action_a_replay_cannot_do_halts_rather_than_being_skipped(self):
        program = replay.compile_program(META, [EVENTS[5]])
        result = self.run_with(FakeScreen([]), program)
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("cannot middle click", result["text"])

    def test_render_text_ends_with_how_to_run_it(self):
        text = replay.render_text(META, replay.compile_program(META, EVENTS))
        self.assertIn("password (secret - must be given)", text)
        self.assertIn("[stops for approval]", text)
        self.assertTrue(text.endswith("[--from N]"))

    def test_replay_requests_are_parsed(self):
        self.assertEqual(replay_task.parse("/replay demo-x from=3 customer=\"Acme Ltd\""),
                         ("demo-x", {"customer": "Acme Ltd"}, 3))
        self.assertIsNone(replay_task.parse("please replay demo-x"))


class TypingIntoAField(unittest.TestCase):
    """Whether a demonstration's typing replaces the field on replay, or adds to what is in it."""

    def entry(self, value, typed="Initech", role="textbox"):
        from decurix_agent.computer import demonstration

        element = {"ref": "e2", "role": role, "name": "Customer name", "value": value, "states": ["focused"],
                   "box": [1, 1, 10, 10], "path": 'site file > textbox "Customer name"', "source": "cdp"}
        action = {"kind": "type", "text": typed, "t": 1.0, "t_end": 2.0}
        return demonstration.Recorder.__new__(demonstration.Recorder)._entry(action, element, None, None)

    def test_a_field_that_reads_as_what_was_typed_was_empty_before(self):
        # The look at the screen came after the typing: the field holds exactly the text.
        self.assertFalse(self.entry("Initech")["append"])
        self.assertFalse(self.entry("  Initech ")["append"])

    def test_a_field_seen_empty_was_empty(self):
        self.assertFalse(self.entry("")["append"])

    def test_a_field_that_already_had_something_is_added_to(self):
        self.assertTrue(self.entry("Acme Initech")["append"])
        self.assertTrue(self.entry("Init", typed="ech")["append"])

    def test_something_that_is_not_a_text_field_is_never_treated_as_empty(self):
        self.assertTrue(self.entry("Initech", role="paragraph")["append"])


@unittest.skipIf(os.name == "nt", "the X recorder's startup; a Windows worker's recorder is in test_windows_computer")
class RecorderStartup(unittest.TestCase):
    def test_ready_before_slow_initial_screen_read_and_keeps_queued_input(self):
        from decurix_agent.computer import demonstration

        with tempfile.TemporaryDirectory() as directory:
            ready = Path(directory) / "ready"
            with mock.patch.object(demonstration.recording, "meta", return_value={}), \
                 mock.patch.object(demonstration.recording, "update"), \
                 mock.patch.object(demonstration.session, "ensure"), \
                 mock.patch.object(demonstration.signal, "signal"), \
                 mock.patch.object(demonstration.progress, "flush"), \
                 mock.patch.object(demonstration, "READY_FILE", ready), \
                 mock.patch.object(demonstration.Recorder, "_select_input"), \
                 mock.patch.object(demonstration.Recorder, "_read_input"), \
                 mock.patch.object(demonstration.Recorder, "_process"):
                recorder = demonstration.Recorder("demo-test")
                recorder.agent_devices = set()

                def observe(**kwargs):
                    self.assertEqual(ready.read_text(), str(demonstration.os.getpid()))
                    recorder.raw.put({"type": "press"})
                    return {"frame": object()}

                with mock.patch.object(demonstration.core, "observe", side_effect=observe):
                    self.assertEqual(recorder.run(), 0)
                self.assertIsNone(recorder.latest)

class RecordingClickTargets(unittest.TestCase):
    def test_a_browser_hit_test_recovers_a_named_control_when_its_cached_box_misses(self):
        from decurix_agent.computer import demonstration

        frame = screen.Frame(2, 2, bytes(12))
        button = {
            "role": "button", "name": "Submit", "value": "", "states": [],
            "box": [1200, 370, 80, 40], "source": "cdp", "group": "g",
            "window_title": "ChatGPT - Chromium", "path": "site chat > button Submit",
        }
        recorder = demonstration.Recorder.__new__(demonstration.Recorder)
        observation = {"frame": frame, "elements": [], "active_title": "ChatGPT - Chromium"}
        action = {"kind": "click", "point": [1248, 390]}

        with (
            mock.patch.object(demonstration.cdp, "available", return_value=True),
            mock.patch.object(
                demonstration.cdp, "element_at_screen_point", return_value=button
            ) as hit,
        ):
            found, note = recorder._element(action, observation, frame, (False, None))

        self.assertIs(found, button)
        self.assertIsNone(note)
        hit.assert_called_once_with(1248, 390, [])


class FakeRecorder:
    """
    A minimal stand-in for computer/demonstration.py's start/status/pause/
    resume/stop, stateful enough that record_task.perform()'s background poll
    (which treats demonstration.status() going None on its own as the
    recording having ended outside record_task.end()) behaves the way the
    real recorder would: status() answers None only once stop() has run.
    """

    def __init__(self, events, session_id="demo-20260914T101500-abc123"):
        self.events = events
        self.session_id = session_id
        self.active = False
        self.paused = False

    def start(self, title="", description="", task_id=""):
        self.active = True
        self.paused = False
        recording.ensure(self.session_id, title=title, task_id=task_id)
        for event in self.events:
            recording.append(self.session_id, event)
        return recording.meta(self.session_id)

    def status(self):
        if not self.active:
            return None
        return {"session": self.session_id, "paused": self.paused, "events": len(self.events)}

    def pause(self, reason=""):
        self.paused = True
        return recording.meta(self.session_id)

    def resume(self):
        self.paused = False
        return recording.meta(self.session_id)

    def stop(self):
        # In the real recorder's order: the recording is marked complete, and
        # only then is the recorder gone. The other way round, record_task's
        # poll could see "gone" before "complete" (it did, on Windows, where
        # writing the file takes longer) and report a recording cut short.
        recording.update(self.session_id, status="complete", ended_at=time.time())
        self.active = False
        return recording.meta(self.session_id)

    def note(self, text):
        recording.append(self.session_id, {"actor": "human", "kind": "note", "outcome": "done", "text": text,
                                            "summary": text})
        self.events.append({"kind": "note", "actor": "human"})
        return recording.meta(self.session_id)


class Note(unittest.TestCase):
    """
    A "reason" is a person's own written explanation of what they are doing,
    recorded at its moment in the timeline - never an action, so never
    replayed, and checked for a credential the way a typed field's text is.
    """

    SESSION = "demo-20260914T190000-abcdef"

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved_root = recording.ROOT
        recording.ROOT = Path(self.tmp.name)
        recording.ensure(self.SESSION, title="test")

    def tearDown(self):
        recording.ROOT = self.saved_root
        self.tmp.cleanup()

    def active(self, paused=False):
        return {"session": self.SESSION, "pid": 0, "started_at": time.time(), "paused": paused}

    def observation(self, frame=None):
        return {"active_title": "Shop - Chromium", "elements": [], "frame": frame}

    def patch_pause(self):
        """
        note() pauses on its own once it has written the note - the real
        pause() signals a recorder subprocess by pid, which these tests have
        none of (a real pid of 0 would signal this test runner's whole
        process group). Records what it was called with instead.
        """

        self.pause_calls = []
        return mock.patch.object(record_task.demonstration, "pause", self.pause_calls.append)

    def test_a_note_is_recorded_at_this_moment_with_a_frame(self):
        frame = screen.Frame(2, 2, bytearray(12))

        with mock.patch.object(record_task.demonstration, "_running", lambda: self.active()), \
                mock.patch.object(core, "observe", lambda remember=False: self.observation(frame)), \
                self.patch_pause():
            event = record_task.demonstration.note("clicking Skip - this account has no invoice yet")

        self.assertEqual(event["kind"], "note")
        self.assertEqual(event["actor"], "human")
        self.assertIn("clicking Skip", event["text"])
        self.assertIn("before", event)

        _, events = recording.load(self.SESSION)
        self.assertEqual(events[-1]["kind"], "note")

        # Writing a reason pauses the recording, with the note itself as why.
        self.assertEqual(len(self.pause_calls), 1)
        self.assertIn("clicking Skip", self.pause_calls[0])

    def test_a_note_that_looks_like_a_credential_is_withheld(self):
        with mock.patch.object(record_task.demonstration, "_running", lambda: self.active()), \
                mock.patch.object(core, "observe", lambda remember=False: self.observation()), \
                self.patch_pause():
            event = record_task.demonstration.note("the API key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz")

        self.assertIsNone(event["text"])
        self.assertIn("API key", event["withheld"]["text"])

        # The pause reason is the note's own (safe) summary, never the raw text.
        self.assertEqual(len(self.pause_calls), 1)
        self.assertNotIn("sk-ant-api03", self.pause_calls[0])
        self.assertIn("API key", self.pause_calls[0])

    def test_a_note_can_be_added_while_already_paused(self):
        # The toolbar pauses before it ever opens the text box, so this is
        # the ordinary case, not an edge case: a note written during a pause
        # must not be refused, and must leave the recording paused rather
        # than resuming it - only an explicit resume() (Continue) does that,
        # since the pause it was added to may predate the note and be about
        # to cover something the note itself must not.
        with mock.patch.object(record_task.demonstration, "_running", lambda: self.active(paused=True)), \
                mock.patch.object(core, "observe", lambda remember=False: self.observation()), \
                self.patch_pause():
            event = record_task.demonstration.note("signing in now")

        self.assertEqual(event["kind"], "note")
        self.assertEqual(len(self.pause_calls), 1)
        self.assertIn("signing in now", self.pause_calls[0])

    def test_a_blank_note_is_refused(self):
        with mock.patch.object(record_task.demonstration, "_running", lambda: self.active()):
            self.assertRaises(ComputerError, record_task.demonstration.note, "   ")

    def test_a_note_is_not_compiled_into_a_replay_step(self):
        recording.append(self.SESSION, {"kind": "click", "actor": "human", "outcome": "done"})
        recording.append(self.SESSION, {"kind": "note", "actor": "human", "outcome": "done", "text": "why"})

        meta, events = recording.load(self.SESSION)
        program = replay.compile_program(meta, events)

        self.assertEqual([s["kind"] for s in program["steps"]], ["click"])

    def test_a_note_shows_up_in_the_chat_of_the_task_it_belongs_to(self):
        # A ticket's session carries the task_id; an ad-hoc demonstration's
        # (self.SESSION, from setUp's plain ensure()) does not.
        with_task = "demo-20260914T190100-taskyes"
        recording.ensure(with_task, title="test", task_id="t0123456789abcdef")

        calls = []

        with mock.patch.object(record_task.demonstration, "_running", lambda: {**self.active(), "session": with_task}), \
                mock.patch.object(core, "observe", lambda remember=False: self.observation()), \
                mock.patch.object(record_task.demonstration.progress, "emit_action",
                                  lambda text, action, before=None, after=None, task_id=None:
                                  calls.append((text, action["kind"], task_id))), \
                self.patch_pause():
            record_task.demonstration.note("checking the fallback path first")

        self.assertEqual(calls, [('"checking the fallback path first"', "note", "t0123456789abcdef")])

        # No task_id on the session (an ad-hoc demonstration) - nothing to report.
        with mock.patch.object(record_task.demonstration, "_running", lambda: self.active()), \
                mock.patch.object(core, "observe", lambda remember=False: self.observation()), \
                mock.patch.object(record_task.demonstration.progress, "emit_action",
                                  lambda *a, **k: calls.append("should not happen")), \
                self.patch_pause():
            record_task.demonstration.note("an ad-hoc note")

        self.assertEqual(len(calls), 1)


class RecordTask(unittest.TestCase):
    """
    "/record <task>" opens a ticket: a person doing that task, recorded,
    never handed to an engine. Opening one does not itself start capturing -
    begin() does, so record_task.perform() (the task the watcher dispatches)
    spends most of its life blocked waiting for end() while a test drives the
    ticket from the "main" thread, the way the GUI toolbar drives it over
    HTTP.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved_root = recording.ROOT
        recording.ROOT = Path(self.tmp.name)
        self.saved_state = state.current_task, state.current_task_id
        state.current_task_id = "t0123456789abcdef"
        record_task._ticket = None
        record_task._ended.clear()

    def tearDown(self):
        recording.ROOT = self.saved_root
        state.current_task, state.current_task_id = self.saved_state
        self.tmp.cleanup()

    def test_record_requests_are_parsed(self):
        self.assertEqual(record_task.parse("/record fill in the invoice form"), "fill in the invoice form")
        self.assertEqual(record_task.parse("  /record  "), "")
        self.assertIsNone(record_task.parse("/recordings"))
        self.assertIsNone(record_task.parse("please /record this"))

    def run_ticket(self, drive, content="/record fill in the invoice form"):
        """
        Run perform() on a background thread, call drive() once the ticket
        appears, then join - the mocks stay active for the thread's whole
        life, not just while drive() runs. Returns the finals write_final_reply
        was called with.
        """

        finals = []

        with mock.patch.object(record_task, "POLL_SECONDS", 0.02), \
                mock.patch.object(record_task.progress, "emit"), \
                mock.patch.object(record_task, "write_final_reply",
                                  lambda reply, kind="final", engine=None, quiet=False:
                                  finals.append((reply, kind, engine, quiet))):
            thread = threading.Thread(target=record_task.perform, args=(content,))
            thread.start()

            deadline = time.monotonic() + 2
            while record_task.status() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertIsNotNone(record_task.status(), "the ticket never appeared")

            drive()

            thread.join(timeout=2)
            self.assertFalse(thread.is_alive(), "perform() did not finish")

        return finals

    def test_opening_a_ticket_does_not_start_capturing(self):
        seen = {}

        def drive():
            seen.update(record_task.status())
            record_task.end()

        finals = self.run_ticket(drive)

        self.assertEqual(seen["title"], "fill in the invoice form")
        self.assertEqual(seen["task_id"], "t0123456789abcdef")
        self.assertIsNone(seen["session_id"])
        self.assertFalse(seen["recording"])
        self.assertIsNone(record_task.status(), "the ticket outlived end()")

        self.assertEqual(len(finals), 1)
        reply, kind, engine, _ = finals[0]
        self.assertEqual((kind, engine), ("error", "human"))
        self.assertIn("nothing was recorded", reply)

    def test_begin_pause_resume_and_end_report_the_recording(self):
        fake = FakeRecorder([{"kind": "click", "actor": "human"}, {"kind": "type", "actor": "human"}])

        def drive():
            with mock.patch.object(record_task.demonstration, "start", fake.start), \
                    mock.patch.object(record_task.demonstration, "status", fake.status), \
                    mock.patch.object(record_task.demonstration, "pause", fake.pause), \
                    mock.patch.object(record_task.demonstration, "resume", fake.resume), \
                    mock.patch.object(record_task.demonstration, "stop", fake.stop):
                started = record_task.begin()
                self.assertEqual(started["session_id"], fake.session_id)
                self.assertTrue(started["recording"])

                record_task.pause("stepping away")
                self.assertTrue(record_task.status()["paused"])

                record_task.resume()
                self.assertFalse(record_task.status()["paused"])

                record_task.end()

        finals = self.run_ticket(drive)
        self.assertIsNone(record_task.status())

        self.assertEqual(len(finals), 1)
        reply, kind, engine, quiet = finals[0]
        self.assertEqual((kind, engine), ("final", "human"))
        self.assertIn("Recorded 2 steps", reply)
        self.assertIn(fake.session_id, reply)
        self.assertTrue(quiet, "a finished recording must not put a note on the Query rail")

    def test_note_is_added_at_this_moment_while_recording(self):
        # record_task.note()'s own gate is the same "has recording started"
        # check pause()/resume() use; whether it is *paused* right now is
        # demonstration.note()'s own concern (Note.test_a_note_can_be_added_while_already_paused).
        fake = FakeRecorder([{"kind": "click", "actor": "human"}])

        def drive():
            with mock.patch.object(record_task.demonstration, "start", fake.start), \
                    mock.patch.object(record_task.demonstration, "status", fake.status), \
                    mock.patch.object(record_task.demonstration, "note", fake.note), \
                    mock.patch.object(record_task.demonstration, "stop", fake.stop):
                # No ticket has begun recording yet - nothing to attach a note to.
                self.assertRaises(ComputerError, record_task.note, "getting ready")

                record_task.begin()
                record_task.note("skipping the confirmation dialog - it's a test amount")
                self.assertEqual(record_task.status()["events"], 2)

                record_task.end()

        finals = self.run_ticket(drive)
        self.assertIn("Recorded 2 steps", finals[0][0])

    def test_ending_a_recording_with_nothing_in_it_is_not_a_success(self):
        fake = FakeRecorder([])

        def drive():
            with mock.patch.object(record_task.demonstration, "start", fake.start), \
                    mock.patch.object(record_task.demonstration, "status", fake.status), \
                    mock.patch.object(record_task.demonstration, "stop", fake.stop):
                record_task.begin()
                record_task.end()

        finals = self.run_ticket(drive)
        self.assertEqual(finals[0][1], "error")
        self.assertIn("nothing in it", finals[0][0])
        self.assertFalse(finals[0][3], "a failed recording still tells the Query rail")

    def test_a_recording_that_ends_outside_end_is_still_reported(self):
        # The recordings page's own Stop, or a crashed recorder - either way
        # nothing ever calls record_task.end() for this recording.
        fake = FakeRecorder([{"kind": "click", "actor": "human"}])

        def drive():
            with mock.patch.object(record_task.demonstration, "start", fake.start), \
                    mock.patch.object(record_task.demonstration, "status", fake.status):
                record_task.begin()
                fake.stop()
                deadline = time.monotonic() + 2
                while record_task.status() is not None and time.monotonic() < deadline:
                    time.sleep(0.01)

        finals = self.run_ticket(drive)
        self.assertEqual(len(finals), 1)
        self.assertEqual(finals[0][1], "final")
        self.assertIn("Recorded 1 step", finals[0][0])

    def test_begin_and_pause_without_an_open_ticket_are_refused(self):
        self.assertIsNone(record_task.status())
        self.assertRaises(ComputerError, record_task.begin)
        self.assertRaises(ComputerError, record_task.pause)
        self.assertRaises(ComputerError, record_task.resume)
        self.assertRaises(ComputerError, record_task.end)


if __name__ == "__main__":
    unittest.main()


class PauseWithNothingInIt(unittest.TestCase):
    """A pause that ends on the screen it began on held no private step - a Reason being written, or Stop and Continue
    with nothing between - so resuming records no "manual" step for a run to stop at."""

    def recorder(self, before_rgb):
        from decurix_agent.computer import demonstration

        rec = object.__new__(demonstration.Recorder)
        rec.session_id = "demo-test"
        rec.task_id = ""
        rec.latest = None
        rec._pause = {"reason": "a note", "frame": screen.Frame(64, 32, before_rgb), "masks": [], "window": "Request"}
        rec._report = mock.Mock()
        return rec

    def resume_onto(self, before_rgb, after_rgb):
        rec = self.recorder(before_rgb)
        after = {"frame": screen.Frame(64, 32, after_rgb), "elements": [], "active_title": "Request", "size": [64, 32]}
        with mock.patch.object(core, "_wait_for_settle"), mock.patch.object(core, "observe", return_value=after), \
                mock.patch.object(recording, "append", return_value={"i": 7}) as append:
            rec._resumed()
        return rec, append

    def test_a_pause_over_an_unchanged_screen_records_nothing(self):
        same = bytes([200, 200, 200]) * (64 * 32)
        rec, append = self.resume_onto(same, same)
        append.assert_not_called()
        rec._report.assert_not_called()
        self.assertIsNotNone(rec.latest, "the screen after the pause is what the next action is compared with")

    def test_a_pause_over_which_the_screen_changed_is_a_private_step(self):
        before = bytes([200, 200, 200]) * (64 * 32)
        after = bytes([20, 20, 20]) * (64 * 32)
        _, append = self.resume_onto(before, after)
        append.assert_called_once()
        self.assertEqual(append.call_args.args[1]["kind"], "manual")

    def test_typing_while_paused_is_a_private_step_however_little_it_shows(self):
        # GST suite (2026-10-21): a password typed during a pause is a few dots - under 1% of the screen - and no
        # manual step was recorded, so the learned skill logged in without one.
        same = bytes([200, 200, 200]) * (64 * 32)
        rec = self.recorder(same)
        rec._input_while_paused = rec._typed_while_paused = True
        after = {"frame": screen.Frame(64, 32, same), "elements": [], "active_title": "Request", "size": [64, 32]}
        with mock.patch.object(core, "_wait_for_settle"), mock.patch.object(core, "observe", return_value=after), \
                mock.patch.object(recording, "append", return_value={"i": 7}) as append:
            rec._resumed()
        append.assert_called_once()
        self.assertEqual(append.call_args.args[1]["kind"], "manual")

    def test_a_pause_with_no_input_from_the_person_is_no_private_step_however_the_screen_changed(self):
        # Found live (2026-10-14): a one-second pause for a Reason while a page was still loading.
        rec = self.recorder(bytes([200, 200, 200]) * (64 * 32))
        rec._input_while_paused = False
        after = {"frame": screen.Frame(64, 32, bytes([20, 20, 20]) * (64 * 32)), "elements": [], "active_title": "Request",
                 "size": [64, 32]}
        with mock.patch.object(core, "_wait_for_settle"), mock.patch.object(core, "observe", return_value=after), \
                mock.patch.object(recording, "append") as append:
            rec._resumed()
        append.assert_not_called()
        self.assertIs(rec.latest, after)


class APressReadLate(unittest.TestCase):
    """A busy desktop reads input late: the screen it then sees may already be what the press made (the web app's Save,
    read 20 s late, recorded as the "Submit" the saved form shows in its place)."""

    def recorder(self):
        from decurix_agent.computer import demonstration
        rec = demonstration.Recorder.__new__(demonstration.Recorder)
        rec._clock_offset = None
        return rec

    def test_how_late_is_measured_from_the_servers_own_timestamps(self):
        rec = self.recorder()
        self.assertEqual(rec._lag(1000.0, 5_000), 0.0)              # the first sets "on time"
        self.assertAlmostEqual(rec._lag(1001.0, 6_000), 0.0)         # read as it happened
        self.assertAlmostEqual(rec._lag(1025.0, 7_000), 23.0)        # read 23 s after it happened
        self.assertEqual(rec._lag(1030.0, 0), 0.0)                   # no timestamp: nothing to go on

    def test_a_late_press_is_not_named_from_a_look_taken_after_it(self):
        from decurix_agent.computer import demonstration
        submit = {"role": "button", "name": "Submit", "value": "", "states": [], "box": [1191, 97, 63, 28],
                  "source": "cdp", "path": "site x > button Submit"}
        frame = screen.Frame(2, 2, bytes(12))
        observation = {"frame": frame, "elements": [submit], "active_title": "PINV - Chromium", "seen_at": 470.0}

        late = {"kind": "click", "point": [1229, 111], "late": True, "happened": 449.0}
        found, note = self.recorder()._element(late, observation, frame, (False, None))
        self.assertIsNone(found)
        self.assertEqual(note, demonstration.LATE_NOTE)

        # On time, the same look names it as before.
        on_time = {"kind": "click", "point": [1229, 111], "happened": 471.0}
        found, note = self.recorder()._element(on_time, observation, frame, (False, None))
        self.assertIs(found, submit)

        # Late, but the look was taken before the press: still a good name.
        early_look = dict(observation, seen_at=440.0)
        found, note = self.recorder()._element(late, early_look, frame, (False, None))
        self.assertIs(found, submit)

    def test_a_late_step_gets_no_picture_to_look_for(self):
        from decurix_agent.computer import demonstration
        entry = demonstration.Recorder.__new__(demonstration.Recorder)._entry(
            {"kind": "click", "point": [1229, 111], "t": 1.0, "t_end": 1.0}, None, demonstration.LATE_NOTE, None)
        self.assertTrue(entry["late"])
        self.assertEqual(entry["element_missing"], demonstration.LATE_NOTE)


class ARecorderOnABusyDesktop(unittest.TestCase):
    """Found live (2026-10-14) on a worker with the recorder running behind: a click on Sign in after typing the
    password, and a click on a drop-down a second after typing into the field above it, were recorded as bare points;
    a click on the password field was recorded as a drag to the next button; and the password typed after it - into a
    field the recorder could not tell - was kept in plain text."""

    @staticmethod
    def frame(paint=()):
        rgb = bytearray(b"\xff" * (200 * 100 * 3))
        for x0, y0, w, h in paint:
            for y in range(y0, y0 + h):
                for x in range(x0, x0 + w):
                    rgb[(y * 200 + x) * 3:(y * 200 + x) * 3 + 3] = b"\x00\x00\x00"
        return screen.Frame(200, 100, rgb)

    def recorder(self):
        from decurix_agent.computer import demonstration
        return demonstration.Recorder.__new__(demonstration.Recorder)

    def test_a_control_whose_own_pixels_did_not_change_is_named_though_the_screen_did(self):
        from decurix_agent.computer import demonstration
        select = {"role": "combobox", "name": "Product line 1", "value": "", "states": [], "box": [100, 60, 80, 30],
                  "source": "cdp", "path": "site x > combobox Product line 1"}
        look = self.frame()
        observation = {"frame": look, "elements": [select], "active_title": "New order - Chromium"}
        # What was typed above it changed a fifth of the screen; the drop-down is drawn as it was (a focus ring on
        # its border does not count).
        pressed = self.frame(paint=[(0, 0, 200, 20), (100, 60, 80, 2)])
        self.assertGreater(screen.changed_fraction(look, pressed), demonstration.STALE_FRACTION)
        found, note = self.recorder()._element({"kind": "click", "point": [140, 75]}, observation, pressed, (False, None))
        self.assertIs(found, select)
        self.assertIsNone(note)

        # The control itself was drawn differently (another control in its place): still not named.
        relabelled = self.frame(paint=[(0, 0, 200, 20), (110, 66, 40, 18)])
        found, note = self.recorder()._element({"kind": "click", "point": [140, 75]}, observation, relabelled, (False, None))
        self.assertIsNone(found)
        self.assertIn("still changing", note)

    def test_a_password_typed_into_a_field_that_cannot_be_told_is_withheld_after_the_page_has_gone(self):
        rec = self.recorder()
        password = {"role": "textbox", "name": "Password", "box": [10, 40, 100, 20], "handle": {"input_type": "password"}}
        rec._seen({"seen_at": 100.0, "elements": [password]})                 # the sign-in page, before the typing
        rec._seen({"seen_at": 130.0, "elements": [{"role": "link", "name": "Try again", "box": [1, 1, 5, 5]}]})
        later = {"elements": [{"role": "link", "name": "Try again", "box": [1, 1, 5, 5]}], "active_title": "x"}
        entry = rec._entry({"kind": "type", "text": "Northwind#2026", "t": 110.0, "t_end": 111.0}, None, None, later)
        self.assertIsNone(entry["text"])
        self.assertIn("password field", entry["withheld"]["text"])

        # Typing done after the page with the password field had gone is kept.
        entry = rec._entry({"kind": "type", "text": "Cafe Bloom", "t": 140.0, "t_end": 141.0}, None, None, later)
        self.assertEqual(entry["text"], "Cafe Bloom")

    def test_frames_are_grabbed_off_the_input_thread_and_waited_for_when_actions_are_made(self):
        import concurrent.futures
        from decurix_agent.computer import demonstration
        rec = self.recorder()
        started = threading.Event()
        release = threading.Event()
        rec._grab = lambda: (started.set(), release.wait(5), "the frame")[2]
        rec._grabs = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        self.addCleanup(rec._grabs.shutdown)
        pending = rec._grab_later()               # returns at once: the reader goes on to the next event
        self.assertTrue(started.wait(5))
        self.assertFalse(pending.done())
        release.set()
        self.assertEqual(demonstration._with_frame({"type": "press", "frame": pending})["frame"], "the frame")
        self.assertEqual(demonstration._with_frame({"type": "release"}), {"type": "release"})

    def test_a_region_of_two_frames_is_compared_pixel_by_pixel(self):
        a, b = self.frame(), self.frame(paint=[(10, 10, 10, 10)])
        self.assertEqual(screen.changed_in(a, b, [50, 50, 20, 20]), 0.0)
        self.assertEqual(screen.changed_in(a, b, [10, 10, 10, 10]), 1.0)
        self.assertEqual(screen.changed_in(a, None, [10, 10, 10, 10]), 1.0)

    def test_typing_after_a_pause_goes_into_the_field_the_last_run_clicked(self):
        import queue
        from decurix_agent.computer import demonstration
        rec = self.recorder()
        rec.raw, rec.latest, rec.session_id, rec.task_id = queue.Queue(), None, "demo-x", ""
        field = {"role": "textbox", "name": "Customer", "value": "", "states": [], "box": [20, 20, 120, 30],
                 "source": "cdp", "path": "site x > textbox Customer"}
        look = {"frame": self.frame(), "elements": [field], "active_title": "New order - Chromium", "size": [200, 100],
                "seen_at": 1.0}
        rec.latest = look
        appended = []
        with mock.patch.object(demonstration.core, "_wait_for_settle", return_value=look["frame"]), \
                mock.patch.object(demonstration.core, "observe", return_value=dict(look)), \
                mock.patch.object(demonstration.core, "compare", return_value={}), \
                mock.patch.object(demonstration.core, "summarize", return_value=""), \
                mock.patch.object(demonstration.core, "expectations", return_value={}), \
                mock.patch.object(demonstration.recording, "append", lambda sid, entry, **kw: appended.append(entry) or entry), \
                mock.patch.object(demonstration, "_report_to_chat"):
            rec._flush([{"type": "press", "t": 2.0, "button": 1, "x": 50, "y": 35, "frame": look["frame"]},
                        {"type": "release", "t": 2.05, "button": 1, "x": 50, "y": 35}])
            # The person paused, then typed: a run of its own, whose first frame differs from the last look
            # (a suggestion list opened) - the field is still where the keys go.
            busy = self.frame(paint=[(0, 60, 200, 40)])
            rec._flush([{"type": "key_down", "t": 4.0, "key": "c", "char": "C", "frame": busy},
                        {"type": "key_up", "t": 4.05, "key": "c", "char": None}])
        self.assertEqual(appended[0]["element"]["name"], "Customer")
        self.assertEqual(appended[1]["kind"], "type")
        self.assertEqual((appended[1]["element"] or {}).get("name"), "Customer")
        self.assertEqual(appended[1]["text"], "C")
