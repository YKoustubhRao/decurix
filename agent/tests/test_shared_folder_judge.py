"""
S1 (2026-10-19, word_letters on a shared-folder worker): a letter saved in /home/headless/PC was judged "not in the
Decurix demo folder" - /home/headless/PC IS that folder. Where a file is is decided in code from the mapping, and the
judge is told the mapping in plain words.
"""

import os
import sys
import time
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from test_shared_folder import HOST, SharedBase

from decurix_agent import produced, shared, verify

REQUEST = ('From receivables.xlsx in the "Decurix demo" folder in my Documents (C:\\Users\\Administrator\\Documents\\'
           'Decurix demo), write payment reminder letters in Word (the Word app on your desktop): one letter per '
           'customer. Save them as one document reminders.docx in the same folder.')


class TheSharedFolderIsWhereTheRequestSaid(SharedBase):
    def test_the_location_fact_passes_in_code_for_the_live_report_and_file(self):
        self.put("reminders.docx", b"PK\x03\x04data")
        with mock.patch.object(produced, "since", lambda: time.time() - 60), \
                mock.patch("urllib.request.urlopen", side_effect=AssertionError("the host was asked")):
            self.assertEqual(produced.missing_on_host(REQUEST), "")
            self.assertEqual(produced.misplaced(REQUEST, root=self.root), "")
            self.assertEqual(produced.saved_on_host(REQUEST), [HOST + "\\reminders.docx"])

    def test_the_judge_is_told_in_plain_words_that_the_folder_is_the_persons(self):
        self.assertEqual(shared.to_host(self.mount / "reminders.docx"), HOST + "\\reminders.docx")
        self.assertEqual(shared.to_host(self.mount), HOST)
        self.assertIsNone(shared.to_host("/home/headless/work/reminders.docx"))
        self.assertIn(self.mount.as_posix() + " IS the person's own folder " + HOST, shared.mapping_note())
        sent = []

        def chat(messages, max_tokens=None):
            sent.append(messages)
            return {"content": '{"verdict": "done", "confidence": 0.95, "seen": "", "why": ""}'}, {}

        self.put("reminders.docx", b"PK\x03\x04data")
        with mock.patch.object(verify.llm_client, "available", lambda: True), \
                mock.patch.object(verify.llm_client, "chat", chat):
            verify.judge(REQUEST, claim="Saved /home/headless/PC/reminders.docx.",
                         produced=produced.text([str(self.mount / "reminders.docx")]), screen=False)
        text = "".join(part["text"] for part in sent[-1][1]["content"] if part.get("type") == "text")
        self.assertIn("IS the person's own folder " + HOST, text)
        self.assertIn("IS saved where asked", text)
        self.assertIn("(= " + HOST + "\\reminders.docx on the person's computer: their shared folder)", text)

    def test_a_desktop_without_a_shared_folder_says_nothing_about_one(self):
        with mock.patch.dict(os.environ, {shared.ENV_FOLDER: ""}):
            self.assertEqual(shared.mapping_note(), "")
            self.assertIsNone(shared.to_host(self.mount / "x.docx"))


class TheJudgeKnowsAnotherAppIsNotTheOneNamed(unittest.TestCase):
    def test_its_rules_say_so_generically(self):
        rule = verify.JUDGE_SYSTEM.split("When the request names the app to do the work in")[1][:400]
        self.assertIn("another app in its place", rule)
        for name in ("Word", "Excel", "Writer", "Calc", "Office"):
            self.assertNotIn(name, rule)

    def test_made_with_tools_then_opened_and_saved_in_the_named_app_is_done_in_it(self):
        rule = verify.JUDGE_SYSTEM.split("When the request names the app to do the work in")[1][:900]
        self.assertIn("made with tools (office, python) and that was then opened in the named app's window, "
                      "checked there and saved from it IS done in that app", rule)
        self.assertIn("never opened and saved in that app, it is not", rule)
        for name in ("Word", "Excel", "PowerPoint", "Writer", "Calc", "Impress"):
            self.assertNotIn(name, rule[:900])


if __name__ == "__main__":
    unittest.main()
