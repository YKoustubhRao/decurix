"""The judge sees the whole task it is given, and when it is unsure only because a part of a file was not shown, it
names what to find and judges once more with those lines - never more than once."""

import os
import tempfile
import unittest
from unittest import mock

from decurix_agent import produced, verify

UNSURE_LOOK = ('{"verdict": "unsure", "confidence": 0.3, "seen": "the CL-4142 row is not shown", '
               '"why": "cannot see it", "look": ["CL-4142"]}')
DONE = '{"verdict": "done", "confidence": 0.95, "seen": "CL-4142 allowed 1200", "why": "matches the answer"}'


class JudgeLooksCloser(unittest.TestCase):
    def setUp(self):
        self.sent, self.replies = [], []

        def chat(messages, max_tokens=None, purpose=None):
            self.sent.append(messages)
            return {"content": self.replies.pop(0)}, {}

        for p in (mock.patch.object(verify.llm_client, "available", lambda: True),
                  mock.patch.object(verify.llm_client, "chat", chat)):
            p.start()
            self.addCleanup(p.stop)

    def text(self, n):
        return "".join(part["text"] for part in self.sent[n][1]["content"] if part.get("type") == "text")

    def test_unsure_for_a_row_not_shown_looks_it_up_and_judges_again(self):
        self.replies = [UNSURE_LOOK, DONE]
        asked = []

        def look_up(terms):
            asked.append(list(terms))
            return "--- audit.xlsx\n(line 43) CL-4142 | Neha Joshi | 1850 | 1200 | 650 | approved by requester"

        found = verify.judge("Check the audit", produced="(a sample of the audit)", screen=False, look_up=look_up)
        self.assertEqual(asked, [["CL-4142"]])
        self.assertEqual(found["verdict"], verify.DONE)
        self.assertNotIn("look", found)
        self.assertIn("(line 43) CL-4142", self.text(1))
        self.assertIn("Do not ask to look again", self.text(1))

    def test_it_looks_once_only(self):
        self.replies = [UNSURE_LOOK, UNSURE_LOOK]
        found = verify.judge("Check the audit", screen=False, look_up=lambda terms: "(line 1) CL-4142")
        self.assertEqual(len(self.sent), 2)
        self.assertEqual(found["verdict"], verify.UNSURE)

    def test_nothing_found_or_no_lookup_keeps_the_first_verdict(self):
        self.replies = [UNSURE_LOOK]
        self.assertEqual(verify.judge("Check", screen=False, look_up=lambda terms: "")["verdict"], verify.UNSURE)
        self.replies = [UNSURE_LOOK]
        self.assertEqual(verify.judge("Check", screen=False)["verdict"], verify.UNSURE)
        self.assertEqual(len(self.sent), 2)

    def test_a_long_task_reaches_the_judge_whole(self):
        # A stage's instruction and inputs, then the person's binding answer far past the old 1,500-character cut.
        self.replies = [DONE]
        task = "THE STAGE TO CHECK: audit.\n" + ("- /home/headless/work/H1/receipts/CL-41xx_receipt.pdf\n" * 60)
        task += "The person's own answers - BINDING: approve Neha's taxi at Rs 1,200."
        verify.judge(task, screen=False)
        self.assertIn("approve Neha's taxi at Rs 1,200", self.text(0))


class LinesWith(unittest.TestCase):
    def test_the_lines_holding_the_terms_with_the_files_first_lines(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "audit.csv")
            rows = ["Line,Employee,Claimed,Allowed"] + [f"CL-41{i:02d},Person {i},100,100" for i in range(60)]
            rows.append("CL-4142,Neha Joshi,\"1,850\",\"1,200\"")
            with open(path, "w", encoding="utf-8") as f:
                f.write("\n".join(rows))
            with mock.patch("decurix_agent.tools.office.read_document",
                            return_value={"text": "\n".join(rows)}):
                out = produced.lines_with([path], ["neha  joshi"])
        self.assertIn("Line,Employee,Claimed,Allowed", out)
        self.assertIn("CL-4142,Neha Joshi", out)
        self.assertNotIn("Person 7", out)
        self.assertEqual(produced.lines_with([path], []), "")


if __name__ == "__main__":
    unittest.main()
