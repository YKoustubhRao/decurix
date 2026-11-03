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
import unittest.mock
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


class GuiOracleTest(unittest.TestCase):
    """The GUI bench's oracle runs inside the request a person would send, under every guard rail as it is."""

    def setUp(self):
        sys.path.insert(0, str(BENCH / "gui"))
        sys.path.insert(0, str(BENCH / "gui" / "fixtures"))
        import importlib.util
        import run as gui_run

        spec = importlib.util.spec_from_file_location("gui_bench_fixture", BENCH / "gui" / "fixtures" / "bench.py")
        gui_fixture = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(gui_fixture)
        from decurix_agent import filler, provenance
        from decurix_agent.computer import approval

        self.run, self.fixture, self.filler, self.provenance, self.approval = gui_run, gui_fixture, filler, provenance, approval
        self.tasks = json.loads((BENCH / "gui" / "tasks.json").read_text(encoding="utf-8"))["tasks"]

    @staticmethod
    def steps(task):
        for step in task.get("oracle") or []:
            if isinstance(step, str):
                yield {"run": step}
            else:
                yield step

    def test_the_request_names_every_value_the_oracle_types(self):
        import shlex
        typed = 0
        for task in self.tasks:
            request = self.run.request_text(task)
            sources = self.provenance.gather(request, [], roots=(), notes=[])
            for step in self.steps(task):
                words = shlex.split(step.get("run") or "")
                if words[:2] not in (["gui", "type"], ["gui", "select"]) or len(words) < 4:
                    continue
                value = words[-1]
                typed += 1
                self.assertEqual(self.filler.invented([("field", value)], request), [], f"{task['id']}: {value!r} looks invented")
                self.assertEqual(sources.unsourced(value), [], f"{task['id']}: the request does not hold {value!r}")
        self.assertGreaterEqual(typed, 10)

    def test_a_web_task_names_the_site_its_browser_is_on(self):
        command = self.fixture.browser_command("/settings")
        self.assertIn(f"--host-resolver-rules=MAP {self.fixture.PANEL_HOST} 127.0.0.1", command)
        self.assertTrue(command[-1].startswith(self.fixture.BROWSER_PANEL + "/autologin"))
        self.assertEqual(self.fixture.BROWSER_PANEL, self.run.PANEL_SITE)
        for task in self.tasks:
            if "browser" in (task.get("start") or {}):
                self.assertTrue(self.approval._names_host(self.run.request_text(task).lower(), self.fixture.PANEL_HOST), task["id"])

    def test_the_panel_holds_no_placeholder_addresses(self):
        text = (BENCH / "gui" / "fixtures" / "adminpanel.py").read_text(encoding="utf-8")
        import re as _re
        for address in _re.findall(r"[\w.]+@[\w.-]+\.\w+", text):
            self.assertFalse(self.filler.looks_filler(address), address)

    def test_every_press_the_gate_asks_about_is_expected_to_stop_or_follows_a_yes(self):
        import shlex
        for task in self.tasks:
            approved = False
            for step in self.steps(task):
                if "approve" in step:
                    approved = True
                    continue
                line = step.get("run") or step.get("submit") or ""
                words = shlex.split(line) if line else []
                if words[:2] != ["gui", "click"] or not words[2].startswith("button "):
                    continue
                name = shlex.split(words[2])[1]
                if self.approval.consequential(name) and not approved:
                    self.assertEqual(step.get("exit"), 3, f"{task['id']}: the gate asks before {name!r}")

    def test_the_oracle_never_approves_locally(self):
        source = (BENCH / "gui" / "run.py").read_text(encoding="utf-8")
        self.assertNotIn("resolve_reply", source)
        calls = []
        task = next(t for t in self.tasks if t.get("approve"))
        with unittest.mock.patch.object(self.run, "docker", side_effect=lambda *a, **k: calls.append(a)):
            result = self.run.run_oracle("c", task)
        self.assertEqual(calls, [], "a task that needs a yes ran steps in the desktop")
        self.assertIn("signed reply", result["skipped"])

    def test_a_listed_form_is_sent_by_the_same_click_checked(self):
        task = next(t for t in self.tasks if t["id"] == "web-create-user")
        calls = []

        def docker(container, *args, **kw):
            calls.append(args)
            line = args[-1] if args[0] == "bash" and args[1] == "-c" else ""
            listed = sum(1 for c in calls if "Create user" in " ".join(c)) == 1
            code = 1 if ("Create user" in line and listed) else 0
            return type("P", (), {"returncode": code, "stdout": "", "stderr": ""})()

        with unittest.mock.patch.object(self.run, "docker", side_effect=docker):
            result = self.run.run_oracle("c", task)
        self.assertEqual(result["errors"], [])
        presses = [c for c in calls if "Create user" in " ".join(c)]
        self.assertEqual(len(presses), 2)
        self.assertTrue(all("--checked" in c[-1] for c in presses))
        context = calls[0]
        self.assertIn("task_context.start", context[2])
        self.assertTrue(context[3].startswith("t") and len(context[3]) == 17)
        self.assertIn("dana@northbeam.test", context[4])
        self.assertIn("admin.bench.test", context[4])


if __name__ == "__main__":
    unittest.main()
