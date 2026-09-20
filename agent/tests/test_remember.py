import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

from flowera_agent import config, messaging


class Recorder(BaseHTTPRequestHandler):
    seen = []
    status = 200

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        Recorder.seen.append((self.path, self.headers.get("Content-Type"), self.rfile.read(length).decode()))
        self.send_response(Recorder.status)
        self.end_headers()

    def log_message(self, *args):
        pass


class Remember(unittest.TestCase):
    def setUp(self):
        Recorder.seen, Recorder.status = [], 200
        self.server = HTTPServer(("127.0.0.1", 0), Recorder)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.shutdown)

        url = f"http://127.0.0.1:{self.server.server_port}"
        for patch in (mock.patch.object(config, "CONTROL_PLANE_URL", url), mock.patch.object(config, "DESKTOP_ID", "box-1")):
            patch.start()
            self.addCleanup(patch.stop)

    def test_it_posts_the_form_the_control_planes_memory_endpoint_reads(self):
        ok = messaging.remember("Skill: Add a customer", "Run it with `/skill x`. Ünïcode & symbols=ok", category="skill")

        self.assertTrue(ok)
        path, content_type, body = Recorder.seen[0]
        self.assertEqual((path, content_type), ("/api/desktops/memory", "application/x-www-form-urlencoded"))

        fields = urllib.parse.parse_qs(body)
        self.assertEqual(fields["desktop_id"], ["box-1"])
        self.assertEqual(fields["title"], ["Skill: Add a customer"])
        self.assertEqual(fields["summary"], ["Run it with `/skill x`. Ünïcode & symbols=ok"])
        self.assertEqual(fields["category"], ["skill"])

    def test_a_refusal_is_reported_not_raised(self):
        Recorder.status = 500
        self.assertFalse(messaging.remember("t", "s"))

    def test_an_unreachable_control_plane_is_reported_not_raised(self):
        self.server.shutdown()
        self.server.server_close()
        self.assertFalse(messaging.remember("t", "s"))


class BlankQuery(unittest.TestCase):
    """An engine that stops with no reason still has to put a question in front of a person."""

    def ask(self, problem):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(config, "QUERYTEXT_DIR", Path(tmp)), \
                mock.patch.object(messaging, "send_query", return_value=True) as sent:
            messaging.ask_for_query(problem)
            written = next(Path(tmp).glob("*.txt")).read_text()

        return sent.call_args.kwargs["reply"], written

    def test_an_empty_problem_becomes_a_question_a_person_can_act_on(self):
        for blank in ("", "   \n", None):
            reply, written = self.ask(blank)
            self.assertEqual(reply, messaging.BLANK_PROBLEM)
            self.assertIn(messaging.BLANK_PROBLEM, written)

    def test_a_real_problem_is_sent_as_written(self):
        reply, _ = self.ask("  Which account should I use?  ")
        self.assertEqual(reply, "Which account should I use?")


if __name__ == "__main__":
    unittest.main()
