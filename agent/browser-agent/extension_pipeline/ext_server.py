"""Extension-facing server: the same atomize -> (expectation) -> act -> verify
pipeline as runner.py/server.py, except "act" targets a live Chrome tab's real
DOM (via the browser-agent Chrome extension's own tab capture + content-script
execution) instead of a Playwright page owned by browser_host.py.

Reuses atomizer.py, reasoning.py and models.py completely unmodified -- they
were already pure functions over (model_key, text, url, html, screenshot_b64)
with no Playwright dependency. worker.get_full_html's HTML-cleaning is also
reused unmodified, via a tiny duck-typed shim below (it only ever calls
page.content() on the object it's given). Only codegen.py's role -- turning
one instruction into something executable -- has no direct equivalent, since
a generated Playwright snippet can't run in a content script; ext_codegen.py
covers that instead with a small grounded-action grammar.

Runs as its own standalone process on its own port. It never imports or
touches server.py/runner.py/worker.py's browser-owning state -- browser_host.py
and the shared Playwright Chromium don't need to be running at all for this to
work, since the Chrome extension supplies its own page snapshots directly from
the user's real tab.
"""
import re
import threading
import time
from typing import Any, Optional

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel

import _pathfix  # noqa: F401
import atomizer
import ext_codegen
import models
import reasoning
import stack_atomizer
import stats as stats_mod
import worker

app = FastAPI()
app.add_middleware(
    CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"],
)

# ---- request inbox -------------------------------------------------------
# The extension runs a listener that pulls from here, so anything at all --
# a script, a CLI, another app -- can hand the agent work by POSTing to
# /requests, and it gets carried out in the user's OWN Chrome with their
# real sessions. Two kinds of request:
#   instruction: a natural-language task for the agent loop
#   program:     replay a saved recording by name, no model calls at all
# In-memory on purpose: an inbox that outlived a server restart would just
# be a confusing surprise.
REQUESTS: list[dict[str, Any]] = []
REQUESTS_LOCK = threading.Lock()


class RequestBody(BaseModel):
    kind: str = "instruction"  # "instruction" | "program"
    task: Optional[str] = None
    program: Optional[str] = None
    start_url: Optional[str] = None
    id: Optional[str] = None
    meta: Optional[dict] = None


class RequestsBody(BaseModel):
    requests: list[RequestBody]
    replace: bool = False


class RequestResultBody(BaseModel):
    id: str
    status: str
    answer: Optional[str] = None
    # Each step dict carries its own "state" (url/title/screenshot_b64/
    # visible_text/html -- see background.js's stateOf()), captured from the
    # live tab regardless of whether vision mode fed a screenshot to the
    # model -- this is the returned record, not a model input.
    steps: Optional[list] = None
    log: Optional[list] = None
    error: Optional[str] = None
    final_url: Optional[str] = None
    final_state: Optional[dict] = None
    check_passed: Optional[bool] = None
    elapsed_s: Optional[float] = None


def _add_request(r: RequestBody):
    rid = r.id or f"r{int(time.time() * 1000)}{len(REQUESTS)}"
    REQUESTS.append({
        **r.model_dump(), "id": rid, "status": "pending", "result": None,
        "live_steps": [], "cancel_requested": False,
    })
    return rid


@app.post("/requests/{request_id}/cancel")
def cancel_request(request_id: str):
    """Cancels a request from OUTSIDE the extension -- the caller (TheBot's
    browser skill, agent/thebot/internal/skills/browser.go) has no way to
    reach into the browser itself, so this just raises a flag the extension
    checks on its own, whether or not the request has actually started yet:

    - still "pending" (queued, never claimed by /requests/next): finished
      right here, with no browser involved at all, since nothing is running
      it yet.
    - "running": left alone here. background.js's own cancellation poll
      (GET .../cancelled) is what actually stops it, mid-step if need be,
      and files the real /requests/result once it has -- whatever it saw up
      to that point, not a synthesized "cancelled" with nothing in it.
    - already "done": a no-op. Nothing to stop, and the real result -
      succeeded, failed, or already reported as cancelled - stands as-is."""
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if str(r.get("id")) != str(request_id):
                continue
            r["cancel_requested"] = True
            if r["status"] == "pending":
                r["status"] = "done"
                r["result"] = {
                    "id": request_id, "status": "cancelled", "answer": None, "steps": [],
                    "log": ["Cancelled before it started."], "error": "Cancelled before it started.",
                    "final_url": None, "check_passed": None, "elapsed_s": 0.0,
                }
            return {"ok": True, "status": r["status"]}
    return {"ok": False, "error": "unknown request id"}


