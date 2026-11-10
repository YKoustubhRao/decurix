"""
Two gate-area holes found in the learning curves (2026-11-10), each closed by structure:

1. Return or Space on a focused button presses it, so it gets the same pre-save checks as a click on it
   (core._commit_check). Keys that move the keyboard and then press, on a page that saves, are refused: which control
   they land on cannot be told until the moving keys have run. Live: "Save voucher" was refused twice, then
   `Tab Tab Return` saved the voucher a second time. The gate's question names the control such a key presses.
2. What the person said holds a control (core._person_hold):
   - a no to it when the gate asked, in this task (approval.declined_controls, kept by the gate);
   - a non-yes reply to the worker's own question naming it;
   - an approval they gave it while teaching.
   Each only ever adds a question; nothing makes a yes. Live: "Save to portal" was refused (or answered "I don't
   know") and pressed anyway a moment later.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_keyed_press_and_holds.py
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval, core


def control(role, name, **extra):
    out = {"role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10], "ref": "e9",
           "path": f'site x > {role} "{name}"', "source": "cdp", "group": "g", "window_title": "Books - Chromium",
           "handle": {"backend_id": 5}}
    out.update(extra)
    return out


def page(*elements, url="http://books.test/voucher/new"):
    return {"elements": list(elements), "page_url": url, "active_title": "Books - Chromium",
            "groups": [{"id": "g", "window_title": "Books - Chromium", "source": "cdp", "url": url}],
            "frame": None, "size": [1280, 720], "windows": []}


class AKeyOnAFocusedButton(unittest.TestCase):

    FORM = {"form": "Voucher", "fields": [["Voucher no", "1072"], ["Amount", "5000"]], "submit": "Save voucher",
            "unset": [], "method": "post"}

    def check(self, keys, before):
        pressed = []
        with mock.patch.object(core.cdp, "available", return_value=False), \
                mock.patch.object(core.cdp, "submit_form_fields", return_value=self.FORM), \
                mock.patch.object(core, "_keyboard_field", return_value=None), \
                mock.patch.object(core, "_form_check", return_value="ENTERED TWICE"):
            said = core._commit_check("key", None, keys, before, pressed=pressed)
        return said, pressed

    def test_return_or_space_on_a_focused_save_is_checked_as_its_click(self):
        save = control("button", "Save voucher", states=["focused"])
        for keys in (["Return"], ["space"]):
            said, pressed = self.check(keys, page(save))
            self.assertEqual(said, "ENTERED TWICE", keys)
            self.assertEqual(pressed[0]["name"], "Save voucher")

    def test_keys_that_move_then_press_are_refused_where_a_page_saves(self):
        said, pressed = self.check(["Tab", "Tab", "Return"], page(control("button", "Save voucher")))
        self.assertTrue(said.startswith("Not pressed"), said)
        self.assertIn("gui key Tab Tab", said)

    def test_not_where_nothing_saves(self):
        said, _ = self.check(["Tab", "Tab", "Return"], page(control("link", "Help")))
        self.assertEqual(said, "")


class _Store(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        for p in (mock.patch.object(approval, "STATE_DIR", self.tmp),
                  mock.patch.object(approval, "STATE_FILE", self.tmp / "approval.json"),
                  mock.patch.object(approval, "_task_now", return_value="t-1")):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()


class TheQuestionNamesWhatAKeyPresses(_Store):

    def test_return_on_a_focused_pay_button_is_asked_about_by_its_name(self):
        pay = control("button", "Pay now", states=["focused"])
        for p in (mock.patch.object(core, "observe", lambda *a, **k: page(pay)),
                  mock.patch.object(core.audit, "record", lambda *a, **k: None),
                  mock.patch.object(core, "_downloads_snapshot", lambda: None),
                  mock.patch.object(core, "_refuse_unsafe_save", lambda *a: None),
                  mock.patch.object(core, "_refuse_number_arrows", lambda *a: None),
                  mock.patch.object(core, "_commit_check", lambda *a, **k: ""),
                  mock.patch.object(core, "_keyboard_field", return_value=None),
                  mock.patch.object(core, "_location_bar", return_value=None, create=True),
                  mock.patch.object(core.cdp, "available", return_value=False),
                  mock.patch.object(core, "_person_hold", return_value=None, create=True),
                  mock.patch.object(core, "_perform", mock.Mock(side_effect=AssertionError("pressed")))):
            p.start()
            self.addCleanup(p.stop)
        done = core.act("key", keys=["Return"], check_form=True)
        self.assertEqual(done["status"], "needs_approval")
        self.assertIn('"Pay now"', approval._local_pending()["action"])  # not "press Return in the screen"


class ANoHoldsTheControl(_Store):

    def test_a_no_to_a_control_is_kept_for_the_task_and_read_back(self):
        save = control("button", "Save to portal")
        asked = approval.check("click", save, require="the plan makes it with your OK first")
        self.assertFalse(asked.allowed)
        with mock.patch.object(approval, "reading", return_value={"decision": "unclear", "condition": ""}):
            self.assertEqual(approval.resolve_reply("I don't know", asked=approval.request_text(asked.request)),
                             "declined")
        self.assertEqual(approval.declined_controls("t-1"), [{"role": "button", "name": "Save to portal"}])
        self.assertEqual(approval.declined_controls("t-2"), [])

    def test_a_yes_keeps_nothing(self):
        save = control("button", "Save to portal")
        asked = approval.check("click", save, require="ask")
        with mock.patch.object(approval, "reading", return_value={"decision": "yes", "condition": ""}):
            self.assertEqual(approval.resolve_reply("yes", asked=approval.request_text(asked.request)), "granted")
        self.assertEqual(approval.declined_controls("t-1"), [])


class WhatThePersonSaidHoldsAPress(unittest.TestCase):

    SAVE = control("button", "Save to portal")

    def hold(self, declined=(), said=(), taught=(), says_yes=False, control_=None):
        with tempfile.TemporaryDirectory() as tmp:
            notes = Path(tmp)
            if said:
                (notes / "t-1.said").write_text("\n".join(json.dumps({"asked": a, "reply": r}) for a, r in said),
                                                encoding="utf-8")
            with mock.patch("decurix_agent.task_context.current", return_value="t-1"), \
                    mock.patch("decurix_agent.computer.evidence.NOTES_DIR", notes), \
                    mock.patch.object(approval, "declined_controls", return_value=list(declined)), \
                    mock.patch.object(approval, "says_yes", return_value=says_yes), \
                    mock.patch("decurix_agent.computer.skills.all_skills", return_value=list(taught)):
                return core._person_hold(control_ or self.SAVE, page(url="http://fileprep.test/gstr1"))

    def test_a_no_from_the_gate_holds_it(self):
        self.assertIn("did not say yes", self.hold(declined=[{"role": "button", "name": "Save to portal"}]))
        self.assertIsNone(self.hold(declined=[{"role": "button", "name": "Import"}]))

    def test_a_non_yes_to_the_workers_own_question_holds_it(self):
        said = [("Shall I click Save to portal now?", "I don't know")]
        self.assertIn("your reply did not say yes", self.hold(said=said))
        self.assertIsNone(self.hold(said=said, says_yes=True))
        self.assertIsNone(self.hold(said=[("Which month is this?", "December")]))

    # Found live (2026-11-10): a question naming a client whole held the client's breadcrumb; "Which company should I
    # use?" held the company's link. A worker's question holds only the control it asks to press.
    QUESTION = ('I need approval before clicking "Save to portal" for Sahyadri Lift Services\' January 2027 GSTR-1. '
                "May I proceed?")

    def links_to(self, address):
        return mock.patch.multiple(core.cdp, link_target=mock.Mock(return_value=address),
                                   submit_form_fields=mock.Mock(return_value=None))

    def test_the_press_a_question_asks_for_is_held_not_a_name_standing_in_it(self):
        said = [(self.QUESTION, "I don't know - that's for you to decide")]
        self.assertIn("asked you to press", self.hold(said=said))  # Save to portal: its object
        breadcrumb = control("link", "Sahyadri Lift Services")
        with self.links_to("http://fileprep.test/client/1"):
            self.assertIsNone(self.hold(said=said, control_=breadcrumb))
        # (a button of that name is not the press asked for either)
        self.assertIsNone(self.hold(said=said, control_=control("button", "Sahyadri Lift Services")))

    def test_a_company_asked_about_is_not_a_press_asked_for(self):
        said = [("Which company should I use? Konkan Coast Interiors or Konkan Coast Traders?", "I don't know")]
        company = control("link", "Konkan Coast Interiors")
        with self.links_to("http://books.test/company/2"):
            self.assertIsNone(self.hold(said=said, control_=company))

    def test_a_link_the_page_says_only_navigates_is_never_held_by_a_worded_question(self):
        said = [("May I click Konkan Coast Interiors?", "not sure")]
        company = control("link", "Konkan Coast Interiors")
        with self.links_to("http://books.test/company/2"):
            self.assertIsNone(self.hold(said=said, control_=company))
        with self.links_to(""):  # not one the page says only goes somewhere: the press asked for is held
            self.assertIn("asked you to press", self.hold(said=said, control_=company))

    def test_the_ways_a_question_asks_to_press(self):
        save = {"name": "Save to portal"}
        for asked in ("May I click Save to portal?", 'Before pressing "Save to portal", check?', "Should I press the "
                      "Save to portal button?", "Can I tap on Save to portal", "approve 'Save to portal' now?"):
            self.assertTrue(core.approval.asks_to_press(asked, save), asked)
        for asked in ("Is Save to portal the right step?", "The page shows Save to portal and Import.",
                      "Click Save to portal draft"[:0] + "Shall I click Save to portals?"):
            self.assertFalse(core.approval.asks_to_press(asked, save), asked)

    def test_an_approval_taught_holds_it_in_every_run_on_its_site(self):
        skill = {"steps": [{"consequential": True, "url": "http://fileprep.test/gstr1/2026-10",
                            "target": {"role": "button", "name": "Save to portal"}}]}
        self.assertIn("when you taught", self.hold(taught=[skill]))
        elsewhere = {"steps": [dict(skill["steps"][0], url="http://other.test/x")]}
        self.assertIsNone(self.hold(taught=[elsewhere]))
        unasked = {"steps": [dict(skill["steps"][0], consequential=False)]}
        self.assertIsNone(self.hold(taught=[unasked]))

    def test_a_field_is_never_held(self):
        self.assertIsNone(self.hold(declined=[{"name": "Save to portal"}], control_=control("textbox", "Save to portal")))


if __name__ == "__main__":
    unittest.main()
