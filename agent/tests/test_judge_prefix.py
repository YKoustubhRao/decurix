"""
Resource plan P1 #8 (2026-11-05): a stage's re-check put its note inside the stage's task text, so its prompt changed
from the middle on and no provider prompt cache could serve it. The note goes last; the first check's prompt is then
the re-check's prefix. This moves a line in the judge's prompt, so it ships only after an eval rerun.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401


class AReJudgeKeepsTheFirstJudgesPrefix(unittest.TestCase):
    """P1 #8: the re-check's note was put inside the stage's task text, so the prompt changed from its middle on and no
    provider cache could serve it. It goes last now (ships after an eval rerun: the line moves in the prompt)."""

    def prompt(self, note):
        from decurix_agent import long_task, verify
        sent = []

        def chat(messages, **k):
            sent.append(messages)
            return {"content": '{"verdict": "done", "confidence": 0.95, "seen": "x", "why": ""}'}, {}
        step = {"id": "s1", "title": "Build the register", "instruction": "Build register.xlsx from the bills.",
                "outputs": ["register.xlsx"], "inputs": []}
        state = {"run_id": "r1"}
        with mock.patch.object(verify, "available", lambda: True), \
                mock.patch.object(verify.llm_client, "chat", chat), \
                mock.patch.object(long_task, "ledger", lambda run_id: {"request": "Build the register"}), \
                mock.patch.object(long_task, "_ledger_text", lambda *a, **k: ""), \
                mock.patch.object(long_task, "sheets_added_later", lambda *a, **k: {}), \
                mock.patch("decurix_agent.skill_task._did_screen_work", lambda: False):
            long_task._judge(state, step, "Built it.", [], [], note=note)
        return json_text(sent[0])

    def test_the_first_judges_prompt_is_the_re_judges_prefix(self):
        first, again = self.prompt(""), self.prompt("BINDING: the previous check refused the total; check it again.")
        task_end = first.index("What the worker reports:")
        self.assertGreater(task_end, 200)
        self.assertEqual(again[:task_end - 1], first[:task_end - 1])
        self.assertIn("BINDING: the previous check refused the total", again)


def json_text(messages):
    """The system message and the user's text parts, in order, as one string."""

    out = []
    for m in messages:
        content = m["content"]
        if isinstance(content, str):
            out.append(content)
        else:
            out.extend(part.get("text", "") for part in content if part.get("type") == "text")
    return "\n".join(out)


if __name__ == "__main__":
    unittest.main()