@app.get("/requests/{request_id}/cancelled")
def is_cancelled(request_id: str):
    """Polled by background.js while a request is running (see
    runListenerLoop) -- deliberately its own tiny endpoint rather than
    folded into /requests/status, which returns every request's full
    state (screenshots included) and would be wasteful to poll every
    second just to read one boolean."""
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if str(r.get("id")) == str(request_id):
                return {"cancelled": bool(r.get("cancel_requested"))}
    return {"cancelled": False}


# Bounds how many in-flight step snapshots (each with a screenshot + HTML)
# a single request holds in memory at once -- a remote viewer polling
# /requests/{id}/live only ever needs recent steps, and an unbounded list
# would grow without limit for a long-running task.
MAX_LIVE_STEPS = 30


class LiveStepBody(BaseModel):
    index: int
    instruction: Optional[str] = None
    action: Optional[dict] = None
    result: Optional[str] = None
    error: Optional[str] = None
    verify: Optional[dict] = None
    state: Optional[dict] = None


@app.post("/requests")
def submit_request(body: RequestBody):
    with REQUESTS_LOCK:
        return {"id": _add_request(body)}


@app.post("/requests/bulk")
def submit_requests(body: RequestsBody):
    with REQUESTS_LOCK:
        if body.replace:
            REQUESTS.clear()
        return {"ids": [_add_request(r) for r in body.requests]}


@app.get("/requests/next")
def next_request():
    """Long-poll-free: the listener asks on an interval and gets null when
    there's nothing waiting, which keeps the extension side trivial."""
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if r["status"] == "pending":
                r["status"] = "running"
                r["started_at"] = time.time()
                return {"request": {k: r.get(k) for k in
                                    ("id", "kind", "task", "program", "start_url", "meta")}}
        return {"request": None}


@app.post("/requests/result")
def request_result(body: RequestResultBody):
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if str(r.get("id")) == str(body.id):
                r["status"] = "done"
                r["result"] = body.model_dump()
                return {"ok": True}
    return {"ok": False, "error": "unknown request id"}


@app.post("/requests/{request_id}/step")
def push_live_step(request_id: str, body: LiveStepBody):
    """Pushed by the extension immediately after EACH step (screenshot +
    HTML included, via background.js's stateOf()) -- not batched into the
    final /requests/result -- so a remote client watching this specific
    request's progress (see /requests/{id}/live) sees it as it happens, for
    someone who isn't the one driving the browser to still have visibility
    into what's happening turn by turn."""
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if str(r.get("id")) == str(request_id):
                live = r.setdefault("live_steps", [])
                live.append(body.model_dump())
                del live[:-MAX_LIVE_STEPS]
                return {"ok": True}
    return {"ok": False, "error": "unknown request id"}


@app.get("/requests/{request_id}/live")
def live_steps(request_id: str, since: int = -1):
    """Polled by a remote viewer. `since` is the highest step index already
    seen -- pass the last index you got back to fetch only what's new,
    instead of re-fetching every screenshot on every poll."""
    with REQUESTS_LOCK:
        for r in REQUESTS:
            if str(r.get("id")) == str(request_id):
                steps = [s for s in r.get("live_steps", []) if s["index"] > since]
                return {"status": r["status"], "steps": steps}
    return {"status": None, "steps": [], "error": "unknown request id"}


