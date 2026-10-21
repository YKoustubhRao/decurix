"""The final check reads the documents a task names, so a record full of
values the document never had cannot pass as done."""

import os
import tempfile
import unittest
from unittest import mock

from decurix_agent import verify


class CheckSeesTheDocument(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.po = os.path.join(self.dir.name, "GF-5522.xlsx")  # any readable document kind
        self.sent = []

        def chat(messages, max_tokens=None):
            self.sent.append(messages)
            return {"content": '{"verdict": "not_done", "confidence": 0.9, "seen": "", "why": "values differ"}'}, {}

        for p in (mock.patch.object(verify.llm_client, "available", lambda: True),
                  mock.patch.object(verify.llm_client, "chat", chat)):
            p.start()
            self.addCleanup(p.stop)

    def _text_sent(self):
        return "".join(part["text"] for part in self.sent[-1][1]["content"] if part.get("type") == "text")

    def test_the_document_the_task_names_is_shown_to_the_check(self):
        read = {"text": "Customer: Gemini Furniture\nPO: GF-5522\nCabinet with Doors 3", "images": []}
        with mock.patch("decurix_agent.tools.office.read_document", return_value=read) as reader:
            verify.judge(f"A customer sent us a purchase order: {self.po}. Enter it as a quotation.",
                         claim="Entered GF-5522 for Green Furnishings.")
        reader.assert_called_once_with(self.po)
        sent = self._text_sent()
        self.assertIn("Gemini Furniture", sent)
        self.assertIn("the values must come from these", sent)
        # The rule is in the check's own instructions, not left to inference.
        self.assertIn("the values entered or reported must be the document's", self.sent[-1][0]["content"])

    def test_an_unreadable_document_is_left_out_not_guessed(self):
        with mock.patch("decurix_agent.tools.office.read_document", side_effect=OSError("gone")):
            verdict = verify.judge(f"Enter {self.po}.", claim="done")
        self.assertEqual(verdict["verdict"], "not_done")
        self.assertNotIn("the values must come from these", self._text_sent())

    def test_a_task_naming_no_document_reads_nothing(self):
        with mock.patch("decurix_agent.tools.office.read_document") as reader:
            verify.judge("what is the kernel version?", claim="6.6")
        reader.assert_not_called()


if __name__ == "__main__":
    unittest.main()
