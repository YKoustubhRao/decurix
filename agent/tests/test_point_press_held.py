"""
A press at a point is the press of the control under it, with every check a click on that control gets; and a replayed
press whose target cannot be identified waits for a yes wherever "can't tell what's there" matters. Found
(2026-11-10): a lesson recorded the OTP form's "Verify" only as a coordinate click, and a replay of it - the look listing
nothing at the point, only the page naming the button - sent the person's OTP with no yes.

Each case drives core.act (check_form="replay" as replay._do_step presses, or True as an engine's `gui click x,y`);
only the screen and the page are mocked.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_point_press_held.py
"""

import contextlib
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval, audit, core

VERIFY = {"role": "button", "name": "Verify", "value": "", "states": [], "box": [30, 370, 60, 30], "ref": "e86",
          "path": 'site x > form "Validate one-time password" > button "Verify"', "source": "cdp", "group": "g",
          "window_title": "Validate one-time password - Portal - Chromium", "handle": {"backend_id": 86}}
OTP_FORM = {"form": "Validate one-time password", "fields": [["Enter the one-time password (OTP)", "482913"]],
            "submit": "Verify", "unset": [], "method": "post"}
PLAIN_FORM = {"form": "Notes", "fields": [["Note", "ok"]], "submit": "Next", "unset": [], "method": "post"}
NEXT = dict(VERIFY, name="Next", ref="e9", path='site x > button "Next"', handle={"backend_id": 9})


def taught(confirmed=True, ask=({"step": 82, "name": "File with EVC"}, {"step": 86}), consequential_86=False):
    """A lesson: File with EVC by name, step 86 (Verify) recorded as a bare point, and a plain step 90."""

    return {"name": "a-taught-task", "approvals": {"confirmed": confirmed, "ask": [dict(a) for a in ask]},
            "steps": [{"n": 82, "id": "s82", "kind": "click", "target": {"role": "button", "name": "File with EVC"}},
                      {"n": 86, "id": "s86", "kind": "click", "point": [54, 384], "consequential": consequential_86},
                      {"n": 90, "id": "s90", "kind": "click", "point": [600, 500]}]}


class _Press(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(approval, "STATE_DIR", Path(tmp.name)),
                  mock.patch.object(approval, "STATE_FILE", Path(tmp.name) / "approval.json"),
                  mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "form-check.json"),
                  mock.patch.object(approval, "_task_now", return_value="t-1")):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()

    def press(self, listed=False, under=VERIFY, form=OTP_FORM, secrets=("482913",), skill=None, step=86,
              check_form="replay", page_url="http://portal.test/gstr1/otp", kind="click", focused=None):
        skill = taught() if skill is None else skill
        screen = {"elements": [under] if (listed and under) else [], "page_url": page_url,
                  "active_title": VERIFY["window_title"], "frame": None, "size": [1280, 720], "windows": [],
                  "groups": [{"id": "g", "window_title": VERIFY["window_title"], "source": "cdp", "url": page_url}]}
        pressed = mock.Mock(side_effect=AssertionError("pressed with no yes"))
        patches = [
            mock.patch.object(core, "observe", lambda *a, **k: screen),
            mock.patch.object(core.audit, "record", lambda *a, **k: None),
            mock.patch.object(core, "_downloads_snapshot", lambda: None),
            mock.patch.object(core, "_perform", pressed),
            mock.patch.object(core.cdp, "available", return_value=True),
            mock.patch.object(core.cdp, "hit", return_value=(dict(under) if under else None, {"url": "x"})),
            mock.patch.object(core.cdp, "focused", return_value=(dict(focused) if focused else None, {"url": "x"})),
            mock.patch.object(core.cdp, "submit_form_fields", return_value=form),
            mock.patch.object(core.cdp, "enter_form_fields", return_value=None),
            mock.patch.object(core.cdp, "link_target", return_value=""),
            mock.patch.object(core.cdp, "page_has_keyboard", return_value=True),
            mock.patch("decurix_agent.computer.ocr.available", return_value=False),
            mock.patch("decurix_agent.task_context.current", return_value="t-1"),
            mock.patch("decurix_agent.task_context.skill", return_value={"name": skill["name"], "version": 1}),
            mock.patch("decurix_agent.computer.skills.load", return_value=skill),
            mock.patch("decurix_agent.computer.skills.all_skills", return_value=[skill]),
            mock.patch.object(core.privacy, "literals", return_value=list(secrets)),
            mock.patch.object(core, "_keyboard_field", return_value=None),
            mock.patch.object(core, "_unsourced_fields", lambda fields: []),
            mock.patch("decurix_agent.filler.invented", lambda *a, **k: []),
            mock.patch.object(core, "_ledger_check", lambda *a, **k: None),
            mock.patch.object(core, "_duplicate_record", lambda *a: ""),
            mock.patch.object(core, "_crossed_record", lambda *a: ""),
            mock.patch.object(core, "_unsearched_record", lambda *a: ""),
            mock.patch.object(core, "_same_named_record", lambda *a: ""),
            mock.patch.object(audit, "origin", {"session": "s", "step": step, "id": f"s{step}"} if step else None),
        ]
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            try:
                if kind == "key":
                    return core.act("key", keys=["Return"], check_form=check_form)["status"]
                return core.act("click", point=(54, 384), check_form=check_form)["status"]
            except AssertionError:
                return "pressed"


class APointPressIsThePressOfWhatIsUnderIt(_Press):

    def test_the_look_lists_the_button_under_the_point(self):
        self.assertEqual(self.press(listed=True, step=None), "needs_approval")

    def test_only_the_page_names_the_button_under_the_point(self):
        self.assertEqual(self.press(listed=False, step=None), "needs_approval")  # held by the secret it sends

    def test_an_engines_point_click_on_it_too(self):
        self.assertEqual(self.press(listed=False, step=None, check_form=True), "needs_approval")

    def test_an_enter_after_a_point_put_the_keyboard_on_it(self):
        self.assertEqual(self.press(listed=False, step=None, kind="key", focused=VERIFY, under=None),
                         "needs_approval")

    def test_a_point_press_on_a_plain_button_sending_nothing_given_still_goes(self):
        self.assertEqual(self.press(under=NEXT, form=PLAIN_FORM, secrets=(), step=90), "pressed")


class AStepThePersonNamedByItsPlace(_Press):

    def test_a_confirmed_entry_with_no_name_holds_its_step(self):
        # No secret in the form, and the page names a plain button: only the step's place holds it.
        self.assertEqual(self.press(under=NEXT, form=PLAIN_FORM, secrets=(), step=86), "needs_approval")

    def test_a_step_approved_while_taught_holds_by_its_place(self):
        skill = taught(ask=(), consequential_86=True)
        self.assertEqual(self.press(under=NEXT, form=PLAIN_FORM, secrets=(), step=86, skill=skill), "needs_approval")


class NothingIdentifiableUnderAReplayedPoint(_Press):

    def test_held_where_the_person_marked_the_step(self):
        self.assertEqual(self.press(under=None, secrets=(), step=86), "needs_approval")

    def test_held_while_no_approvals_answer_is_confirmed(self):
        self.assertEqual(self.press(under=None, secrets=(), step=90, skill=taught(confirmed=False)), "needs_approval")

    def test_held_where_there_is_no_page_to_ask(self):
        self.assertEqual(self.press(under=None, secrets=(), step=90, page_url=""), "needs_approval")

    def test_blank_space_in_a_plain_step_with_a_confirmed_answer_goes(self):
        self.assertEqual(self.press(under=None, secrets=(), step=90), "pressed")


if __name__ == "__main__":
    unittest.main()
