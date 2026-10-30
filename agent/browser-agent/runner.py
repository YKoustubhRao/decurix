"""Runs a list of sub-instructions against the shared browser: snapshot -> state
an expectation -> codegen -> exec -> verify the expectation held -> retry with
the real failure reason (an exception, or a mismatch reported by verify) fed
back if it didn't.

Deliberately linear otherwise -- no per-step ACTION/REASONING classification,
no BeautifulSoup/download sandbox helpers, no early-exit-while-still-running
checks. Those were tried and worked, but added more moving parts than wanted
right now; the code is still in reasoning.py/worker.py to be wired back in
later, just not called from here."""
import json
import subprocess
import sys
import tempfile
from pathlib import Path

import codegen
import models
import planner
import reasoning

HERE = Path(__file__).parent
PYTHON = sys.executable
WORKER = str(HERE / "worker.py")
MAX_RETRIES = 2
STEP_TIMEOUT_S = 60


def run_worker(cdp, mode, code_file=None, vision=False, timeout=STEP_TIMEOUT_S):
    cmd = [PYTHON, WORKER, "--cdp", cdp, "--mode", mode]
    if code_file:
        cmd += ["--code-file", code_file]
    if vision:
        cmd.append("--vision")
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"worker process failed:\n{proc.stderr[-2000:]}")
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _run_code(cdp, code):
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, encoding="utf-8") as f:
        f.write(code)
        path = f.name
    try:
        return run_worker(cdp, "exec", code_file=path)
    finally:
        Path(path).unlink(missing_ok=True)


def _restore(cdp, url, on_event, i):
    """Reverts to the page as it was before this instruction's first attempt.
    Uses domcontentloaded, not the default wait_until="load" -- the same fix
    codegen's own navigations got, but this hardcoded goto had been missing it,
    which could hang/fail on a heavy page and leave the NEXT attempt's snapshot
    reading a stale or mid-navigation page instead of the reverted one."""
    try:
        _run_code(cdp, f'page.goto({url!r}, wait_until="domcontentloaded")')
    except Exception as exc:
        on_event({"type": "restore_failed", "index": i, "error": str(exc)})


def _call_stage(on_event, i, model_key, purpose):
    model_name = models.MODELS[model_key]["name"]
    stage = "swapping_model" if models.will_swap(model_key) else "calling_model"
    on_event({"type": "stage", "index": i, "stage": stage, "model": model_name, "purpose": purpose})