@app.get("/requests/status")
def requests_status():
    with REQUESTS_LOCK:
        counts = {"pending": 0, "running": 0, "done": 0}
        for r in REQUESTS:
            counts[r["status"]] = counts.get(r["status"], 0) + 1
        return {
            "counts": counts,
            "requests": [
                {"id": r.get("id"), "kind": r.get("kind"), "task": r.get("task"),
                 "program": r.get("program"), "meta": r.get("meta"),
                 "status": r["status"], "result": r.get("result")}
                for r in REQUESTS
            ],
        }


@app.post("/requests/clear")
def clear_requests():
    with REQUESTS_LOCK:
        REQUESTS.clear()
        return {"ok": True}


class _RawHtmlPage:
    """Duck-typed stand-in for a Playwright `page` -- just enough surface
    (a .content() method) for worker.get_full_html to run its BeautifulSoup
    cleaning on HTML that came from the extension's own tab instead of a
    real page.content() call."""

    def __init__(self, html):
        self._html = html

    def content(self):
        return self._html


def _clean(raw_html, live_fields=None):
    """Same worker.get_full_html cleaning, plus the same LIVE FORM FIELD
    VALUES text block worker.py's own snapshot mode appends afterwards (see
    worker.py's main(), snapshot branch) -- outerHTML is the DOM's original
    markup and never reflects a value set later via typing/JS, so without
    this a just-typed field still reads as empty to verify_outcome. worker.py
    gets live_fields via Playwright's eval_on_selector_all; here the content
    script already collected them directly, so this only needs the same
    text formatting, not the same code path."""
    html = worker.get_full_html(_RawHtmlPage(raw_html))
    if live_fields:
        lines = [f"{f['tag']}#{f['id'] or f['name']}: {f['value']!r}" for f in live_fields]
        html += (
            "\n\n--- LIVE FORM FIELD VALUES (not visible in the HTML above -- browsers "
            "never reflect a value set via .fill()/typing in outerHTML) ---\n" + "\n".join(lines)
        )
    return html


def _detect_captcha_text(title, raw_html):
    """Text-only reuse of worker.detect_captcha's signals (its own constants,
    imported not copied) -- no iframe/is_visible() check here since there's no
    live Playwright page to query, just the title and raw HTML the extension sent."""
    title_l = (title or "").lower()
    for t in worker.CAPTCHA_TITLES:
        if t in title_l:
            return f"page title suggests a challenge page: {title_l!r}"
    text = re.sub(r"<[^>]+>", " ", raw_html or "").lower()
    for pat in worker.CAPTCHA_TEXT_PATTERNS:
        if re.search(pat, text):
            return f"page text matched captcha pattern: {pat!r}"
    return None


class StepBody(BaseModel):
    full_task: str
    remaining: str
    url: str
    raw_html: str
    title: str = ""
    model_key: str = models.DEFAULT_MODEL
    screenshot_b64: Optional[str] = None
    last_error: Optional[str] = None
    verify_enabled: bool = True
    live_fields: Optional[list] = None
    visible_text: Optional[str] = None
    fast: bool = True
    memory: str = ""
    flavour: str = "live"
    stack: Optional[list] = None


class VerifyBody(BaseModel):
    expectation: str
    url: str
    raw_html: str
    title: str = ""
    model_key: str = models.DEFAULT_MODEL
    screenshot_b64: Optional[str] = None
    live_fields: Optional[list] = None


class AskBody(BaseModel):
    question: str
    url: str
    raw_html: str
    model_key: str = models.DEFAULT_MODEL
    screenshot_b64: Optional[str] = None


@app.get("/models")
def list_models():
    return {key: {"label": cfg["label"], "vision": cfg["vision"]} for key, cfg in models.MODELS.items()}


@app.get("/stats")
def get_stats():
    """Same VRAM/RAM/loaded-models/last-request snapshot as server.py's /stats
    -- stats.py has no Playwright dependency at all, so it's reused as-is."""
    return stats_mod.snapshot()


