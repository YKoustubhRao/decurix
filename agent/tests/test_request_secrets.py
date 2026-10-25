"""
Finding AC (2026-10-20, round 5 P2): the request gave a portal sign-in in its own words ("user clerk, password
Northwind#2026"), and the password reached the chat in a question. The secrets a request spells out are remembered as
hashes at task start, and every line that reaches the chat, a step, a task record or a recording withholds them.
"""

import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import messaging, progress
from decurix_agent.computer import privacy

LIVE = ("1. Enter every approved order in the Northwind Back Office portal (http://host.docker.internal:8701, user "
        "clerk, password Northwind#2026): one order per customer PO or chat, with only what we ship now.")
LEAK = ('I was about to type "clerk", "Northwind#2026", but the request names /home/headless/work/inbox/P2/'
        'PO_HH-PO-7781.eml, which I could not read, so I have not typed it.')


class RequestSecrets(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patch = mock.patch.dict(os.environ, {"DECURIX_DIR": self.tmp.name})
        patch.start()
        self.addCleanup(patch.stop)
        privacy._known_cache.clear()
        privacy.remember_secrets(LIVE)

    def test_the_password_after_its_name_is_found_and_kept_only_as_a_hash(self):
        self.assertEqual(privacy.request_secrets(LIVE), ["Northwind#2026"])
        kept = (Path(self.tmp.name) / "withheld-secrets").read_text(encoding="utf-8")
        self.assertNotIn("Northwind", kept)
        for text, found in (('the PIN: 4821 for the door', ["4821"]), ('api key=sk_live_abc123', ["sk_live_abc123"]),
                            ('the password "Ttlh"', ["Ttlh"]), ("Pass the file on to the team.", []),
                            ("a token of thanks", []),
                            # Found live (2026-10-22): "has" was kept as a secret and withheld from every later reply.
                            ("No OTP has come to the client's phone yet.", []), ("the PIN has changed", []),
                            ("OTP 529749", ["529749"]), ("the password is sunshine", ["sunshine"])):
            self.assertEqual(privacy.request_secrets(text), found, text)

    def test_the_live_question_never_shows_it(self):
        shown = privacy.withhold_known(LEAK)
        self.assertNotIn("Northwind#2026", shown)
        self.assertIn('"[withheld]"', shown)
        self.assertIn('"clerk"', shown)  # a user name is not a secret
        self.assertEqual(privacy.withhold_known("Running: gui type e5 'Northwind#2026'"), "Running: gui type e5 '[withheld]'")

    def test_typed_anywhere_it_is_a_credential_so_recordings_withhold_it(self):
        self.assertEqual(privacy.credential_in("Northwind#2026"), "password the request gave")
        self.assertIsNone(privacy.credential_in("clerk"))

    def test_the_chat_and_the_steps_get_it_withheld(self):
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

        with mock.patch("urllib.request.urlopen", urlopen), mock.patch.object(messaging.progress, "flush", lambda: None):
            messaging.send_query("q.txt", LEAK, kind="query", reply=LEAK)
        self.assertTrue(sent)
        self.assertNotIn("Northwind#2026", "".join(sent))

        queued = []
        with mock.patch.object(progress, "_ensure_sender", lambda: None), \
                mock.patch.object(progress._pending, "put_nowait", queued.append):
            progress.emit("Running: gui type e5 'Northwind#2026'")
        self.assertEqual(queued[0]["text"], "Running: gui type e5 '[withheld]'")

    def test_nothing_remembered_changes_nothing(self):
        (Path(self.tmp.name) / "withheld-secrets").unlink()
        privacy._known_cache.clear()
        self.assertEqual(privacy.withhold_known("clerk Northwind#2026"), "clerk Northwind#2026")


if __name__ == "__main__":
    unittest.main()
