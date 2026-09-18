"""Runs a task, choosing the next instruction one of two ways (`flavour`),
each of which can execute steps one of two ways (`fast`):

Atomizer flavours:
- flavour="live" (default): the instruction is decided live, one step at a
  time, from the actual current page -- no upfront plan. Can react to
  anything (a cookie banner, an unexpected redirect) simply by deciding
  differently next call, but a decide-call has to happen every single step.
- flavour="stack": stack_atomizer.plan() runs ONCE at the start and decides
  the full ordered list of atomic steps up front, held as a stack (see
  core/stack_atomizer.py). Each step still grounds the CURRENT top-of-stack
  instruction in the live page -- a plan can't know the live DOM -- but no
  longer decides WHAT the next instruction is. It can still react to
  something the plan couldn't have known about via push_before: a new
  instruction inserted ahead of the current one, addressed by THIS turn's
  action, leaving the original instruction on the stack for next turn.

Step execution (orthogonal to flavour, but flavour="stack" always uses the
fast style since there's no stack-aware slow path):
- fast=True (default): ONE structured-output call per step covering what
  atomize+expectation used to do separately, plus a self-check of the last
  action and a memory scratchpad. Simple actions execute deterministically
  via worker.py's "action" mode, no codegen call at all; only action.type
  "complex" falls through to the codegen path below. This is the
  Playwright-side counterpart to ext_server.py's body.fast /
  ext_codegen.decide_and_act.
- fast=False: the original atomize -> (expectation) -> codegen -> exec ->
  (verify) chain, kept as a fallback since it can still be useful to compare
  against or to isolate which stage a regression is in. Only meaningful with
  flavour="live".

On ANY failure (or hitting max_steps) the run PAUSES rather than auto-
retrying -- retry is manual, via server.py's /retry, which resumes exactly
where this left off (same full_task/remaining-or-stack/memory/index), with
the failure reason fed to the next call so it can actually react to it. A
deterministic stagnation guard (mirroring extension/background.js's
repeatKey/repeatCount) also pauses a fast-path run that keeps repeating the
same action with no visible effect, rather than burning the whole step
budget on a stuck loop."""
import json
import subprocess
import sys
import tempfile
from pathlib import Path

import _pathfix  # noqa: F401
import atomizer
import codegen
import fast_step
import models
import reasoning
import stack_atomizer

HERE = Path(__file__).parent
PYTHON = sys.executable
WORKER = str(HERE.parent / "core" / "worker.py")
STEP_TIMEOUT_S = 60
MAX_STEPS = 40


def run_worker(cdp, mode, code_file=None, action_file=None, vision=False, timeout=STEP_TIMEOUT_S):
    cmd = [PYTHON, WORKER, "--cdp", cdp, "--mode", mode]
    if code_file:
        cmd += ["--code-file", code_file]
    if action_file:
        cmd += ["--action-file", action_file]
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


def _run_action(cdp, action):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8") as f:
        json.dump(action, f)
        path = f.name
    try:
        return run_worker(cdp, "action", action_file=path)
    finally:
        Path(path).unlink(missing_ok=True)


def _call_stage(on_event, i, model_key, purpose):
    model_name = models.MODELS[model_key]["name"]
    stage = "swapping_model" if models.will_swap(model_key) else "calling_model"
    on_event({"type": "stage", "index": i, "stage": stage, "model": model_name, "purpose": purpose})


def _pause(on_event, i, error, full_task, remaining, memory="", stack=None):
    """Emits step_failed with everything server.py needs to resume this exact
    point later via /retry -- same task/remaining-or-stack/memory/index, plus
    the failure reason so the next call can actually take it into account."""
    resume = {"full_task": full_task, "remaining": remaining, "last_error": error,
              "start_index": i, "memory": memory}
    if stack is not None:
        resume["stack"] = stack
    on_event({"type": "step_failed", "index": i, "error": error, "resume": resume})


