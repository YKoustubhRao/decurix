import json
import time
import urllib.error
import urllib.request

from flowera_agent import config, progress, state
from flowera_agent.identity import get_identity_context
from flowera_agent.logging_utils import log
from flowera_agent.memory_context import get_memory_context
from flowera_agent.messaging import (
    ask_for_help,
    backend_failure_note,
    write_final_message,
)
from flowera_agent.readiness import wait_for_http_ok


# ============================================================
# TheBot harness
#
# Unlike task_runner.py's hand-rolled loop and claude_harness.py's
# CLI-per-task model, TheBot's `soul` binary is a persistent HTTP server
# (started once as a background daemon by start_agent_terminal.sh, not
# shelled out to per task) with its own atomizer/scheduler/skills loop
# inside it. This harness only ever submits a message over HTTP and follows
# the real job to completion - it does not reimplement any of TheBot's
# reasoning. Exposes the same perform_task/continue_after_help signatures
# so harness.py can dispatch to it interchangeably with the other two.
#
# The job is followed over soul's own SSE stream rather than polled, so
# every reasoning step it emits reaches the Flowera chat live through
# progress.emit. Polling remains the fallback for a stream that drops
# before the job reports a final event.
#
# soul reports each job's outcome as a structured field - answered,
# needs_approval, needs_input or failed - and keeps a paused job's
# checkpoint per session. So a human's reply to a blocked desktop is just
# the next message in the same session: soul resumes the paused job where
# it stopped (running the approved command, or handing the operator the
# answer it asked for) instead of starting the task over.
# ============================================================

APPROVAL_MARKER = "NEEDS_APPROVAL:"

POLL_INTERVAL_SECONDS = 2

# soul bounds a job at 25 minutes itself (THEBOT_JOB_TIMEOUT_SECONDS) and
# reports what it finished when that runs out; this is only the backstop
# for a soul that stopped responding altogether.
TASK_TIMEOUT_SECONDS = 1800

SAFETY_PREAMBLE = f"""
Approval rules for this desktop: before making or approving any payment,
sending or approving any outgoing message on the human's behalf (WhatsApp,
email, SMS, or any other platform), publishing anything, or taking any
other action with real-world consequences you are not fully confident
about - stop before doing it. Ask for approval with HELP, or begin your
reply with exactly "{APPROVAL_MARKER}" followed by what you want to do and
why; a human will review it on the Flowera dashboard.
""".strip()


def _post_json(url, payload):
    body = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.loads(response.read().decode("utf-8"))


def _get_json(url):
    request = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.loads(response.read().decode("utf-8"))


def _stream_job(base, job_id):
    """
    Follow one job over soul's SSE stream, reporting each log line as a
    live step, and return the final job snapshot.

    Returns None if the stream ended without ever delivering a final
    event, which leaves the caller to fall back to polling rather than
    treating a dropped connection as a failed task - the job itself keeps
    running inside soul either way.
    """

    url = base + f"/api/jobs/{job_id}/stream"

    request = urllib.request.Request(url, method="GET")

    deadline = time.monotonic() + TASK_TIMEOUT_SECONDS

    # The read timeout is the whole task budget on purpose: soul sends no
    # keep-alive frames, so a genuinely long-running step looks exactly
    # like an idle connection from here.
    with urllib.request.urlopen(request, timeout=TASK_TIMEOUT_SECONDS) as response:
        for raw in response:
            if time.monotonic() > deadline:
                return None

            line = raw.decode("utf-8", "replace").strip()

            if not line.startswith("data:"):
                continue

            try:
                event = json.loads(line[len("data:"):].strip())
            except ValueError:
                continue

            if event.get("type") == "log":
                progress.emit(event.get("line") or "")

            elif event.get("type") == "final":
                return event.get("job") or {}

    return None


