"""A final report says whether the final check judged it, as data."""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import messaging


class _Response:
    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False


class CheckedReports(unittest.TestCase):
    def setUp(self):
        self.sent = []
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)

        def urlopen(request, timeout=None):
            self.sent.append(json.loads(request.data))
            return _Response()

        for p in (
            mock.patch.object(messaging.urllib.request, "urlopen", urlopen),
            mock.patch.object(messaging.config, "QUERYTEXT_DIR", Path(tmp.name)),
            mock.patch.object(messaging, "_final_gate", None),
            mock.patch.object(messaging.progress, "flush", lambda *a, **k: None),
        ):
            p.start()
            self.addCleanup(p.stop)

    def test_a_checked_report_says_so(self):
        messaging.write_final_reply("Filed INV-7.", checked=True)
        self.assertIs(self.sent[-1]["checked"], True)

    def test_an_unchecked_report_says_so(self):
        messaging.write_final_reply("6.6")
        self.assertIs(self.sent[-1]["checked"], False)

    # A question is not a result; "checked" means nothing there.
    def test_a_question_carries_no_checked(self):
        messaging.send_query("q.txt", "DECURIX QUERY REQUEST", kind="query", reply="Which one?")
        self.assertNotIn("checked", self.sent[-1])


if __name__ == "__main__":
    unittest.main()
