"""
A reply that approves a changed action is no grant for the action the gate holds (review F-02).

With no model to read it - an Ollama-brain desktop's gate, or a hosted model that failed - a reply beyond a plain yes
or a plain no is "unclear": the action waits and is asked again. It is never taken for a yes to the original amount,
payee, account or time.
"""

import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import gate_server as gate
from decurix_agent import llm_client
from decurix_agent.computer import approval

KEY = "k3y"
PAY = {"kind": "click", "element": {"role": "button", "name": "Pay", "window_title": "Bank"},
       "context": {"url": "https://bank.example/pay"}, "about": "Payee Vendor One, amount 50,000", "sends": "d1"}

# Replies a person writes that change the action, by the part they change.
AMENDING = {
    "amount": "ok, make it 5000 not 50000",
    "payee": "yes, pay vendor two instead",
    "account": "sure, send it to the other account",
    "deferral": "go ahead tomorrow",
    "instead": "fine, but do the smaller one instead",
}


class AnAmendedYesGrantsNothing(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = Path(self.tmp.name)
        self.patches = [mock.patch.object(approval, "STATE_DIR", d), mock.patch.object(approval, "STATE_FILE", d / "approval.json"),
                        mock.patch.object(approval, "IN_GATE", True), mock.patch.dict(gate._key, {"value": KEY})]
        for p in self.patches:
            p.start()
        gate._settled.clear()
        gate._nonces.clear()
        approval._READ.clear()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(lambda: [p.stop() for p in self.patches])

    def settle(self, text):
        request = gate.handle({"op": "check", "material": dict(PAY), "reason": "it pays"})["request"]
        gate.settle(text, request["question"], "approval", request["id"])
        return gate.handle({"op": "check", "material": dict(PAY), "reason": "it pays"})["allowed"]

    def test_with_no_model_an_amending_reply_grants_nothing(self):
        with mock.patch.object(llm_client, "available", return_value=False):
            for part, text in AMENDING.items():
                approval._local_reset()
                with self.subTest(part=part):
                    self.assertFalse(self.settle(text), text)
                    self.assertEqual(approval.reading(text, "click Pay")["decision"], "unclear")

    def test_with_a_model_that_fails_an_amending_reply_grants_nothing(self):
        with mock.patch.object(llm_client, "available", return_value=True), \
                mock.patch.object(llm_client, "chat", side_effect=RuntimeError("no answer")):
            for part, text in AMENDING.items():
                approval._local_reset()
                approval._READ.clear()
                with self.subTest(part=part):
                    self.assertFalse(self.settle(text), text)

    def test_a_plain_yes_still_grants_with_no_model(self):
        with mock.patch.object(llm_client, "available", return_value=False):
            for text in ("yes", "ok", "go ahead"):
                approval._local_reset()
                with self.subTest(text=text):
                    self.assertTrue(self.settle(text))

    def test_the_model_is_told_a_changed_action_is_a_no(self):
        self.assertIn("different amount", approval.READ_SYSTEM)
        self.assertIn("wants", approval.READ_SYSTEM)


if __name__ == "__main__":
    unittest.main()
