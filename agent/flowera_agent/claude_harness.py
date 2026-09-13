import json
import os
import subprocess
import threading

from flowera_agent import config, progress, state
from flowera_agent.logging_utils import log
from flowera_agent.identity import get_identity_context
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
#
# The CLI is run with --output-format stream-json rather than json, so
# the turn is readable while it happens: Claude Code emits one JSON
# object per line as it thinks and calls tools, and those become the
# chat's live step view through progress.emit. The final "result" event
# carries exactly the same fields the single-shot json format returned,
# so everything downstream of _run_claude is unchanged.
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

# Tool inputs are arbitrary JSON; these are the keys worth showing in a
# one-line step, in the order that best identifies what the tool is doing
# (a Bash command, the file being edited, the thing being searched for).
TOOL_SUMMARY_KEYS = (
    "command",
    "file_path",
    "path",
    "pattern",
    "url",
    "description",
    "prompt",
)

# This harness's own continuation state: the Claude Code session ID to
# resume when a human response arrives. Not part of flowera_agent.state
# because the Ollama harness has no equivalent concept (it keeps its
# conversation as an in-memory `history` list instead).
_current_session_id = None


def _first_line(text):
    for line in text.strip().splitlines():
        line = line.strip()

        if line:
            return line

    return ""


def _describe_tool(block):
    """
    Summarize one tool call as a single step line.
    """

    name = block.get("name") or "tool"
    params = block.get("input") or {}

    if isinstance(params, dict):
        for key in TOOL_SUMMARY_KEYS:
            value = params.get(key)

            if isinstance(value, str) and value.strip():
                return f"{name}: {_first_line(value)}"

    return name


def _emit_event(event):
    """
    Turn one stream-json event into the chat's live step view.

    Only assistant turns say anything a human wants to watch; the rest of
    the stream (init banners, tool results echoed back into the model) is
    noise at this altitude and is deliberately dropped.
    """

    if event.get("type") != "assistant":
        return

    for block in (event.get("message") or {}).get("content") or []:
        if not isinstance(block, dict):
            continue

        if block.get("type") == "text":
            text = _first_line(block.get("text") or "")

            if text:
                progress.emit(text)

        elif block.get("type") == "tool_use":
            progress.emit(_describe_tool(block))


def _run_claude(prompt, resume_session_id=None):
    """
    Run one non-interactive Claude Code turn and return (result, error).

    `result` is the parsed final "result" event on success, which carries
    the same fields --output-format json used to return in one shot.
    `error` is a human-readable string on failure. Exactly one of the
    two is non-None.
    """

    cmd = [
        "claude",
        "--bare",
        "-p", prompt,
        # stream-json requires --verbose; without it the CLI refuses to
        # stream in print mode and the loop below would see nothing.
        "--output-format", "stream-json",
        "--verbose",
        "--dangerously-skip-permissions",
        "--max-budget-usd", MAX_BUDGET_USD,
        "--append-system-prompt", SAFETY_SYSTEM_PROMPT,
        # Which desktop this is. Claude Code can of course run `hostname`
        # to find out, but a question about the desktop it is already
        # running on should not cost a tool call - and on the other two
        # harnesses not knowing this turned into a help request that
        # blocked the desktop outright. Same facts, same wording, all
        # three harnesses.
        "--append-system-prompt", get_identity_context("Claude Code"),
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
        process = subprocess.Popen(
            cmd,
            cwd=str(config.WORK_DIR),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
    except OSError as exc:
        return None, f"could not start claude: {exc}"

    # stderr is drained on its own thread rather than read after the fact:
    # a process that fills the stderr pipe buffer blocks forever while we
    # are still reading stdout, which would hang the task rather than fail
    # it. Same reason the timeout kills the process instead of waiting.
    stderr_chunks = []

    def drain_stderr():
        for line in process.stderr:
            stderr_chunks.append(line)

    stderr_thread = threading.Thread(target=drain_stderr, daemon=True)
    stderr_thread.start()

    timed_out = threading.Event()

    def kill_on_timeout():
        timed_out.set()
        process.kill()

    watchdog = threading.Timer(TASK_TIMEOUT_SECONDS, kill_on_timeout)
    watchdog.start()

    result = None

    try:
        for line in process.stdout:
            line = line.strip()

            if not line:
                continue

            try:
                event = json.loads(line)
            except ValueError:
                # A non-JSON line is not fatal - the CLI occasionally
                # writes plain text around the stream. Keep reading.
                continue

            if event.get("type") == "result":
                result = event
                continue

            _emit_event(event)
    finally:
        watchdog.cancel()
        process.wait()
        stderr_thread.join(timeout=5)

    if timed_out.is_set():
        return None, (
            f"claude did not finish within {TASK_TIMEOUT_SECONDS} seconds"
        )

    if result is None:
        return None, (
            "".join(stderr_chunks)
            or f"claude exited with code {process.returncode}"
        ).strip()

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
