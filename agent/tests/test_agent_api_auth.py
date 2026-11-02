"""
Who may call a desktop's :8081 API, and what counts as the reply to an open question (review 2026-11-02).

:8081 carries a person's replies, approvals included. It was open to every other desktop on the Docker network
and to any web page in the desktop's Chrome (a text/plain POST needs no CORS), and a request with no task ID while
a question was open was taken as its answer - so anything that reached the port could answer an approval "yes".

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p "test_agent_api_auth.py"
"""

import json
import os
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import test_watcher_restart
from test_watcher_restart import TASK, FakeThread

from decurix_agent import config, http_api, messaging, state, watchers

TOKEN = "ab" * 32


def refusal(method="GET", parts=("recordings",), token=TOKEN, **headers):
    return http_api.refusal(method, list(parts), {k.replace("_", "-"): v for k, v in headers.items()}, token=token)


class RefusalTest(unittest.TestCase):

    def test_the_token_is_required_on_every_route_but_health(self):
        self.assertEqual(refusal()[0], 401)
        self.assertEqual(refusal(Authorization="Bearer " + "cd" * 32)[0], 401)
        self.assertEqual(refusal(Authorization=TOKEN)[0], 401, "a bare token is not a bearer token")
        self.assertIsNone(refusal(Authorization="Bearer " + TOKEN))
        self.assertIsNone(refusal(Authorization="bearer " + TOKEN), "the scheme is case-insensitive")
        self.assertIsNone(refusal(parts=("health",)))
        self.assertEqual(refusal("POST", ("request",), Content_Type="application/json", Content_Length="20")[0], 401)

    def test_a_web_page_is_refused_even_with_the_token(self):
        good = {"Authorization": "Bearer " + TOKEN}
        self.assertEqual(refusal(Origin="https://evil.example", **good)[0], 403)
        self.assertEqual(refusal(Origin="null", **good)[0], 403)
        self.assertEqual(refusal(Sec_Fetch_Site="cross-site", **good)[0], 403, "an <img> or a no-cors GET")
        self.assertEqual(refusal(Host="rebound.evil.example:8081", **good)[0], 403, "DNS rebinding")
        self.assertEqual(refusal(parts=("health",), Origin="https://evil.example")[0], 403)

    def test_the_control_planes_own_hosts_pass(self):
        for host in ("172.18.0.5:8081", "127.0.0.1:18081", "[::1]:8081", "desktop_box-a:8081", "localhost", ""):
            with self.subTest(host=host):
                self.assertIsNone(refusal(Host=host, Authorization="Bearer " + TOKEN))

    def test_a_write_must_be_json(self):
        good = {"Authorization": "Bearer " + TOKEN}
        self.assertEqual(refusal("POST", ("request",), Content_Type="text/plain", Content_Length="9", **good)[0], 415)
        self.assertEqual(refusal("POST", ("request",), Content_Type="application/x-www-form-urlencoded",
                                 Content_Length="9", **good)[0], 415)
        self.assertEqual(refusal("POST", ("request",), Content_Length="9", **good)[0], 415, "a body that says nothing")
        self.assertIsNone(refusal("POST", ("request",), Content_Type="application/json; charset=utf-8",
                                  Content_Length="9", **good))
        self.assertIsNone(refusal("POST", ("ticket", "end"), **good), "the control plane's body-less writes")

    def test_a_desktop_without_a_token_still_refuses_pages_and_non_json(self):
        self.assertIsNone(refusal(token=""))
        self.assertIsNone(refusal("POST", ("request",), token="", Content_Type="application/json", Content_Length="9"))
        self.assertEqual(refusal("POST", ("request",), token="", Content_Type="text/plain", Content_Length="9")[0], 415)
        self.assertEqual(refusal("POST", ("request",), token="", Origin="http://a.example",
                                 Content_Type="application/json", Content_Length="9")[0], 403)

    def test_the_token_comes_from_the_environment(self):
        with mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: TOKEN}):
            self.assertEqual(config.agent_auth_headers(), {"Authorization": "Bearer " + TOKEN})
            self.assertEqual(http_api.refusal("GET", ["skills"], {})[0], 401)
        with mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: ""}):
            self.assertEqual(config.agent_auth_headers(), {})
            self.assertIsNone(http_api.refusal("GET", ["skills"], {}))


