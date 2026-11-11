"""
Every Python model call says what it is for (R3 of the plan of 2026-11-03; PLAN-P2 C1, D3).

The control plane's proxy stores every hosted call with its purpose, task and caller, read from headers: the prompt
bytes stay the same. A call with no purpose is stored as "untagged" and counted, and once a clean live round shows none
the proxy refuses it - so a missed call site must fail here first:
- an AST walk: every call of `chat`, `ask_once` or `ask_model` under agent/decurix_agent passes `purpose=`, a string
  literal of Python's own names (py.* or legacy.*) or a parameter named `purpose` that a wrapper passes on;
- each literal is one of the purposes below, so the proxy's stats name Python's share - the calls still to move
  behind soul (plan rule 3);
- `purpose` is keyword-only with no default on llm_client.chat/ask_once and harness.ask_model, so a caller that says
  nothing is a TypeError;
- llm_client sends the three headers (a fake HTTP server), and DECURIX_CALLER names the caller.
"""

import ast
import inspect
import json
import os
import re
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import harness, llm_client, state

ROOT = Path(__file__).resolve().parents[1] / "decurix_agent"
CALLS = {"chat", "ask_once", "ask_model"}
PATTERN = re.compile(r"^(py|legacy)\.[a-z0-9_.]+$")

# Python's model calls, by what they are for. Adding a call site adds its purpose here, on purpose.
PURPOSES = {
    "py.judge",               # verify.judge: the final check
    "py.judge.spot",          # skill_task: a trusted replay's sampled spot judgement (W0.5)
    "py.skill.ask",           # skill_task._ask_model's default
    "py.skill.calls_off",     # skill_task: does the reply call the work off
    "py.stage",               # long_task._ask_json: a planned run's own questions
    "py.binding.fill",        # binding: one document's values for a taught skill
    "py.approval.read",       # approval: what a reply to an approval decides
    "py.approval.choice",     # approval: which offered choice a reply takes
    "py.ground",              # grounder: where on the screen a described thing is
    "py.escalation.ground",   # escalation ladder: which element a step means
    "py.escalation.judge",    # escalation ladder: did the step do what it should
    "py.escalation.detour",   # escalation ladder: a way around a blocked step
    "py.escalation.point",    # escalation ladder: a point on a picture
    "py.mimic",               # mimic: this request's values for a recorded task
    "py.skill_engine.fits",   # skill engine: does a taught skill fit this request
    "py.kept_facts",          # kept facts: what a request's answers teach
    "py.playbook",            # playbooks: a proven way of doing a kind of task
    "py.study",               # study: what a demonstration teaches
    "py.lesson.gates",        # learning: which steps a lesson reply says need the person's OK
    "py.tool.args",           # tool_task: a learned tool's arguments
}


def _name(func):
    if isinstance(func, ast.Attribute):
        return func.attr
    if isinstance(func, ast.Name):
        return func.id
    return None


def _calls():
    """(file, line, purpose node or None) for every model call in the agent's code."""

    for path in sorted(ROOT.rglob("*.py")):
        if "thebot" in path.relative_to(ROOT).parts:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Call) and _name(node.func) in CALLS:
                given = next((k.value for k in node.keywords if k.arg == "purpose"), None)
                yield path.relative_to(ROOT).as_posix(), node.lineno, given


class EveryCallSaysWhatItIsFor(unittest.TestCase):
    def test_every_call_passes_a_purpose(self):
        missing, wrong, used = [], [], set()
        for where, line, given in _calls():
            if given is None:
                missing.append(f"{where}:{line}")
            elif isinstance(given, ast.Constant) and isinstance(given.value, str):
                if not PATTERN.match(given.value) or given.value not in PURPOSES:
                    wrong.append(f"{where}:{line}: {given.value!r}")
                used.add(given.value)
            elif not (isinstance(given, ast.Name) and given.id == "purpose"):
                wrong.append(f"{where}:{line}: not a literal or a passed-on `purpose`")
        self.assertEqual(missing, [], "model calls with no purpose=")
        self.assertEqual(wrong, [])
        self.assertGreater(len(used), 10, "the walk found the call sites")

    def test_the_wrappers_defaults_are_known_purposes(self):
        for path in sorted(ROOT.rglob("*.py")):
            if "thebot" in path.relative_to(ROOT).parts:
                continue
            for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    args = node.args.args + node.args.kwonlyargs
                    defaults = ([None] * (len(node.args.args) - len(node.args.defaults)) + node.args.defaults
                                + node.args.kw_defaults)
                    for arg, default in zip(args, defaults):
                        if arg.arg == "purpose" and isinstance(default, ast.Constant) and default.value is not None:
                            self.assertIn(default.value, PURPOSES, f"{path.name}:{node.lineno} {node.name}")

    def test_purpose_is_required_and_keyword_only(self):
        for func in (llm_client.chat, llm_client.ask_once, harness.ask_model):
            param = inspect.signature(func).parameters["purpose"]
            self.assertEqual(param.kind, inspect.Parameter.KEYWORD_ONLY, func.__name__)
            self.assertIs(param.default, inspect.Parameter.empty, func.__name__)
        with self.assertRaises(TypeError):
            llm_client.chat([{"role": "user", "content": "x"}])


class _Proxy(BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        _Proxy.seen.append({k: v for k, v in self.headers.items()})
        body = json.dumps({"choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": 1, "completion_tokens": 1}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class TheHeadersReachTheProxy(unittest.TestCase):
    def setUp(self):
        _Proxy.seen = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Proxy)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        url = f"http://127.0.0.1:{self.server.server_address[1]}/api/llm/azure/chat"
        for p in (mock.patch.object(llm_client, "CHAT_URL", url),
                  mock.patch.object(llm_client.usage, "add", lambda **kw: None)):
            p.start()
            self.addCleanup(p.stop)

    def headers(self):
        return {k.lower(): v for k, v in _Proxy.seen[-1].items()}

    def test_purpose_task_and_caller(self):
        env = {k: v for k, v in os.environ.items() if k != "DECURIX_CALLER"}
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(state, "current_task_id",
                                                                             "t0123456789abcdef"):
            self.assertEqual(llm_client.ask_once("system", "question", purpose="py.judge"), "ok")
        sent = self.headers()
        self.assertEqual(sent["x-decurix-purpose"], "py.judge")
        self.assertEqual(sent["x-decurix-task"], "t0123456789abcdef")
        self.assertEqual(sent["x-decurix-caller"], "agent")

    def test_no_task_sends_no_task_header(self):
        with mock.patch.object(state, "current_task_id", ""), \
                mock.patch("decurix_agent.task_context.current", lambda: ""):
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
        self.assertNotIn("x-decurix-task", self.headers())

    def test_decurix_caller_is_honoured(self):
        with mock.patch.dict(os.environ, {"DECURIX_CALLER": "gate"}):
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.approval.read")
        self.assertEqual(self.headers()["x-decurix-caller"], "gate")

    def test_the_body_is_the_same_whatever_the_purpose(self):
        bodies = []
        original = llm_client.urllib.request.Request

        def keep(url, data=None, **kw):
            bodies.append(data)
            return original(url, data=data, **kw)
        with mock.patch.object(llm_client.urllib.request, "Request", side_effect=keep):
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.study")
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.judge")
        self.assertEqual(bodies[0], bodies[1])


if __name__ == "__main__":
    unittest.main()
