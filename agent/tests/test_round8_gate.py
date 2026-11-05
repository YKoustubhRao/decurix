"""
Live round 3, F-R3-3 (2026-11-04): on a pay-approval case the worker clicked by ref, and the gate asked "Approval needed
before I run the command `gui click e33` ...". A person could not tell they were approving the Pay button. A held
`gui` command on a ref now carries what the ref resolves to on the screen (material "target"), and the gate's question
names it - role, accessible name and window. A ref nothing resolved is said to be unknown, never shown bare as if it
said what it is. The fingerprint stays over the raw command.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, gate_server as gate, gated_command  # noqa: E402
from decurix_agent.computer import approval, privacy  # noqa: E402

TASK = "t000000000000000a"
OBSERVATION = {"elements": [{"ref": "e33", "role": "button", "name": "Pay ₹12,400", "group": "g1"},
                            {"ref": "e7", "role": "textbox", "name": "Notes", "group": "g1"}],
               "groups": [{"id": "g1", "window_title": "Checkout - Chromium"}], "active_title": "Checkout - Chromium"}


class InTheGate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(config, "DECURIX_DIR", Path(tmp.name)), mock.patch.object(approval, "IN_GATE", True)):
            p.start()
            self.addCleanup(p.stop)

    def ask(self, material):
        out = gate.handle({"op": "check", "material": dict(material, task_id=TASK)})
        self.assertFalse(out["allowed"])
        return out["request"]


class AClickByRef(InTheGate):
    def test_the_question_names_the_control(self):
        with mock.patch("decurix_agent.computer.core._load_state", lambda: OBSERVATION):
            held = gated_command.material("gui click e33", request="pay the vendor", workdir="/home/headless/work")
        self.assertEqual(held["target"], {"ref": "e33", "role": "button", "name": "Pay ₹12,400",
                                          "window_title": "Checkout - Chromium"})
        request = self.ask(held)
        self.assertIn('button "Pay ₹12,400" (e33)', request["action"])
        self.assertIn("Checkout - Chromium", request["action"])
        self.assertIn("Pay ₹12,400", request["question"])

    def test_the_fingerprint_is_the_raw_command(self):
        named = dict(gated_command.material("gui click e33", "r", "/w"), target=approval.command_target(
            "gui click e33", OBSERVATION))
        bare = gated_command.material("gui click e33", "r", "/w")
        bare.pop("target", None)
        self.assertEqual(approval.from_material(named)[0], approval.from_material(bare)[0])


class ARefNothingResolved(InTheGate):
    def test_is_said_to_be_unknown(self):
        with mock.patch("decurix_agent.computer.core._load_state", lambda: {"elements": []}):
            held = gated_command.material("gui click e33", "r", "/w")
        self.assertEqual(held["target"], {"ref": "e33", "unresolved": True})
        request = self.ask(held)
        self.assertIn("what e33 is cannot be told from here", request["action"])
        self.assertNotRegex(request["action"], r"^run the command `gui click e33`")

    def test_soul_sending_no_target_is_the_same(self):
        request = self.ask({"kind": "command", "command": "gui click e33", "request": "r", "workdir": "/w"})
        self.assertIn("what e33 is cannot be told from here", request["action"])

    def test_a_target_of_another_ref_names_nothing(self):
        request = self.ask({"kind": "command", "command": "gui click e33", "request": "r", "workdir": "/w",
                            "target": {"ref": "e7", "role": "textbox", "name": "Notes"}})
        self.assertIn("what e33 is cannot be told from here", request["action"])
        self.assertNotIn("Notes", request["action"])


class TheNamedWordsAreWithheld(InTheGate):
    def test_a_secret_in_the_name(self):
        gate.keep_secrets(TASK, ["Copper71meadowRiver"])
        request = self.ask({"kind": "command", "command": "gui click e33", "request": "r", "workdir": "/w",
                            "target": {"ref": "e33", "role": "button", "name": "Use key Copper71meadowRiver",
                                       "window_title": "Portal"}})
        self.assertNotIn("Copper71meadowRiver", request["action"] + request["question"])
        self.assertIn('button "Use key [withheld]" (e33)', request["action"])

    def test_a_name_that_cannot_be_checked_refuses(self):
        real = approval.shown_by_gate
        with mock.patch.object(approval, "shown_by_gate",
                               lambda text: privacy.HELD_BACK if text == "Pay" else real(text)):
            out = gate.handle({"op": "check", "material": {
                "kind": "command", "command": "gui click e33", "request": "r", "workdir": "/w", "task_id": TASK,
                "target": {"ref": "e33", "role": "button", "name": "Pay", "window_title": "Shop"}}})
        self.assertIs(out.get("refused"), True)
        self.assertIsNone(out["request"])


class OtherCommands(InTheGate):
    def test_a_command_with_no_ref_reads_as_before(self):
        self.assertIsNone(gated_command.material("rm -rf build", "r", "/w").get("target"))
        request = self.ask(gated_command.material("rm -rf build", "r", "/w"))
        self.assertEqual(request["action"], "run the command `rm -rf build` in /w")

    def test_typing_by_ref_names_the_field(self):
        request = self.ask({"kind": "command", "command": 'gui type --enter e7 "hello there"', "request": "r",
                            "workdir": "/w", "target": approval.command_target("gui type --enter e7 x", OBSERVATION)})
        self.assertIn('type textbox "Notes" (e7)', request["action"])


if __name__ == "__main__":
    unittest.main()
