import json
import time
import urllib.error
import urllib.request

from flowera_agent import config, state
from flowera_agent.logging_utils import log
from flowera_agent.memory_context import get_memory_context
from flowera_agent.messaging import ask_for_help, write_final_message


# ============================================================
# TheBot harness
#
# Unlike task_runner.py's hand-rolled loop and claude_harness.py's
# CLI-per-task model, TheBot's `soul` binary is a persistent HTTP server
# (started once as a background daemon by start_agent_terminal.sh, not
# shelled out to per task) with its own atomizer/scheduler/skills loop
# inside it. This harness only ever submits a task over HTTP and polls
# for the real job to finish - it does not reimplement any of TheBot's
# reasoning. Exposes the same perform_task/continue_after_help signatures
# so harness.py can dispatch to it interchangeably with the other two.
# ============================================================

APPROVAL_MARKER = "NEEDS_APPROVAL:"

POLL_INTERVAL_SECONDS = 2
TASK_TIMEOUT_SECONDS = 1800

SAFETY_PREAMBLE = f"""
IMPORTANT SAFETY RULE: before making or approving any payment, sending or
approving any outgoing message on the human's behalf (WhatsApp, email,
SMS, or any other platform), or taking any other action with real-world
consequences you are not fully confident about - stop before doing it.
Reply with a message that begins with exactly "{APPROVAL_MARKER}" followed
by what you want to do and why; a human will review it on the Flowera
dashboard.
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


def _run_thebot(prompt):
    """
    Submit one real task to TheBot's /api/chat and poll the real job until
    it finishes.

    Returns (final_answer, meta, error). `error` is a human-readable
    string on failure; exactly one of (final_answer, error) is non-None.
    """

    base = config.THEBOT_URL.rstrip("/")

    try:
        submitted = _post_json(base + "/api/chat", {"prompt": prompt})
    except (urllib.error.URLError, OSError, ValueError) as exc:
        return None, None, f"could not submit task to TheBot: {exc}"

    job_id = submitted.get("id")
    if not job_id:
        return None, None, f"TheBot did not return a job id: {submitted}"

    deadline = time.monotonic() + TASK_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        time.sleep(POLL_INTERVAL_SECONDS)
        try:
            job = _get_json(base + f"/api/jobs/{job_id}")
        except (urllib.error.URLError, OSError, ValueError) as exc:
            return None, None, f"could not poll TheBot job {job_id}: {exc}"

        status = job.get("status")
        if status in ("done", "error"):
            results = job.get("results") or []
            meta = results[0].get("Meta") if results else None
            return job.get("finalAnswer", ""), meta, None

    return None, None, (
        f"TheBot job {job_id} did not finish within "
        f"{TASK_TIMEOUT_SECONDS} seconds"
    )


def _handle_result(final_answer, meta, error):
    if error:
        ask_for_help(
            f"""
TheBot harness failed to run the task.

Error:
{error}
""".strip()
        )
        return

    # Two real, distinct paths to human escalation: TheBot's own
    # structural self-help failure (Meta.needsUserInput, set by its
    # failure-reasoning-then-negative-feedback loop when it genuinely
    # can't proceed on its own - see TheBot's internal/scheduler), and
    # the same NEEDS_APPROVAL text convention claude_harness.py uses for
    # an intentional safety pause.
    if meta and meta.get("needsUserInput"):
        reason = (
            meta.get("failureReasoning")
            or final_answer
            or "TheBot could not resolve this on its own."
        )
        ask_for_help(reason)
        return

    text = (final_answer or "").strip()

    if text.startswith(APPROVAL_MARKER):
        ask_for_help(text[len(APPROVAL_MARKER):].strip())
        return

    write_final_message(text or "(no result text)")


def _build_prompt(task):
    parts = [SAFETY_PREAMBLE]

    memory_context = get_memory_context()
    if memory_context:
        parts.append(
            "Persistent worker memory for this desktop:\n\n" + memory_context
        )

    parts.append(task)
    return "\n\n---\n\n".join(parts)


def perform_task(task, task_file):
    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_help = False

    log("========================================")
    log("STARTING TASK (TheBot harness)")
    log(task)
    log("========================================")

    final_answer, meta, error = _run_thebot(_build_prompt(task))
    _handle_result(final_answer, meta, error)


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

    # TheBot has no session-resume concept (unlike Claude Code's
    # --resume) - the human's response is submitted as a fresh task,
    # carrying the original task as context so it isn't lost.
    prompt = (
        f"Continuing task: {state.current_task}\n\n"
        f"Human response: {help_response}"
    )
    final_answer, meta, error = _run_thebot(_build_prompt(prompt))
    _handle_result(final_answer, meta, error)
