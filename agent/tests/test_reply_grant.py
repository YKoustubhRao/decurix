"""The person's latest reply is the yes of the action the gate stops next only when the question it answered named that
very control and the reply, read against that question and that action, approves it - one reply, one action (found live
2026-10-26: "Yes, file the return", then the gate asked for the FILE click). A word the reply shares with the action is
not that (review 2026-11-02: a reply that shared a four-letter word with an action, or with the gate's own reason text,
stood as its yes)."""
import pathlib
import tempfile
import time
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import llm_client
from decurix_agent.computer import approval

FILE = {"role": "button", "name": "FILE WITH EVC", "window_title": "Returns - Chromium", "source": "cdp"}
SEND = {"role": "button", "name": "Send", "window_title": "Chat - Chromium", "source": "cdp"}
ASKED_FILE = "Before I file: the figures match the register. May I click FILE WITH EVC now?"


class ReplyGrants(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        d = pathlib.Path(self.tmp.name)
        self.reads = []
        for p in (mock.patch.object(approval, "STATE_DIR", d),
                  mock.patch.object(approval, "STATE_FILE", d / "approval.json"),
                  mock.patch.object(approval, "assess", lambda *a, **k: "it does something on your behalf"),
                  mock.patch.object(approval, "reading", self.read)):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()

    def read(self, reply, action="", plain=True):
        """The model's reading, scripted: what it is asked is kept."""

        self.reads.append({"reply": reply, "action": action, "plain": plain})
        low = reply.lower()
        if low.startswith("no"):
            return {"decision": "no", "condition": ""}
        if "if " in low or "provided" in low:
            return {"decision": "if", "condition": "x"}
        if low.startswith(("yes", "file", "send", "haan")):
            return {"decision": "yes", "condition": ""}
        return {"decision": "unclear", "condition": ""}

    def test_a_yes_to_a_question_that_named_the_control_is_its_grant_once(self):
        approval.note_reply("Yes, file the return now - the figures match.", asked=ASKED_FILE)
        d = approval.check("click", FILE)
        self.assertTrue(d.allowed and d.approved)
        self.assertIn("file the return", d.by_reply)
        # Read by the model against the question and this exact action - never settled as a plain yes by code.
        self.assertFalse(self.reads[-1]["plain"])
        self.assertIn(ASKED_FILE, self.reads[-1]["action"])
        self.assertIn("FILE WITH EVC", self.reads[-1]["action"])
        # spent: the same action again, or another one, is asked
        self.assertFalse(approval.check("click", FILE).allowed)

    def test_a_yes_to_a_question_that_did_not_name_the_control_grants_nothing(self):
        # Shares "file" with the action and "something" with its reason: still not the question about it.
        approval.note_reply("Yes, file the return; something else later.", asked="Should I prepare the return file?")
        self.assertFalse(approval.check("click", FILE).allowed)
        self.assertEqual(self.reads, [])                                   # not even read: it was not about this
        approval.reset()
        approval.note_reply("Yes, send that message to the client.", asked="May I click Send on the chat?")
        self.assertFalse(approval.check("click", FILE).allowed)            # names Send, not FILE WITH EVC
        approval.reset()
        approval.note_reply("Yes, send that message to the client.", asked="May I click Send on the chat?")
        self.assertTrue(approval.check("click", SEND).allowed)

    def test_a_question_that_names_part_of_a_label_is_not_about_it(self):
        self.assertFalse(approval.names_control("May I file it now?", FILE))
        self.assertFalse(approval.names_control("Should I resend it?", SEND))
        self.assertTrue(approval.names_control("May I press 'File with EVC'?", FILE))

    def test_a_conditional_an_unclear_or_a_no_never_grants(self):
        for reply in ("File it only if the summary equals the register.", "No - do not file yet, fix table 7 first.",
                      "what does it file?"):
            with self.subTest(reply=reply):
                approval.reset()
                approval.note_reply(reply, asked=ASKED_FILE)
                self.assertFalse(approval.check("click", FILE).allowed)

    def test_an_old_reply_or_one_that_settled_an_approval_is_spent(self):
        approval.note_reply("Yes, file the return.", asked=ASKED_FILE)
        with approval._locked() as st:
            st["last_reply"]["at"] = time.time() - approval.REPLY_GRANT_SECONDS - 1
        self.assertFalse(approval.check("click", FILE).allowed)
        approval.reset()
        self.assertFalse(approval.check("click", FILE).allowed)  # pending now
        asked = approval.request_text(approval.pending())
        approval.note_reply("yes", asked=asked)
        self.assertEqual(approval.resolve_reply("yes", asked=asked), "granted")
        # the grant from resolve_reply is consumed by the matching click; the reply itself grants no second action
        self.assertTrue(approval.check("click", FILE).allowed)
        self.assertFalse(approval.check("click", FILE).allowed)
        self.assertFalse(approval.check("click", SEND).allowed)


class WithNoModel(unittest.TestCase):
    """With no model to read it, a reply to another question is never taken for this action's yes: the gate asks."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        d = pathlib.Path(self.tmp.name)
        for p in (mock.patch.object(approval, "STATE_DIR", d),
                  mock.patch.object(approval, "STATE_FILE", d / "approval.json"),
                  mock.patch.object(approval, "assess", lambda *a, **k: "it files a return"),
                  mock.patch.object(llm_client, "available", lambda: False)):
            p.start()
            self.addCleanup(p.stop)
        approval._READ.clear()
        approval.reset()

    def test_a_plain_yes_is_not_settled_by_code_here(self):
        approval.note_reply("yes", asked=ASKED_FILE)
        d = approval.check("click", FILE)
        self.assertFalse(d.allowed)
        self.assertIsNotNone(approval.pending())


if __name__ == "__main__":
    unittest.main()