@app.get("/will_swap")
def will_swap(model_key: str = models.DEFAULT_MODEL):
    """Lets the extension warn "loading model into VRAM, can take 10-30s"
    BEFORE making the (possibly slow) call, the same way server.py's SSE
    stream emits a "swapping_model" stage ahead of time -- checked here since
    models.will_swap reflects state at call time, and /step's own response
    only arrives after the call already happened."""
    return {"swap": models.will_swap(model_key)}


def _step_stack(body, html):
    """flavour="stack" branch of /step: plans once (when body.stack is None --
    the sentinel for "not planned yet", distinct from [] meaning "plan
    exhausted"), then executes the current top-of-stack instruction. Loops
    internally (bounded) over action.type "skip" -- a step already satisfied
    by the current page needs no browser round-trip, so resolving it here
    saves the client a whole extra /step call, mirroring how
    playwright_pipeline/runner.py's stack branch handles skip with an inline
    `continue` in its own loop.

    Stack-pop contract: the returned `stack` still has the current target
    (next_instruction, i.e. the pushed detour if any, else stack[0]) at the
    front -- NOT yet popped, since the caller hasn't executed/verified the
    action yet. The caller pops exactly one item (stack.slice(1) in JS) only
    after that action succeeds (and verify, if enabled, passes), then sends
    that trimmed stack on the next call. This mirrors how `remaining` is
    only advanced after a successful step in the non-stack flavour."""
    stack = body.stack
    plan_steps = None
    if stack is None:
        try:
            stack = stack_atomizer.plan(body.model_key, body.full_task)
        except Exception as exc:
            return {"error": f"planning failed: {exc}"}
        plan_steps = stack

    memory = body.memory
    for _ in range(10):  # bounded: a model stuck emitting "skip" forever must not hang the request
        if not stack:
            resp = {"done": True, "memory": memory, "stack": stack}
            if plan_steps is not None:
                resp["planned"] = plan_steps
            return resp

        current_instr = stack[0]
        try:
            action, expectation, push_before, memory, evaluation, raw = ext_codegen.decide_stack_step(
                body.model_key, current_instr, body.full_task, body.url, html,
                memory=memory, prior_error=body.last_error, screenshot_b64=body.screenshot_b64,
            )
        except Exception as exc:
            return {"error": f"step decision failed: {exc}"}

        if push_before:
            stack = [push_before, *stack]
        if action is None:
            resp = {"done": True, "memory": memory, "evaluation_previous_goal": evaluation, "stack": []}
            if plan_steps is not None:
                resp["planned"] = plan_steps
            return resp
        if action.get("type") == "answer":
            resp = {"done": True, "answer": action.get("text", ""), "memory": memory,
                    "evaluation_previous_goal": evaluation, "stack": stack}
            if plan_steps is not None:
                resp["planned"] = plan_steps
            return resp
        if action.get("type") == "skip":
            stack = stack[1:]
            continue

        resp = {
            "done": False, "captcha": None,
            "next_instruction": push_before or current_instr,
            "remaining": f"{len(stack) - 1} step(s) pending after this",
            "expectation": expectation if body.verify_enabled else None,
            "action": action,
            "memory": memory,
            "evaluation_previous_goal": evaluation,
            "stack": stack,
        }
        if plan_steps is not None:
            resp["planned"] = plan_steps
        return resp

    return {"error": "too many consecutive skipped steps -- stopping to avoid a runaway loop"}