def run_task(cdp, instructions, on_event, model_key=models.DEFAULT_MODEL, vision=False,
             should_stop=lambda: False, skip_planning=False):
    """on_event(dict) fires on every state change so a UI can stream progress live.
    should_stop() is polled between steps (and before/after planning) so a Reset
    button can abort a run already in progress."""
    use_vision = vision and models.MODELS[model_key]["vision"]

    if not skip_planning:
        on_event({"type": "stage", "index": -1, "stage": "planning"})

        def on_plan_chunk(partial):
            on_event({"type": "generating", "index": -1, "partial": partial})

        try:
            expanded = planner.decompose(instructions, on_chunk=on_plan_chunk)
        except Exception as exc:
            on_event({"type": "plan_failed", "error": str(exc)})
            expanded = instructions
        on_event({"type": "plan_ready", "original": instructions, "expanded": expanded})
        instructions = expanded

    if should_stop():
        on_event({"type": "cancelled"})
        return

    for i, instruction in enumerate(instructions):
        if should_stop():
            on_event({"type": "cancelled", "index": i})
            return

        on_event({"type": "step_start", "index": i, "instruction": instruction})

        on_event({"type": "stage", "index": i, "stage": "snapshotting"})
        try:
            start_snap = run_worker(cdp, "snapshot", vision=use_vision)
        except subprocess.TimeoutExpired:
            on_event({"type": "step_failed", "index": i, "error": "snapshot timed out"})
            return
        if start_snap.get("captcha"):
            on_event({"type": "halted_captcha", "index": i, "detail": start_snap["captcha"]})
            return
        start_url = start_snap["url"]

        _call_stage(on_event, i, model_key, "expectation")

        def on_expect_chunk(partial):
            on_event({"type": "generating", "index": i, "phase": "expectation", "partial": partial})

        try:
            expectation = reasoning.expected_outcome(
                model_key, instruction, start_snap["url"], start_snap["html"], on_chunk=on_expect_chunk,
            )
        except Exception as exc:
            expectation = None
            on_event({"type": "expectation_failed", "index": i, "error": str(exc)})
        if expectation:
            on_event({"type": "expectation_set", "index": i, "text": expectation})

        snap = start_snap
        error = None
        for attempt in range(MAX_RETRIES + 1):
            if should_stop():
                on_event({"type": "cancelled", "index": i})
                return

            # Fresh snapshot every attempt after the first -- after a restore (or a
            # failed attempt that partially navigated before erroring) the page may
            # no longer match whatever was read before this loop started, and
            # generating code against a stale snapshot is exactly what turns one
            # failure into a cascade of confusing Playwright errors.
            if attempt > 0:
                on_event({"type": "stage", "index": i, "stage": "snapshotting"})
                try:
                    snap = run_worker(cdp, "snapshot", vision=use_vision)
                except subprocess.TimeoutExpired:
                    pass  # keep the previous attempt's snapshot rather than fail the step over this
                if snap.get("captcha"):
                    on_event({"type": "halted_captcha", "index": i, "detail": snap["captcha"]})
                    return

            _call_stage(on_event, i, model_key, "codegen")

            def on_code_chunk(partial):
                on_event({"type": "generating", "index": i, "phase": "code", "partial": partial})

            try:
                code = codegen.generate_step_code(
                    model_key, instruction, snap["url"], snap["html"], prior_error=error,
                    screenshot_b64=snap.get("screenshot_b64"), on_chunk=on_code_chunk,
                )
            except Exception as exc:
                on_event({"type": "step_failed", "index": i, "error": f"codegen failed: {exc}"})
                return
            on_event({"type": "code_generated", "index": i, "code": code, "attempt": attempt})

            on_event({"type": "stage", "index": i, "stage": "executing"})
            try:
                result = _run_code(cdp, code)
            except subprocess.TimeoutExpired:
                result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}

            if result.get("captcha"):
                on_event({"type": "halted_captcha", "index": i, "detail": result["captcha"]})
                return

            if not result["success"]:
                error = result["error"]
                on_event({"type": "step_error", "index": i, "attempt": attempt, "error": error})
                if attempt < MAX_RETRIES:
                    on_event({"type": "restoring", "index": i, "url": start_url})
                    _restore(cdp, start_url, on_event, i)
                continue

            if not expectation:
                # No expectation to check against (its own call failed) -- accept the
                # step on "ran without error" alone rather than block indefinitely.
                on_event({"type": "step_done", "index": i, "url": result["url"]})
                break

            _call_stage(on_event, i, model_key, "verify")
            try:
                post_snap = run_worker(cdp, "snapshot", vision=use_vision)
            except subprocess.TimeoutExpired:
                post_snap = {"url": result["url"], "html": "(unavailable)", "title": ""}
            try:
                met, reason = reasoning.verify_outcome(
                    model_key, expectation, post_snap.get("url", result["url"]),
                    post_snap.get("html", ""), title=post_snap.get("title", ""),
                    screenshot_b64=post_snap.get("screenshot_b64"),
                )
            except Exception as exc:
                on_event({"type": "verification", "index": i, "met": None,
                          "reason": f"verification call failed: {exc}"})
                on_event({"type": "step_done", "index": i, "url": result["url"]})
                break

            on_event({"type": "verification", "index": i, "met": met, "reason": reason})
            if met:
                on_event({"type": "step_done", "index": i, "url": result["url"]})
                break

            error = f"Expected: {expectation}\nBut: {reason}"
            on_event({"type": "step_error", "index": i, "attempt": attempt, "error": error})
            if attempt < MAX_RETRIES:
                on_event({"type": "restoring", "index": i, "url": start_url})
                _restore(cdp, start_url, on_event, i)
        else:
            on_event({"type": "step_failed", "index": i, "error": error})
            return
