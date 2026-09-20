import unittest
from unittest.mock import patch

from flowera_agent import state, task_runner


def _run(replies, results):
    """Drive perform_task with a scripted model and scripted command results."""

    replies = list(replies)
    results = list(results)
    seen = {"histories": [], "done": [], "queries": []}

    def fake_ask(task, history):
        seen["histories"].append([dict(m) for m in history])
        return replies.pop(0)

    with patch.object(task_runner, "ask_agent", side_effect=fake_ask), \
         patch.object(task_runner, "run_command", side_effect=lambda c: results.pop(0)), \
         patch.object(task_runner, "write_final_reply", side_effect=lambda t, **k: seen["done"].append(t)), \
         patch.object(task_runner, "ask_for_query", side_effect=lambda t, **k: seen["queries"].append(t)), \
         patch.object(task_runner, "pause_for_screen_approval", return_value=False), \
         patch.object(task_runner.progress, "emit"):
        task_runner.perform_task("how many files are in /etc?", None)

    state.current_task = None
    return seen


OK = {"exit_code": 0, "output": "123"}


class TestSuccessQueryNudge(unittest.TestCase):
    def test_help_after_success_is_pushed_back_once(self):
        seen = _run(
            [
                "COMMAND: ls -la /etc | wc -l",
                "HELP: It seems there was an error with the command.",
                "DONE: 123",
            ],
            [OK],
        )
        self.assertEqual(seen["done"], ["123"])
        self.assertEqual(seen["queries"], [])
        nudge = seen["histories"][2][-1]["content"]
        self.assertIn("exit code 0", nudge)
        self.assertIn("123", nudge)

    def test_second_help_is_delivered(self):
        seen = _run(
            [
                "COMMAND: ls /etc | wc -l",
                "HELP: something odd",
                "HELP: still odd",
            ],
            [OK],
        )
        self.assertEqual(seen["queries"], ["still odd"])

    def test_human_only_help_is_never_argued_with(self):
        seen = _run(
            [
                "COMMAND: ls /etc | wc -l",
                "HELP: Do you want me to send an email to the landlord?",
            ],
            [OK],
        )
        self.assertEqual(len(seen["queries"]), 1)

    def test_help_after_a_failure_gets_two_more_tries_then_is_delivered(self):
        fail = {"exit_code": 1, "output": ""}
        seen = _run(
            [
                'COMMAND: df -h | grep "root"',
                "HELP: the command failed",
                "COMMAND: df -h /",
                "HELP: still failing",
                "COMMAND: df",
                "HELP: I give up",
            ],
            [fail, fail, fail],
        )
        self.assertEqual(seen["queries"], ["I give up"])
        self.assertIn("did not work", seen["histories"][2][-1]["content"])
        self.assertIn("exit code 1", seen["histories"][2][-1]["content"])

    def test_failure_pushback_lets_the_agent_recover(self):
        seen = _run(
            [
                'COMMAND: df -h | grep "root"',
                "HELP: the command failed",
                "COMMAND: df -h /",
                "DONE: 8G free",
            ],
            [{"exit_code": 1, "output": ""}, {"exit_code": 0, "output": "8G"}],
        )
        self.assertEqual(seen["done"], ["8G free"])
        self.assertEqual(seen["queries"], [])

    def test_boilerplate_about_permissions_is_not_a_human_only_ask(self):
        seen = _run(
            [
                'COMMAND: df -h | grep "root"',
                "HELP: The command failed to execute. Please check the path or permissions and try again.",
                "COMMAND: df -h /",
                "DONE: 8G free",
            ],
            [{"exit_code": 1, "output": ""}, {"exit_code": 0, "output": "8G"}],
        )
        self.assertEqual(seen["done"], ["8G free"])
        self.assertEqual(seen["queries"], [])

    def test_a_real_permission_question_is_delivered_at_once(self):
        seen = _run(
            ["COMMAND: rm -rf x", "HELP: Do I have your permission to delete the project?"],
            [{"exit_code": 1, "output": "busy"}],
        )
        self.assertEqual(len(seen["queries"]), 1)

    def test_human_only_help_after_a_failure_is_delivered_at_once(self):
        seen = _run(
            ["COMMAND: git push", "HELP: it needs my password or token"],
            [{"exit_code": 1, "output": "auth failed"}],
        )
        self.assertEqual(len(seen["queries"]), 1)


class TestSafetyAndFormatEdges(unittest.TestCase):
    def test_a_safety_question_before_any_command_is_not_argued_away(self):
        seen = _run(
            ["HELP: Are you sure you want to delete all files in /etc? This is irreversible."],
            [],
        )
        self.assertEqual(len(seen["queries"]), 1)
        self.assertEqual(seen["done"], [])

    def test_an_ordinary_question_before_any_command_is_still_pushed_back(self):
        seen = _run(
            ["HELP: Why do you want to know?", "DONE: mk4"],
            [],
        )
        self.assertEqual(seen["done"], ["mk4"])

    def test_a_bare_help_is_never_delivered_as_the_answer(self):
        seen = _run(["HELP"] * 6, [])
        self.assertEqual(seen["done"], [])
        self.assertEqual(len(seen["queries"]), 1)

    def test_code_with_no_marker_is_told_to_write_and_run_it(self):
        seen = _run(
            [
                "```python\nprint(1)\n```",
                "COMMAND: printf 'print(1)' > hello.py && python3 hello.py",
                "DONE: it printed 1",
            ],
            [{"exit_code": 0, "output": "1"}],
        )
        self.assertEqual(seen["done"], ["it printed 1"])
        self.assertIn("nothing has\nbeen written or run", seen["histories"][1][-1]["content"])


