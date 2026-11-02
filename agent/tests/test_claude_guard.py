"""
Claude Code runs headless with every tool allowed, behind a PreToolUse hook (claude_guard.py) that refuses a command
driving the screen or the browser around the approval gate, reaching this desktop's agent or DevTools ports, or
touching the gate's state and the person's reply channel. A guard rail, not a boundary (the shell is the gate's own
user). Review 2026-11-02: under --dangerously-skip-permissions alone, Bash could xdotool a click on Pay.
"""

import io
import json
import unittest
from unittest import mock

from decurix_agent import claude_guard, claude_harness


def bash(command):
    return {"tool_name": "Bash", "tool_input": {"command": command}}


class WhatTheGuardRefuses(unittest.TestCase):
    def test_driving_the_screen_the_browser_or_the_agent(self):
        for command in ("xdotool search --name Pay click 1", "DISPLAY=:1 xte 'mouseclick 1'",
                        "curl -s http://127.0.0.1:9222/json", "curl localhost:8081/api/approval",
                        "wget -qO- http://0.0.0.0:4900/api/chat", "echo yes > ~/.decurix/command/123-reply.txt",
                        "python3 -c \"open('/home/headless/.decurix/computer/approval.json','w')\"",
                        "cat $DECURIX_AGENT_TOKEN"):
            with self.subTest(command=command):
                self.assertTrue(claude_guard.verdict(bash(command)))

    def test_ordinary_work_is_let_through(self):
        for command in ("ls -la ~/work", "python3 report.py in.csv out.xlsx", "gui observe", "office read a.pdf",
                        "grep -n total sales.csv"):
            with self.subTest(command=command):
                self.assertEqual(claude_guard.verdict(bash(command)), "")
        self.assertEqual(claude_guard.verdict({"tool_name": "Write", "tool_input": {"file_path": "/home/u/work/a.txt"}}), "")
        self.assertEqual(claude_guard.verdict({"tool_name": "Read", "tool_input": {"file_path": "x"}}), "")

    def test_writing_the_gates_files(self):
        call = {"tool_name": "Write", "tool_input": {"file_path": "/home/u/.decurix/computer/approval.json"}}
        self.assertTrue(claude_guard.verdict(call))
        self.assertTrue(claude_guard.verdict({"tool_name": "WebFetch", "tool_input": {"url": "http://localhost:8092/x"}}))

    def test_it_blocks_with_exit_2_and_fails_closed(self):
        for stdin, code in ((json.dumps(bash("xdotool key Return")), 2), ("not json", 2), (json.dumps(bash("ls")), 0)):
            with self.subTest(stdin=stdin), mock.patch("sys.stdin", io.StringIO(stdin)), \
                    mock.patch("sys.stderr", io.StringIO()) as err:
                self.assertEqual(claude_guard.main(), code)
                if code:
                    self.assertIn("Refused", err.getvalue())


class TheHarnessRunsBehindIt(unittest.TestCase):
    def test_no_dangerous_skip_and_the_hook_is_registered(self):
        seen = {}

        def popen(cmd, **kw):
            seen["cmd"] = cmd
            raise OSError("not here")

        with mock.patch.object(claude_harness.subprocess, "Popen", popen), \
                mock.patch.object(claude_harness, "get_memory_context", lambda: ""):
            claude_harness._run_claude("do it")
        cmd = seen["cmd"]
        self.assertNotIn("--dangerously-skip-permissions", cmd)
        self.assertEqual(cmd[cmd.index("--permission-mode") + 1], "bypassPermissions")
        settings = json.loads(cmd[cmd.index("--settings") + 1])
        bash_hooks = [h for h in settings["hooks"]["PreToolUse"] if h["matcher"] == "Bash"]
        self.assertIn("claude_guard.py", bash_hooks[0]["hooks"][0]["command"])
        self.assertTrue(claude_harness.GUARD_SCRIPT.exists())
        self.assertIn("Bash(xdotool:*)", cmd[cmd.index("--disallowedTools") + 1])


if __name__ == "__main__":
    unittest.main()
