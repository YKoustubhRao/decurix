"""The person's latest reply that plainly says yes to the very action the gate stops next is that action's yes -
one reply, one action (found live 2026-10-26: "Yes, file the return", then the gate asked for the FILE click)."""
import pathlib
import tempfile
import time
import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import approval

FILE = {"role": "button", "name": "FILE WITH EVC", "window_title": "Returns - Chromium", "source": "cdp"}
SEND = {"role": "button", "name": "Send", "window_title": "Chat - Chromium", "source": "cdp"}


class ReplyGrants(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        d = pathlib.Path(self.tmp.name)
        for p in (mock.patch.object(approval, "STATE_DIR", d),
                  mock.patch.object(approval, "STATE_FILE", d / "approval.json"),
                  mock.patch.object(approval, "assess", lambda *a, **k: "it files a return"),
                  mock.patch.object(approval, "reading", self.read)):
            p.start()
            self.addCleanup(p.stop)
        approval.reset()

    @staticmethod
    def read(reply, action=""):
        low = reply.lower()
        if low.startswith("no"):
            return {"decision": "no", "condition": ""}
        if "if " in low or "provided" in low:
            return {"decision": "if", "condition": "x"}
        if low.startswith("yes") or low.startswith("file") or low.startswith("send"):
            return {"decision": "yes", "condition": ""}
        return {"decision": "unclear", "condition": ""}

    def test_a_yes_naming_the_action_is_its_grant_once(self):
        approval.note_reply("Yes, file the return now - the figures match.", asked="Before I file: ...")
        d = approval.check("click", FILE)
        self.assertTrue(d.allowed and d.approved)
        self.assertIn("file the return", d.by_reply)
        # spent: the same action again, or another one, is asked
        self.assertFalse(approval.check("click", FILE).allowed)

    def test_a_yes_to_something_else_grants_nothing(self):
        approval.note_reply("Yes - those two places: my message and the client's chat. Quote the line.")
        self.assertFalse(approval.check("click", FILE).allowed)
        approval.reset()
        approval.note_reply("Yes, send that message to the client.")
        self.assertFalse(approval.check("click", FILE).allowed)  # names send, not file
        approval.reset()
        approval.note_reply("Yes, send that message to the client.")
        self.assertTrue(approval.check("click", SEND).allowed)

    def test_a_conditional_or_a_no_never_grants(self):
        approval.note_reply("File it only if the summary equals the register.")
        self.assertFalse(approval.check("click", FILE).allowed)
        approval.reset()
        approval.note_reply("No - do not file yet, fix table 7 first.")
        self.assertFalse(approval.check("click", FILE).allowed)

    def test_an_old_reply_or_one_that_settled_an_approval_is_spent(self):
        approval.note_reply("Yes, file the return.")
        with approval._locked() as st:
            st["last_reply"]["at"] = time.time() - approval.REPLY_GRANT_SECONDS - 1
        self.assertFalse(approval.check("click", FILE).allowed)
        approval.reset()
        self.assertFalse(approval.check("click", FILE).allowed)  # pending now
        approval.note_reply("yes")
        self.assertEqual(approval.resolve_reply("yes", asked="Approval needed before I click FILE WITH EVC"), "granted")
        # the grant from resolve_reply is consumed by the matching click; the reply itself grants no second action
        self.assertTrue(approval.check("click", FILE).allowed)
        self.assertFalse(approval.check("click", SEND).allowed)


if __name__ == "__main__":
    unittest.main()
