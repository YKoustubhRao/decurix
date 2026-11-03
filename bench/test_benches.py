"""
Offline checks of the benches' own plumbing (no Docker, no model). Run:

    py -3 -m unittest bench/test_benches.py
"""

import json
import os
import re
import subprocess
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

BENCH = Path(__file__).resolve().parent
REPO = BENCH.parent
sys.path.insert(0, str(BENCH))
sys.path.insert(0, str(REPO / "agent"))

import agent_token  # noqa: E402


def agent_refusal():
    """The agent's own rule for who may call :8081 (http_api.refusal), or None when it cannot be imported here."""
    try:
        from decurix_agent import http_api
    except Exception:  # noqa: BLE001 - an agent module that needs the desktop: the stand-in below is used
        return None
    return http_api.refusal


class AgentStandIn(BaseHTTPRequestHandler):
    """A desktop agent's :8081 that serves only a caller with the desktop's token, as http_api.refusal decides."""

    token = ""
    rule = None

    def _serve(self, method):
        parts = [p for p in self.path.split("?")[0].strip("/").split("/") if p]
        if self.rule is not None:
            refused = self.rule(method, parts, self.headers, token=self.token)
        else:
            given = self.headers.get("Authorization") or ""
            refused = None if given == "Bearer " + self.token else (401, "this desktop's agent token is required")
        body = json.dumps({"error": refused[1]} if refused else {"path": self.path}).encode()
        self.send_response(refused[0] if refused else 200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._serve("GET")

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        self._serve("POST")

    def log_message(self, *a):
        pass


class AgentTokenTest(unittest.TestCase):
    def setUp(self):
        AgentStandIn.token = agent_token.TOKEN
        rule = agent_refusal()
        AgentStandIn.rule = staticmethod(rule) if rule else None
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), AgentStandIn)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def call_in_desktop(self, statement, env_token):
        """Run PY_AGENT + statement as `docker exec ... python3 -c` would inside a desktop started with env_token."""
        code = agent_token.PY_AGENT.replace("http://localhost:8081", self.base) + statement
        env = {k: v for k, v in os.environ.items() if k != agent_token.ENV}
        if env_token is not None:
            env[agent_token.ENV] = env_token
        return subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, env=env, timeout=30)

    def test_a_desktop_started_with_the_token_answers_the_bench(self):
        r = self.call_in_desktop("print(agent('/skills')); print(agent('/request', {'content': 'x'}))", agent_token.TOKEN)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('"/skills"', r.stdout)
        self.assertIn('"/request"', r.stdout)

    def test_without_the_token_the_desktop_refuses(self):
        for token in (None, "another"):
            r = self.call_in_desktop("print(agent('/skills'))", token)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("401", r.stderr)

    def test_docker_env_names_the_agents_variable(self):
        self.assertEqual(agent_token.docker_env("abc"), ["-e", "DECURIX_AGENT_TOKEN=abc"])
        self.assertEqual(agent_token.ENV, "DECURIX_AGENT_TOKEN")
        self.assertGreaterEqual(len(agent_token.TOKEN), 32)


class BenchDesktopsTest(unittest.TestCase):
    """Every bench that starts its own desktop gives it a token, and calls its agent with it."""

    def bench_files(self):
        return [p for p in BENCH.rglob("*.py") if p.name != "test_benches.py" and "__pycache__" not in p.parts]

    def test_every_desktop_a_bench_starts_has_a_token(self):
        starts = 0
        for p in self.bench_files():
            text = p.read_text(encoding="utf-8")
            if '"docker", "run"' in text and "/opt/decurix_agent" in text:
                starts += 1
                self.assertIn("token_env()", text, f"{p.relative_to(REPO)} starts a desktop with no agent token")
        self.assertGreaterEqual(starts, 4)

    def test_no_bench_calls_the_agent_without_its_token(self):
        bare = re.compile(r"urlopen\([^)]*(localhost|127\.0\.0\.1):8081")
        for p in self.bench_files():
            for n, line in enumerate(p.read_text(encoding="utf-8").splitlines(), 1):
                self.assertIsNone(bare.search(line), f"{p.relative_to(REPO)}:{n} calls :8081 without the token")


class TeachAnswersTest(unittest.TestCase):
    """The skills bench answers a real lesson's questions by their structure, read through its own probe."""

    def setUp(self):
        sys.path.insert(0, str(REPO / "agent" / "tests"))
        sys.path.insert(0, str(BENCH / "skills"))
        import teach_answers
        from decurix_agent.computer import learning
        from test_skills import demo

        self.answers, self.learning, self.demo = teach_answers, learning, demo
        self.home = Path(self.enterContext(__import__("tempfile").TemporaryDirectory()))
        (self.home / ".decurix" / "skills").mkdir(parents=True)

    def probe(self, skill):
        """Run the bench's PROBE as the desktop would, against this skill saved as the draft."""
        (self.home / ".decurix" / "skills" / "draft.json").write_text(json.dumps(skill), encoding="utf-8")
        env = dict(os.environ, HOME=str(self.home), USERPROFILE=str(self.home), PYTHONPATH=str(REPO / "agent"))
        r = subprocess.run([sys.executable, "-c", self.answers.PROBE], capture_output=True, text=True, env=env, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr[-800:])
        return json.loads(r.stdout)

    def teach(self, skill):
        kinds = []
        while (q := self.learning.next_question(skill)) is not None:
            reply = self.answers.answer(self.probe(skill), "Some words first. " + q["text"])
            kinds.append(q["kind"])
            self.assertTrue(self.learning.answer(skill, q, reply)["understood"], f"{q['kind']}: {reply!r}")
            self.assertLess(len(kinds), 40, "the lesson never ended")
        return kinds

    def test_a_lesson_without_a_model(self):
        skill = self.learning.build([self.demo("demo-20260914T101500-aaaaaa", "Acme"), self.demo("demo-20260914T101600-bbbbbb", "Globex")])
        kinds = self.teach(skill)
        self.assertIn("goal", kinds)
        self.assertEqual(kinds[-1], "confirm")

    def test_a_lesson_a_hosted_model_studied(self):
        skill = self.learning.build([self.demo("demo-20260914T101500-aaaaaa", "Acme")])
        topics = ["sources", "changes", "filing", "matching", "prechecks"]
        skill["understanding"] = {"summary": "Saves a customer.", "done_when": "Saved shows",
                                  "questions": [{"id": f"b_{t}", "topic": t, "text": f"Question about {t}?"} for t in topics]}
        kinds = self.teach(skill)
        self.assertEqual(kinds[:6], ["goal"] + ["study"] * 5)
        self.assertEqual(skill["answers"]["study:b_filing"], self.answers.FACTS["filing"])
        self.assertEqual(skill["answers"]["goal"], "yes")

    def test_an_unknown_kind_or_topic_stops_the_bench(self):
        with self.assertRaises(self.answers.UnknownQuestion):
            self.answers.answer({"question": {"kind": "loop", "text": "Repeat?"}})
        with self.assertRaises(self.answers.UnknownQuestion):
            self.answers.answer({"question": {"kind": "study", "id": "study:q9", "text": "?"}, "study": [{"id": "q9", "topic": "weather"}]})
        with self.assertRaises(self.answers.UnknownQuestion):
            self.answers.answer({"question": {"kind": "start", "text": "Should I check that?"}}, "Something else entirely was asked.")


if __name__ == "__main__":
    unittest.main()
