"""
Every worker-side model call goes through soul (plan rule 3, phase 1; agent/decurix_agent/soul_call.py).

- llm_client.chat and ollama_client.ask_once ask soul's /api/llm/call when soul is up (a fake soul here), with the
  purpose, task, turn, the task's secret values and the agent token (or soul's call key from its file), and the proxy
  is not asked;
- the answer reads as the proxy's did: same return shape, the usage counted once, a cut-off answer asked again with
  room, a provider's 429 retried and its 400 not;
- when soul is not running, has no such route or refuses the call, the call goes straight to the proxy, as before, and
  that is logged once;
- the approval gate never asks soul (IN_GATE, DECURIX_CALLER=gate): its calls go to the proxy as the gate's;
- the signatures callers use are unchanged.
"""

import inspect
import json
import socket
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, llm_client, ollama_client, soul_call, state, task_context
from decurix_agent.computer import approval

TOKEN = "agent-token-for-tests"


class _Recorder(BaseHTTPRequestHandler):
    """A fake HTTP endpoint: keeps each request and answers from the class's queue (status, body)."""

    seen = None
    answers = None

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        type(self).seen.append({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()},
                                "body": json.loads(raw or b"{}")})
        status, body = type(self).answers.pop(0) if type(self).answers else (200, type(self).default)
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


class _Soul(_Recorder):
    default = {"text": "from soul", "truncated": False, "model": "default", "provider": "openai",
               "usage": {"prompt_tokens": 120, "completion_tokens": 7, "cached_tokens": 100}}


class _Proxy(_Recorder):
    default = {"choices": [{"message": {"content": "from the proxy"}, "finish_reason": "stop"}],
               "usage": {"prompt_tokens": 3, "completion_tokens": 1}}


def _serve(test, handler):
    handler.seen, handler.answers = [], []
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
    test.addCleanup(server.server_close)
    test.addCleanup(server.shutdown)
    return f"http://127.0.0.1:{server.server_address[1]}"


def _closed_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class _Base(unittest.TestCase):
    def setUp(self):
        soul_url = _serve(self, _Soul)
        proxy_url = _serve(self, _Proxy)
        self.home = TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        self.added = []
        env = {"DECURIX_AGENT_TOKEN": TOKEN, "DECURIX_TURN_ID": "j42:7", "DECURIX_CALLER": ""}
        for p in (mock.patch.dict("os.environ", env),
                  mock.patch.object(config, "THEBOT_URL", soul_url),
                  mock.patch.object(config, "DECURIX_DIR", Path(self.home.name)),
                  mock.patch.object(llm_client, "CHAT_URL", proxy_url + "/api/llm/azure/chat"),
                  mock.patch.object(llm_client.usage, "add", lambda **kw: self.added.append(kw)),
                  mock.patch.object(state, "current_task_id", "t0123456789abcdef"),
                  mock.patch.object(task_context, "secret_values", lambda task_id=None: ["s3cret-value"]),
                  mock.patch.object(approval, "IN_GATE", False),
                  mock.patch.dict(soul_call._state, {"down_until": 0.0, "absent": False, "noted": False}),
                  mock.patch.object(llm_client.time, "sleep", lambda s: None)):
            p.start()
            self.addCleanup(p.stop)


