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
NOT_ASKED = ["Make Purchase Receipt", "Make Payment Entry", "Stock Transfer", "Material Transfer", "Post Date",
             "Get Items From Purchase Order", "Buy Price", "Purchase Taxes and Charges", "Delete row", "Delete Rows",
             "Delete selected rows", "Send to back", "Payment history", "View post", "Sign in", "Sign up", "Sign out",
             "File returns", "Save", "Save & Submit", "Create", "Update", "Accept"]


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

    def test_deleting_a_form_part_is_not_deleting_a_record(self):
        self.assertIsNone(approval.press_effect(control("button", "Delete row")))
        self.assertEqual(approval.press_effect(control("button", "Delete invoice")), "asks")
        self.assertEqual(approval.press_effect(control("button", "Delete permanently")), "asks")

    def test_the_same_in_a_link_and_a_menu(self):
        for role in ("link", "menuitem"):
            self.assertEqual(approval.press_effect(control(role, "File GSTR-1")), "asks")
            self.assertIsNone(approval.press_effect(control(role, "Stock Transfer")))


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

    def test_the_gate_takes_a_task_whose_commands_changed_things(self):
        with mock.patch.object(skill_task.guidance, "get", lambda: None), \
                mock.patch.object(skill_task, "_did_screen_work", lambda: False), \
                mock.patch.object(skill_task.produced, "files", lambda *a, **k: []), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.procedures, "in_request", lambda t: False), \
                mock.patch.object(skill_task.produced, "requested_names", lambda t: []):
            with mock.patch.object(playbooks, "changed_things", lambda commands=None: []):
                self.assertFalse(skill_task._gate("Done.", "final", "thebot"))
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


if __name__ == "__main__":
    unittest.main()
