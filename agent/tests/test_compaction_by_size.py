"""
The Ollama loop's history is compacted by size, whatever the number of turns (review 2026-11-03, E-32): twelve turns
of 4,000-character outputs are past num_ctx 8192, and Ollama then drops the oldest message - the task itself.
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.task_runner import (MAX_COMMAND_OUTPUT_CHARS, MAX_HISTORY_CHARS_BEFORE_COMPACTION, compact_history,
                                       format_command_result)


def history(turns, size):
    out = []
    for i in range(turns):
        out.append({"role": "assistant", "content": f"COMMAND: step_{i}"})
        out.append({"role": "user", "content": format_command_result({"exit_code": 0, "output": f"s{i}:" + "x" * size})})
    return out


def chars(messages):
    return sum(len(m["content"]) for m in messages)


class BySize(unittest.TestCase):
    def test_a_few_huge_turns_are_cut_to_the_budget(self):
        long = history(8, MAX_COMMAND_OUTPUT_CHARS)
        self.assertGreater(chars(long), MAX_HISTORY_CHARS_BEFORE_COMPACTION)
        out = compact_history(long)
        self.assertLessEqual(chars(out), MAX_HISTORY_CHARS_BEFORE_COMPACTION)
        self.assertEqual(len(out), len(long), "no message is dropped")
        self.assertEqual(out[-1], long[-1], "the last exchange is kept whole")
        self.assertEqual([m["content"] for m in out if m["role"] == "assistant"],
                         [m["content"] for m in long if m["role"] == "assistant"], "every command is still there")

    def test_old_outputs_are_cut_before_new_ones(self):
        out = compact_history(history(6, 3000))
        cut = [i for i, m in enumerate(out) if "[earlier output compacted" in m["content"]]
        self.assertTrue(cut)
        whole = [i for i, m in enumerate(out) if m["role"] == "user" and i not in cut]
        self.assertLess(max(cut), min(whole))

    def test_a_history_within_budget_is_the_exact_same(self):
        small = history(10, 50)
        self.assertIs(compact_history(small), small)


if __name__ == "__main__":
    unittest.main()