def _poll_job(base, job_id):
    """
    Wait for a job to finish by polling, for when the stream is unusable.
    """

    deadline = time.monotonic() + TASK_TIMEOUT_SECONDS

    while time.monotonic() < deadline:
        time.sleep(POLL_INTERVAL_SECONDS)

        try:
            job = _get_json(base + f"/api/jobs/{job_id}")
        except (urllib.error.URLError, OSError, ValueError) as exc:
            log(f"Could not poll TheBot job {job_id}: {exc}")
            return None

        if job.get("status") in ("done", "error"):
            return job

    return None


def _cancel_job(base, job_id):
    """
    Stop a job this harness has given up waiting for, so it does not keep
    running commands on the desktop after the human was told it failed.
    """

    try:
        _post_json(base + f"/api/jobs/{job_id}/cancel", {})
    except (urllib.error.URLError, OSError, ValueError) as exc:
        log(f"Could not cancel TheBot job {job_id}: {exc}")


def _build_context():
    """
    The standing context soul folds into every model call's system frame:
    who this desktop is, its persistent worker memory, and the approval
    rules it runs under.

    Sent as its own field, never glued onto the task. It used to be glued
    on - task first, then identity, memory and the safety rules - and soul
    classified, planned and remembered that whole blob as the human's
    request. "hello, whats your name?" took 148 seconds and came back as
    "NEEDS_APPROVAL: Before sending any real-world consequences..." - an
    answer to the safety rule, not to the question.
    """

    parts = [get_identity_context("TheBot (soul)")]

    memory_context = get_memory_context()
    if memory_context:
        parts.append(
            "Persistent worker memory for this desktop:\n\n" + memory_context
        )

    parts.append(SAFETY_PREAMBLE)

    return "\n\n".join(parts)


def _run_thebot(message, reply):
    """
    Submit one message to TheBot's /api/chat and follow the job until it
    finishes.

    `reply` says whether the message answers the job this desktop's
    session has paused: True resumes it, False tells soul to drop it and
    treat the message as a new request.

    Returns (job, error): the final job snapshot, or a human-readable
    error string when there is no job to report on.
    """

    base = config.THEBOT_URL.rstrip("/")

    try:
        submitted = _post_json(
            base + "/api/chat",
            {
                "prompt": message,
                "context": _build_context(),
                "workdir": str(config.WORK_DIR),
                # One session per desktop, for the whole life of the
                # container: soul keeps the conversation's history and the
                # paused job (if any) under it.
                "sessionId": config.DESKTOP_ID or "flowera-desktop",
                "reply": reply,
            },
        )
    except (urllib.error.URLError, OSError, ValueError) as exc:
        return None, f"could not submit task to TheBot: {exc}"

    job_id = submitted.get("id")
    if not job_id:
        return None, f"TheBot did not return a job id: {submitted}"

    try:
        job = _stream_job(base, job_id)
    except (urllib.error.URLError, OSError, ValueError) as exc:
        log(f"TheBot job stream for {job_id} failed, polling instead: {exc}")
        job = None

    if job is None:
        job = _poll_job(base, job_id)

    if job is None:
        _cancel_job(base, job_id)
        return None, (
            f"TheBot job {job_id} did not finish within "
            f"{TASK_TIMEOUT_SECONDS} seconds"
        )

    return job, None


# How soul words a failure that was really its Ollama backend giving up:
# "could not reason about the failure (ollama: request failed: Post
# ".../api/chat": context deadline exceeded) - stopping here". That is a
# true sentence about a slow machine, and it reaches a human as the entire
# explanation of why their desktop stopped.
# soul says this, and only this, when its own reasoning call errored out -
# reasonAboutFailure returns it exclusively on a non-nil error from the
# provider, so the phrase alone is conclusive.
_BACKEND_FAILURE_PHRASES = (
    "could not reason about the failure",
    "could not reason about the task",
    "the reasoning model failed",
)