def run_task(cdp, on_event, model_key=models.DEFAULT_MODEL, vision=False,
             should_stop=lambda: False, verify_enabled=True, fast=True, flavour="live",
             max_steps=MAX_STEPS, instructions=None, full_task=None, remaining=None,
             last_error=None, start_index=0, memory="", stack=None):
    """Start fresh with `instructions` (a list of raw lines from the UI), or
    resume a paused run with full_task/remaining/last_error/start_index/
    memory/stack (as captured from that run's own step_failed event).
    max_steps counts steps taken by THIS call, so a resumed run gets a fresh
    budget, same as extension/background.js's per-runLoop-invocation
    MAX_STEPS."""
    if instructions is not None:
        full_task = "\n".join(instructions)
        remaining = full_task
        last_error = None
        memory = ""
        stack = None

    use_vision = vision and models.MODELS[model_key]["vision"]
    i = start_index
    steps_taken = 0
    repeat_key = None
    repeat_count = 0
    last_signature = None

    stack_obj = None
    if flavour == "stack":
        if stack is not None:
            stack_obj = stack_atomizer.Stack.from_list(stack)
        else:
            _call_stage(on_event, i, model_key, "plan")
            try:
                steps = stack_atomizer.plan(model_key, full_task)
            except Exception as exc:
                _pause(on_event, i, f"planning failed: {exc}", full_task, remaining, memory, stack=[])
                return
            stack_obj = stack_atomizer.Stack(steps)
            on_event({"type": "planned", "index": i, "steps": steps})

    while True:
        if flavour == "stack":
            if stack_obj.is_empty():
                on_event({"type": "task_complete"})
                return
        elif atomizer.is_done(remaining):
            on_event({"type": "task_complete"})
            return
        if should_stop():
            on_event({"type": "cancelled", "index": i})
            return
        if steps_taken >= max_steps:
            _pause(on_event, i, "reached the max-steps safety cap", full_task, remaining, memory,
                   stack=stack_obj.to_list() if stack_obj else None)
            return

        on_event({"type": "stage", "index": i, "stage": "snapshotting"})
        try:
            snap = run_worker(cdp, "snapshot", vision=use_vision)
        except subprocess.TimeoutExpired:
            _pause(on_event, i, "snapshot timed out", full_task, remaining, memory,
                   stack=stack_obj.to_list() if stack_obj else None)
            return
        if snap.get("captcha"):
            on_event({"type": "halted_captcha", "index": i, "detail": snap["captcha"]})
            return

        action = None  # a fixed action dict when the direct-execute branch was taken
        push_before = ""

        if flavour == "stack":
            current_instr = stack_obj.peek()
            _call_stage(on_event, i, model_key, "atomize")

            def on_decide_chunk(partial):
                on_event({"type": "generating", "index": i, "phase": "atomize", "partial": partial})

            try:
                action, expectation, push_before, memory, evaluation, raw = fast_step.decide_stack_step(
                    model_key, current_instr, full_task, snap["url"], snap["html"], memory=memory,
                    prior_error=last_error, screenshot_b64=snap.get("screenshot_b64"),
                    on_chunk=on_decide_chunk,
                )
            except Exception as exc:
                _pause(on_event, i, f"step decision failed: {exc}", full_task, remaining, memory,
                       stack=stack_obj.to_list())
                return

            if push_before:
                stack_obj.push_before(push_before)
                on_event({"type": "atomized", "index": i, "instruction": push_before,
                           "remaining": f"(detour) {len(stack_obj.to_list()) - 1} step(s) pending after this"})
            else:
                on_event({"type": "atomized", "index": i, "instruction": current_instr,
                           "remaining": f"{len(stack_obj.to_list()) - 1} step(s) pending after this"})
            if evaluation:
                on_event({"type": "self_check", "index": i, "text": evaluation})

            if action is None:
                # Model signalled the whole task is already done -- ends the
                # run early even if steps are still pending on the stack.
                on_event({"type": "task_complete"})
                return
            if action.get("type") == "answer":
                on_event({"type": "task_complete"})
                return

            if action["type"] == "complex":
                complex_instr = action["instruction"]
                if expectation:
                    on_event({"type": "expectation_set", "index": i, "text": expectation})
                _call_stage(on_event, i, model_key, "codegen")

                def on_code_chunk(partial):
                    on_event({"type": "generating", "index": i, "phase": "code", "partial": partial})

                try:
                    code = codegen.generate_step_code(
                        model_key, complex_instr, snap["url"], snap["html"],
                        screenshot_b64=snap.get("screenshot_b64"), on_chunk=on_code_chunk,
                    )
                except Exception as exc:
                    _pause(on_event, i, f"codegen failed: {exc}", full_task, remaining, memory,
                           stack=stack_obj.to_list())
                    return
                on_event({"type": "code_generated", "index": i, "code": code})

                on_event({"type": "stage", "index": i, "stage": "executing"})
                try:
                    result = _run_code(cdp, code)
                except subprocess.TimeoutExpired:
                    result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}
            else:
                on_event({"type": "stage", "index": i, "stage": "executing"})
                try:
                    result = _run_action(cdp, action)
                except subprocess.TimeoutExpired:
                    result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}

        elif fast:
            _call_stage(on_event, i, model_key, "atomize")

            def on_decide_chunk(partial):
                on_event({"type": "generating", "index": i, "phase": "atomize", "partial": partial})

            try:
                (next_instr, remaining_after, expectation, action,
                 memory, evaluation, raw) = fast_step.decide(
                    model_key, full_task, remaining, snap["url"], snap["html"], memory=memory,
                    prior_error=last_error, screenshot_b64=snap.get("screenshot_b64"),
                    on_chunk=on_decide_chunk,
                )
            except Exception as exc:
                _pause(on_event, i, f"step decision failed: {exc}", full_task, remaining, memory)
                return

            if action is None and atomizer.is_done(remaining_after):
                on_event({"type": "task_complete"})
                return
            if action is not None and action.get("type") == "answer":
                on_event({"type": "atomized", "index": i, "instruction": next_instr or "answer",
                           "remaining": "done"})
                on_event({"type": "task_complete"})
                return
            if not action:
                _pause(on_event, i, f"model returned no usable action: {raw!r}", full_task, remaining, memory)
                return

            on_event({"type": "atomized", "index": i, "instruction": next_instr or remaining,
                       "remaining": remaining_after})
            if evaluation:
                on_event({"type": "self_check", "index": i, "text": evaluation})

            if action["type"] == "complex":
                # No separate atomize call needed here -- fast_step.decide()
                # already produced the instruction and expectation.
                complex_instr = action["instruction"]
                if expectation:
                    on_event({"type": "expectation_set", "index": i, "text": expectation})
                _call_stage(on_event, i, model_key, "codegen")

                def on_code_chunk(partial):
                    on_event({"type": "generating", "index": i, "phase": "code", "partial": partial})

                try:
                    code = codegen.generate_step_code(
                        model_key, complex_instr, snap["url"], snap["html"],
                        screenshot_b64=snap.get("screenshot_b64"), on_chunk=on_code_chunk,
                    )
                except Exception as exc:
                    _pause(on_event, i, f"codegen failed: {exc}", full_task, remaining, memory)
                    return
                on_event({"type": "code_generated", "index": i, "code": code})

                on_event({"type": "stage", "index": i, "stage": "executing"})
                try:
                    result = _run_code(cdp, code)
                except subprocess.TimeoutExpired:
                    result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}
            else:
                on_event({"type": "stage", "index": i, "stage": "executing"})
                try:
                    result = _run_action(cdp, action)
                except subprocess.TimeoutExpired:
                    result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}

        else:
            def on_atomize_chunk(partial):
                on_event({"type": "generating", "index": i, "phase": "atomize", "partial": partial})

            _call_stage(on_event, i, model_key, "atomize")
            try:
                next_instr, remaining_after = atomizer.next_step(
                    model_key, full_task, remaining, snap["url"], snap["html"],
                    prior_error=last_error, screenshot_b64=snap.get("screenshot_b64"),
                    on_chunk=on_atomize_chunk,
                )
            except Exception as exc:
                _pause(on_event, i, f"atomize failed: {exc}", full_task, remaining, memory)
                return
            if not next_instr:
                _pause(on_event, i, "atomizer did not return a next instruction", full_task, remaining, memory)
                return
            on_event({"type": "atomized", "index": i, "instruction": next_instr, "remaining": remaining_after})

            expectation = None
            if verify_enabled:
                _call_stage(on_event, i, model_key, "expectation")

                def on_expect_chunk(partial):
                    on_event({"type": "generating", "index": i, "phase": "expectation", "partial": partial})

                try:
                    expectation = reasoning.expected_outcome(
                        model_key, next_instr, snap["url"], snap["html"], on_chunk=on_expect_chunk,
                    )
                except Exception as exc:
                    on_event({"type": "expectation_failed", "index": i, "error": str(exc)})
                if expectation:
                    on_event({"type": "expectation_set", "index": i, "text": expectation})

            _call_stage(on_event, i, model_key, "codegen")

            def on_code_chunk(partial):
                on_event({"type": "generating", "index": i, "phase": "code", "partial": partial})

            try:
                code = codegen.generate_step_code(
                    model_key, next_instr, snap["url"], snap["html"],
                    screenshot_b64=snap.get("screenshot_b64"), on_chunk=on_code_chunk,
                )
            except Exception as exc:
                _pause(on_event, i, f"codegen failed: {exc}", full_task, remaining, memory)
                return
            on_event({"type": "code_generated", "index": i, "code": code})

            on_event({"type": "stage", "index": i, "stage": "executing"})
            try:
                result = _run_code(cdp, code)
            except subprocess.TimeoutExpired:
                result = {"success": False, "error": "step timed out", "url": snap["url"], "captcha": None}

        if result.get("captcha"):
            on_event({"type": "halted_captcha", "index": i, "detail": result["captcha"]})
            return
        if not result["success"]:
            _pause(on_event, i, result["error"], full_task, remaining, memory,
                   stack=stack_obj.to_list() if stack_obj else None)
            return

        # Deterministic runaway guard for direct-execute actions, mirroring
        # extension/background.js's repeatKey/repeatCount: the model
        # repeatedly re-issuing an identical action that visibly changes
        # nothing is a no-op by definition, so stop on the evidence rather
        # than burning the whole step budget on it. Complex/codegen steps
        # aren't covered -- generated code varies enough per call that an
        # identical-action signature rarely applies there.
        if action is not None and action.get("type") != "complex":
            action_key = json.dumps(action, sort_keys=True)
            signature = f"{snap['url']}|{len(snap.get('html') or '')}"
            page_unchanged = signature == last_signature
            last_signature = signature
            if action_key == repeat_key:
                repeat_count += 1
                if page_unchanged:
                    _pause(on_event, i, "repeated the same action and the page did not change",
                           full_task, remaining, memory, stack=stack_obj.to_list() if stack_obj else None)
                    return
                if repeat_count >= 3:
                    _pause(on_event, i, "the same action repeated 3 times without changing the page",
                           full_task, remaining, memory, stack=stack_obj.to_list() if stack_obj else None)
                    return
            else:
                repeat_key = action_key
                repeat_count = 1
        else:
            repeat_key = None
            repeat_count = 0

        if verify_enabled and expectation:
            _call_stage(on_event, i, model_key, "verify")
            try:
                post_snap = run_worker(cdp, "snapshot", vision=use_vision)
            except subprocess.TimeoutExpired:
                post_snap = {"url": result["url"], "html": "(unavailable)", "title": ""}

            met = True
            try:
                met, reason = reasoning.verify_outcome(
                    model_key, expectation, post_snap.get("url", result["url"]),
                    post_snap.get("html", ""), title=post_snap.get("title", ""),
                    screenshot_b64=post_snap.get("screenshot_b64"),
                )
                on_event({"type": "verification", "index": i, "met": met, "reason": reason})
            except Exception as exc:
                on_event({"type": "verification", "index": i, "met": None,
                          "reason": f"verification call failed: {exc}"})
                reason = None  # can't verify -- don't block progress on a broken verify call itself

            if not met:
                _pause(on_event, i, f"Expected: {expectation}\nBut: {reason}", full_task, remaining, memory,
                       stack=stack_obj.to_list() if stack_obj else None)
                return

        on_event({"type": "step_done", "index": i, "url": result["url"]})
        if flavour == "stack":
            # Whichever item is on top right now -- the just-pushed detour,
            # or (if no push_before this turn) the original current_instr --
            # is the one this turn's action+verify just satisfied.
            stack_obj.pop()
        else:
            remaining = remaining_after
        last_error = None
        i += 1
        steps_taken += 1
