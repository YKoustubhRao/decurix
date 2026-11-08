"""
A plan the person agreed to that says "each with your OK first" is kept (found live 2026-11-08): TheBot's operator puts
the plan's steps to the person with "Then, each with your OK first: Press Save bill", and sets DECURIX_PLAN_ASK_FIRST
on its gui commands; a committing press that is one of those steps is put to the approval gate (core._plan_hold), and
goes only on the gate's yes for that one action. A replay's own record of what its steps entered is given back however
it ends (replay.entered_value, cli.ENTERED_MARKER), so a halted run's figures are credited.
"""

import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval, cli, core, replay
from test_recordings import EVENTS, META, FakeScreen, field


class Pressed(Exception):
    """The press was made (core._perform reached)."""


def button(name, ref="e3"):
    return {"role": "button", "name": name, "value": "", "states": [], "box": [0, 0, 10, 10], "ref": ref,
            "path": f'site x > button "{name}"', "source": "cdp", "group": "g", "window_title": "Bills - Chromium",
            "handle": {"backend_id": 7}}


def page(*elements):
    return {"elements": list(elements), "groups": [{"id": "g", "window_title": "Bills - Chromium", "source": "cdp",
                                                    "url": "http://localhost/bills.html"}],
            "active_title": "Bills - Chromium", "frame": None, "size": [1280, 720], "windows": []}


def commit_check_passed(kind, element, keys, before, checked=False, text=None, listing=True, pressed=None):
    """_commit_check after the listing was confirmed (--checked): the checks pass, and the press commits."""

    if pressed is not None and element is not None:
        pressed.append(element)
    return ""


class APromisedPressWaitsForTheGate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        self.save = button("Save bill")
        self.perform = mock.Mock(side_effect=Pressed)
        for p in (mock.patch.object(approval, "STATE_DIR", self.tmp),
                  mock.patch.object(approval, "STATE_FILE", self.tmp / "approval.json"),
                  mock.patch.object(approval, "remote", lambda: False),
                  mock.patch.object(core, "FORM_CHECK_FILE", self.tmp / "form-check.json"),
                  mock.patch.object(core, "observe", lambda *a, **k: page(self.save)),
                  mock.patch.object(core.audit, "record", lambda *a, **k: None),
                  mock.patch.object(core, "_downloads_snapshot", lambda: None),
                  mock.patch.object(core, "_refuse_unsafe_save", lambda *a: None),
                  mock.patch.object(core, "_refuse_number_arrows", lambda *a: None),
                  mock.patch.object(core, "_commit_check", commit_check_passed),
                  mock.patch.object(core, "_what_it_sends", lambda *a, **k: [["Amount", "7350.50", False]]),
                  mock.patch.object(core, "_perform", self.perform)):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()

    def press(self, plan=None):
        env = {} if plan is None else {core.PLAN_ASK_FIRST_ENV: plan}
        with mock.patch.dict(os.environ, env, clear=False):
            if plan is None:
                os.environ.pop(core.PLAN_ASK_FIRST_ENV, None)
            return core.act("click", ref="e3", named=True, check_form="checked")

    def test_the_press_waits_for_the_yes_and_goes_after_it(self):
        held = self.press('["Save bill"]')
        self.assertEqual(held["status"], "needs_approval")
        self.assertIn('click button "Save bill"', held["text"])
        self.assertIn("with your OK first", held["text"])
        self.perform.assert_not_called()
        asked = approval.request_text(approval.pending())
        self.assertEqual(approval.resolve_reply("yes", asked=asked), "granted")
        with self.assertRaises(Pressed):
            self.press('["Save bill"]')
        # A yes is one action's: the next save is asked again.
        self.assertEqual(self.press('["Save bill"]')["status"], "needs_approval")

    def test_a_no_leaves_it_unpressed(self):
        self.press('["Save bill"]')
        asked = approval.request_text(approval.pending())
        self.assertNotEqual(approval.resolve_reply("no, not yet", asked=asked), "granted")
        self.assertEqual(self.press('["Save bill"]')["status"], "needs_approval")
        self.perform.assert_not_called()

    def test_without_a_plan_it_goes_as_before(self):
        with self.assertRaises(Pressed):
            self.press()
        with self.assertRaises(Pressed):
            self.press("[]")

    def test_which_presses_a_plan_holds(self):
        # Another step's control: this press is not one of the plan's.
        with self.assertRaises(Pressed):
            self.press('["Send invoice"]')
        # A step whose control the plan did not know, or a value that cannot be read: every committing press is held.
        for plan in ('[""]', '["Send invoice", ""]', "not json", '{"a": 1}'):
            with self.subTest(plan=plan):
                self.assertEqual(self.press(plan)["status"], "needs_approval")
                approval.reset()


