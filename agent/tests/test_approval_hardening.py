"""
The approval gate, hardened after an independent recheck (review 2026-11-02, second round):

1. a consequential word anywhere in a control's name asks ("Confirm & Pay", "Securely pay", "Pay now ->"), a read of
   one does not ("Payment history", "View post");
2. a yes is bound to every field the press sends, not the first three;
3. Enter that submits a form is a press on its submit control, asked for like one;
4. a replay goes through the pre-save checks and the gate, sees a result only when the step made it, says a run it
   did not see is not done, never redoes a step that acted, and never starts a halted run again from step 1;
7. the form-check note is per task; a broken pipe is an error.
(5 and 6 - the script sandbox and Claude Code's guard - are test_script_sandbox and test_claude_guard.)
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval, cdp, cli, core, replay, skill_engine, skills
from test_recordings import FakeScreen


def control(role, name, **extra):
    base_ = {"role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10],
             "path": f'site x > {role} "{name}"', "source": "cdp", "group": "g", "window_title": "Pay - Chromium"}
    base_.update(extra)
    return base_


class ConsequentialAnywhereInTheName(unittest.TestCase):
    def test_a_paying_word_anywhere_asks(self):
        for name in ("Confirm & Pay", "\U0001f512 Pay", "Securely pay", "Continue to payment", "Authorize",
                     "Pay now →", "Approve payment", "View & Pay", "Review and submit payment", "Go to checkout"):
            with self.subTest(name=name):
                self.assertIsNotNone(approval.assess("click", control("button", name)))

    def test_a_read_of_one_does_not(self):
        for name in ("Payment history", "View submitted"):
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("button", name)))
        # A label with a committing word reads as a place only on a link (review 2026-11-03, D-04): on a button it asks.
        for name in ("View post", "Transfer history", "Pay slip", "New Purchase Invoice", "Track transfer"):
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("link", name)))
                self.assertIsNotNone(approval.assess("click", control("button", name)))

    def test_destroying_is_never_a_read(self):
        self.assertIsNotNone(approval.assess("click", control("button", "Delete history")))

    def test_one_decision_for_the_gate_and_the_pre_save_checks(self):
        self.assertEqual(approval.press_effect(control("button", "Confirm & Pay")), "asks")
        self.assertEqual(approval.press_effect(control("button", "\U0001f512 Save")), "commits")
        self.assertTrue(approval.commits(control("button", "Securely pay")))
        self.assertIsNone(approval.press_effect(control("option", "Pay now")))      # a choice in a list


class _Store(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        for p in (mock.patch.object(approval, "STATE_DIR", self.tmp),
                  mock.patch.object(approval, "STATE_FILE", self.tmp / "approval.json"),
                  mock.patch.object(core, "FORM_CHECK_FILE", self.tmp / "form-check.json")):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()


FIELDS = [["Payee", "Acme"], ["Account", "0012"], ["IFSC", "HDFC0001"], ["Memo", "INV-7"], ["Amount", "500"]]


class AYesIsBoundToEveryFieldSent(_Store):
    def test_another_amount_in_the_fifth_field_is_another_action(self):
        pay = control("button", "Pay now")
        asked = approval.check("click", pay, sends=FIELDS)
        self.assertFalse(asked.allowed)
        self.assertEqual(approval.resolve_reply("yes", asked=approval.request_text(asked.request)), "granted")

        changed = [f if f[0] != "Amount" else ["Amount", "5000"] for f in FIELDS]
        self.assertFalse(approval.check("click", pay, sends=changed).allowed)
        self.assertTrue(approval.check("click", pay, sends=FIELDS).allowed)        # the very action approved

    def test_a_secret_counts_as_filled_never_by_its_value(self):
        self.assertEqual(approval.sent_digest([["PIN", "1111", True]]), approval.sent_digest([["PIN", "2222", True]]))
        self.assertNotEqual(approval.sent_digest([["PIN", "1111", True]]), approval.sent_digest([]))

    def test_what_a_press_sends_is_every_filled_field_of_its_window(self):
        page = {"elements": [control("textbox", n, value=v) for n, v in FIELDS]
                + [control("textbox", "Password", value="hunter22", states=["protected"])],
                "groups": [{"id": "g", "window_title": "Pay - Chromium"}], "active_title": "Pay - Chromium"}
        sent = core._what_it_sends(page, {"group": "g"})
        self.assertIn(["Amount", "500", False], sent)
        self.assertEqual(len(sent), 6)
        self.assertNotIn("hunter22", json.dumps(sent))


def act_page(field):
    return {"elements": [field], "groups": [{"id": "g", "window_title": "Pay - Chromium", "source": "cdp",
                                             "url": "https://shop.test/pay"}],
            "active_title": "Pay - Chromium", "frame": None, "size": [1280, 720], "windows": []}


class EnterIsAPressOnTheSubmitControl(_Store):
    def setUp(self):
        super().setUp()
        self.box = control("textbox", "Name on card", value="A Rao", ref="e1", handle={"backend_id": 7})
        for p in (mock.patch.object(core, "observe", lambda *a, **k: act_page(self.box)),
                  mock.patch.object(core.audit, "record", lambda *a, **k: None),
                  mock.patch.object(core, "_downloads_snapshot", lambda: None),
                  mock.patch.object(core, "_refuse_unsafe_save", lambda *a: None),
                  mock.patch.object(core, "_refuse_number_arrows", lambda *a: None),
                  mock.patch.object(core, "_commit_check", lambda *a, **k: ""),
                  mock.patch.object(core, "_perform", mock.Mock(side_effect=AssertionError("pressed")))):
            p.start()
            self.addCleanup(p.stop)

    def test_enter_in_a_small_form_whose_submit_pays_is_asked_for(self):
        form = {"form": "Checkout", "fields": [["Name on card", "A Rao"]], "submit": "Pay now"}
        with mock.patch.object(cdp, "enter_form_fields", return_value=form):
            done = core.act("key", ref="e1", keys=["Return"], check_form=True)
            self.assertEqual(done["status"], "needs_approval")
            self.assertIn("Pay now", done["text"])
            typed = core.act("type", ref="e1", text="A Rao\n", check_form=True)
            self.assertEqual(typed["status"], "needs_approval")

    def test_enter_in_a_search_form_is_not(self):
        form = {"form": "Find", "fields": [["q", "mugs"]], "submit": "Search"}
        with mock.patch.object(cdp, "enter_form_fields", return_value=form):
            submit = core._enter_submits("key", self.box, ["Return"], None, act_page(self.box))
        self.assertIsNone(approval.assess("click", submit))


class AReplaysCommittingPress(_Store):
    FULL = [["Customer", "Cafe Bloom"], ["Customer PO number", "WA-1810"], ["Quantity", "2"], ["Rate", "149"]]

    def _form(self, **patches):
        save = {"ref": "e2", "role": "button", "name": "Save", "path": "div > button Save", "source": "cdp", "group": "g"}
        with mock.patch.object(core, "_ledger_check", lambda fields: None),                 mock.patch.object(core, "_unsourced_fields", patches.get("unsourced", lambda fields: [])):
            return core._form_check(save, found={"form": "New order", "fields": self.FULL}, listing="ask")

    def test_a_replay_saving_sourced_values_asks_nobody(self):
        # (review 2026-11-03: it asked a person at every save of a form of 3 fields or more)
        self.assertEqual(self._form(), "")

    def test_a_replay_saving_a_value_in_no_source_is_refused_not_asked(self):
        held = self._form(unsourced=lambda fields: [("Quantity", "2")])
        self.assertNotIsInstance(held, core.AskFirst)
        self.assertIn("Quantity", held)
        self.assertIn("Not pressed", held)

    def test_a_value_taken_from_the_demonstration_is_the_persons_own(self):
        core.TAUGHT_VALUES.add("2")
        self.addCleanup(core.TAUGHT_VALUES.clear)
        self.assertEqual(self._form(unsourced=lambda fields: [("Quantity", "2")]), "")

    def test_a_replay_asks_for_the_checks(self):
        fake = FakeScreen([control("button", "Save", path="p/save")])
        with mock.patch.object(replay, "core", fake), mock.patch.object(replay, "FIND_SECONDS", 0), \
                mock.patch.object(replay, "EXPECT_SECONDS", 0):
            replay.run({"session": "s", "params": [], "steps": [
                {"n": 1, "kind": "click", "summary": "click Save", "target": {"role": "button", "name": "Save",
                                                                             "path": "p/save"}}]}, report=lambda l: None)
        self.assertEqual(fake.acted[0][1].get("check_form"), "replay")


class SeenOnlyWhenTheStepMadeIt(unittest.TestCase):
    def run_one(self, before_has_it, consequential=False, halt=None, name="Save"):
        saved = control("heading", "Saved")
        fake = FakeScreen([control("button", "Save", path="p/save"), saved])
        before = {"elements": [saved] if before_has_it else [], "windows": [], "active_title": "Shop - Chromium"}
        act = fake.act
        fake.act = lambda kind, **kw: dict(act(kind, **kw), before=before)
        step = {"n": 1, "kind": "click", "summary": "click Save", "consequential": consequential,
                "target": {"role": "button", "name": name, "path": "p/save"},
                "expect": [{"type": "element", "role": "heading", "name": "Saved"}]}
        patches = [mock.patch.object(replay, "core", fake), mock.patch.object(replay, "FIND_SECONDS", 0),
                   mock.patch.object(replay, "EXPECT_SECONDS", 0), mock.patch.object(replay, "POLL_SECONDS", 0)]
        if halt:
            patches.append(mock.patch.object(replay, "_run_step", mock.Mock(side_effect=halt)))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        return replay.run({"session": "s", "params": [], "steps": [step, dict(step, n=2)]}, report=lambda l: None)

    def test_a_result_already_on_the_screen_shows_nothing_of_the_step(self):
        self.assertEqual(self.run_one(before_has_it=True)["outcome"], replay.COMPLETED_UNVERIFIED)
        self.assertEqual(self.run_one(before_has_it=False)["outcome"], replay.VERIFIED)

    def test_a_step_that_acted_and_then_failed_is_never_done_again(self):
        acted = replay._Halt("what it typed did not hold", acted=True)
        done = self.run_one(False, consequential=True, halt=acted)
        self.assertEqual(done["outcome"], replay.RECONCILIATION_REQUIRED)
        self.assertIsNone(done["resume_from"])
        self.assertEqual(self.run_one(False, halt=acted)["outcome"], replay.RECONCILIATION_REQUIRED)  # a Save commits
        done = self.run_one(False, halt=replay._Halt("the field did not keep it", acted=True), name="Next")
        self.assertEqual(done["outcome"], replay.HALTED)              # not "before doing anything"
        self.assertEqual(done["resume_from"], 1)

    def test_a_run_not_seen_is_not_done_for_the_engine(self):
        result = {"outcome": replay.COMPLETED_UNVERIFIED, "text": "ran", "session": "s", "resume_from": None}
        with mock.patch("builtins.print"):
            self.assertEqual(cli._print_replay(result), cli.EXIT_VERIFY_FAILED)


class AHaltedRunCarriesOnFromItsStop(unittest.TestCase):
    SKILL = {"name": "pay-bill", "title": "Pay a bill", "status": skills.READY, "params": [],
             "steps": [{"n": n, "id": f"s{n}", "kind": "click", "window": "", "summary": f"step {n}"} for n in (1, 2, 3)]}

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(skill_engine, "REPLAYED_DIR", Path(tmp.name)),
                  mock.patch.object(skill_engine.task_context, "current", lambda: "t1"),
                  mock.patch.object(skill_engine, "other_system", lambda *a: ""),
                  mock.patch.object(skill_engine.skills, "note_run", lambda name: None)):
            p.start()
            self.addCleanup(p.stop)

    def run_skill(self, start, **result):
        with mock.patch.object(skill_engine.replay, "run", return_value=dict({"text": "ok"}, **result)) as ran:
            said = skill_engine.run(dict(self.SKILL), {}, start=start)
        return said, ran

    def test_never_from_step_1_again(self):
        self.run_skill(1, outcome=replay.HALTED, step=3, resume_from=3, done=2)
        said, ran = self.run_skill(1, outcome=replay.VERIFIED)
        ran.assert_not_called()
        self.assertIn("--from 3", said["text"])
        _, ran = self.run_skill(3, outcome=replay.VERIFIED)
        ran.assert_called_once()

    def test_a_step_that_may_have_happened_is_not_run_again(self):
        self.run_skill(1, outcome=replay.RECONCILIATION_REQUIRED, step=2, resume_from=None, done=2)
        for start in (1, 2):
            said, ran = self.run_skill(start, outcome=replay.VERIFIED)
            ran.assert_not_called()
            self.assertIn("may or may not have taken effect", said["text"])


class OneNotePerTask(_Store):
    def test_a_listing_in_one_task_is_no_confirmation_in_the_next(self):
        save = {"ref": "e2", "role": "button", "name": "Save", "path": "div > button Save", "source": "cdp", "group": "g"}
        found = {"form": "New order", "fields": AReplaysCommittingPress.FULL}
        with mock.patch.object(core, "_unsourced_fields", lambda fields: []), \
                mock.patch.object(core, "_ledger_check", lambda fields: None), \
                mock.patch("decurix_agent.task_context.current", lambda: "t" + "a" * 16):
            self.assertIn("Not pressed yet", core._form_check(save, found=found))
            self.assertEqual(core._form_check(save, checked=True, found=found), "")
            self.assertIn("ta", core._form_check_file().name)
        with mock.patch.object(core, "_unsourced_fields", lambda fields: []), \
                mock.patch.object(core, "_ledger_check", lambda fields: None), \
                mock.patch("decurix_agent.task_context.current", lambda: "t" + "b" * 16):
            self.assertIn("Not pressed yet", core._form_check(save, checked=True, found=found))


class ABrokenPipeIsAnError(unittest.TestCase):
    def test_it_exits_with_an_error(self):
        with mock.patch.object(cli, "run", side_effect=BrokenPipeError()), \
                mock.patch.object(cli, "save_screenshot", lambda argv: None), \
                mock.patch.object(cli.progress, "flush", lambda: None), \
                mock.patch.object(cli.logging_utils, "log_to_file", lambda path: None), \
                mock.patch.object(cli.shown, "note", lambda text: None), \
                mock.patch("sys.stderr"):
            self.assertEqual(cli.main(["observe"]), cli.EXIT_ERROR)


if __name__ == "__main__":
    unittest.main()
