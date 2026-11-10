"""
Whether the approval gate holds a replayed press of a committing control with no person's yes, whatever the taught
step says (consequential: false). The replay presses each step through core.act(check_form="replay")
(replay._do_step), so this drives that very call with only the screen and the page mocked: an approval request must
be made and nothing pressed.

Two controls from the return-prep workflow: "Save to portal" (a filled form's submit that saves to an outside site)
and "Verify" on the OTP form (the filing). Each is pressed by a click and by Return on the focused button.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_replayed_commit_held.py
"""

import contextlib
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval, core


def control(role, name, **extra):
    out = {"role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10], "ref": "e7",
           "path": f'site x > {role} "{name}"', "source": "cdp", "group": "g", "window_title": "Return - Chromium",
           "handle": {"backend_id": 3}}
    out.update(extra)
    return out


FORMS = {
    "Save to portal": {"form": "Form GSTR-1", "fields": [["GSTIN", "29ACKPK4821M1Z5"], ["Period", "December 2026"],
                                                         ["Taxable value", "200000"]],
                       "submit": "Save to portal", "unset": [], "method": "post"},
    "Verify": {"form": "Validate one-time password", "fields": [["Enter the one-time password (OTP)", "482913"]],
               "submit": "Verify", "unset": [], "method": "post"},
}


class AReplayedCommitIsHeld(unittest.TestCase):

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

    def press(self, name, how, skill=None, secrets=(), form=None):
        button = control("button", name, states=["focused"] if how == "key" else [])
        form = form or FORMS[name]
        fields = [control("textbox", label, value=value, ref=f"e{20 + i}") for i, (label, value) in enumerate(form["fields"])]
        screen = {"elements": fields + [button], "page_url": "http://portal.test/return", "active_title": "Return - Chromium",
                  "groups": [{"id": "g", "window_title": "Return - Chromium", "source": "cdp",
                              "url": "http://portal.test/return"}], "frame": None, "size": [1280, 720], "windows": []}
        pressed = mock.Mock(side_effect=AssertionError("pressed with no yes"))
        # Only the screen, the page and the record lookups are stood in for; the checks, the plan, the person's holds
        # and the gate are real.
        patches = [
            mock.patch.object(core, "observe", lambda *a, **k: screen),
            mock.patch.object(core.audit, "record", lambda *a, **k: None),
            mock.patch.object(core, "_downloads_snapshot", lambda: None),
            mock.patch.object(core, "_perform", pressed),
            mock.patch.object(core.cdp, "available", return_value=False),
            mock.patch.object(core.cdp, "submit_form_fields", return_value=form),
            mock.patch("decurix_agent.task_context.current", return_value="t-1"),
            mock.patch("decurix_agent.task_context.skill", return_value={"name": "s", "version": 1} if skill else None),
            mock.patch("decurix_agent.computer.skills.load", return_value=skill),
            mock.patch.object(core.privacy, "literals", return_value=list(secrets)),
            mock.patch.object(core.cdp, "enter_form_fields", return_value=None),
            mock.patch.object(core, "_keyboard_field", return_value=None),
            mock.patch.object(core, "_unsourced_fields", lambda fields: []),
            mock.patch("decurix_agent.filler.invented", lambda *a, **k: []),
            mock.patch.object(core, "_ledger_check", lambda *a, **k: None),
            mock.patch.object(core, "_duplicate_record", lambda *a: ""),
            mock.patch.object(core, "_crossed_record", lambda *a: ""),
            mock.patch.object(core, "_unsearched_record", lambda *a: ""),
            mock.patch.object(core, "_same_named_record", lambda *a: ""),
            mock.patch("decurix_agent.computer.skills.all_skills", return_value=[skill] if skill else []),
        ]
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            try:
                if how == "click":
                    done = core.act("click", ref="e7", check_form="replay")
                else:
                    done = core.act("key", keys=["Return"], check_form="replay")
            except AssertionError:
                done = {"status": "pressed"}  # the stand-in press was reached: nothing held it
        return done, pressed

    def test_each_press_waits_for_a_yes(self):
        for name in FORMS:
            for how in ("click", "key"):
                with self.subTest(control=name, pressed_by=how):
                    approval.reset()
                    done, pressed = self.press(name, how)
                    self.assertEqual(done["status"], "needs_approval", done)
                    pressed.assert_not_called()

    # A taught task whose person said which presses need their OK (the lesson's confirmed approvals).
    def taught(self, ask, consequential=False):
        return {"name": "s", "approvals": {"confirmed": True, "ask": ask, "said": "ask me before saving to the portal"},
                "steps": [{"n": 40, "id": "s40", "kind": "click", "consequential": consequential,
                           "url": "http://portal.test/return", "target": {"role": "button", "name": "Save to portal"}}]}

    def test_a_confirmed_hold_holds_though_the_step_says_consequential_false(self):
        skill = self.taught([{"step": "s40", "role": "button", "name": "Save to portal"}], consequential=False)
        for how in ("click", "key"):
            approval.reset()
            done, pressed = self.press("Save to portal", how, skill=skill)
            self.assertEqual(done["status"], "needs_approval", how)
            pressed.assert_not_called()

    def test_the_otp_that_files_is_asked_even_where_the_person_confirmed_nothing_needs_them(self):
        skill = self.taught([])
        for how in ("click", "key"):
            approval.reset()
            done, pressed = self.press("Verify", how, skill=skill, secrets=["482913"])
            self.assertEqual(done["status"], "needs_approval", how)
            pressed.assert_not_called()
        hidden = dict(FORMS["Verify"], fields=[], secret_filled=True)  # a password-type code field, never read
        approval.reset()
        done, pressed = self.press("Verify", "click", skill=skill, form=hidden)
        self.assertEqual(done["status"], "needs_approval")

    def test_a_confirmed_go_lets_a_replayed_save_of_sourced_values_through(self):
        skill = self.taught([])
        save = {"form": "Voucher", "fields": [["Party", "Udyavara Apartments"], ["Amount", "5000"], ["Date", "4-12-2026"]],
                "submit": "Save", "unset": [], "method": "post"}
        done, pressed = self.press("Save", "click", skill=skill, form=save)
        self.assertEqual(done["status"], "pressed")  # the 2026-11-03 rule stands for a person who said saves need no OK

    def test_a_sign_in_is_not_asked_for_its_password(self):
        login = {"form": "Sign in", "fields": [["User name", "konkan.coast"]], "submit": "Sign in", "unset": [],
                 "method": "post", "signs_in": True, "secret_filled": True}
        done, pressed = self.press("Sign in", "click", skill=self.taught([]), form=login)
        self.assertEqual(done["status"], "pressed")


if __name__ == "__main__":
    unittest.main()
