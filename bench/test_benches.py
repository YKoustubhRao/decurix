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

    def test_the_plan_question_is_known_by_the_agents_own_marker(self):
        """F-B2': a planning brain's first reply in a run is its plan; the bench agrees it, and knows it by the marker
        the agent itself writes (read on the desktop by PLAN_MARKER_PROBE), which soul's plan.go writes too."""
        env = dict(os.environ, PYTHONPATH=str(REPO / "agent"))
        r = subprocess.run([sys.executable, "-c", self.answers.PLAN_MARKER_PROBE], capture_output=True, text=True, env=env, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr[-800:])
        marker = r.stdout.strip()
        self.assertIn(marker, (REPO / "agent" / "thebot" / "internal" / "skills" / "plan.go").read_text(encoding="utf-8"))
        plan = {"kind": "query", "reply": marker + "\n- Covering: one customer\nIs there a list I should check?"}
        self.assertTrue(self.answers.is_plan(plan, marker))
        heal = {"kind": "query", "reply": 'The button "Save" is not on the screen. 1) button "Save changes"'}
        self.assertFalse(self.answers.is_plan(heal, marker))
        self.assertFalse(self.answers.is_plan({"kind": "final", "reply": marker}, marker))
        self.assertFalse(self.answers.is_plan(plan, ""))
        self.assertIn(self.answers.FACTS["sources"], self.answers.PLAN_ANSWER)


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
        # The gate gets its signed copy of the request first, then the agent's task context the same request (F-B4').
        gate, context = calls[0], calls[1]
        self.assertIn("sign_request", gate[2])
        self.assertIn("task_context.start", context[2])
        self.assertTrue(context[3].startswith("t") and len(context[3]) == 17)
        self.assertEqual(gate[3:5], context[3:5])
        self.assertIn("dana@northbeam.test", context[4])
        self.assertIn("admin.bench.test", context[4])

    def test_the_gate_copy_is_signed_inside_the_desktop_as_root(self):
        task = next(t for t in self.tasks if t["id"] == "web-login")
        seen = []

        def docker(container, *args, **kw):
            seen.append((args, kw.get("user", "headless")))
            return type("P", (), {"returncode": 0, "stdout": "", "stderr": ""})()

        with unittest.mock.patch.object(self.run, "docker", side_effect=docker):
            self.run.run_oracle("c", task)
        (gate, user), (context, context_user) = seen[0], seen[1]
        self.assertEqual((user, context_user), ("root", "headless"))
        self.assertIn("gate_server.read_key('/var/lib/decurix-gate')", gate[2])
        self.assertIn("127.0.0.1:8083/request", gate[2])
        self.assertIn(self.fixture.PANEL_HOST, gate[4])

    def test_a_new_record_stopped_once_is_saved_again_only_on_the_guards_note(self):
        task = next(t for t in self.tasks if t["id"] == "web-create-user")
        for noted, presses_wanted in ((True, 2), (False, 1)):
            calls = []

            def docker(container, *args, **kw):
                calls.append(args)
                line = args[2] if args[0] == "bash" and args[1] == "-c" else ""
                if line == self.run.RECORD_STOPPED:
                    return type("P", (), {"returncode": 0 if noted else 1, "stdout": "", "stderr": ""})()
                if line == self.run.FORM_LISTED:
                    return type("P", (), {"returncode": 1, "stdout": "", "stderr": ""})()
                first = sum(1 for c in calls if "Create user" in " ".join(c)) == 1
                code = 1 if ("Create user" in line and first) else 0
                return type("P", (), {"returncode": code, "stdout": "", "stderr": ""})()

            with unittest.mock.patch.object(self.run, "docker", side_effect=docker):
                result = self.run.run_oracle("c", task)
            presses = [c for c in calls if "Create user" in " ".join(c)]
            self.assertEqual(len(presses), presses_wanted, noted)
            self.assertEqual(result["errors"] == [], noted, result["errors"])

    def test_an_app_open_before_the_task_is_brought_forward_before_keys_go_to_it(self):
        """Keys and typing with no target go to the window in front; one open before the task began is refused until
        the task works in it (core._left_from_before). These three press keys before acting on the app's own window
        by ref (the first acts in a dialog), so they bring it forward first, as a worker must (live, LIVE-2 F-B4')."""
        for task in self.tasks:
            if task["id"] in ("calc-rename-sheet", "writer-new-doc", "mousepad-replace"):
                self.assertTrue(next(self.steps(task))["run"].startswith("gui front "), task["id"])