class ThroughSoulWhenItIsUp(_Base):
    def test_a_hosted_call_goes_through_soul_and_reads_as_before(self):
        message, spent = llm_client.chat([{"role": "system", "content": "You judge."},
                                          {"role": "user", "content": "Is it done?"}],
                                         max_tokens=1500, response_format={"type": "json_object"}, purpose="py.judge")
        self.assertEqual(message["content"], "from soul")
        self.assertEqual(spent["prompt_tokens"], 120)
        self.assertEqual(_Proxy.seen, [], "the proxy was asked directly")
        self.assertEqual(len(_Soul.seen), 1)
        sent = _Soul.seen[0]
        self.assertEqual(sent["path"], "/api/llm/call")
        self.assertEqual(sent["headers"]["authorization"], "Bearer " + TOKEN)
        body = sent["body"]
        self.assertEqual((body["brain"], body["purpose"], body["task"], body["turn"], body["max_tokens"]),
                         ("hosted", "py.judge", "t0123456789abcdef", "j42:7", 1500))
        self.assertEqual(body["response_format"], {"type": "json_object"})
        self.assertEqual(body["secrets"], ["s3cret-value"])
        self.assertEqual(body["messages"][1], {"role": "user", "content": "Is it done?"})
        # Counted once, here: soul counts a call it makes for the agent in no job of its own.
        self.assertEqual(self.added, [{"input_tokens": 120, "output_tokens": 7, "cached_input_tokens": 100}])

    def test_ask_once_keeps_its_shape(self):
        self.assertEqual(llm_client.ask_once("system", "question", purpose="py.skill.ask"), "from soul")
        self.assertEqual(_Soul.seen[0]["body"]["purpose"], "py.skill.ask")

    def test_a_worker_command_shows_souls_call_key_from_its_file(self):
        (Path(self.home.name) / soul_call.KEY_FILE).write_text("the-call-key\n", encoding="utf-8")
        with mock.patch.dict("os.environ", {"DECURIX_AGENT_TOKEN": ""}):
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.ground")
        self.assertEqual(_Soul.seen[0]["headers"]["authorization"], "Bearer the-call-key")
        self.assertEqual(_Proxy.seen, [])

    def test_an_answer_cut_off_is_asked_again_with_room(self):
        _Soul.answers = [(200, {"text": "", "truncated": True, "usage": {"prompt_tokens": 5, "completion_tokens": 800}})]
        message, _ = llm_client.chat([{"role": "user", "content": "x"}], max_tokens=800, purpose="py.study")
        self.assertEqual(message["content"], "from soul")
        self.assertEqual([s["body"]["max_tokens"] for s in _Soul.seen], [800, llm_client.EMPTY_RETRY_TOKENS])

    def test_a_providers_rate_limit_is_retried_and_its_refusal_is_not(self):
        _Soul.answers = [(429, {"error": "rate limited", "where": "provider"})]
        message, _ = llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual(message["content"], "from soul")
        self.assertEqual(len(_Soul.seen), 2)

        _Soul.seen.clear()
        _Soul.answers = [(400, {"error": "bad request", "where": "provider"})]
        with self.assertRaises(llm_client.LLMError) as caught:
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertIn("answered 400", str(caught.exception))
        self.assertEqual((len(_Soul.seen), _Proxy.seen), (1, []), "a provider's refusal is not asked elsewhere")

    def test_a_local_question_goes_through_soul_with_ollamas_options(self):
        with mock.patch.object(ollama_client, "ensure_model_resident", lambda: None), \
                mock.patch.object(ollama_client.usage, "add", lambda **kw: self.added.append(kw)), \
                mock.patch.object(ollama_client, "ollama_request", side_effect=AssertionError("asked Ollama directly")):
            self.assertEqual(ollama_client.ask_once("system", "question", purpose="py.escalation.judge"), "from soul")
        body = _Soul.seen[0]["body"]
        self.assertEqual((body["brain"], body["purpose"], body["temperature"], body["max_tokens"], body["stop"]),
                         ("local", "py.escalation.judge", 0.2, ollama_client.NUM_PREDICT, ollama_client.STOP_SEQUENCES))
        self.assertEqual(self.added, [{"input_tokens": 120, "output_tokens": 7, "cost_usd": 0}])


