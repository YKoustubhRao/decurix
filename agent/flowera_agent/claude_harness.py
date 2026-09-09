import json
import os
import subprocess

from flowera_agent import config, state
from flowera_agent.logging_utils import log
from flowera_agent.memory_context import get_memory_context
from flowera_agent.messaging import ask_for_help, write_final_message


# ============================================================
# Claude Code harness
#
# Alternative to the Ollama-driven loop in task_runner.py: instead of
# our own COMMAND:/DONE:/HELP: protocol, each task is handed to the
# `claude` CLI in non-interactive mode, which runs its own agentic
# loop (including executing shell commands) to completion and returns
# one final JSON result. Exposes the same perform_task/
# continue_after_help signatures as task_runner.py so harness.py can
# dispatch to either one interchangeably.
# ============================================================

APPROVAL_MARKER = "NEEDS_APPROVAL:"

TASK_TIMEOUT_SECONDS = int(
    os.environ.get("CLAUDE_TASK_TIMEOUT_SECONDS", "3600")
)

MAX_BUDGET_USD = os.environ.get("CLAUDE_MAX_BUDGET_USD", "5")

SAFETY_SYSTEM_PROMPT = f"""
IMPORTANT SAFETY RULE:

Before making or approving any payment or financial transaction,
sending or approving any outgoing message on behalf of the human
(WhatsApp, email, SMS, or any other messaging platform), or taking any
other action with real-world consequences that you are not fully
confident about: stop before performing that action. Do not perform it
yet.

Instead, respond with a message that begins with exactly:

{APPROVAL_MARKER}

followed by a clear description of exactly what you want to do and
why. A human will review this on the Flowera dashboard and respond
with approval or further instructions before you continue.
""".strip()

# This harness's own continuation state: the Claude Code session ID to
# resume when a human response arrives. Not part of flowera_agent.state
# because the Ollama harness has no equivalent concept (it keeps its
# conversation as an in-memory `history` list instead).
_current_session_id = None


def _run_claude(prompt, resume_session_id=None):
    """
    Run one non-interactive Claude Code turn and return (result, error).

    `result` is the parsed --output-format json object on success.
    `error` is a human-readable string on failure. Exactly one of the
    two is non-None.
    """

    cmd = [
        "claude",
        "--bare",
        "-p", prompt,
        "--output-format", "json",
        "--dangerously-skip-permissions",
        "--max-budget-usd", MAX_BUDGET_USD,
        "--append-system-prompt", SAFETY_SYSTEM_PROMPT,
    ]

    memory_context = get_memory_context()
    if memory_context:
        cmd += [
            "--append-system-prompt",
            "Persistent worker memory for this desktop:\n\n" + memory_context,
        ]

    if resume_session_id:
        cmd += ["--resume", resume_session_id]

    log(f"Running Claude Code harness (resume={resume_session_id!r})")

    try:
        process = subprocess.run(
            cmd,
            cwd=str(config.WORK_DIR),
            capture_output=True,
            text=True,
            timeout=TASK_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired:
        return None, (
            f"claude did not finish within {TASK_TIMEOUT_SECONDS} seconds"
        )

    try:
        result = json.loads(process.stdout)
    except (ValueError, TypeError):
        result = None

    if result is None:
        error_text = (
            process.stderr
            or process.stdout
            or f"claude exited with code {process.returncode}"
        ).strip()

        return None, error_text

    return result, None


def _handle_result(result):
    """
    Route a successful Claude Code result to either write_final_message
    or ask_for_help, updating this harness's session-continuation state.
    """

    global _current_session_id

    session_id = result.get("session_id")
    if session_id:
        _current_session_id = session_id

    if result.get("is_error"):
        ask_for_help(
            result.get("result")
            or "Claude Code reported an error but gave no details."
        )
        return

    result_text = (result.get("result") or "").strip()

    if result_text.startswith(APPROVAL_MARKER):
        ask_for_help(result_text[len(APPROVAL_MARKER):].strip())
        return

    write_final_message(result_text or "(no result text)")


def perform_task(task, task_file):
    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_help = False

    log("========================================")
    log("STARTING TASK (Claude Code harness)")
    log(task)
    log("========================================")

    result, error = _run_claude(task)

    if error:
        ask_for_help(
            f"""
Claude Code harness failed to run the task.

Error:
{error}
""".strip()
        )
        return

    _handle_result(result)


def continue_after_help(help_response):
    if not state.current_task:
        log(
            "Received help response but no "
            "active task exists."
        )
        return

    state.waiting_for_help = False

    log("========================================")
    log("RECEIVED HUMAN HELP (Claude Code harness)")
    log(help_response)
    log("========================================")

    result, error = _run_claude(
        help_response,
        resume_session_id=_current_session_id,
    )

    if error:
        ask_for_help(
            f"""
The agent tried to continue after human help,
but the Claude Code harness failed again.

Error:
{error}
""".strip()
        )
        return

    _handle_result(result)
