"""Live Chromium record-once/learn/transfer tests inside an isolated desktop.

This runs on the desktop, not the host. It uses actual core actions and audit
recordings, the production learning questions and skill executor, and an HTTP
fixture application with 25 different operations. It does not emulate native
Excel/Word/PowerPoint; separate GUI tests cover those applications.
"""
import argparse
import html
import json
import threading
import time
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from transfer import SCENARIOS, evaluate

CASES = {c["id"]: c for c in SCENARIOS}
RESULTS = {}
VARIANTS = {}


class Application(BaseHTTPRequestHandler):
    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        key = urllib.parse.parse_qs(url.query).get("case", [SCENARIOS[0]["id"]])[0]
        if url.path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
            return
        case = CASES[key]
        mode = VARIANTS.get(key, "holdout")
        label = "Source data:" if mode == "renamed-control" else "Source data"
        padding = "margin:150px 0 0 250px" if mode == "moved-layout" else "margin:20px"
        page = f'''<!doctype html><meta charset="utf-8"><title>Workflow Lab</title>
<main style="font-family:Arial;{padding}"><h1>{html.escape(case['title'])}</h1>
<label>{label}<textarea aria-label="{label}" id="source" rows="5" cols="70"></textarea></label>
<label>Output mode<select aria-label="Output mode"><option>Review</option><option>Preview</option></select></label>
<button onclick="run()">Run operation</button><p id="status" role="status"></p><pre id="out"></pre></main>
<script>async function run(){{let r=await fetch('/operate?case={key}',{{method:'POST',body:document.getElementById('source').value}});
let v=await r.json();document.getElementById('out').textContent=JSON.stringify(v);document.getElementById('status').textContent=r.ok?'Result ready':'Operation failed';}}</script>'''
        data = page.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        key = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)["case"][0]
        try:
            raw = self.rfile.read(int(self.headers["Content-Length"])).decode()
            value = evaluate(CASES[key]["operation"], raw)
            RESULTS[key] = {"input": raw, "result": value}
            data, code = json.dumps(value).encode(), 200
        except Exception as exc:
            data, code = json.dumps({"error": str(exc)}).encode(), 422
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


def teach(case, session):
    from decurix_agent.computer import recording, learning, skills
    meta, events = recording.load(session)
    learned = learning.build([(meta, events)])
    for _ in range(100):
        question = learning.next_question(learned)
        if question is None:
            break
        answers = {"goal": "the page shows Result ready", "start": "no", "variable": "changes",
                   "variables": "all change", "confirm": "yes", "optional": "always", "coordinates": "keep",
                   "unsupported": "manual", "loop": "fixed", "approval": "go ahead"}
        response = learning.answer(learned, question, answers[question["kind"]])
        if response.get("decision"):
            break
    learned["name"] = "lab-" + case["id"]
    skills.save(learned)
    return skills.load(learned["name"]), len(events)


def run_case(case, port, variants):
    from decurix_agent import task_context
    from decurix_agent.computer import core, recording, replay, skill_engine, skills
    task_id = "t" + uuid.uuid4().hex[:16]
    url = f"http://127.0.0.1:{port}/?case={case['id']}"
    task_context.start(task_id, case["title"] + " " + case["example"])
    VARIANTS[case["id"]] = "holdout"
    RESULTS.pop(case["id"], None)
    core.open_target(url)
    typed = core.act("type", role="textbox", name="Source data", text=case["example"])
    assert typed.get("status") == "done", typed
    clicked = core.act("click", role="button", name="Run operation")
    assert clicked.get("status") == "done", clicked
    baseline_ui = core.verify(expect_text="Result ready")
    assert baseline_ui.get("ok") is True, baseline_ui
    baseline = RESULTS.get(case["id"], {})
    assert baseline.get("input") == case["example"] and "result" in baseline, baseline
    session = recording.session_for_task(task_id)
    recording.update(session, status="complete", title=case["title"])
    _, recorded = recording.load(session)
    assert any(e.get("kind") == "type" and e.get("text") == case["example"] and e.get("outcome") == "done" for e in recorded), recorded
    assert any(e.get("kind") == "click" and e.get("outcome") == "done" for e in recorded), recorded
    task_context.start("", "")
    learned, events = teach(case, session)
    assert learned["status"] == "ready", learned.get("status")
    outcomes = []
    for variant in variants:
        VARIANTS[case["id"]] = variant
        RESULTS.pop(case["id"], None)
        task_context.start("t" + uuid.uuid4().hex[:16], case["title"] + " " + case["holdout"])
        # The open in the stored recording loads a fresh fixture page and mode.
        params = {p["name"]: case["holdout"] for p in learned["params"] if p.get("varies")}
        result = skill_engine.run(skills.load(learned["name"]), params=params)
        ui = core.verify(expect_text="Result ready")
        actual = RESULTS.get(case["id"], {})
        passed = (result.get("outcome") in replay.SUCCESS and ui.get("ok") is True and
                  actual.get("result") == case["expected"] and actual.get("input") == case["holdout"])
        screenshot = Path("/home/headless/work/transfer-screens") / f"{case['id']}-{variant}.png"
        screenshot.parent.mkdir(parents=True, exist_ok=True)
        screenshot.write_bytes(core.observe()["frame"].png())
        outcomes.append({"variant": variant, "passed": passed, "outcome": result.get("outcome"),
                         "ui_verified":ui.get("ok") is True,
                         "actual": actual, "expected": case["expected"], "screenshot": str(screenshot),
                         "detail": result.get("text", "")[:1000]})
        print(json.dumps({"task": case["id"], **outcomes[-1]}), flush=True)
    task_context.start("", "")
    return {"task": case["id"], "recording": session, "recorded_events": events,
            "recording_capture":"production audit of agent-driven core actions", "teacher_baseline":baseline,
            "skill": learned["name"], "parameters": [p["name"] for p in learned["params"]],
            "passed": all(o["passed"] for o in outcomes), "variants": outcomes}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--only", help="Comma-separated scenario IDs")
    p.add_argument("--variants", default="holdout,moved-layout,renamed-control")
    p.add_argument("--port", type=int, default=18765)
    p.add_argument("--output", type=Path, default=Path("/home/headless/work/live-transfer.json"))
    args = p.parse_args()
    cases = [c for c in SCENARIOS if not args.only or c["id"] in args.only.split(",")]
    if not cases:
        p.error("No selected scenarios")
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Application)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    started, outcomes = time.monotonic(), []
    try:
        for case in cases:
            try:
                outcomes.append(run_case(case, args.port, args.variants.split(",")))
            except Exception as exc:
                outcomes.append({"task": case["id"], "passed": False, "error": str(exc)})
                print(json.dumps(outcomes[-1]), flush=True)
            report = {"mode": "live_chromium_fixture_record_learn_transfer", "live_apps_tested": True,
                      "application": "Chromium with local deterministic fixture", "families": len(outcomes),
                      "elapsed_seconds": round(time.monotonic()-started, 3),
                      "passed": all(r["passed"] for r in outcomes), "results": outcomes}
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(json.dumps(report, indent=2), encoding="utf-8")
    finally:
        server.shutdown()
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
