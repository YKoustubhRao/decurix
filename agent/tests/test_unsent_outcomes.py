"""
A task's outcome the control plane could not be told (it was restarting) is kept and sent again. Found live
(2026-10-20, round 5): the report of a request stopped a moment before a control-plane restart never reached the chat,
which said "running" for good.

    PYTHONPATH="agent;agent/tests" python -m unittest test_unsent_outcomes
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401  (stubs decurix_agent.config on a machine with no desktop)

from decurix_agent import config, messaging


class TheControlPlanesAnswer(unittest.TestCase):
    """How _post_querytext reads the control plane's answer (W1, 2026-11-03: a 409 is its no for an outcome naming a
    task that is not open on this desktop - it ended, or is not its)."""

    def answered(self, code):
        import urllib.error

        def urlopen(request, timeout=None):
            raise urllib.error.HTTPError(request.full_url, code, "no", {}, None)
        with mock.patch("urllib.request.urlopen", urlopen):
            return messaging._post_querytext({"kind": "final", "reply": "x"})[0]

    def test_a_409_is_refused_and_so_let_go_and_a_5xx_is_tried_again(self):
        self.assertEqual(self.answered(409), "refused")
        self.assertEqual(self.answered(400), "refused")
        self.assertEqual(self.answered(503), "unreachable")

    def test_an_answer_about_who_or_when_is_tried_again_not_let_go(self):
        """A 403 from a guard (found live 2026-11-08: a freed address range still refused) left the task running for
        good once the outcome was let go."""

        for code in (401, 403, 404, 408, 429):
            self.assertEqual(self.answered(code), "unreachable", code)
        for code in (400, 409, 413, 422):
            self.assertEqual(self.answered(code), "refused", code)


class UnsentOutcomes(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        for patch in (mock.patch.object(config, "DECURIX_DIR", Path(self.tmp.name)),
                      mock.patch.object(messaging, "SEND_RETRY_SECONDS", 0),
                      mock.patch.object(messaging.progress, "flush", lambda *a, **k: None)):
            patch.start()
            self.addCleanup(patch.stop)
        self.posted = []

    def kept(self):
        return sorted((Path(self.tmp.name) / "unsent").glob("*.json"))

    def post(self, *answers):
        answers = list(answers)

        def fake(payload):
            self.posted.append(payload)
            return answers.pop(0) if answers else ("sent", "")
        return mock.patch.object(messaging, "_post_querytext", fake)

    def test_a_final_report_that_cannot_be_delivered_is_kept_and_sent_later(self):
        with self.post(("unreachable", "connection refused")):
            self.assertFalse(messaging.send_query("f.txt", "Stopped.", kind="final", reply="Stopped.", task_id="t0123456789abcdef"))
        kept = self.kept()
        self.assertEqual(len(kept), 1)
        self.assertEqual(json.loads(kept[0].read_text(encoding="utf-8"))["reply"], "Stopped.")
        # The control plane is back: it is sent, and let go.
        with self.post(("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual(self.kept(), [])
        self.assertEqual(self.posted[-1]["task_id"], "t0123456789abcdef")

    def test_a_kept_outcome_the_control_plane_refuses_is_dropped_not_retried(self):
        # Lead review 2026-11-03 (L-03): once the control plane refuses an outcome for a task that is closed (or not
        # this execution's), sending it again forever would only be refused forever. It is let go, and the next one
        # behind it is still sent.
        with self.post(("unreachable", "x"), ("unreachable", "x")):
            messaging.send_query("a.txt", "stale", kind="final", reply="stale", task_id="t0123456789abcdef")
            messaging.send_query("b.txt", "fresh", kind="final", reply="fresh", task_id="tfedcba9876543210")
        self.assertEqual(len(self.kept()), 2)
        with self.post(("refused", "HTTP 409 the task is closed"), ("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        # (Review round 2, B7: a refused outcome is kept once more in its least form, then let go if refused again.)
        self.assertEqual([p.name.endswith(".min.json") for p in self.kept()], [True])
        with self.post(("refused", "HTTP 409 the task is closed")):
            self.assertEqual(messaging.resend_unsent(), 0)
        self.assertEqual(self.kept(), [])
        with self.post():
            self.assertEqual(messaging.resend_unsent(), 0)
        # (The second outcome is never posted ahead of the kept first: its send tries the first again, still down, and
        # it is kept behind it - test_kept_notes.)
        self.assertEqual([p["reply"] for p in self.posted], ["stale", "stale", "stale", "fresh", "stale"])

    def test_still_unreachable_keeps_them_all_in_order(self):
        with self.post(("unreachable", "x"), ("unreachable", "x")):
            messaging.send_query("a.txt", "first", kind="final", reply="first")
            messaging.send_query("b.txt", "second", kind="error", reply="second")
        self.assertEqual(len(self.kept()), 2)
        with self.post(("unreachable", "still down")):
            self.assertEqual(messaging.resend_unsent(), 0)
        self.assertEqual(len(self.kept()), 2)
        with self.post(("sent", ""), ("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 2)
        self.assertEqual([p["reply"] for p in self.posted[-2:]], ["first", "second"])

    def test_what_the_control_plane_refuses_is_not_kept_and_a_question_it_refuses_is_not_kept(self):
        # (A question it could not be told IS kept since 2026-11-11, as an outcome is: test_kept_notes.)
        with self.post(("refused", "HTTP 400")):
            messaging.send_query("f.txt", "x", kind="final", reply="x")
        with self.post(("refused", "HTTP 409"), ("refused", "HTTP 409")):
            messaging.send_query("q.txt", "Which one?", kind="query", reply="Which one?")
        self.assertEqual(self.kept(), [])

    def test_a_server_error_while_it_restarts_is_tried_again(self):
        with mock.patch.object(messaging, "SEND_RETRY_SECONDS", 60), mock.patch.object(messaging.time, "sleep"), \
                self.post(("unreachable", "HTTP 502"), ("sent", "")):
            self.assertTrue(messaging.send_query("f.txt", "Done.", kind="final", reply="Done."))
        self.assertEqual(len(self.posted), 2)
        self.assertEqual(self.kept(), [])

    def test_a_5xx_answer_counts_as_not_there_yet_and_a_4xx_as_its_word(self):
        import urllib.error
        for code, expect in ((503, "unreachable"), (400, "refused")):
            err = urllib.error.HTTPError("http://x", code, "x", {}, None)
            with mock.patch.object(messaging.urllib.request, "urlopen", side_effect=err):
                self.assertEqual(messaging._post_querytext({"a": 1})[0], expect)


if __name__ == "__main__":
    unittest.main()