class ResourceBenchTest(unittest.TestCase):
    """The resource bench's viewer follows the dashboard's own timers, and its sums and groups are right."""

    def setUp(self):
        import importlib.util
        sys.path.insert(0, str(BENCH / "resources"))
        import viewer
        # By its path: bench/gui's run.py is imported as "run" by the GUI bench's tests.
        spec = importlib.util.spec_from_file_location("resources_run", BENCH / "resources" / "run.py")
        resources_run = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(resources_run)
        self.viewer, self.run = viewer, resources_run

    def test_the_viewer_polls_on_the_dashboards_own_timers(self):
        text = (REPO / "web" / "templates" / "index.html").read_text(encoding="utf-8")
        theme = (REPO / "web" / "templates" / "theme.html").read_text(encoding="utf-8")
        found = {}
        for m in re.finditer(r"\bpoll\((\w+),\s*(\d+)(?:,\s*\{hidden:\s*(\d+)\})?\)", text + "\n" + theme):
            found[m.group(1)] = (int(m.group(2)), int(m.group(3) or 0))
        mirrored = {name: (ms, hidden) for name, ms, hidden, *_ in self.viewer.POLLS}
        for name, timing in mirrored.items():
            self.assertEqual(found.get(name), timing, f"{name}: the page polls it as {found.get(name)}")
        unaccounted = set(found) - set(mirrored) - set(self.viewer.SILENT)
        self.assertEqual(unaccounted, set(), "a dashboard timer the viewer neither mirrors nor names as silent")
        shot = re.search(r"SHOT_INTERVAL_MS = (\d+)", text)
        self.assertEqual(int(shot.group(1)), self.viewer.SHOT_INTERVAL_MS)
        # Each mirrored poll asks what the page's function asks.
        for name, _, _, _, path, _ in self.viewer.POLLS:
            body = re.search(r"function " + name + r"\(\)\{(.*?)\n  \}", text + theme, re.S)
            self.assertIsNotNone(body, name)
            self.assertIn(path.split("?")[0].split("{")[0], body.group(1), name)

    def test_the_viewer_reads_the_cards_and_asks_what_they_hold(self):
        page = ('<article class="dcard" data-desktop-id="a"><span data-base-status="running"></span>'
                '<img data-shot="a"><a data-activity="a"></a><details data-mem-list="a"></details>'
                '<div data-task-pick="a"></div></article>'
                '<article class="dcard" data-desktop-id="b"><span data-base-status="paused"></span><img data-shot="b"></article>')
        seen = []

        class Plane(BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append(self.path)
                body = page.encode() if self.path == "/" else (b"\x89PNG" if "screenshot" in self.path else b"{}")
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Plane)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            got = self.viewer.watch(f"http://127.0.0.1:{server.server_address[1]}", 1.5)
        finally:
            server.shutdown()
            server.server_close()
        self.assertEqual(seen[0], "/")
        self.assertEqual(got["desktops"], 2)
        shots = [p for p in seen if p.startswith("/api/desktops/screenshot")]
        self.assertEqual({p.split("id=")[1].split("&")[0] for p in shots}, {"a", "b"})
        # One poll for every card's parts and the rail, asked at load for every card the page holds.
        self.assertIn("/api/desktops/overview?ids=a%2Cb", seen)
        for path in ("/api/desktops/activity", "/api/desktops/tasks", "/api/desktops/memory", "/api/querynotes",
                     "/api/desktops/resource-alerts"):
            self.assertFalse([p for p in seen if p.startswith(path)], f"{path}: the overview asks it now")
        for path in ("/api/model", "/api/docker/status", "/api/tasks/finished"):
            self.assertIn(path, seen)
        self.assertEqual(got["requests"], len(seen) - 1)
        self.assertGreater(got["bytes"], 0)

    def test_a_hidden_tab_keeps_only_its_slow_polls(self):
        hidden = {name for name, ms, hidden_ms, *_ in self.viewer.POLLS if hidden_ms}
        self.assertEqual(hidden, {"pollOverview", "refreshDocker", "refreshFinished"})

    def test_processes_are_grouped_by_what_they_run(self):
        g = self.run.group_of
        self.assertEqual(g("/usr/lib/chromium/chromium --type=renderer"), "chrome")
        self.assertEqual(g("/usr/bin/python3 -I -c import sys, runpy; runpy.run_module(\"decurix_agent.gate_server\")"), "gate")
        self.assertEqual(g("/usr/bin/python3 /opt/vm_server.py"), "agent")
        self.assertEqual(g("python3 -m decurix_agent.agent"), "agent")
        self.assertEqual(g("/opt/soul serve"), "soul")
        self.assertEqual(g("python3 /usr/bin/websockify --web /usr/share/novnc 6901 localhost:5901"), "websockify")
        self.assertEqual(g("xfdesktop --display :1"), "xfdesktop")
        self.assertEqual(g("/usr/lib/x86_64-linux-gnu/xfce4/panel/wrapper-2.0 /usr/lib/x86_64-linux-gnu/xfce4/panel/plugins/libsystray.so"), "panel")
        self.assertEqual(g("/usr/bin/Xtigervnc :1 -desktop"), "x")
        self.assertEqual(g("/bin/bash /dockerstartup/startup.sh"), "other")

    def test_tokens_per_task_and_the_cached_share(self):
        calls = {"t1": [{"input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 5, "purpose": "operator.turn"},
                        {"input_tokens": 300, "cached_input_tokens": 0, "output_tokens": 9, "purpose": "agent.py.judge"}],
                 "t2": [{"input_tokens": 600, "cached_input_tokens": 420, "output_tokens": 1, "purpose": "operator.turn"}],
                 "t3": []}

        class Plane:
            def get(self, path):
                return {"calls": calls[path.split("task=")[1]]}

        got = self.run.tokens(Plane(), ["t1", "t2", "t3"])
        self.assertEqual(got["tasks"], 2)  # a task with no recorded call is left out
        self.assertEqual(got["input_per_task_median"], 500)
        self.assertEqual(got["cached_share"], 0.5)
        self.assertEqual(list(got["by_purpose"]), ["operator.turn", "agent.py.judge"])
        self.assertEqual(got["by_purpose"]["operator.turn"]["cached_share"], 0.714)
        md = self.run.table({"at": "now", "tokens": got, "gui": {"shell_overhead_ms_p50": 3.0,
                             "all": {"n": 1, "failed": 0, "p50_ms": 1, "p95_ms": 1, "own_cpu_ms_p50": 1, "container_cpu_ms_p50": 1},
                             "by_kind": {}}})
        self.assertIn("| 2 | 500 |", md)
        self.assertIn("## gui", md)

    def density(self, start_mb=3000, per_desktop_mb=600, fail_at=None, stop_ids=(), **kw):
        """The density count against a stand-in host whose available memory falls by per_desktop_mb a desktop."""
        made_by_cp, calls = [], []

        class Plane:
            def create(self, name):
                if fail_at is not None and len(made_by_cp) + 1 == fail_at:
                    raise RuntimeError("Docker refused: not enough memory")
                made_by_cp.append(name)
                return {"id": name}

            def wait_ready(self, desktop_id):
                return {"ready": True}

            def post(self, path, form, timeout=300):
                calls.append((path, form["id"]))
                return {}

        class Host:
            def docker(self, *args, timeout=300):
                status = "exited" if args[0] == "inspect" and any(s in args[-1] for s in stop_ids) else "running"
                return subprocess.CompletedProcess(args, 0, status + "\n", "")

            def sh(self, script, *args, timeout=300):
                return subprocess.CompletedProcess(args, 0, "", "")

        def memory(host):
            return {"docker_host_available_mb": start_mb - per_desktop_mb * len(made_by_cp)}

        made = []
        with unittest.mock.patch.object(self.run, "host_memory", memory), \
                unittest.mock.patch.object(self.run, "measure", lambda h, c, s: {"memory_mb": 400.0, "anon_mb": 180.0, "cpu_pct": 1.0}), \
                unittest.mock.patch.object(self.run, "wait_agent", lambda *a, **k: None), \
                unittest.mock.patch.object(self.run, "put_form", lambda *a: None), \
                unittest.mock.patch.object(self.run.time, "sleep", lambda s: None):
            got = self.run.density_bench(Host(), Plane(), made, lambda m: None, settle=0, **kw)
        return got, made, calls

    def test_density_counts_desktops_until_the_memory_floor(self):
        got, made, calls = self.density(floor_mb=1024)
        # 3000, 2400, 1800 and 1200 MB before each: four start; at 600 the fifth would go under the floor.
        self.assertEqual((got["made"], got["fit"]), (4, 4))
        self.assertIn("memory floor", got["stopped"])
        self.assertEqual(len(made), 4, "every desktop made is handed back to be deleted")
        self.assertEqual([d["available_before_mb"] for d in got["desktops"]], [3000, 2400, 1800, 1200])
        # Each one is paused and woken, and its wake is timed.
        self.assertEqual([p for p, _ in calls].count("/api/desktops/pause"), 4)
        self.assertEqual([p for p, _ in calls].count("/api/desktops/unpause"), 4)
        self.assertTrue(all(d.get("wake_s") is not None for d in got["desktops"]))
        self.assertEqual(got["memory_mb_median"], 400.0)
        md = self.run.table({"at": "now", "density": got})
        self.assertIn("**4 desktops fit**", md)
        self.assertIn("idle, no browser", md)

    def test_density_stops_at_its_cap_and_on_a_refused_start(self):
        got, made, _ = self.density(max_desktops=2)
        self.assertEqual((got["fit"], got["stopped"]), (2, "--max 2 reached"))
        got, made, _ = self.density(fail_at=3)
        self.assertEqual(got["made"], 2)
        self.assertIn("desktop 3 did not start", got["stopped"])
        self.assertEqual(len(made), 2)
        # A desktop the control plane stopped to make room for a later one does not count as fitting.
        got, _, _ = self.density(max_desktops=3, stop_ids=("resbench-d1-",))
        self.assertEqual((got["made"], got["fit"]), (3, 2))

    def test_density_runs_only_when_named(self):
        self.assertNotIn("density", self.run.SECTIONS)
        self.assertIn("density", self.run.EXTRA_SECTIONS)

    def test_percentiles(self):
        self.assertEqual(self.run.pct([1, 2, 3, 4], 0.5), 2.5)
        self.assertEqual(self.run.pct([10], 0.95), 10)
        self.assertIsNone(self.run.pct([], 0.5))


if __name__ == "__main__":
    unittest.main()
