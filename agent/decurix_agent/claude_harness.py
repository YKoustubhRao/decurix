import json
import os
import subprocess
import threading
from pathlib import Path

from decurix_agent import config, progress, state, usage
from decurix_agent.computer import approval
from decurix_agent.logging_utils import log, log_banner
from decurix_agent.identity import get_identity_context
from decurix_agent.memory_context import get_memory_context
from decurix_agent.messaging import ask_for_query, write_final_reply


# ============================================================
# Claude Code harness
#
# Alternative to the Ollama-driven loop in task_runner.py: instead of
# our own COMMAND:/DONE:/HELP: protocol, each task is handed to the
# `claude` CLI in non-interactive mode, which runs its own agentic
# loop (including executing shell commands) to completion and returns
# one final JSON result. Exposes the same perform_task/
# continue_after_query signatures as task_runner.py so harness.py can
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


def _claude_failure_note(error):
    """
    Turn a raw claude-CLI failure into a note that says what to do about
    it, the same principle as messaging.backend_failure_note - but this
    harness's failures are a local subprocess (didn't start, timed out,
    exited non-zero), not a networked backend, so that helper's "at
    <url>" phrasing doesn't fit and this mirrors it instead of reusing it.
    """

    text = str(error)
    lowered = text.lower()

    if "did not finish within" in lowered:
        cause = "The Claude Code CLI did not finish this task in time."
        fix = (
            "Send the request again - a smaller task, or a lower "
            "CLAUDE_TASK_TIMEOUT_SECONDS worth waiting out, may help."
        )
    elif "could not start claude" in lowered:
        cause = "The Claude Code CLI itself failed to start on this desktop."
        fix = (
            "Check that the `claude` binary is installed and that this "
            "desktop has completed the shared Claude Code login, then "
            "send the request again."
        )
    else:
        cause = "The Claude Code CLI failed while working on this task."
        fix = "Send the request again, or check the desktop's own logs."

    return f"{cause}\n\n{fix}\n\nTechnical detail:\n{text}"

SAFETY_SYSTEM_PROMPT = f"""
IMPORTANT SAFETY RULE:

Before making or approving any payment or financial transaction,
sending or approving any outgoing message on behalf of the human
(WhatsApp, email, SMS, or any other messaging platform), or taking any
other action with real-world consequences that you are not fully
confident about: stop before performing that action. Do not perform it
yet.

Instead, respond with a reply that begins with exactly:

{APPROVAL_MARKER}

followed by a clear description of exactly what you want to do and
why. A human will review this on the Decurix dashboard and respond
with approval or further instructions before you continue.
""".strip()

SCREEN_SYSTEM_PROMPT = f"""
This desktop has a graphical screen (XFCE, Chromium, LibreOffice). To work
in a window, use the computer tools rather than shell tricks like xdotool:
mcp__computer__observe (screenshot + elements with refs), mcp__computer__act
(click/type/key/scroll/select on a ref; coordinates only when no element
fits), mcp__computer__verify (confirm the result), mcp__computer__open
(a URL, file or app). Act on refs from your latest observation, and verify
before reporting that something on screen is done. If a person has taught
this desktop the task, mcp__computer__skills finds it and
mcp__computer__run_skill does it with no model: prefer that to working the
task out yourself.

Never drive the screen or the browser from the shell (xdotool, the DevTools
port, this desktop's own agent ports) and never write into ~/.decurix: such
commands are refused, and the computer tools are the way.

You do not need to ask before clicking an on-screen control: the act tool
itself stops before anything that sends, pays, buys or publishes. When it
returns NEEDS_APPROVAL, stop and reply beginning with {APPROVAL_MARKER}
describing that action; after approval, repeat the same action.
""".strip()

# The computer MCP server (decurix_agent/computer/mcp_server.py), run from
# the directory this package lives in so `python3 -m` finds it whatever the
# task's working directory is.
MCP_CONFIG = json.dumps({
    "mcpServers": {
        "computer": {
            "type": "stdio",
            "command": "python3",
            "args": ["-m", "decurix_agent.computer.mcp_server"],
            "env": {"PYTHONPATH": str(Path(__file__).resolve().parent.parent)},
        }
    }
})

