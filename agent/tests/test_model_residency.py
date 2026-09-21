import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

from decurix_agent import config, ollama_client


class FakeOllama:
    """/api/ps says nothing is loaded until /api/generate has been asked."""

    def __init__(self, delay=0.0, resident=False, fail=False):
        self.loaded = resident
        self.generates = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                models = (
                    [{"name": config.OLLAMA_MODEL, "context_length": ollama_client.NUM_CTX}]
                    if outer.loaded
                    else []
                )
                self._send(200, {"models": models})

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                body["_path"] = self.path
                outer.generates.append(body)
                if fail:
                    self._send(404, {"error": "model not found"})
                    return
                threading.Event().wait(delay)
                outer.loaded = True
                self._send(200, {"done": True})

            def _send(self, code, obj):
                data = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class TestModelResidency(unittest.TestCase):
    def setUp(self):
        ollama_client._resident_at = 0.0

    def test_cold_model_is_loaded_with_the_options_requests_use(self):
        fake = FakeOllama(delay=0.2)
        self.addCleanup(fake.close)

        with patch.object(config, "OLLAMA_URL", fake.url), \
             patch("decurix_agent.progress.emit") as emit:
            ollama_client.ensure_model_resident()

        self.assertEqual(len(fake.generates), 1)
        sent = fake.generates[0]
        self.assertEqual(sent["prompt"], "")
        self.assertEqual(sent["options"]["num_ctx"], ollama_client.NUM_CTX)
        self.assertEqual(sent["options"]["num_thread"], ollama_client.model_options()["num_thread"])
        self.assertEqual(sent["options"]["num_batch"], ollama_client.model_options()["num_batch"])
        self.assertTrue(any("Loading the model" in c.args[0] for c in emit.call_args_list))

    def test_resident_model_is_left_alone(self):
        fake = FakeOllama(resident=True)
        self.addCleanup(fake.close)

        with patch.object(config, "OLLAMA_URL", fake.url):
            ollama_client.ensure_model_resident()

        self.assertEqual(fake.generates, [])

    def test_missing_model_is_an_error_not_a_wait(self):
        fake = FakeOllama(fail=True)
        self.addCleanup(fake.close)

        with patch.object(config, "OLLAMA_URL", fake.url), \
             patch("decurix_agent.progress.emit"):
            with self.assertRaises(RuntimeError):
                ollama_client.ensure_model_resident()

    def test_priming_sends_the_real_system_prompt_once_and_only_that(self):
        from decurix_agent import state

        fake = FakeOllama(resident=True)
        self.addCleanup(fake.close)
        state.current_task = None

        with patch.object(config, "OLLAMA_URL", fake.url):
            ollama_client.prime_prompt_cache()

        chats = [g for g in fake.generates if g["_path"] == "/api/chat"]
        self.assertEqual(len(chats), 1)
        self.assertEqual(chats[0]["messages"][0]["content"], ollama_client.SYSTEM_PROMPT)
        self.assertEqual(chats[0]["options"]["num_predict"], 4)
        # same runner options as a real request, so it cannot trigger a reload
        real = ollama_client.model_options()
        for key in ("num_ctx", "num_thread", "num_batch"):
            self.assertEqual(chats[0]["options"][key], real[key])

    def test_priming_never_replaces_the_cache_under_a_running_task(self):
        from decurix_agent import state

        fake = FakeOllama(resident=True)
        self.addCleanup(fake.close)
        state.current_task = "something in progress"
        self.addCleanup(setattr, state, "current_task", None)

        with patch.object(config, "OLLAMA_URL", fake.url):
            ollama_client.prime_prompt_cache()

        self.assertEqual([g for g in fake.generates if g["_path"] == "/api/chat"], [])

    def test_priming_never_raises(self):
        with patch.object(config, "OLLAMA_URL", "http://127.0.0.1:9"):
            ollama_client.prime_prompt_cache()

    def test_openai_style_endpoint_is_never_probed(self):
        with patch.object(config, "OLLAMA_URL", "http://127.0.0.1:9/v1"):
            ollama_client.ensure_model_resident()


if __name__ == "__main__":
    unittest.main()
