"""Local UI backend: enter a task as a list of sub-instructions, pick a model,
click Run All, watch each instruction get turned into Playwright code and
executed against the one shared, embedded-in-the-UI Chromium window."""
import json
import queue
import subprocess
import sys
import threading
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse

import _pathfix  # noqa: F401
import interrogate
import models
import runner
import spec_backend
import stats as stats_mod
from screencast import Screencaster

HERE = Path(__file__).parent
PYTHON = sys.executable
BASE_CDP_PORT = 9333

STATE = {"host_proc": None, "cdp": None, "screencaster": None, "cancel": threading.Event(),
         "next_port": BASE_CDP_PORT, "paused": None}


def _start_browser():
    """Launches a fresh browser_host.py (own Chromium, own throwaway profile) on
    a port that hasn't been used yet this session, so a /reset never races the
    just-killed previous instance for the same debug port."""
    port = STATE["next_port"]
    STATE["next_port"] += 1
    proc = subprocess.Popen(
        [PYTHON, str(HERE / "browser_host.py"), "--port", str(port)],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    cdp = proc.stdout.readline().strip()
    if not cdp:
        stderr = proc.stderr.read()
        raise RuntimeError(f"browser_host.py failed to start:\n{stderr}")
    STATE["host_proc"] = proc
    STATE["cdp"] = cdp
    STATE["screencaster"] = Screencaster(cdp)
    print(f"[browser-agent] shared browser ready at {cdp}", flush=True)


def _stop_browser():
    if STATE["screencaster"]:
        STATE["screencaster"].stop()
    if STATE["host_proc"]:
        STATE["host_proc"].terminate()
        try:
            STATE["host_proc"].wait(timeout=5)
        except subprocess.TimeoutExpired:
            STATE["host_proc"].kill()


@asynccontextmanager
async def lifespan(app: FastAPI):
    _start_browser()
    yield
    _stop_browser()
    spec_backend.stop_if_running()


app = FastAPI(lifespan=lifespan)


@app.get("/")
def index():
    return FileResponse(HERE / "static" / "index.html")


@app.get("/models")
def list_models():
    return {key: {"label": cfg["label"], "vision": cfg["vision"]} for key, cfg in models.MODELS.items()}


@app.get("/stats")
def get_stats():
    return stats_mod.snapshot()


@app.get("/screencast.mjpg")
def screencast_mjpg():
    caster = STATE["screencaster"]

    def gen():
        boundary = b"--frame\r\n"
        # If /reset swaps in a new Screencaster mid-stream, this one's browser is
        # gone -- end the response so the client's own reconnect loop picks up
        # the new caster on its next fetch, instead of hanging on dead frames.
        while STATE["screencaster"] is caster and not caster._stopped:
            frame = caster.get_latest(timeout=2.0)
            if frame is None:
                continue
            yield boundary + b"Content-Type: image/jpeg\r\n\r\n" + frame + b"\r\n"

    return StreamingResponse(gen(), media_type="multipart/x-mixed-replace; boundary=frame")


@app.post("/interrogate")
async def do_interrogate(request: Request):
    body = await request.json()
    question = (body.get("question") or "").strip()
    model_key = body.get("model", models.DEFAULT_MODEL)
    vision = bool(body.get("vision"))
    if not question:
        return JSONResponse({"error": "empty question"}, status_code=400)
    try:
        answer, snap = interrogate.ask(STATE["cdp"], question, model_key=model_key, vision=vision)
    except Exception as exc:
        return JSONResponse({"error": str(exc)}, status_code=500)
    return {"answer": answer, "url": snap["url"], "title": snap.get("title", "")}


def _stream(run_kwargs, model_key, vision, verify_enabled, fast=True, flavour="live"):
    """Runs runner.run_task in a background thread, streaming its events out as
    SSE. Shared by /run and /retry since both just differ in how run_kwargs is
    built (fresh instructions vs. a resumed full_task/remaining-or-stack/
    last_error/memory). Tracks STATE["paused"] here (not in runner.py) so
    /retry can pick up the resume payload regardless of which endpoint
    produced it, carrying forward model_key/vision/verify_enabled/fast/
    flavour for whichever step comes next."""
    STATE["cancel"].clear()

    def generate():
        q = queue.Queue()

        def on_event(ev):
            if ev["type"] == "step_failed":
                STATE["paused"] = {**ev["resume"], "model_key": model_key, "vision": vision,
                                    "verify_enabled": verify_enabled, "fast": fast, "flavour": flavour}
            elif ev["type"] in ("task_complete", "cancelled", "halted_captcha", "fatal_error"):
                STATE["paused"] = None
            q.put(ev)

        def worker():
            try:
                runner.run_task(
                    STATE["cdp"], on_event, model_key=model_key, vision=vision,
                    should_stop=STATE["cancel"].is_set, verify_enabled=verify_enabled, fast=fast,
                    flavour=flavour, **run_kwargs,
                )
            except Exception as exc:
                q.put({"type": "fatal_error", "error": str(exc)})
            finally:
                q.put(None)

        threading.Thread(target=worker, daemon=True).start()

        while True:
            ev = q.get()
            if ev is None:
                break
            yield f"data: {json.dumps(ev)}\n\n"

    return StreamingResponse(generate(), media_type="text/event-stream")


@app.post("/run")
async def run(request: Request):
    body = await request.json()
    instructions = [s.strip() for s in body.get("instructions", []) if s.strip()]
    model_key = body.get("model", models.DEFAULT_MODEL)
    vision = bool(body.get("vision"))
    verify_enabled = bool(body.get("verify_enabled", True))
    fast = bool(body.get("fast", True))
    flavour = body.get("flavour", "live")
    return _stream({"instructions": instructions}, model_key, vision, verify_enabled, fast, flavour)


@app.post("/retry")
async def retry():
    """Resumes a paused run at exactly the step that failed, using the resume
    payload /run's on_event captured from that step's step_failed event."""
    paused = STATE["paused"]
    if not paused:
        return JSONResponse({"error": "nothing to retry"}, status_code=400)
    run_kwargs = {k: paused[k] for k in ("full_task", "remaining", "last_error", "start_index", "memory")}
    if "stack" in paused:
        run_kwargs["stack"] = paused["stack"]
    return _stream(run_kwargs, paused["model_key"], paused["vision"], paused["verify_enabled"],
                   paused["fast"], paused.get("flavour", "live"))


@app.post("/reset")
def reset():
    """Aborts any run in progress and replaces the shared browser with a brand
    new instance -- a fresh throwaway profile, not just a blanked-out tab in
    the same one (no leftover cookies, localStorage, service workers, etc)."""
    STATE["cancel"].set()
    STATE["paused"] = None
    try:
        _stop_browser()
        _start_browser()
    except Exception as exc:
        return JSONResponse({"ok": False, "error": str(exc)}, status_code=500)
    return {"ok": True, "cdp": STATE["cdp"]}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=8090)