class WhatAPlanHolds(unittest.TestCase):
    def hold(self, controls, name):
        with mock.patch.dict(os.environ, {core.PLAN_ASK_FIRST_ENV: json.dumps(controls)}):
            return core._plan_hold({"role": "button", "name": name})

    def test_a_control_named_either_way_as_a_whole_phrase(self):
        self.assertIsNotNone(self.hold(["Save bill"], "Save bill"))
        self.assertIsNotNone(self.hold(["Save bill"], "Save"))                  # the plan's names the press
        self.assertIsNotNone(self.hold(["Save"], "Save bill"))                  # the press names the plan's
        self.assertIsNone(self.hold(["Save bill"], "Save draft"))
        self.assertIsNotNone(self.hold(["Save bill"], ""))                      # a press with no name
        self.assertIsNone(core._plan_hold(None))                               # not a committing press

    def test_the_commit_check_says_what_a_press_commits_with(self):
        pressed = []
        with mock.patch.object(core.cdp, "submit_form_fields", lambda *a, **k: None), \
                mock.patch.object(core, "_form_check", lambda *a, **k: ""):
            core._commit_check("click", button("Save bill"), None, page(), checked=True, pressed=pressed)
            self.assertEqual([p["name"] for p in pressed], ["Save bill"])
            pressed.clear()
            core._commit_check("click", button("Help", ref="e4") | {"role": "link"}, None, page(), pressed=pressed)
            self.assertEqual(pressed, [])


    def test_the_screen_broker_passes_it_to_the_screen_side(self):
        # In broker mode the press is made on the screen side, which takes a caller's variable only when the agent's
        # code reads it by name and it is not the screen side's own (daemon.Broker.environment).
        from decurix_agent.computer import daemon
        self.assertIn(core.PLAN_ASK_FIRST_ENV, daemon.env_reads(Path(core.__file__).read_text(encoding="utf-8")))
        self.assertTrue(daemon.caller_may_set(core.PLAN_ASK_FIRST_ENV))


class AReplaysRecordOfWhatItEntered(unittest.TestCase):
    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved

    def run_with(self, fake, program, **kwargs):
        replay.core = fake
        return replay.run(program, report=lambda line: None, **kwargs)

    def test_a_halted_run_gives_back_what_its_earlier_steps_typed(self):
        program = replay.compile_program(META, [EVENTS[1], EVENTS[4]])
        result = self.run_with(FakeScreen([field("textbox", "Customer name")]), program,
                               params={"customer_name": "Globex Traders"})
        self.assertEqual(result["outcome"], replay.HALTED, result["text"])
        self.assertEqual(result["entered"], ["Globex Traders"])
        out = io.StringIO()
        with redirect_stdout(out):
            cli._print_replay(result, command="gui do pay-invoice")
        line = next(l for l in out.getvalue().splitlines() if l.startswith(cli.ENTERED_MARKER))
        self.assertEqual(json.loads(line[len(cli.ENTERED_MARKER):]), ["Globex Traders"])

    def test_a_run_held_for_approval_gives_them_back_too(self):
        pay = field("button", "Pay now")
        program = replay.compile_program(META, [EVENTS[1], EVENTS[4]])
        result = self.run_with(FakeScreen([field("textbox", "Customer name"), pay], approval_on=pay["path"]), program)
        self.assertEqual(result["outcome"], replay.NEEDS_APPROVAL)
        self.assertEqual(result["entered"], ["Acme"])
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(cli._print_replay(result), cli.EXIT_NEEDS_APPROVAL)
        self.assertIn(cli.ENTERED_MARKER + ' ["Acme"]', out.getvalue())

    def test_secret_and_empty_values_are_never_given_back(self):
        self.assertIsNone(replay.entered_value({"kind": "type", "param": "password"}, {"password": "x1"}, {"password"}))
        self.assertIsNone(replay.entered_value({"kind": "type", "param": "memo"}, {"memo": "  "}))
        self.assertIsNone(replay.entered_value({"kind": "click", "param": "memo"}, {"memo": "x"}))
        self.assertEqual(replay.entered_value({"kind": "select", "param": "state"}, {"state": "Karnataka"}), "Karnataka")
        self.assertEqual(cli._entered_line({"entered": []}), "")


if __name__ == "__main__":
    unittest.main()