class StraightToTheProxyWhenSoulCannot(_Base):
    def test_soul_not_running(self):
        with mock.patch.object(config, "THEBOT_URL", f"http://127.0.0.1:{_closed_port()}"), \
                mock.patch.object(soul_call, "log") as logged:
            message, _ = llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual(message["content"], "from the proxy")
        self.assertEqual(len(_Proxy.seen), 2)
        self.assertEqual(logged.call_count, 1, "the fallback is logged once")
        self.assertGreater(soul_call._state["down_until"], 0)
        self.assertEqual(_Proxy.seen[0]["headers"]["x-decurix-purpose"], "py.study")

    def test_an_older_soul_with_no_call_route(self):
        _Soul.answers = [(404, {"error": "not found"})]
        message, _ = llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual(message["content"], "from the proxy")
        self.assertEqual((len(_Soul.seen), len(_Proxy.seen)), (1, 2), "an older soul is not asked again")

    def test_soul_refusing_one_call(self):
        _Soul.answers = [(400, {"error": "this soul's brain is local", "where": "soul"})]
        message, _ = llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual(message["content"], "from the proxy")
        llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual(len(_Soul.seen), 2, "one refusal does not keep soul out of the next call")

    def test_no_token_and_no_key(self):
        with mock.patch.dict("os.environ", {"DECURIX_AGENT_TOKEN": ""}):
            message, _ = llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertEqual((message["content"], _Soul.seen), ("from the proxy", []))

    def test_a_local_question_with_soul_away(self):
        with mock.patch.object(config, "THEBOT_URL", f"http://127.0.0.1:{_closed_port()}"), \
                mock.patch.object(ollama_client, "ensure_model_resident", lambda: None), \
                mock.patch.object(ollama_client, "ollama_request", return_value="from ollama") as direct:
            self.assertEqual(ollama_client.ask_once("system", "question", purpose="py.escalation.judge"), "from ollama")
        direct.assert_called_once()


class TheGateNeverAsksSoul(_Base):
    """The gate is another user's process: it must not depend on, or believe an answer from, the worker's soul."""

    def test_in_gate_the_call_goes_to_the_proxy_as_the_gates(self):
        with mock.patch.object(approval, "IN_GATE", True), mock.patch.dict("os.environ", {"DECURIX_CALLER": "gate"}):
            message, _ = llm_client.chat([{"role": "user", "content": "yes"}], purpose="py.approval.read")
        self.assertEqual(message["content"], "from the proxy")
        self.assertEqual(_Soul.seen, [])
        self.assertEqual(_Proxy.seen[0]["headers"]["x-decurix-caller"], "gate")

    def test_in_gate_alone_is_enough(self):
        with mock.patch.object(approval, "IN_GATE", True):
            self.assertFalse(soul_call.usable())
            with self.assertRaises(soul_call.Unusable):
                soul_call.call("hosted", [{"role": "user", "content": "x"}], purpose="py.approval.choice",
                               max_tokens=10, timeout=5)
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.approval.choice")
        self.assertEqual(_Soul.seen, [])

    def test_the_gate_caller_alone_is_enough(self):
        with mock.patch.dict("os.environ", {"DECURIX_CALLER": "gate"}):
            self.assertFalse(soul_call.usable())
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.approval.read")
        self.assertEqual(_Soul.seen, [])

    def test_the_gate_server_is_the_gate(self):
        source = (Path(__file__).resolve().parents[1] / "decurix_agent" / "gate_server.py").read_text(encoding="utf-8")
        self.assertIn("approval.IN_GATE = True", source)
        self.assertIn('os.environ["DECURIX_CALLER"] = "gate"', source)


class TheSignaturesCallersUseAreUnchanged(unittest.TestCase):
    def test_signatures(self):
        self.assertEqual(str(inspect.signature(llm_client.chat)),
                         "(messages, tools=None, model='default', max_tokens=4000, temperature=None, "
                         "response_format=None, *, purpose)")
        self.assertEqual(str(inspect.signature(llm_client.ask_once)), "(system, prompt, *, purpose)")
        self.assertEqual(str(inspect.signature(ollama_client.ask_once)), "(system, prompt, *, purpose)")


if __name__ == "__main__":
    unittest.main()