# How Claude Code is let use its tools: every tool without a prompt (a headless run has nobody to answer one), behind a
# PreToolUse hook (claude_guard.py) that refuses a Bash command or a file write that drives the screen or the browser
# around the approval gate, reaches this desktop's own agent and DevTools ports, or touches the gate's state and the
# person's reply channel - and deny rules for the plainest of them, which hold in every permission mode. Review
# 2026-11-02: under --dangerously-skip-permissions alone, Bash could `xdotool` a click on Pay or write a yes into the
# reply channel. A GUARD RAIL, NOT A BOUNDARY: the shell runs as the same Linux user as the gate, so a determined
# command gets past words; the boundary is a separate user for the gate and its state (not built yet).
GUARD_SCRIPT = Path(__file__).resolve().parent / "claude_guard.py"
_GUARD_HOOK = [{"type": "command", "command": f"python3 {GUARD_SCRIPT.as_posix()}"}]
GUARD_SETTINGS = json.dumps({
    "hooks": {"PreToolUse": [{"matcher": tool, "hooks": _GUARD_HOOK}
                             for tool in ("Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch")]},
})
PERMISSION_FLAGS = [
    # Every tool allowed without a prompt, as before - the mode named rather than "dangerously" skipped, so the hook
    # and the deny rules below apply in it.
    "--permission-mode", "bypassPermissions",
    "--settings", GUARD_SETTINGS,
    "--disallowedTools", "Bash(xdotool:*),Bash(xte:*),Bash(ydotool:*),Bash(wmctrl:*)",
]

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
# resume when a human response arrives. Not part of decurix_agent.state
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
            # The computer tools report themselves, with screenshots, from
            # inside the action layer (computer/audit.py) - the same steps the
            # other engines produce. A second line here would only repeat them.
            if (block.get("name") or "").startswith("mcp__computer__"):
                continue
            if block.get("name") == "Bash":
                # The final check reads what shell work could have changed (playbooks.changed_things).
                try:
                    from decurix_agent import playbooks
                    playbooks.note_command((block.get("input") or {}).get("command") or "")
                except Exception:  # noqa: BLE001 - noting it is for the final check only
                    pass
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
        # No --bare: it forces API-key-only auth ("OAuth and keychain are
        # never read") and this harness authenticates with the shared
        # Claude Code login a human completes through the dashboard's
        # popup (internal/desktop/claudelogin.go, saved by
        # internal/claudeauth) instead of an ANTHROPIC_API_KEY - so the CLI
        # needs to be free to read the OAuth credentials seeded into
        # ~/.claude/.credentials.json when this desktop was created. The
        # trade-off is the rest of what --bare would have skipped (hooks,
        # LSP, plugin sync, attribution, auto-memory, CLAUDE.md
        # auto-discovery) is back on, same as running `claude` anywhere
        # else - none of it applies inside a desktop's empty work dir.
        "-p", prompt,
        # stream-json requires --verbose; without it the CLI refuses to
        # stream in print mode and the loop below would see nothing.
        "--output-format", "stream-json",
        "--verbose",
        *PERMISSION_FLAGS,
        "--max-budget-usd", MAX_BUDGET_USD,
        "--append-system-prompt", SAFETY_SYSTEM_PROMPT,
        # Which desktop this is. Claude Code can of course run `hostname`
        # to find out, but a question about the desktop it is already
        # running on should not cost a tool call - and on the other two
        # harnesses not knowing this turned into a query that
        # blocked the desktop outright. Same facts, same wording, all
        # three harnesses.
        "--append-system-prompt", get_identity_context("Claude Code"),
        # The screen, as tools - the same observe/act/verify layer the other
        # two engines reach through the `gui` command, with screenshots,
        # because Claude can read them.
        "--mcp-config", MCP_CONFIG,
        "--append-system-prompt", SCREEN_SYSTEM_PROMPT,
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
                _record_usage(event)
                continue

            if event.get("type") == "system" and event.get("subtype") == "init":
                usage.set_model(event.get("model"))

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