class TestDangerousCommandGate(unittest.TestCase):
    def test_detector_flags_the_catastrophic_and_only_that(self):
        from flowera_agent.commands import dangerous_reason as d

        for cmd in ["rm -rf /", "rm -rf /etc/*", "rm -fr ~", "rm -rf $HOME", "sudo rm -rf /usr",
                    "cd /tmp && rm -rf /", "rm -rf /home/headless/*", "mkfs.ext4 /dev/sda1",
                    "dd if=/dev/zero of=/dev/sda", ":(){ :|:& };:", "find / -name x -delete"]:
            self.assertTrue(d(cmd), cmd)

        for cmd in ["rm -rf build/", "rm -rf /tmp/foo", "rm hello.txt", "echo rm -rf /",
                    "dd if=/dev/zero of=test.img count=1", "find . -name '*.pyc' -delete",
                    "rm -r /home/headless/work/old", "ls -la /etc", "git rm -r --cached ."]:
            self.assertIsNone(d(cmd), cmd)

    def test_a_catastrophic_command_is_held_and_never_run(self):
        ran = []
        replies = ["COMMAND: rm -rf /etc/*"]
        queries = []

        with patch.object(task_runner, "ask_agent", side_effect=lambda t, h: replies.pop(0)), \
             patch.object(task_runner, "run_command", side_effect=lambda c: ran.append(c)), \
             patch.object(task_runner, "ask_for_query", side_effect=lambda t, **k: queries.append((t, k))), \
             patch.object(task_runner.progress, "emit"):
            task_runner.perform_task("delete every file in /etc", None)

        state.current_task = None
        self.assertEqual(ran, [])
        self.assertEqual(len(queries), 1)
        self.assertEqual(queries[0][1].get("kind"), "approval")
        self.assertIn("rm -rf /etc/*", queries[0][0])

    def test_a_yes_runs_exactly_the_held_command_once(self):
        task_runner._pending_dangerous_command = "rm -rf /home/headless/work/../x"
        history = []
        ran = []

        with patch.object(task_runner, "run_command", side_effect=lambda c: ran.append(c) or {"exit_code": 0, "output": ""}), \
             patch.object(task_runner.progress, "emit"):
            command, result = task_runner.resume_approved_dangerous_command(history, "yes, go ahead")
            again, _ = task_runner.resume_approved_dangerous_command(history, "yes")

        self.assertEqual(ran, ["rm -rf /home/headless/work/../x"])
        self.assertEqual(command, "rm -rf /home/headless/work/../x")
        self.assertIsNone(again)
        self.assertEqual(history[0]["content"], "COMMAND:\nrm -rf /home/headless/work/../x")

    def test_anything_but_a_yes_drops_the_held_command(self):
        for reply in ["no", "don't", "yes but not that", "do something else"]:
            task_runner._pending_dangerous_command = "mkfs.ext4 /dev/sda1"

            with patch.object(task_runner, "run_command") as run:
                command, _ = task_runner.resume_approved_dangerous_command([], reply)

            self.assertIsNone(command, reply)
            run.assert_not_called()
            self.assertIsNone(task_runner._pending_dangerous_command)


class TestMissingFileHint(unittest.TestCase):
    def test_a_missing_file_failure_says_to_create_it_first(self):
        nudge = task_runner.failure_nudge(
            {"exit_code": 2, "output": "python3: can't open file 'fib.py': [Errno 2] No such file or directory"}
        )
        self.assertIn("does not exist yet", nudge)
        self.assertIn("cat > name.py", nudge)

    def test_other_failures_get_no_such_hint(self):
        nudge = task_runner.failure_nudge({"exit_code": 1, "output": ""})
        self.assertNotIn("does not exist yet", nudge)


class TestTranscriptKeepsOnlyTheCommand(unittest.TestCase):
    def test_guessed_answer_beside_a_command_is_not_kept(self):
        seen = _run(
            [
                "COMMAND: df -h\nDONE: 1.8T total, 20G used",
                "DONE: 121G used",
            ],
            [{"exit_code": 0, "output": "1007G 121G 835G"}],
        )
        assistant = [m["content"] for m in seen["histories"][1] if m["role"] == "assistant"]
        self.assertEqual(assistant, ["COMMAND:\ndf -h"])
        self.assertEqual(seen["done"], ["121G used"])


if __name__ == "__main__":
    unittest.main()
