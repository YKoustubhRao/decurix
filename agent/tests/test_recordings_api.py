"""
The agent's recordings API for what this change added: importing a recording
made elsewhere, and the readiness (lint) that comes with every recording.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import http.client
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401
from test_ingest import LOGIN, sel

from decurix_agent import http_api
from decurix_agent.computer import recording


class RecordingsAPI(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT
        recording.ROOT = Path(self.tmp.name)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        recording.ROOT = self.saved
        self.tmp.cleanup()

    def call(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base + path, data=data, method="POST" if body is not None else "GET")
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as err:
            with err:
                return err.code, json.loads(err.read() or b"{}")

    def test_an_export_is_imported_and_says_what_it_left_out(self):
        status, body = self.call("/recordings/import", {"title": "Month-end login", "data": LOGIN})

        self.assertEqual(status, 200, body)
        self.assertEqual(body["format"], "chrome-recorder")
        self.assertGreater(body["imported"], 5)
        self.assertIn("hover", [s["type"] for s in body["skipped"]])

        status, listed = self.call("/recordings")
        self.assertEqual([r["id"] for r in listed["recordings"]], [body["session"]])
        self.assertEqual(listed["recordings"][0]["title"], "Month-end login")

    def test_the_file_may_arrive_as_text_the_way_the_control_plane_sends_it(self):
        status, body = self.call("/recordings/import", {"data": json.dumps(LOGIN)})
        self.assertEqual(status, 200, body)

    def test_negative_content_length_is_rejected_without_reading(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.server.server_address[1], timeout=2)
        try:
            conn.request("POST", "/recordings/import", headers={"Content-Length": "-1"})
            response = conn.getresponse()
            self.assertEqual(response.status, 400)
            self.assertIn("invalid content length", response.read().decode())
        finally:
            conn.close()

    def test_a_file_that_is_not_a_recording_is_a_400_with_words_for_a_person(self):
        for data in ("not json", [1, 2], {"steps": []}, {"nothing": "here"}):
            status, body = self.call("/recordings/import", {"data": data})
            self.assertEqual(status, 400, data)
            self.assertTrue(body["error"], data)

        self.assertEqual(self.call("/recordings")[1]["recordings"], [])

    def test_a_recording_comes_with_how_far_to_trust_it(self):
        session = self.call("/recordings/import", {"data": LOGIN})[1]["session"]
        status, body = self.call(f"/recordings/{session}")

        self.assertEqual(status, 200)
        self.assertEqual(body["lint"]["grade"], "risky")
        self.assertIn("no_target", [f["code"] for f in body["lint"]["findings"]])
        self.assertTrue(body["lint"]["summary"])

    def test_the_lint_of_a_clean_recording_is_ready(self):
        session = self.call("/recordings/import", {"data": {"steps": [
            {"type": "click", "selectors": sel('aria/Sign in[role="button"]'),
             "assertedEvents": [{"type": "navigation", "url": "https://app.test/home", "title": "Home"}]},
        ]}})[1]["session"]

        self.assertEqual(self.call(f"/recordings/{session}")[1]["lint"]["grade"], "ready")


if __name__ == "__main__":
    unittest.main()