def ask_once(system, prompt):
    """
    One question to Claude with every tool switched off: what the computer
    layer's grounder and judge (computer/escalation.py) use. Answers with the
    reply text, or raises RuntimeError - a caller treats "could not ask" as
    "not sure", never as a yes.
    """

    cmd = ["claude", "-p", prompt, "--output-format", "json", "--tools", "",
           "--max-budget-usd", "0.25", "--append-system-prompt", system]

    try:
        done = subprocess.run(cmd, cwd=str(config.WORK_DIR), capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError) as exc:
        raise RuntimeError(f"could not ask claude: {exc}")

    try:
        result = json.loads(done.stdout)
    except ValueError:
        raise RuntimeError((done.stderr or done.stdout or f"claude exited with code {done.returncode}").strip()[:300])

    _record_usage(result)

    if result.get("is_error") or not isinstance(result.get("result"), str):
        raise RuntimeError(str(result.get("result") or "claude returned no answer")[:300])

    return result["result"]


def _record_usage(result):
    """
    Add one Claude Code run's spend to the task's totals. Each run - the
    task, and every resume after a human's reply - is its own process, and
    its result event's total_cost_usd is taken as that process's spend, so
    the runs are summed. Assumed rather than verified on a live resume: if
    a resumed run reports the session's running total, this double-counts.
    """

    tokens = result.get("usage") or {}

    input_tokens = sum(
        tokens.get(key) or 0
        for key in (
            "input_tokens",
            "cache_creation_input_tokens",
            "cache_read_input_tokens",
        )
    )

    usage.add(
        input_tokens=input_tokens if tokens else None,
        output_tokens=tokens.get("output_tokens"),
        cost_usd=result.get("total_cost_usd"),
        cached_input_tokens=tokens.get("cache_read_input_tokens") if tokens else None,
    )


def _handle_result(result):
    """
    Route a successful Claude Code result to either write_final_reply
    or ask_for_query, updating this harness's session-continuation state.
    """

    global _current_session_id

    session_id = result.get("session_id")
    if session_id:
        _current_session_id = session_id

    if result.get("is_error"):
        ask_for_query(
            result.get("result")
            or "Claude Code reported an error but gave no details."
        )
        return

    result_text = (result.get("result") or "").strip()

    if result_text.startswith(APPROVAL_MARKER):
        said = result_text[len(APPROVAL_MARKER):].strip()
        blocked = approval.pending()
        if blocked is not None:
            # The gate's own words for the action it holds lead: a yes settles only the action it was asked for
            # (approval.asked_about), and Claude's paraphrase may name another.
            said = approval.request_text(blocked) + (f"\n\nThe agent's own words:\n{said}" if said else "")
        ask_for_query(said, kind="approval")
        return

    # The act tool stopped an on-screen action for approval, and the turn
    # ended without saying so. Whatever Claude wrote instead, the task is
    # paused on that action - a reply that skipped the pause must not reach
    # the human as a finished answer.
    blocked = approval.pending()
    if blocked is not None:
        note = approval.request_text(blocked)
        if result_text:
            note += f"\n\nThe agent's own last words:\n{result_text}"
        ask_for_query(note, kind="approval")
        return

    write_final_reply(result_text or "(no result text)")


def perform_task(task, task_file):
    state.begin(task, task_file)

    log_banner("STARTING TASK (Claude Code harness)", task)

    result, error = _run_claude(task)

    if error:
        # The task is over, not paused - matching task_runner.py and
        # thebot_harness.py. This used to be ask_for_query, which leaves
        # the desktop waiting on a reply that has nothing to add to a CLI
        # that never produced a result; the human's next, unrelated
        # request would be routed as a "reply" to this one instead.
        write_final_reply(_claude_failure_note(error), kind="error")
        return

    _handle_result(result)


def continue_after_query(query_response):
    if not state.current_task:
        log(
            "Received a reply to a query but no "
            "active task exists."
        )
        return

    state.waiting_for_query = False

    log_banner("RECEIVED HUMAN REPLY TO QUERY (Claude Code harness)", query_response)

    result, error = _run_claude(
        query_response,
        resume_session_id=_current_session_id,
    )

    if error:
        # Unlike perform_task's fresh-task failure above, this is the
        # documented exception: a failure while resuming after a human's
        # reply keeps the task paused, so a resend of that same reply
        # still carries the original session rather than starting a new,
        # unrelated task.
        ask_for_query(
            "The agent tried to continue after your reply, but the "
            "Claude Code harness failed again.\n\n"
            + _claude_failure_note(error)
        )
        return

    _handle_result(result)
