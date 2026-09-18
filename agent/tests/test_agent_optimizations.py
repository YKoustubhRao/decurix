import unittest
from unittest.mock import patch

from flowera_agent.parser import clean_command, parse_agent_response
from flowera_agent.ollama_client import ask_agent
from flowera_agent.task_runner import compact_history, format_command_result


class TestAgentParserOptimizations(unittest.TestCase):
    def test_clean_command_strips_code_fences(self):
        fenced_bash = "```bash\nls -la /tmp\n```"
        self.assertEqual(clean_command(fenced_bash), "ls -la /tmp")

        fenced_sh = "```sh\necho 'hello world'\n```"
        self.assertEqual(clean_command(fenced_sh), "echo 'hello world'")

        fenced_plain = "```\ncat /etc/hosts\n```"
        self.assertEqual(clean_command(fenced_plain), "cat /etc/hosts")

        backticks = "`uname -a`"
        self.assertEqual(clean_command(backticks), "uname -a")

    def test_parse_agent_response_unwraps_fenced_command(self):
        resp = "COMMAND:\n```bash\nls -la\n```"
        action, value = parse_agent_response(resp)
        self.assertEqual(action, "command")
        self.assertEqual(value, "ls -la")

        resp_backticks = "COMMAND: `df -h`"
        action, value = parse_agent_response(resp_backticks)
        self.assertEqual(action, "command")
        self.assertEqual(value, "df -h")

    def test_parse_agent_response_done_retains_markdown(self):
        resp = "DONE:\n```json\n{\"status\": \"ok\"}\n```"
        action, value = parse_agent_response(resp)
        self.assertEqual(action, "done")
        self.assertEqual(value, "```json\n{\"status\": \"ok\"}\n```")


class TestOllamaClientMessageOrdering(unittest.TestCase):
    @patch("flowera_agent.ollama_client.ollama_request")
    def test_ask_agent_puts_task_before_history(self, mock_ollama):
        mock_ollama.return_value = "DONE: all set"

        task = "list files in /var"
        history = [
            {"role": "assistant", "content": "COMMAND: ls /var"},
            {"role": "user", "content": "log tmp run"},
        ]

        ask_agent(task, history)

        self.assertTrue(mock_ollama.called)
        sent_messages = mock_ollama.call_args[0][0]

        # Verify roles: system messages first, then user task, then history
        system_msgs = [m for m in sent_messages if m["role"] == "system"]
        self.assertTrue(len(system_msgs) >= 2)

        non_system = [m for m in sent_messages if m["role"] != "system"]
        # The very first non-system message MUST be the original user task
        self.assertEqual(non_system[0]["role"], "user")
        self.assertEqual(non_system[0]["content"], task)

        # The subsequent messages must be the history turns
        self.assertEqual(non_system[1:], history)


class TestTaskRunnerContextCompaction(unittest.TestCase):
    def test_compact_history_within_limit_unchanged(self):
        history = [
            {"role": "assistant", "content": "COMMAND: ls"},
            {"role": "user", "content": "output"},
        ]
        self.assertEqual(compact_history(history, max_recent=6), history)

    def test_compact_history_compresses_old_turns(self):
        history = []
        long_output = "x" * 2000
        for i in range(10):
            history.append({"role": "assistant", "content": f"COMMAND: step_{i}"})
            history.append({
                "role": "user",
                "content": format_command_result({"exit_code": 0, "output": f"step_{i}: {long_output}"})
            })

        # max_recent = 4 (last 8 messages kept in full)
        compacted = compact_history(history, max_recent=4)
        self.assertEqual(len(compacted), len(history))

        # Check an old user message (e.g. index 1)
        old_user_msg = compacted[1]["content"]
        self.assertIn("[earlier output compacted for context budget]", old_user_msg)
        self.assertTrue(len(old_user_msg) < 500)

        # Check a recent user message (e.g. index 19, the last one)
        recent_user_msg = compacted[19]["content"]
        self.assertNotIn("[earlier output compacted for context budget]", recent_user_msg)
        self.assertIn("step_9: xxxx", recent_user_msg)

    def test_compact_history_preserves_prefix_when_under_budget(self):
        # 14 messages (7 turns) with small outputs (< 16000 chars) must NOT be mutated
        # so llama.cpp's KV prefix cache remains valid.
        history = []
        for i in range(7):
            history.append({"role": "assistant", "content": f"COMMAND: step_{i}"})
            history.append({
                "role": "user",
                "content": format_command_result({"exit_code": 0, "output": f"small output {i}"})
            })

        compacted = compact_history(history)
        self.assertEqual(compacted, history)


class TestInferenceEndpointSelection(unittest.TestCase):
    def test_is_openai_endpoint(self):
        from flowera_agent.ollama_client import is_openai_endpoint
        self.assertTrue(is_openai_endpoint("http://vllm-server:8000/v1"))
        self.assertTrue(is_openai_endpoint("http://vllm-server:8000/v1/"))
        self.assertFalse(is_openai_endpoint("http://localhost:11434"))

    def test_get_optimal_threads(self):
        from flowera_agent.ollama_client import get_optimal_threads
        threads = get_optimal_threads()
        self.assertIsInstance(threads, int)
        self.assertGreater(threads, 0)


if __name__ == "__main__":
    unittest.main()

