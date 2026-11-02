"""
Review 2026-11-02: secrets withholding failed open - an error in it, or a secrets file it could not read, let the text
through as it was - and it withheld only secrets named in the request. Every withholding path now fails closed (the
text is replaced by a note that something was held back), and secrets in their own shapes are withheld too, without
touching the figures a back-office worker's text is made of.

    PYTHONPATH=agent py -3 -m unittest discover -s agent/tests -p "test_privacy*.py"
"""

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import config, messaging, progress
from decurix_agent.computer import privacy

ORDINARY = ("GSTIN 29ABCDE1234F1Z5, PAN ABCDE1234F, invoice INV-2026-0087 dated 2026-10-21 for Rs 1,25,000.00 "
            "(taxable 1,05,932.20, CGST 9,533.90); account 002501234567, IFSC ICIC0000025; PIN code 576104; "
            "ack AB1234567890123C; 18% GST; 12,345 tokens used; the password is incorrect; no OTP has come yet")


class _Home(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patch = mock.patch.dict(os.environ, {"DECURIX_DIR": self.tmp.name})
        patch.start()
        self.addCleanup(patch.stop)
        privacy._known_cache.clear()
        privacy._unsaved.clear()
        self.addCleanup(privacy._unsaved.clear)
        self.addCleanup(privacy._known_cache.clear)


class FailsClosed(_Home):
    def test_an_error_withholds_the_whole_text(self):
        with mock.patch.object(privacy, "_withhold_known", side_effect=RuntimeError("boom")):
            self.assertEqual(privacy.withhold_known("password Lift$2026"), privacy.HELD_BACK)
        with mock.patch.object(privacy, "_withhold_shapes", side_effect=RuntimeError("boom")):
            self.assertEqual(privacy.withhold_spoken_secrets("password: Lift$2026"), privacy.HELD_BACK)
        with mock.patch.object(privacy, "_credential_in", side_effect=RuntimeError("boom")):
            self.assertTrue(privacy.credential_in("anything"))

    def test_a_secrets_file_that_cannot_be_read_lets_nothing_through(self):
        privacy.remember_secrets("user clerk, password Northwind#2026")
        privacy._known_cache.clear()
        with mock.patch.object(Path, "read_text", side_effect=PermissionError("denied")):
            self.assertEqual(privacy.withhold_known("typed Northwind#2026"), privacy.HELD_BACK)
            self.assertTrue(privacy.known_secret_in("typed Northwind#2026"))

    def test_a_secret_that_could_not_be_written_down_is_still_withheld(self):
        with mock.patch.object(Path, "write_text", side_effect=OSError("disk full")):
            privacy.remember_secrets("the portal password is Kavya@2026")
        self.assertNotIn("Kavya@2026", privacy.withhold_known("I signed in with Kavya@2026"))

    def test_the_chat_and_the_steps_get_the_note_never_the_text(self):
        sent = []

        class Answer:
            status = 200

            def read(self):
                return b"{}"

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        def urlopen(request, timeout=None):
            sent.append(request.data.decode("utf-8"))
            return Answer()

        with mock.patch.object(privacy, "withhold_known", side_effect=RuntimeError("boom")), \
                mock.patch("urllib.request.urlopen", urlopen), mock.patch.object(messaging.progress, "flush", lambda: None):
            messaging.send_query("q.txt", "the code is Zx9!", kind="query", reply="the code is Zx9!",
                                 fallback="the code is Zx9!")
            progress._post({"text": "typed Zx9!", "action": {"text": "Zx9!"}, "task_id": "t1"})
            messaging.remember("Portal", "the code is Zx9!")
        self.assertEqual(len(sent), 3)
        self.assertNotIn("Zx9!", "".join(sent))
        self.assertIn("held back", json.loads(sent[0])["reply"])

    def test_a_steps_action_is_withheld_too(self):
        privacy.remember_secrets("password Northwind#2026")
        queued = []
        with mock.patch.object(progress, "_ensure_sender", lambda: None), \
                mock.patch.object(progress._pending, "put_nowait", queued.append):
            progress.emit_action("typed Northwind#2026", {"kind": "type", "text": "Northwind#2026"}, before_png=b"x",
                                 task_id="t1")
        self.assertNotIn("Northwind#2026", json.dumps(queued[0]))


class SecretsByTheirShape(_Home):
    def test_ordinary_figures_are_never_touched(self):
        self.assertEqual(privacy.withhold_known(ORDINARY), ORDINARY)
        self.assertIsNone(privacy.credential_in(ORDINARY))

    def test_secret_shapes_are_withheld(self):
        for text, secret in (("password: Lift$2026", "Lift$2026"), ("DB_PASSWORD=hunter22", "hunter22"),
                             ("client_secret: 9fK2mQ8xL4vB7nR1", "9fK2mQ8xL4vB7nR1"),
                             ("the password is Lift$2026.", "Lift$2026"),
                             ("Authorization: Bearer abcdefGHIJ1234567890xyz", "abcdefGHIJ1234567890xyz"),
                             ("https://admin:S3cret!@portal.example.com/login", "S3cret!"),
                             ("https://x.example/cb?access_token=ya29a0AfH6SMBx1234&state=29", "ya29a0AfH6SMBx1234"),
                             ("OTP 529749 for the login", "529749"), ("verification code: 4821", "4821"),
                             ("CVV 123 on the back", "123"),
                             ("api key 9fK2mQ8xL4vB7nR1tZ6wY3pA for the portal", "9fK2mQ8xL4vB7nR1tZ6wY3pA"),
                             ("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----",
                              "b3BlbnNzaC1rZXk")):
            shown = privacy.withhold_known(text)
            self.assertNotIn(secret, shown, text)
            self.assertIn("[withheld", shown, text)
            self.assertTrue(privacy.credential_in(text), text)

    def test_a_portal_address_keeps_everything_but_the_secret(self):
        self.assertEqual(privacy.withhold_known("https://services.gst.gov.in/search?gstin=29ABCDE1234F1Z5&state_code=29"),
                         "https://services.gst.gov.in/search?gstin=29ABCDE1234F1Z5&state_code=29")


class WhatFileMayLeave(_Home):
    def setUp(self):
        super().setUp()
        self.home = Path(self.tmp.name)
        self.work = self.home / "work"
        (self.work / "inbox" / "t1").mkdir(parents=True)
        (self.work / "inbox" / "t1" / "bill.pdf").write_bytes(b"%PDF")
        for p in (mock.patch.object(config, "WORK_DIR", self.work), mock.patch.object(config, "DECURIX_DIR", self.home / ".decurix"),
                  mock.patch.dict(os.environ, {"DECURIX_SHARED_FOLDER": ""})):
            p.start()
            self.addCleanup(p.stop)

    def test_only_a_file_of_the_work(self):
        self.assertEqual(privacy.file_may_leave(self.work / "inbox" / "t1" / "bill.pdf")[0],
                         Path(os.path.abspath(str(self.work / "inbox" / "t1" / "bill.pdf"))))
        self.assertEqual(privacy.file_may_leave("inbox/t1/bill.pdf")[0].name, "bill.pdf", "relative to the work folder")
        for path in (self.home / ".claude" / ".credentials.json", self.work / ".git" / "config",
                     self.home / ".decurix" / "withheld-secrets", self.home / "elsewhere.txt",
                     self.work / ".." / ".claude" / ".credentials.json", ""):
            allowed, why = privacy.file_may_leave(path)
            self.assertIsNone(allowed, path)
            self.assertTrue(why, path)

    def test_a_path_that_cannot_be_checked_does_not_leave(self):
        with mock.patch.object(privacy, "_send_roots", side_effect=RuntimeError("boom")):
            self.assertIsNone(privacy.file_may_leave(self.work / "inbox" / "t1" / "bill.pdf")[0])


if __name__ == "__main__":
    unittest.main()
