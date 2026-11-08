"""
On a desktop with an approval gate, a shell command held for a yes runs only on the gate's grant (review 2026-11-03,
F-09). The reply's words never decide: the gate is asked for exactly the held command (gated_command), and a gate that
does not answer allows nothing. The legacy harnesses that first held commands this way (the Ollama task runner, Azure)
were removed on 2026-11-07 (O-4); long_task, workflow_task and watchers hold them now.
"""

import unittest
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent import gated_command
from decurix_agent.computer import approval

COMMAND = "rm -rf /home/headless/work/old"


class FakeGate:
    """The gate's socket: "check" answers from `grants` (fingerprint-free here: one held command per test)."""

    def __init__(self, grants=0, down=False):
        self.grants, self.down, self.calls = grants, down, []

    def __call__(self, op, timeout=None, **args):
        self.calls.append((op, args))
        if self.down:
            raise OSError("no gate")
        if op != "check":
            return {}
        if self.grants:
            self.grants -= 1
            return {"allowed": True, "approved": True, "request": None}
        return {"allowed": False, "request": {"id": "r1", "fingerprint": "f1", "question": "q", "sig": "s"}}


class AGateThatIsDown(unittest.TestCase):
    def test_a_command_held_while_the_gate_is_down_is_not_run_and_not_asked(self):
        with patch.object(approval, "gate_call", FakeGate(down=True)):
            self.assertEqual(gated_command.check(gated_command.material(COMMAND)), "no gate")


class AgainstTheRealGateStore(unittest.TestCase):
    """A held command end to end against the gate's own store (gate_server.handle in this process, as
    test_gate_server drives it): a held command is the gate's pending request, a signed yes to it makes one grant,
    granted() is true once, and an amended reply grants nothing. Review 2026-11-03 rework."""

    KEY = "k3y"

    def setUp(self):
        import tempfile
        from pathlib import Path
        from decurix_agent import gate_server
        self.gate = gate_server
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        d = Path(self.tmp.name)
        for p in (patch.object(approval, "STATE_DIR", d), patch.object(approval, "STATE_FILE", d / "approval.json"),
                  patch.object(approval, "IN_GATE", True), patch.dict(gate_server._key, {"value": self.KEY}),
                  # The agent's side: this desktop has a gate, reached in this process.
                  patch.object(approval, "remote", return_value=True),
                  patch.object(approval, "gate_call", side_effect=lambda op, timeout=None, **a:
                               gate_server.handle(dict(a, op=op)))):
            p.start()
            self.addCleanup(p.stop)
        gate_server._settled.clear()
        gate_server._nonces.clear()
        self.held = gated_command.material(COMMAND, request="tidy the work folder")

    def ask(self):
        self.assertEqual(gated_command.check(self.held), "asked")
        return approval._local_pending()

    def settle(self, text, reading):
        request = self.ask()
        question = approval.request_text(request)
        with patch.object(approval, "reading", return_value=reading):
            self.gate.settle(text, question, "approval", request["id"])

    def test_a_signed_yes_grants_exactly_once(self):
        self.settle("yes", {"decision": "yes", "condition": ""})
        self.assertTrue(gated_command.granted(self.held, "yes", "q"))
        self.assertFalse(gated_command.granted(self.held, "yes", "q"), "a grant is one action's, once")

    def test_an_amended_reply_grants_nothing(self):
        self.settle("yes, but in the other folder", {"decision": "no", "condition": "", "wants": "the other folder"})
        self.assertFalse(gated_command.granted(self.held, "yes, but in the other folder", "q"))

    def test_a_yes_for_another_command_is_no_yes_for_this_one(self):
        self.settle("yes", {"decision": "yes", "condition": ""})
        other = gated_command.material("rm -rf /home/headless/work/new", request="tidy the work folder")
        self.assertFalse(gated_command.granted(other, "yes", "q"))
        self.assertTrue(gated_command.granted(self.held, "yes", "q"))


if __name__ == "__main__":
    unittest.main()
