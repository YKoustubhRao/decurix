"""A final report says whether the final check judged it, as data."""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import messaging, verdict


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

    # Found live (2026-10-24): a local import added to the report's path made task_context a local name, and every
    # ending that skipped it - a /command, a quiet one, an interrupted one - failed before the task was closed.
    def test_every_kind_of_ending_reports(self):
        from decurix_agent import state
        with mock.patch.object(state, "current_task", '/learn demo-1 name="x"'):
            messaging.write_final_reply('Learned "x".')
        messaging.write_final_reply("Recording kept.", quiet=True)
        messaging.write_final_reply("Stopped.", interrupted=True)
        self.assertTrue(self.sent, "nothing was reported")

    def test_a_checked_report_says_so(self):
        # Checked means a verdict of done with a check that passed behind it; a bare checked=True is a claim and goes
        # out not checked (fail closed, verdict.derive; 2026-11-07).
        done = verdict.make("done", "skill_gate", checks=[verdict.check("files.read_back", "pass", "code")])
        messaging.write_final_reply("Filed INV-7.", checked=True, verdict=done)
        self.assertIs(self.sent[-1]["checked"], True)
        messaging.write_final_reply("Filed INV-7.", checked=True)
        self.assertIs(self.sent[-1]["checked"], False)

    def test_an_unchecked_report_says_so(self):
        messaging.write_final_reply("6.6")
        self.assertIs(self.sent[-1]["checked"], False)

    def test_a_report_the_gate_settled_without_judging_it_done_skips_the_gate_and_says_not_checked(self):
        # verify.outcome's "unchecked": the gate wrote the report itself, so it is not gated twice - and not checked.
        gate = mock.Mock(return_value=True)
        with mock.patch.object(messaging, "_final_gate", gate):
            messaging.write_final_reply("Not checked - no model here. Made result.xlsx.", gated=True)
        gate.assert_not_called()
        self.assertIs(self.sent[-1]["checked"], False)

    # A question is not a result; "checked" means nothing there.
    def test_a_question_carries_no_checked(self):
        messaging.send_query("q.txt", "DECURIX QUERY REQUEST", kind="query", reply="Which one?")
        self.assertNotIn("checked", self.sent[-1])


if __name__ == "__main__":
    unittest.main()
