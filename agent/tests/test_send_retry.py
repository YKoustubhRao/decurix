"""A task's outcome is sent again while the control plane cannot be reached."""

import io
import unittest
import urllib.error
from unittest import mock

from decurix_agent import messaging


class AnOutcomeSurvivesARestart(unittest.TestCase):
    def test_a_refused_connection_is_tried_again_until_it_answers(self):
        # Found live (2026-10-15): a final reply met a control-plane restart and was never sent.
        calls = []

        def urlopen(request, timeout=None):
            # Progress lines from other tests' background sender go through urlopen too: only the outcome counts.
            if not getattr(request, "full_url", "").endswith("/api/querytext"):
                return mock.MagicMock(__enter__=lambda s: mock.MagicMock(status=200), __exit__=lambda *a: False)
            calls.append(1)
            if len(calls) < 3:
                raise urllib.error.URLError(ConnectionRefusedError(111, "Connection refused"))
            return mock.MagicMock(__enter__=lambda s: mock.MagicMock(status=200), __exit__=lambda *a: False)

        with mock.patch.object(messaging.urllib.request, "urlopen", urlopen), mock.patch.object(messaging.time, "sleep"):
            self.assertTrue(messaging.send_query("f.txt", "note", kind="final", reply="done"))
        self.assertEqual(len(calls), 3)

    def test_an_answer_is_not_retried(self):
        calls = []

        def urlopen(request, timeout=None):
            if not getattr(request, "full_url", "").endswith("/api/querytext"):
                return mock.MagicMock(__enter__=lambda s: mock.MagicMock(status=200), __exit__=lambda *a: False)
            calls.append(1)
            raise urllib.error.HTTPError(request.full_url, 400, "bad", {}, io.BytesIO(b""))

        with mock.patch.object(messaging.urllib.request, "urlopen", urlopen), mock.patch.object(messaging.time, "sleep"):
            self.assertFalse(messaging.send_query("f.txt", "note", kind="final", reply="done"))
        # A refusal is the control plane's word: not retried as it was - sent once more short and plain
        # (test_long_messages), and then let go.
        self.assertEqual(len(calls), 2)


if __name__ == "__main__":
    unittest.main()
