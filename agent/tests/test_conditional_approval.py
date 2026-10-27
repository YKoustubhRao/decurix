"""A yes on a condition grants nothing yet (2026-10-23)."""
import json
import unittest
from unittest import mock

from decurix_agent import llm_client
from decurix_agent.computer import approval


class AReplyToAnApprovalIsReadForWhatItDecides(unittest.TestCase):

    def model_says(self, decision):
        answer = {"content": json.dumps({"decision": decision, "condition": "x"})}
        return [mock.patch.object(llm_client, "available", lambda: True),
                mock.patch.object(llm_client, "chat", lambda *a, **k: (answer, {}))]

    def test_a_bare_yes_or_no_needs_no_model(self):
        with mock.patch.object(llm_client, "chat", side_effect=AssertionError("asked")):
            self.assertEqual(approval.read_reply("yes", "file"), "yes")
            self.assertEqual(approval.read_reply("ok go ahead", "file"), "yes")
            self.assertEqual(approval.read_reply("no", "file"), "no")

    def test_a_yes_on_a_condition_is_not_a_grant(self):
        reply = ("Provided you looked in the client's folder and ClientChat and found nothing saying 455 was "
                 "cancelled, yes - file GSTR-1 with EVC and ask me for the OTP.")
        patches = self.model_says("if")
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.assertEqual(approval.read_reply(reply, 'click button "FILE WITH EVC"'), "if")

    def test_a_yes_with_remarks_is_a_yes(self):
        patches = self.model_says("yes")
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.assertEqual(approval.read_reply("yes - the client has paid the challan; offset and file.", "offset"), "yes")

    def test_with_no_model_the_word_list_decides_as_before(self):
        with mock.patch.object(llm_client, "available", lambda: False):
            self.assertEqual(approval.read_reply("yes - the client has paid the challan; offset and file.", "x"), "yes")
            self.assertEqual(approval.read_reply("no, do not file it yet please", "x"), "no")


if __name__ == "__main__":
    unittest.main()