class ServerTest(unittest.TestCase):
    """The checks on the wire: a refused request reaches no route."""

    def setUp(self):
        self.sent = []
        patches = [
            mock.patch.object(http_api, "save_request",
                              lambda content, task_id="": self.sent.append((content, task_id)) or Path("1.txt")),
            mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: TOKEN}),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def call(self, path, body=None, headers=None, raw=None):
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        request = urllib.request.Request(self.base + path, data=data, headers=headers or {},
                                         method="POST" if data is not None else "GET")
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as err:
            with err:
                return err.code, json.loads(err.read() or b"{}")

    def test_a_request_with_the_token_is_saved(self):
        status, body = self.call("/request", {"content": "file it", "task_id": TASK},
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
        self.assertEqual((status, body["status"]), (200, "received"))
        self.assertEqual(self.sent, [("file it", TASK)])

    def test_a_request_without_it_or_from_a_page_never_reaches_the_inbox(self):
        cases = [
            ({"Content-Type": "application/json"}, None, 401),
            ({"Content-Type": "application/json", "Authorization": "Bearer nope"}, None, 401),
            ({"Content-Type": "text/plain", "Authorization": "Bearer " + TOKEN}, None, 415),
            ({"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN,
              "Origin": "https://evil.example"}, None, 403),
        ]
        for headers, raw, code in cases:
            with self.subTest(headers=headers):
                status, body = self.call("/request", {"content": "yes"}, headers, raw)
                self.assertEqual(status, code)
                self.assertEqual(body["status"], "error")
        self.assertEqual(self.sent, [])

    def test_reads_need_it_too_and_health_does_not(self):
        self.assertEqual(self.call("/recordings")[0], 401)
        self.assertEqual(self.call("/health"), (200, {"status": "ok"}))

    def test_a_body_that_is_not_an_object_is_refused(self):
        headers = {"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN}
        self.assertEqual(self.call("/request", raw=b"[1]", headers=headers)[0], 400)
        self.assertEqual(self.call("/request", {"content": "x", "task_id": 7}, headers)[0], 400)
        self.assertEqual(self.sent, [])


class ReplyNamesItsQuestionTest(test_watcher_restart.Base):
    """A reply must name the task whose question is open; nothing else answers it."""

    def test_only_the_waiting_tasks_id_marks_a_reply(self):
        state.waiting_for_query = True
        state.current_task_id = TASK
        self.assertTrue(watchers.is_reply_file(messaging.save_request("yes", TASK).name))
        self.assertFalse(watchers.is_reply_file(messaging.save_request("yes", "").name), "no task ID")
        self.assertFalse(watchers.is_reply_file(messaging.save_request("yes", "t1111111111111111").name))

    def test_a_question_asked_with_no_task_takes_no_reply(self):
        state.waiting_for_query = True
        state.current_task_id = ""
        self.assertFalse(watchers.is_reply_file(messaging.save_request("yes", "").name))
        self.assertFalse(watchers.is_reply_file(messaging.save_request("yes", TASK).name))

    def test_a_message_not_saved_as_the_reply_answers_and_grants_nothing(self):
        # What a long request hands over when it reads a message as the answer (long_task._take_message).
        state.waiting_for_query = True
        state.current_task_id = TASK
        state.current_task = "pay the invoice"
        self.addCleanup(setattr, state, "last_query_kind", getattr(state, "last_query_kind", ""))
        state.last_query_kind = "approval"
        path = self.command_dir / f"20261102_101500_ab12cd34.{TASK}.txt"
        path.write_text("yes", encoding="utf-8")
        with mock.patch.object(watchers.approval, "note_reply") as noted:
            watchers.answer_waiting(path, "yes")
        noted.assert_not_called()
        watchers.approval.resolve_reply.assert_not_called()
        self.assertEqual(FakeThread.started, [], "the engine is not handed it")
        self.assertTrue(state.waiting_for_query, "the question stays open")
        self.assertFalse(path.exists(), "it is not read again")

    def test_the_reply_itself_still_settles_it(self):
        state.waiting_for_query = True
        state.current_task_id = TASK
        state.current_task = "pay the invoice"
        self.addCleanup(setattr, state, "last_query_kind", getattr(state, "last_query_kind", ""))
        state.last_query_kind = "approval"
        path = messaging.save_request("yes", TASK)
        with mock.patch.object(watchers.approval, "note_reply"), mock.patch.object(watchers, "lessons"), \
                mock.patch.object(watchers, "guidance"), mock.patch.object(watchers, "skill_task"):
            watchers.answer_waiting(path, "yes")
        watchers.approval.resolve_reply.assert_called_once()
        self.assertEqual(len(FakeThread.started), 1)


if __name__ == "__main__":
    unittest.main()
