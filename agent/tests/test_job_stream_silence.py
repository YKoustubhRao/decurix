"""
Following a soul job over its SSE stream notices a dead connection. soul sends an SSE comment (": ping") every 15 s
while a job runs; the stream's read timeout is STREAM_READ_TIMEOUT_SECONDS (90 s), not the whole task budget, so a
half-open connection no longer waits out two hours. 90 s of silence - a dead connection, or an older soul with no
heartbeat in a long step - switches to polling, which follows the job to its end with the full budget, and the outcome
is reported once.

    PYTHONPATH="agent;agent/tests" python -m unittest test_job_stream_silence
"""

import json
import socket
import unittest
from unittest import mock

import config_stub  # noqa: F401  (stubs decurix_agent.config on a machine with no desktop)

from decurix_agent import thebot_harness as th

DONE = {"id": "j1", "status": "done", "outcome": "answered", "finalAnswer": "42"}


def sse(*items):
    """Lines of an SSE stream: dicts are data events, strings are sent as they are (": ping", blank lines)."""

    out = []
    for item in items:
        if isinstance(item, dict):
            out += [("data: " + json.dumps(item) + "\n").encode(), b"\n"]
        else:
            out.append((item + "\n").encode())
    return out


class Stream:
    """A response that yields its lines, then raises `then` (a read timeout) or ends."""

    def __init__(self, lines, then=None):
        self.lines, self.then = lines, then

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def __iter__(self):
        yield from self.lines
        if self.then is not None:
            raise self.then


class FollowAJob(unittest.TestCase):

    def setUp(self):
        self.steps, self.timeouts, self.polls, self.cancelled = [], [], [], []
        for patch in (mock.patch.object(th, "_emit_step", self.steps.append),
                      mock.patch.object(th, "_cancel_job", lambda *a: self.cancelled.append(a)),
                      mock.patch.object(th.time, "sleep", lambda s: None),
                      mock.patch.object(th, "log", lambda *a, **k: None)):
            patch.start()
            self.addCleanup(patch.stop)

    def stream(self, response):
        def urlopen(request, timeout=None):
            self.timeouts.append(timeout)
            if isinstance(response, BaseException):
                raise response
            return response
        return mock.patch.object(th.urllib.request, "urlopen", urlopen)

    def poll(self, *jobs):
        jobs = list(jobs)

        def get(url):
            self.polls.append(url)
            return jobs.pop(0) if len(jobs) > 1 else jobs[0]
        return mock.patch.object(th, "_get_json", get)

    def test_the_stream_reads_with_a_short_timeout_and_skips_the_heartbeat(self):
        lines = sse({"type": "log", "line": "step one"}, ": ping", ": ping", {"type": "log", "line": "step two"},
                    {"type": "final", "job": DONE})
        with self.stream(Stream(lines)), self.poll({"status": "running"}):
            job, why = th._follow_job("http://soul", "j1")
        self.assertEqual((job, why), (DONE, None))
        self.assertEqual(self.timeouts, [th.STREAM_READ_TIMEOUT_SECONDS])
        self.assertEqual(th.STREAM_READ_TIMEOUT_SECONDS, 90)
        self.assertLess(th.STREAM_READ_TIMEOUT_SECONDS, th.TASK_TIMEOUT_SECONDS)
        self.assertEqual(self.steps, ["step one", "step two"])
        self.assertEqual(self.polls, [])  # the stream's final event is the outcome: not asked for again

    def test_a_silent_stream_switches_to_polling_and_reports_the_outcome_once(self):
        # A soul without the heartbeat, in a long step: the stream times out; polling follows the job to its end.
        lines = sse({"type": "log", "line": "step one"})
        with self.stream(Stream(lines, then=socket.timeout("timed out"))), \
                self.poll({"status": "running"}, {"status": "running"}, DONE):
            job, why = th._follow_job("http://soul", "j1")
        self.assertEqual((job, why), (DONE, None))
        self.assertEqual(self.steps, ["step one"])  # every line the stream gave was shown, once
        self.assertEqual(len(self.polls), 3)
        self.assertTrue(all(url.endswith("/api/jobs/j1") for url in self.polls))
        self.assertEqual(self.cancelled, [])

    def test_a_timeout_wrapped_by_urllib_also_switches_to_polling(self):
        import urllib.error
        with self.stream(urllib.error.URLError(socket.timeout("timed out"))), self.poll(DONE):
            self.assertEqual(th._follow_job("http://soul", "j1"), (DONE, None))

    def test_polling_after_a_silent_stream_has_the_full_budget(self):
        # A long step is still followed for as long as before: the poll's own deadline is the whole task budget.
        clock = {"now": 0.0}
        seen = []

        def get(url):
            seen.append(clock["now"])
            clock["now"] += 600  # ten minutes per poll
            return DONE if clock["now"] >= th.TASK_TIMEOUT_SECONDS - 600 else {"status": "running"}

        with self.stream(Stream([], then=socket.timeout("timed out"))), \
                mock.patch.object(th, "_get_json", get), \
                mock.patch.object(th.time, "monotonic", lambda: clock["now"]):
            job, why = th._follow_job("http://soul", "j1")
        self.assertEqual((job, why), (DONE, None))
        self.assertGreater(max(seen), th.TASK_TIMEOUT_SECONDS - 1300)

    def test_a_job_that_outlives_the_budget_is_cancelled_as_before(self):
        clock = {"now": 0.0}

        def get(url):
            clock["now"] += 600
            return {"status": "running"}

        with self.stream(Stream([], then=socket.timeout("timed out"))), \
                mock.patch.object(th, "_get_json", get), \
                mock.patch.object(th.time, "monotonic", lambda: clock["now"]):
            job, why = th._follow_job("http://soul", "j1")
        self.assertIsNone(job)
        self.assertIn("did not finish", why)
        self.assertEqual(len(self.cancelled), 1)

    def test_the_engine_gone_while_polling_is_said_as_before(self):
        import urllib.error

        def get(url):
            raise urllib.error.URLError(ConnectionRefusedError(111, "refused"))

        with self.stream(Stream([], then=socket.timeout("timed out"))), mock.patch.object(th, "_get_json", get):
            self.assertEqual(th._follow_job("http://soul", "j1"), (None, th.ENGINE_STOPPED))


if __name__ == "__main__":
    unittest.main()