# Otherwise a provider error has to look like one: the provider named, and
# a transport failure alongside it. Matching a bare "request failed" or
# "connection refused" would relabel a task's own failed HTTP call - a web
# fetch soul made on purpose - as the reasoning model being down, and send
# the human a note about tuning OLLAMA_MODEL for something unrelated.
_BACKEND_FAILURE_SHAPES = (
    "deadline exceeded",
    "request failed",
    "connection refused",
    "no such host",
    "timed out",
    "returned http",
)


def _looks_like_backend_failure(text):
    lowered = (text or "").lower()

    if any(phrase in lowered for phrase in _BACKEND_FAILURE_PHRASES):
        return True

    if "ollama" not in lowered:
        return False

    return any(shape in lowered for shape in _BACKEND_FAILURE_SHAPES)


def _backend_note(reason):
    return backend_failure_note(
        f"TheBot's reasoning model ({config.OLLAMA_MODEL})",
        config.OLLAMA_URL,
        reason,
    )


def _escalate(reason):
    """
    Send one reason to the human, rewritten if it is really about the
    backend rather than about the task.
    """

    if _looks_like_backend_failure(reason):
        ask_for_help(_backend_note(reason))
        return

    ask_for_help(reason)


def _handle_job(job, error):
    if error:
        _escalate(error)
        return

    outcome = job.get("outcome") or ""
    answer = (job.get("finalAnswer") or "").strip()
    question = (job.get("question") or "").strip() or answer

    if outcome == "needs_approval":
        # soul's own safety gate stopped a consequential action, and holds
        # the job paused. The human's reply comes back through
        # continue_after_help and resumes it.
        ask_for_help(question)
        return

    if outcome == "needs_input":
        _escalate(question)
        return

    if outcome == "failed":
        if _looks_like_backend_failure(answer):
            ask_for_help(_backend_note(answer))
            return
        write_final_message(answer or "TheBot could not finish this.", kind="error")
        return

    # Answered - or a soul too old to report an outcome, whose only signal
    # was a flag on its first result.
    results = job.get("results") or []
    meta = (results[0].get("Meta") if results else None) or {}
    if not outcome and meta.get("needsUserInput"):
        _escalate(meta.get("failureReasoning") or answer or "TheBot could not resolve this on its own.")
        return

    # The model can still pause on its own, in words, with the marker the
    # approval rules above ask for.
    if answer.startswith(APPROVAL_MARKER):
        ask_for_help(answer[len(APPROVAL_MARKER):].strip())
        return

    write_final_message(answer or "(no result text)")


def wait_for_backend():
    """
    Wait for both halves of this harness's backend.

    soul is a local daemon that takes a moment to bind :4900, and it
    reasons with the same Ollama server every desktop is handed. Waiting
    for both is what lets watchers.py queue an early message instead of
    dispatching it into "could not reason about the failure ...
    connection refused" and turning a backend that was still starting into
    a help request for a human.
    """

    wait_for_http_ok(
        config.OLLAMA_URL.rstrip("/") + "/api/tags",
        f"Ollama at {config.OLLAMA_URL}",
    )

    wait_for_http_ok(
        config.THEBOT_URL.rstrip("/") + "/api/health",
        f"TheBot soul at {config.THEBOT_URL}",
    )


def perform_task(task, task_file):
    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_help = False

    log("========================================")
    log("STARTING TASK (TheBot harness)")
    log(task)
    log("========================================")

    # A new task, not a reply: if soul still holds a paused job for this
    # desktop (the human moved on without answering it), it is dropped.
    job, error = _run_thebot(task, reply=False)
    _handle_job(job, error)


def continue_after_help(help_response):
    if not state.current_task:
        log(
            "Received help response but no "
            "active task exists."
        )
        return

    state.waiting_for_help = False

    log("========================================")
    log("RECEIVED HUMAN HELP (TheBot harness)")
    log(help_response)
    log("========================================")

    # soul resumes the job it paused for this session, with the reply as
    # the answer - approving the exact command it stopped before, or
    # handing it to the operator that asked. No need to restate the task:
    # the checkpoint still has it.
    job, error = _run_thebot(help_response, reply=True)
    _handle_job(job, error)
