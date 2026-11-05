"""
Round 3 of the review (2026-11-03): labels that commit the person and labels that only name a thing, Return/Space on a
focused button, a click at a spot nothing names, the final gate for shell-only and taught runs, and a learned tool's
email drafted once, after its check.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import playbooks, produced, skill_task, tool_task, verify
from decurix_agent.computer import approval, core


def control(role, name, **extra):
    found = {"role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10],
             "path": f'site x > {role} "{name}"', "source": "cdp", "group": "g", "window_title": "Shop - Chromium"}
    found.update(extra)
    return found


ASKED = ["Confirm & Pay", "\U0001f512 Pay", "Securely pay", "Continue to payment", "Authorize", "Approve", "Approve payment",
         "File GSTR-1", "File return", "File the declaration", "Release funds", "Submit order", "E-sign", "Sign",
         "Sign & submit", "Cash out", "Request payout", "Envoyer", "Pagar", "Pay now →", "Send money"]
# Not asked on any control: no committing word, a form's own part, or a drawing arranged (structure).
NOT_ASKED = ["Delete row", "Delete Rows", "Delete selected rows", "Send to back", "Sign in", "Sign up", "Sign out",
             "File returns", "Save", "Save & Submit", "Create", "Update", "Accept", "Payment history"]
# Names of things that hold a committing word: read as a place only on a control that goes somewhere (a link, a tab, a
# navigation menu's item). On a button a committing word anywhere asks (review 2026-11-03, D-04).
NAMES = ["Make Purchase Receipt", "Make Payment Entry", "Stock Transfer", "Material Transfer", "Post Date",
         "Get Items From Purchase Order", "Buy Price", "Purchase Taxes and Charges", "View post"]


def nav(role, name):
    return control(role, name, path=f'site x > navigation > {role} "{name}"')


class WhichLabelsAsk(unittest.TestCase):
    def test_a_label_that_commits_the_person_asks(self):
        for name in ASKED:
            with self.subTest(name=name):
                self.assertEqual(approval.press_effect(control("button", name)), "asks")
                self.assertIsNotNone(approval.assess("click", control("button", name)))

    def test_a_label_that_names_a_thing_or_saves_a_record_does_not(self):
        for name in NOT_ASKED:
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("button", name)))

    def test_a_name_holding_a_committing_word_is_a_place_only_where_the_control_goes_somewhere(self):
        for name in NAMES:
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("link", name)))
                self.assertIsNone(approval.assess("click", nav("menuitem", name)))
                self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_deleting_a_form_part_is_not_deleting_a_record(self):
        self.assertIsNone(approval.press_effect(control("button", "Delete row")))
        self.assertEqual(approval.press_effect(control("button", "Delete invoice")), "asks")
        self.assertEqual(approval.press_effect(control("button", "Delete permanently")), "asks")

    def test_the_same_in_a_link_and_a_menu(self):
        for role in ("link", "menuitem"):
            self.assertEqual(approval.press_effect(control(role, "File GSTR-1")), "asks")
            self.assertIsNone(approval.press_effect(nav(role, "Stock Transfer")))
        # An item of an action menu acts where it is, like a button.
        self.assertEqual(approval.press_effect(control("menuitem", "Stock Transfer")), "asks")


class ReturnOnAButton(unittest.TestCase):
    def before(self, *elements):
        return {"elements": list(elements), "active_title": "Pay - Chromium", "page_url": ""}

    def test_return_on_a_focused_pay_button_is_a_press_on_it(self):
        pay = control("button", "Pay now", states=["focused"])
        pressed = core._pressed_by_key("key", None, ["Return"], self.before(pay))
        self.assertIs(pressed, pay)
        self.assertIsNotNone(approval.assess("click", pressed))

    def test_space_too(self):
        pay = control("button", "Release funds", states=["focused"])
        self.assertIs(core._pressed_by_key("key", None, ["space"], self.before(pay)), pay)

    def test_a_focused_save_button_is_found_and_not_asked(self):
        save = control("button", "Save", states=["focused"])
        pressed = core._pressed_by_key("key", None, ["Return"], self.before(save))
        self.assertIs(pressed, save)
        self.assertIsNone(approval.assess("click", pressed))

    def test_other_keys_and_a_focused_field_press_nothing(self):
        pay = control("button", "Pay now", states=["focused"])
        self.assertIsNone(core._pressed_by_key("key", None, ["Tab"], self.before(pay)))
        field = control("textbox", "Amount", states=["focused"])
        self.assertIsNone(core._pressed_by_key("key", None, ["Return"], self.before(field)))


class AClickAtASpot(unittest.TestCase):
    def test_a_spot_the_page_names_is_judged_as_that_control(self):
        pay = control("button", "Pay now")
        before = {"elements": [], "page_url": "https://shop.example/cart", "active_title": "Cart", "frame": None}
        with mock.patch.object(core.cdp, "available", lambda: True), \
                mock.patch.object(core.cdp, "hit", lambda *a, **k: (pay, {"title": "Cart"})):
            found, why = core._unresolved_press("click", None, (5, 5), before)
        self.assertIs(found, pay)
        self.assertIsNone(why)

    def test_plain_page_where_the_page_says_no_control_is_let_through(self):
        before = {"elements": [], "page_url": "https://shop.example/cart", "active_title": "Cart", "frame": None}
        with mock.patch.object(core.cdp, "available", lambda: True), \
                mock.patch.object(core.cdp, "hit", lambda *a, **k: (None, {"title": "Cart"})):
            self.assertEqual(core._unresolved_press("click", None, (5, 5), before), (None, None))

    def test_a_spot_nothing_could_name_is_put_to_the_person(self):
        before = {"elements": [], "page_url": "", "active_title": "Some app", "frame": None}
        found, why = core._unresolved_press("click", None, (5, 5), before)
        self.assertIsNone(found)
        self.assertIn("nothing on the screen names", why)

    def test_text_under_the_spot_is_its_label(self):
        before = {"elements": [], "page_url": "", "active_title": "Some app", "frame": object()}
        from decurix_agent.computer import ocr
        with mock.patch.object(ocr, "available", lambda: True), mock.patch.object(ocr, "read_frame", lambda f: [1]), \
                mock.patch.object(ocr, "line_at", lambda lines, point: {"text": "File GSTR-1", "box": [0, 0, 1, 1]}):
            found, why = core._unresolved_press("click", None, (5, 5), before)
        self.assertEqual(found["name"], "File GSTR-1")
        self.assertIsNotNone(approval.assess("click", found))

    def test_a_click_that_resolved_is_not_this_ones(self):
        self.assertEqual(core._unresolved_press("click", control("button", "x"), (1, 1), {}), (None, None))
        self.assertEqual(core._unresolved_press("type", None, (1, 1), {}), (None, None))


class ShellOnlyWork(unittest.TestCase):
    def test_a_command_that_changes_things_is_told_from_one_that_reads(self):
        ran = ["ls ~/work", "cat a.txt | wc -l", "grep shell notes.txt", "echo hi > out.txt", "python3 fix.py",
               "office update a.xlsx -", "mv a b"]
        self.assertEqual(playbooks.changed_things(ran), ["echo hi > out.txt", "python3 fix.py",
                                                         "office update a.xlsx -", "mv a b"])

    def test_read_only_answers_with_python_curl_or_git_are_not_changes(self):
        # Round-3 recheck: these went to the final check and came back "Not checked".
        reads = ["git status", "git -C ~/work log --oneline -5", "git diff HEAD~1", "git branch", "curl -s https://x.example",
                 "curl -sfL https://x.example/api | jq .", "python3 -c \"print(sum([1, 2]))\"",
                 "python3 - <<'PY'\nimport json\nprint(json.loads('[1]'))\nPY", "ls run.sh", "office read a.xlsx",
                 "grep -r x . 2>/dev/null", "find . -name '*.py' > /dev/null"]
        self.assertEqual(playbooks.changed_things(reads), [])
        writes = ["git commit -am x", "git push", "git branch -D old", "curl -d @a.json https://x.example",
                  "curl -o out.zip https://x.example/f", "curl -X POST https://x.example", "python3 fix.py",
                  "python3 -c \"open('a','w').write('x')\"", "python3 - <<'PY'\nimport os\nos.remove('a')\nPY",
                  "bash deploy.sh", "cat cmds | sh", "node -e \"require('fs').writeFileSync('a','x')\""]
        self.assertEqual(playbooks.changed_things(writes), writes)

    def test_every_engines_commands_are_seen(self):
        playbooks.begin()
        playbooks.note_command("rm old.txt")
        playbooks.note_command("ls")
        self.assertEqual(playbooks.changed_things(), ["rm old.txt"])
        playbooks.begin()
        self.assertEqual(playbooks.changed_things(), [])

    def test_the_gate_takes_a_task_whose_commands_changed_things(self):
        with mock.patch.object(skill_task.guidance, "get", lambda: None), \
                mock.patch.object(skill_task, "_did_screen_work", lambda: False), \
                mock.patch.object(skill_task.produced, "files", lambda *a, **k: []), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.procedures, "in_request", lambda t: False), \
                mock.patch.object(skill_task.produced, "requested_names", lambda t: []):
            said = []
            # Nothing changed and nothing read: not judged, and sent said NOT checked (E-02/T-01, review 2026-11-03).
            with mock.patch.object(playbooks, "changed_things", lambda commands=None: []),                     mock.patch.object(skill_task.evidence, "observed", lambda *a, **k: []),                     mock.patch.object(skill_task.evidence, "files_read", lambda *a, **k: []),                     mock.patch.object(skill_task, "_commands_text", lambda: ""),                     mock.patch.object(skill_task, "write_final_reply", lambda text, **k: said.append((text, k))):
                self.assertTrue(skill_task._gate("Done.", "final", "thebot"))
            self.assertTrue(said[-1][0].startswith(skill_task.UNCHECKED_ANSWER))
            self.assertIs(said[-1][1].get("checked"), False)
            began = []
            with mock.patch.object(playbooks, "changed_things", lambda commands=None: ["rm old.txt"]), \
                    mock.patch.object(skill_task.guidance, "begin",
                                      lambda **k: began.append(k) or {"mode": "engine", "run": None, "task": "t",
                                                                      "attempt": 1}), \
                    mock.patch.object(skill_task, "write_final_reply") as sent, \
                    mock.patch.object(skill_task.verify, "judge", lambda *a, **k: {
                        "verdict": verify.UNSURE, "confidence": 0.0, "seen": "", "why": "no model"}), \
                    mock.patch.object(skill_task.guidance, "refused", lambda: None), \
                    mock.patch.object(skill_task.guidance, "end", lambda: None), \
                    mock.patch.object(skill_task.source_guard, "check_and_restore", lambda *a: ""), \
                    mock.patch.object(skill_task.source_guard, "tidy_own_backups", lambda *a: None), \
                    mock.patch.object(skill_task.source_guard, "resaved", lambda *a: []):
                self.assertTrue(skill_task._gate("Done.", "final", "thebot"))
        self.assertTrue(began)
        self.assertFalse(sent.call_args.kwargs["checked"])
        self.assertIn("Not checked", sent.call_args.args[0])


class ATaughtRunReadsItsFilesBack(unittest.TestCase):
    def run_dict(self):
        return {"skill": {"name": "s", "steps": [], "version": 1}, "patches": [], "asked_ids": [], "ladder": mock.Mock(),
                "one_off": False}

    def finish(self, facts, checked_by="screen"):
        sent = []
        with mock.patch.object(skill_task.produced, "files", lambda *a, **k: ["/tmp/out.xlsx"]), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.source_guard, "is_source", lambda *a: False), \
                mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts), \
                mock.patch.object(skill_task, "_record_verdict"), mock.patch.object(skill_task, "_own_run"), \
                mock.patch.object(skill_task.experience, "commit", lambda *a, **k: []), \
                mock.patch.object(skill_task.experience, "note_verdict"), \
                mock.patch.object(skill_task.experience, "advice", lambda s: ""), \
                mock.patch.object(skill_task.skills, "save"), \
                mock.patch.object(skill_task, "write_final_reply", lambda *a, **k: sent.append((a, k))):
            skill_task._succeed(self.run_dict(), {"text": "Saved it."}, checked_by)
        return sent

    def test_files_that_read_back_clean_are_checked_done(self):
        sent = self.finish(produced.FileFacts())
        self.assertEqual(len(sent), 1)
        self.assertTrue(sent[0][1]["checked"])
        self.assertEqual(sent[0][1]["kind"], "final")

    def test_a_fault_in_a_file_is_not_done(self):
        sent = self.finish(produced.FileFacts(file_faults=["out.xlsx has a placeholder left"]))
        self.assertEqual(sent[0][1]["kind"], "error")
        self.assertIn("Not done", sent[0][0][0])

    def test_a_check_that_could_not_be_made_is_not_checked_never_done(self):
        sent = self.finish(produced.FileFacts(could_not=["the payee check raised"]))
        self.assertFalse(sent[0][1]["checked"])
        self.assertTrue(sent[0][1]["gated"])
        self.assertIn("Not checked", sent[0][0][0])

    def test_a_persons_confirmation_is_theirs(self):
        sent = self.finish(produced.FileFacts(could_not=["x"]), checked_by="person")
        self.assertTrue(sent[0][1]["checked"])

    def test_a_run_not_checked_counts_neither_for_nor_against_the_skill(self):
        # Round-3 recheck: counted as a failure, a skill whose files cannot all be read back never matured.
        for facts, counted in ((produced.FileFacts(could_not=["the payee check raised"]), False),
                               (produced.FileFacts(file_faults=["out.xlsx has a placeholder left"]), True)):
            with mock.patch.object(skill_task, "_record_verdict") as verdict:
                with mock.patch.object(skill_task.produced, "files", lambda *a, **k: ["/tmp/out.xlsx"]), \
                        mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                        mock.patch.object(skill_task.source_guard, "is_source", lambda *a: False), \
                        mock.patch.object(skill_task.produced, "file_facts", lambda *a, f=facts, **k: f), \
                        mock.patch.object(skill_task, "_own_run"), \
                        mock.patch.object(skill_task, "write_final_reply", lambda *a, **k: None):
                    skill_task._succeed(self.run_dict(), {"text": "Saved it."}, "screen")
            self.assertEqual(verdict.called, counted, facts)
            if counted:
                self.assertFalse(verdict.call_args.args[1])


class ADraftIsMadeOnceAfterTheCheck(unittest.TestCase):
    RECORD = {"to": ["a@x.com"], "cc": [], "subject": "Invoice", "body": "Hello", "attachments": [], "paths": []}

    def test_a_dry_draft_is_read_from_the_script_and_nothing_is_sent(self):
        from decurix_agent.tools import outbox
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / "drafts.jsonl"
            outbox._note(log, {"to": ["a@x.com"], "subject": "Invoice", "body": "Hello", "attachments": []})
            out = []
            with mock.patch.object(outbox, "draft", side_effect=AssertionError("drafted for real")):
                self.assertEqual(tool_task._dry_drafts(log, Path(tmp), out), "")
        self.assertEqual(out[0]["to"], ["a@x.com"])

    def test_the_real_draft_is_made_once_whatever_the_number_of_passes(self):
        from decurix_agent.tools import outbox
        made = []

        def draft(to, subject, body, cc=None, attachments=None):
            made.append({"to": to, "cc": cc or [], "subject": subject, "body": body, "attachments": []})

        with mock.patch.object(outbox, "draft", draft), mock.patch.object(tool_task.produced, "drafts", lambda: list(made)):
            self.assertEqual(tool_task._draft_once([self.RECORD]), "")
            self.assertEqual(tool_task._draft_once([self.RECORD]), "")  # a redo
        self.assertEqual(len(made), 1)

    def test_a_different_letter_is_drafted(self):
        made = []
        from decurix_agent.tools import outbox
        other = dict(self.RECORD, body="Another")
        with mock.patch.object(outbox, "draft", lambda to, s, b, cc=None, attachments=None: made.append(b)), \
                mock.patch.object(tool_task.produced, "drafts", lambda: [dict(self.RECORD)]):
            tool_task._draft_once([self.RECORD, other])
        self.assertEqual(made, ["Another"])


class ReportsThatAreNotChecks(unittest.TestCase):
    def test_a_workflow_that_stopped_on_an_error_is_not_a_checked_result(self):
        from decurix_agent import workflow_task
        with mock.patch("decurix_agent.messaging.write_final_reply") as sent, \
                mock.patch.object(workflow_task, "runner", side_effect=RuntimeError("boom")):
            workflow_task.perform("/workflow status nope")
        self.assertFalse(sent.call_args.kwargs["checked"])


# Recheck of round 3: verbs of moving money, trading, letting go and signing off; and keys that move the keyboard
# before they press.
MONEY_ASKED = ["Bank Transfer", "Wire transfer", "Fund Transfer", "NEFT Transfer", "Make bank transfer",
               "Initiate bank transfer", "Disburse", "Disburse loan", "Execute trade", "Execute order", "Place trade",
               "Submit trade", "Confirm trade", "Sell", "Sell now", "Sell all shares", "Release", "Release payment",
               "Fund release", "Sign off", "Sign-off", "Signoff", "Approve and sign off", "Refund", "Issue refund",
               "Liquidate"]
# No committing word as a word (or structure): not asked on any control.
MONEY_NOT_ASKED = ["Delete row", "Selling Price", "Sign on", "Disbursement", "Trade history"]
# Names on a link or a navigation item; on a button they ask (D-04).
MONEY_NAMES = ["Stock Transfer", "Material Transfer", "Make Purchase Receipt", "Post Date", "New Bank Transfer",
               "Bank Transfer Details", "Bank Transfer history", "Sell Price", "Release Notes", "Release Date",
               "Sign off date", "Refund policy"]


class MovingMoneyAsks(unittest.TestCase):
    def test_moving_money_trading_releasing_and_signing_off_ask(self):
        for name in MONEY_ASKED:
            with self.subTest(name=name):
                self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_the_same_words_as_names_of_things_do_not(self):
        for name in MONEY_NOT_ASKED:
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("button", name)))
        for name in MONEY_NAMES:
            with self.subTest(name=name):
                self.assertIsNone(approval.assess("click", control("link", name)))
                self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_stock_transfer_stays_a_record_type_in_a_menu(self):
        for role in ("link", "menuitem"):
            self.assertIsNone(approval.press_effect(nav(role, "Stock Transfer")))
            self.assertEqual(approval.press_effect(nav(role, "Bank Transfer")), "asks")


class KeysThatMoveTheKeyboardFirst(unittest.TestCase):
    PAY = control("button", "Pay now")
    SAVE = control("button", "Save")

    def test_which_presses_come_after_a_move(self):
        moved = approval.press_after_focus_move
        self.assertEqual(moved(["Tab", "Return"]), "Return")
        self.assertEqual(moved(["shift+Tab", "space"]), "space")
        self.assertEqual(moved(["Tab+Return"]), "Tab+Return")
        self.assertEqual(moved(["Down", "Down", "Enter"]), "Enter")
        self.assertEqual(moved(["Escape", "Return"]), "Return")
        self.assertEqual(moved(["Return", "Return"]), "Return")  # the first may open a dialog
        self.assertEqual(moved(["Tab", "Return"], control("textbox", "Amount")), "Return")

    def test_keys_that_stay_in_a_field_are_not_a_move(self):
        moved = approval.press_after_focus_move
        self.assertIsNone(moved(["Return"]))
        self.assertIsNone(moved(["space"]))
        self.assertIsNone(moved(["ctrl+a", "BackSpace", "Return"], control("textbox", "Amount")))
        self.assertIsNone(moved(["h", "i", "space", "End", "Return"], control("textbox", "Amount")))
        self.assertIsNone(moved(["Down", "Return"], control("searchbox", "Search")))
        self.assertIsNone(moved(["Return", "Return"], control("document", "Body")))
        self.assertIsNone(moved(["Tab"]))

    def test_tab_then_return_where_a_pay_button_is_asks(self):
        why = approval.assess("key", None, keys=["Tab", "Return"], context={"controls": [self.SAVE, self.PAY]})
        self.assertIn("Pay now", why)
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab+Return"], context={"controls": [self.PAY]}))

    def test_a_screen_with_nothing_to_commit_is_not_asked(self):
        self.assertIsNone(approval.assess("key", None, keys=["Tab", "Return"], context={"controls": [self.SAVE]}))

    def test_a_screen_not_known_is_taken_to_hold_one(self):
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab", "Return"], context={}))

    def test_a_dialog_that_cannot_be_taken_back_counts(self):
        yes = control("button", "Yes", path='dialog "Permanently delete INV-7?" > button "Yes"')
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab", "space"], context={"controls": [yes]}))

    def test_leaving_a_search_box_in_a_chat_and_pressing_enter_sends(self):
        search = control("searchbox", "Search messages")
        chat = {"window": "Slack - Chromium", "focused": search, "controls": []}
        self.assertIsNone(approval.assess("key", None, keys=["Return"], context=chat))
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab", "Return"], context=chat))

    def test_a_single_return_in_an_editor_or_a_search_box_is_not_asked(self):
        for field in (control("document", "Body"), control("searchbox", "Search")):
            with self.subTest(role=field["role"]):
                self.assertIsNone(approval.assess("key", field, keys=["Return"], context={"controls": [self.PAY]}))
                self.assertIsNone(approval.assess("key", field, keys=["ctrl+a", "x", "Return"],
                                                  context={"controls": [self.PAY]}))

    def test_the_button_focused_before_tab_is_not_the_one_pressed(self):
        save = control("button", "Save", states=["focused"])
        before = {"elements": [save], "active_title": "Pay - Chromium", "page_url": ""}
        self.assertIsNone(core._pressed_by_key("key", None, ["Tab", "Return"], before))
        self.assertIs(core._pressed_by_key("key", None, ["Return", "Tab", "Return"], before), save)


class KeysThroughAct(unittest.TestCase):
    """core.act gives the gate the screen's controls when keys move the keyboard and then press."""

    def act(self, keys, *elements):
        page = {"elements": list(elements), "groups": [{"id": "g", "window_title": "Pay - Chromium", "source": "cdp",
                                                       "url": "https://shop.test/pay"}],
                "active_title": "Pay - Chromium", "frame": None, "size": [1280, 720], "windows": []}
        seen = {}

        class Stop(Exception):
            pass

        def check(kind, element, text=None, keys=None, point=None, context=None, require=None, **_):
            seen["why"] = approval.assess(kind, element, text=text, keys=keys, context=context) or require
            raise Stop()

        with mock.patch.object(core, "observe", lambda *a, **k: page), \
                mock.patch.object(core.audit, "record", lambda *a, **k: None), \
                mock.patch.object(core, "_downloads_snapshot", lambda: None), \
                mock.patch.object(core, "_refuse_unsafe_save", lambda *a: None), \
                mock.patch.object(core, "_refuse_number_arrows", lambda *a: None), \
                mock.patch.object(core, "_commit_check", lambda *a, **k: ""), \
                mock.patch.object(core.cdp, "available", lambda: False), \
                mock.patch.object(core, "_perform", mock.Mock(side_effect=AssertionError("pressed"))), \
                mock.patch.object(approval, "check", check):
            with self.assertRaises(Stop):
                core.act("key", keys=keys, check_form=True)
        return seen["why"]

    def test_tab_and_return_in_one_call_on_a_page_with_pay_is_asked(self):
        self.assertIn("Pay now", self.act(["Tab", "Return"], control("textbox", "Name", states=["focused"]),
                                          control("button", "Pay now", ref="e2")))

    def test_the_same_keys_on_a_page_that_only_saves_are_not(self):
        self.assertIsNone(self.act(["Tab", "Return"], control("textbox", "Name", states=["focused"]),
                                   control("button", "Save", ref="e2")))

    def test_typing_and_return_in_a_field_are_not(self):
        self.assertIsNone(self.act(["ctrl+a", "BackSpace", "Return"], control("textbox", "Name", states=["focused"]),
                                   control("button", "Pay now", ref="e2")))


if __name__ == "__main__":
    unittest.main()
