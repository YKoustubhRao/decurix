"""
On a desktop with an approval gate, a shell command held for a yes runs only on the gate's grant (review 2026-11-03,
F-09; owner decision O-4). The legacy harnesses (the Ollama task runner, Azure) used to decide that yes from their own
reading of the reply. Here the reply's words never decide: the gate is asked for exactly the held command, and a gate
that does not answer allows nothing.
"""

import unittest
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent import azure_harness, gated_command, state, task_runner
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


def never(*args, **kwargs):
    raise AssertionError("the reply's words must not decide a yes on a gated desktop")


class TheTaskRunner(unittest.TestCase):
    def setUp(self):
        self.ran, self.queries, self.finals = [], [], []
        for p in (patch.object(approval, "remote", return_value=True),
                  patch.object(approval, "reading", side_effect=never),
                  patch.object(task_runner, "run_command",
                               side_effect=lambda c: self.ran.append(c) or {"exit_code": 0, "output": ""}),
                  patch.object(task_runner, "ask_for_query", side_effect=lambda t, **k: self.queries.append((t, k))),
                  patch.object(task_runner, "write_final_reply", side_effect=lambda t, **k: self.finals.append((t, k))),
                  patch.object(task_runner.progress, "emit")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(setattr, task_runner, "_pending_dangerous_command", None)
        self.addCleanup(setattr, task_runner, "_pending_gate_material", None)

    def test_the_held_command_is_put_to_the_gate_and_asked_as_an_approval(self):
        gate = FakeGate()
        with patch.object(approval, "gate_call", gate):
            task_runner.pause_for_dangerous_command(COMMAND, "it deletes files")
        self.assertEqual(gate.calls[0][0], "check")
        self.assertEqual(gate.calls[0][1]["material"]["kind"], "command")
        self.assertEqual(gate.calls[0][1]["material"]["command"], COMMAND)
        self.assertEqual(self.queries[0][1].get("kind"), "approval")

    def test_a_plain_yes_the_gate_did_not_grant_runs_nothing(self):
        gate = FakeGate()
        with patch.object(approval, "gate_call", gate):
            task_runner.pause_for_dangerous_command(COMMAND, "it deletes files")
            command, _ = task_runner.resume_approved_dangerous_command([], "yes")
        self.assertIsNone(command)
        self.assertEqual(self.ran, [])

    def test_the_gates_grant_runs_exactly_the_held_command_once(self):
        gate = FakeGate()
        with patch.object(approval, "gate_call", gate):
            task_runner.pause_for_dangerous_command(COMMAND, "it deletes files")
            gate.grants = 1  # the person's signed yes made one grant
            command, _ = task_runner.resume_approved_dangerous_command([], "whatever the words")
            again, _ = task_runner.resume_approved_dangerous_command([], "yes")
        self.assertEqual(command, COMMAND)
        self.assertIsNone(again)
        self.assertEqual(self.ran, [COMMAND])

    def test_a_gate_that_does_not_answer_allows_nothing_and_asks_nothing(self):
        with patch.object(approval, "gate_call", FakeGate(down=True)):
            task_runner.pause_for_dangerous_command(COMMAND, "it deletes files")
            command, _ = task_runner.resume_approved_dangerous_command([], "yes")
        self.assertEqual(self.queries, [])
        self.assertIsNone(command)
        self.assertEqual(self.ran, [])
        self.assertEqual(self.finals[0][1]["kind"], "error")
        self.assertIn("Not run", self.finals[0][0])


class TheAzureHarness(unittest.TestCase):
    def setUp(self):
        self.shell = []
        for p in (patch.object(approval, "remote", return_value=True),
                  patch.object(approval, "says_yes", side_effect=never),
                  patch.object(azure_harness, "_shell", side_effect=lambda c, *a: self.shell.append(c) or "exit 0"),
                  patch.object(azure_harness, "_run", side_effect=lambda messages: setattr(self, "messages", messages)),
                  patch.object(azure_harness, "log_banner")):
            p.start()
            self.addCleanup(p.stop)
        saved = (state.current_task, state.waiting_for_query, azure_harness._conversation, azure_harness._paused)
        self.addCleanup(lambda: (setattr(state, "current_task", saved[0]), setattr(state, "waiting_for_query", saved[1]),
                                 setattr(azure_harness, "_conversation", saved[2]),
                                 setattr(azure_harness, "_paused", saved[3])))
        state.current_task = "tidy the work folder"

    def pause(self, gate):
        azure_harness._conversation = []
        azure_harness._paused = {"call_id": "d1", "tool": "shell", "command": COMMAND,
                                 "gate": gated_command.material(COMMAND, request=state.current_task)}

    def answer(self):
        return [m for m in self.messages if m.get("tool_call_id") == "d1"][0]["content"]

    def test_a_yes_the_gate_did_not_grant_runs_nothing(self):
        gate = FakeGate()
        self.pause(gate)
        with patch.object(approval, "gate_call", gate):
            azure_harness.continue_after_query("yes")
        self.assertEqual(self.shell, [])
        self.assertIn("did not approve", self.answer())

    def test_the_gates_grant_runs_it(self):
        gate = FakeGate(grants=1)
        self.pause(gate)
        with patch.object(approval, "gate_call", gate):
            azure_harness.continue_after_query("yes")
        self.assertEqual(self.shell, [COMMAND])

    def test_a_command_held_while_the_gate_is_down_is_not_run_and_not_asked(self):
        with patch.object(approval, "gate_call", FakeGate(down=True)):
            self.assertEqual(gated_command.check(gated_command.material(COMMAND)), "no gate")


if __name__ == "__main__":
    unittest.main()