@app.post("/step")
def step(body: StepBody):
    # Captcha check FIRST. A blocked page is the truth about where the
    # browser is, regardless of what the model claimed was left to do --
    # checking is_done first let a Google /sorry gate be reported as a clean
    # "done" because the model had written REMAINING: done.
    captcha = _detect_captcha_text(body.title, body.raw_html)
    if captcha:
        return {"done": False, "captcha": captcha}

    html = _clean(body.raw_html, body.live_fields)
    if body.visible_text:
        # Put the rendered prose FIRST. A big article cleans to ~100k chars
        # of markup and the fact being asked for sits buried in the middle of
        # it; the same fact is a few hundred characters into the visible
        # text, where a small model can actually find it.
        html = (
            "--- VISIBLE TEXT ON THE PAGE (read facts from here) ---\n"
            f"{body.visible_text}\n--- END VISIBLE TEXT ---\n\n" + html
        )

    if body.flavour == "stack":
        return _step_stack(body, html)

    if atomizer.is_done(body.remaining):
        return {"done": True}

    if body.fast:
        # One call decides the step, what's left, the expectation AND the
        # action (plus a self-judgment of the PREVIOUS action and a memory
        # scratchpad carried across steps). The three-call path below re-sent
        # the whole page HTML for each, which measured ~25-30s apiece on real
        # pages -- ~100s/step, enough that a two-step search task couldn't
        # finish in two minutes. Grammar-constrained JSON (STEP_SCHEMA) means
        # a malformed response is no longer possible, so there's no more
        # "could not parse an action" case to report here.
        try:
            (next_instr, remaining_after, expectation, action,
             memory, evaluation, raw_action) = ext_codegen.decide_and_act(
                body.model_key, body.full_task, body.remaining, body.url, html,
                memory=body.memory, prior_error=body.last_error, screenshot_b64=body.screenshot_b64,
            )
        except Exception as exc:
            return {"error": f"step decision failed: {exc}"}
        if action is None and atomizer.is_done(remaining_after):
            return {"done": True, "memory": memory, "evaluation_previous_goal": evaluation}
        if action is not None and action.get("type") == "answer":
            # A question, answered off the page -- the task is finished and
            # the answer is the result. Nothing to execute in the browser.
            return {"done": True, "answer": action.get("text", ""), "memory": memory,
                    "evaluation_previous_goal": evaluation}
        if not action:
            return {"error": f"model returned no usable action: {raw_action!r}"}
        return {
            "done": False, "captcha": None,
            "next_instruction": next_instr or body.remaining,
            "remaining": remaining_after,
            "expectation": expectation if body.verify_enabled else None,
            "action": action,
            "memory": memory,
            "evaluation_previous_goal": evaluation,
        }

    try:
        next_instr, remaining_after = atomizer.next_step(
            body.model_key, body.full_task, body.remaining, body.url, html,
            prior_error=body.last_error, screenshot_b64=body.screenshot_b64,
        )
    except Exception as exc:
        return {"error": f"atomize failed: {exc}"}
    if not next_instr:
        return {"error": "atomizer did not return a next instruction"}

    expectation = None
    if body.verify_enabled:
        try:
            expectation = reasoning.expected_outcome(body.model_key, next_instr, body.url, html)
        except Exception:
            expectation = None

    try:
        action, raw_action = ext_codegen.generate_step_action(
            body.model_key, next_instr, body.url, html,
            prior_error=body.last_error, screenshot_b64=body.screenshot_b64,
        )
    except Exception as exc:
        return {"error": f"action generation failed: {exc}"}
    if not action:
        return {"error": f"could not parse an action from: {raw_action!r}"}

    return {
        "done": False, "captcha": None, "next_instruction": next_instr,
        "remaining": remaining_after, "expectation": expectation, "action": action,
    }


@app.post("/verify")
def verify(body: VerifyBody):
    html = _clean(body.raw_html, body.live_fields)
    met, reason = reasoning.verify_outcome(
        body.model_key, body.expectation, body.url, html,
        title=body.title, screenshot_b64=body.screenshot_b64,
    )
    return {"met": met, "reason": reason}


@app.post("/ask")
def ask(body: AskBody):
    html = _clean(body.raw_html)
    answer = reasoning.answer_from_page(
        body.model_key, body.question, body.url, html,
        screenshot_b64=body.screenshot_b64, max_tokens=300,
    )
    return {"answer": answer}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=8092)
