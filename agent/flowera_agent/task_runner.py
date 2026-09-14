from flowera_agent import config, progress, state
from flowera_agent.commands import run_command
from flowera_agent.computer import approval
from flowera_agent.logging_utils import log
from flowera_agent.messaging import (
    ask_for_help,
    backend_failure_note,
    write_final_message,
)
from flowera_agent.ollama_client import (
    ask_agent,
    build_system_messages,
    ollama_request,
)
from flowera_agent.parser import parse_agent_response


# ============================================================
# Task execution
# ============================================================

# Caps how much of a single command's output is fed back into the model as
# conversation history. Left unbounded, one verbose command (a build log, an
# `apt`/`pip` install, `find /`) inflates the prompt for every remaining step
# of the task, making each subsequent request slower and heavier - the model
# still only needs enough of the output to reason about what happened.
MAX_COMMAND_OUTPUT_CHARS = 4000


# What a HELP gets answered with, once, when it arrives before the agent
# has run a single command.
#
# The failure this exists for: "hello, whats your desktop name?" came back
# as HELP "Is there a specific reason you need to know the desktop name?"
# - a conversational question dressed as a blocker, which stopped the
# desktop and paged a human. A model that has not tried anything yet is
# not blocked by definition, so it gets told so once and given another
# turn. Once: a second HELP is taken at face value and sent on, because
# the alternative is an agent that cannot escalate.
#
# The last paragraph is not padding. HELP is also the safety brake (see
# SYSTEM_PROMPT), and a pause for approval legitimately happens before any
# command runs - so the one thing this must never do is argue an agent out
# of asking permission.
PREMATURE_HELP_NUDGE = """
You asked for help before running a single command, so nothing has
actually been tried yet.

If this is something you can already answer - including anything in the
facts you were given about this desktop - answer it now with DONE.

If you could find it out by running something, run it with COMMAND.

Only reply HELP again if a human genuinely has to act before you can
continue.

If you were pausing for approval before a payment, an outgoing message,
or another real-world consequence: reply HELP again with the same
request, and it will be sent to the human.
""".strip()


# How many replies in a row may miss the output contract before the loop
# stops arguing about format and just uses what it was given.
MAX_INVALID_RESPONSES = 3


def invalid_format_nudge(response):
    """
    Ask for the same answer back, with a marker on the front.

    Quoting the model's own words matters more than it looks. The generic
    version of this message - a restatement of the three markers, with no
    reference to what was actually said - was measured against the real
    model on the real failure below: it answered a correct, marker-less
    "241" with a HELP request asking the human whether they needed
    anything further. The same conversation, nudged with the opening of
    its own reply quoted back, answered "DONE: 241".
    """

    opening = " ".join((response or "").split())[:60]

    return f"""
Your last reply started with "{opening}", which is not one of the three
markers, so it could not be used.

Send it again, unchanged, with the right marker in front:

If that was your final answer to the human, prefix it with DONE:
If it was a shell command to run, prefix it with COMMAND:
If you need the human before you can continue, prefix it with HELP:
""".strip()


def repeated_command_nudge(command, result):
    """
    Tell the agent it has already run this exact command, and hand it the
    output again.

    Measured on the real model, on "what is the current kernel version?":
    it replied `COMMAND: uname -r` with a hallucinated `DONE:
    5.10.124-microsoft-standard-WSL2` underneath, and on being given the
    real output replied with the identical command again, and again, for
    all fifty steps - it had already decided it was finished, and the
    loop kept throwing that conclusion away. The same conversation, told
    once that it had already run the command and shown the output, ended
    it with `DONE: 6.6.87.2-microsoft-standard-WSL2`.
    """

    output = (result["output"] or "").strip() or "(no output)"

    return f"""
You have already run this command, and it finished with exit code
{result['exit_code']}:

$ {command}

{output}

Do not run it again. If that answers the task, reply now with DONE:
followed by the answer. Only send a COMMAND if it is a DIFFERENT command
you still genuinely need.
""".strip()


def final_answer_from_command(command, result):
    """
    Report what a command actually produced, as the agent's answer.

    The agent's own trailing DONE: is deliberately not used here even
    when it has written one. On the very first step of the kernel-version
    case above it predicted `5.10.124-microsoft-standard-WSL2` before
    running anything, and the real answer was `6.6.87.2`. Output that was
    actually observed is the only thing safe to report as fact.
    """

    output = (result["output"] or "").strip()

    if not output:
        return (
            f"`{command}` finished with exit code "
            f"{result['exit_code']} and produced no output."
        )

    if len(output) > MAX_COMMAND_OUTPUT_CHARS:
        output = output[:MAX_COMMAND_OUTPUT_CHARS] + "\n... [truncated]"

    return output


def salvage_unmarked_response(response):
    """
    Take a reply that never produced a marker at face value, as the
    agent's answer.

    The alternative, which this replaces, was to keep re-asking for the
    format until the 50-step limit ran out and then tell the human the
    agent "may be stuck in a loop". Measured on the real model: asked how
    many files are in /etc, it ran the right command, read the right
    number, and replied "241" - the task was finished at step two. It
    then spent forty-eight more steps being told about markers, and the
    human got a help request about a loop, for a question that had been
    answered correctly minutes earlier. An answer without a marker is
    still an answer; a false alarm is not.
    """

    text = (response or "").strip()

    if not text:
        return False

    log(
        "Agent never produced a control signal - taking its last reply "
        "as the final answer."
    )

    write_final_message(text)

    return True


def format_command_result(result):
    """
    Render a command's exit code and output as the history message the model
    sees, truncating output so a single noisy command can't balloon the
    prompt sent on every later step of the task.
    """

    output = result["output"] or ""

    if len(output) > MAX_COMMAND_OUTPUT_CHARS:
        omitted = len(output) - MAX_COMMAND_OUTPUT_CHARS
        output = (
            output[:MAX_COMMAND_OUTPUT_CHARS]
            + f"\n... [{omitted} more characters truncated]"
        )

    return (
        "Command execution result:\n\n"
        f"exit_code: {result['exit_code']}\n\n"
        f"output:\n{output}"
    )


def describe_result(result):
    """
    Summarize a command's outcome as one line for the chat's live step view.

    The full output still goes to the model and the agent log; this is only
    the headline, since a step line sits inside a chat bubble.
    """

    lines = (result["output"] or "").strip().splitlines()
    headline = lines[0].strip() if lines else ""

    summary = f"exit {result['exit_code']}"

    if headline:
        summary += " · " + headline[:160]

    return summary


# The `gui` command the approval gate stopped, kept so that an approving
# reply can run exactly that command again. Module-private for the same
# reason claude_harness keeps its session id: continue_after_help starts a
# fresh conversation, and this is the one thing from the old one it needs.
_pending_gui_command = None


def pause_for_screen_approval(command, result):
    """
    Pause the task if `command` was an on-screen action the approval gate
    stopped. Returns True when it did.

    Decided here, from the command's exit code, not left to the model: a 3B
    model told NEEDS_APPROVAL is as likely to try the same click by
    coordinates as to ask, and a pause that depends on the model noticing is
    not a pause.
    """

    global _pending_gui_command

    blocked = approval.blocked_command(result["exit_code"], result["output"])

    if blocked is None:
        return False

    log("On-screen action stopped for approval - pausing the task.")

    _pending_gui_command = command
    ask_for_help(blocked, kind="approval")

    return True


def resume_approved_screen_action(history):
    """
    After a human replied to a screen approval: if they approved, run the
    stopped command now and put its result in front of the model. Returns
    (command, result), or (None, None) when there was nothing to run.
    """

    global _pending_gui_command

    command = _pending_gui_command
    _pending_gui_command = None

    # watchers.command_watcher has already turned an approving reply into a
    # grant; no grant means the human said no, which the model reads in the
    # reply itself.
    if not command or not approval.has_grant():
        return None, None

    log("Human approved the on-screen action - running it now.")

    progress.emit(f"$ {command}")
    result = run_command(command)
    progress.emit(describe_result(result))

    history.append({"role": "assistant", "content": f"COMMAND:\n{command}"})
    history.append({"role": "user", "content": format_command_result(result)})

    return command, result


def perform_task(task, task_file):
    global _pending_gui_command

    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_help = False
    _pending_gui_command = None

    log("========================================")
    log("STARTING TASK")
    log(task)
    log("========================================")

    history = []

    # Whether the agent has actually run anything yet, and whether it has
    # already been told that asking for help before trying counts as not
    # having tried. Both gate PREMATURE_HELP_NUDGE, which fires at most
    # once per task.
    commands_run = 0
    nudged = False
    invalid_in_a_row = 0

    # The last command actually executed and what it produced, so an
    # agent that has stopped making progress can be caught deterministically
    # rather than left to burn the whole step budget. Reset whenever the
    # agent moves on to a genuinely different command.
    last_command = None
    last_result = None
    command_repeats = 0

    # Limit the number of autonomous steps.
    #
    # This prevents an agent from looping forever.
    max_steps = 50

    for step in range(max_steps):

        log(
            f"Agent step "
            f"{step + 1}/{max_steps}"
        )

        try:
            response = ask_agent(
                task,
                history,
            )

        except Exception as exc:

            # The task is over, not paused. This used to be ask_for_help,
            # which leaves the desktop waiting on a reply - but the note
            # tells the human to send their message again, and there is
            # nothing a reply could add to a model that never answered.
            # Measured live: the human sent a new task instead, it was
            # routed to continue_after_help as the "reply" to a greeting,
            # and the finished note reported it under the greeting's Task.
            write_final_message(
                backend_failure_note(
                    "The reasoning model "
                    f"({config.OLLAMA_MODEL})",
                    config.OLLAMA_URL,
                    exc,
                ),
                kind="error",
            )

            return

        log("Agent response:")
        log(response)

        action, value = parse_agent_response(
            response
        )

        if action == "done":

            write_final_message(value)

            return

        if action == "help":

            if commands_run == 0 and not nudged:

                nudged = True

                log(
                    "Agent asked for help before running anything - "
                    "pushing back once."
                )

                history.append(
                    {
                        "role": "assistant",
                        "content": response,
                    }
                )

                history.append(
                    {
                        "role": "user",
                        "content": PREMATURE_HELP_NUDGE,
                    }
                )

                continue

            ask_for_help(value)

            return

        if action == "command":

            command = value

            if not command:
                ask_for_help(
                    "The agent generated an empty COMMAND."
                )
                return

            if command == last_command and last_result is not None:

                command_repeats += 1

                if command_repeats == 1:

                    log(
                        "Agent repeated a command it has already run - "
                        "showing it the output again."
                    )

                    history.append(
                        {
                            "role": "assistant",
                            "content": response,
                        }
                    )

                    history.append(
                        {
                            "role": "user",
                            "content": repeated_command_nudge(
                                command,
                                last_result,
                            ),
                        }
                    )

                    continue

                # Told once and still repeating: it is not going to
                # finish on its own. A command that already succeeded has
                # answered the question, whatever the agent does with it.
                log(
                    "Agent is stuck repeating a command - ending the "
                    "task with what that command produced."
                )

                if last_result["exit_code"] == 0:
                    write_final_message(
                        final_answer_from_command(command, last_result)
                    )
                    return

                ask_for_help(
                    f"""
The agent kept retrying the same command, which keeps failing with exit
code {last_result['exit_code']}:

$ {command}

{(last_result['output'] or '').strip()[:1000] or '(no output)'}

It is not going to get past this on its own.
""".strip()
                )

                return

            progress.emit(f"$ {command}")

            commands_run += 1
            invalid_in_a_row = 0
            command_repeats = 0

            result = run_command(
                command
            )

            last_command = command
            last_result = result

            progress.emit(describe_result(result))

            if pause_for_screen_approval(command, result):
                return

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": format_command_result(result),
                }
            )

            continue

        if action == "invalid":

            invalid_in_a_row += 1

            if invalid_in_a_row > MAX_INVALID_RESPONSES:

                if salvage_unmarked_response(response):
                    return

                ask_for_help(
                    "The agent stopped producing usable responses - its "
                    f"last {MAX_INVALID_RESPONSES} replies had no "
                    "COMMAND, DONE or HELP marker and the final one was "
                    "empty."
                )

                return

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": invalid_format_nudge(response),
                }
            )

            continue

    # Agent exhausted its step limit.
    ask_for_help(
        f"""
The agent ran its full budget of {max_steps} steps without finishing
the task, having run {commands_run} command(s).

It is most likely repeating an approach that cannot work.

Please look at the steps above and tell it what to do differently.
""".strip()
    )


# ============================================================
# Help response detection
# ============================================================

def is_help_response(filename, content):
    """
    A message received while waiting for help is treated
    as the human's response to the current blocked task.
    """

    if not state.waiting_for_help:
        return False

    lower_name = filename.lower()

    # Ignore our own generated result/help files.
    if lower_name.startswith("help_"):
        return False

    if lower_name.startswith("result_"):
        return False

    return True


def continue_after_help(help_response):
    """
    Continue the current task after a human provides help.
    """

    if not state.current_task:
        log(
            "Received help response but no "
            "active task exists."
        )
        return

    task = state.current_task

    state.waiting_for_help = False

    log("========================================")
    log("RECEIVED HUMAN HELP")
    log(help_response)
    log("========================================")

    # Continue the same task with the human's
    # response appended as context.
    #
    # We run this in the same task thread.
    history = [
        {
            "role": "user",
            "content": (
                "The human has provided help:\n\n"
                + help_response
            ),
        }
    ]

    # The same system prefix perform_task sends, byte for byte. It used
    # to be rebuilt here from SYSTEM_PROMPT plus memory in a slightly
    # different order, which meant answering a human dropped Ollama's
    # cached prompt prefix and paid to re-evaluate the whole contract -
    # tens of seconds on a CPU-only host, on the one turn a person is
    # sitting there waiting for.
    system_messages = build_system_messages()

    invalid_in_a_row = 0

    # Same guard as perform_task, for the same reason: the loop below is
    # just as capable of re-running one command until the step budget is
    # gone, and it does it while a human is waiting on their own reply.
    last_command = None
    last_result = None
    command_repeats = 0

    # A reply that approved an on-screen action runs that action first -
    # the model is not asked to find the same button again.
    last_command, last_result = resume_approved_screen_action(history)

    if last_result is not None and pause_for_screen_approval(last_command, last_result):
        return

    max_steps = 50

    for step in range(max_steps):

        try:
            response = ollama_request(
                [
                    *system_messages,
                    {
                        "role": "user",
                        "content": (
                            f"Original task:\n\n"
                            f"{task}\n\n"
                            f"Human help:\n\n"
                            f"{help_response}\n\n"
                            f"Continue the task."
                        ),
                    },
                    *history,
                ]
            )

        except Exception as exc:

            ask_for_help(
                "The agent tried to continue after your reply, but the "
                "reasoning model failed again.\n\n"
                + backend_failure_note(
                    f"The reasoning model ({config.OLLAMA_MODEL})",
                    config.OLLAMA_URL,
                    exc,
                )
            )

            return

        log("Agent response:")
        log(response)

        action, value = parse_agent_response(
            response
        )

        if action == "done":

            write_final_message(value)
            return

        if action == "help":

            ask_for_help(value)
            return

        if action == "command":

            if value == last_command and last_result is not None:

                command_repeats += 1

                if command_repeats == 1:

                    log(
                        "Agent repeated a command it has already run - "
                        "showing it the output again."
                    )

                    history.append(
                        {
                            "role": "assistant",
                            "content": response,
                        }
                    )

                    history.append(
                        {
                            "role": "user",
                            "content": repeated_command_nudge(
                                value,
                                last_result,
                            ),
                        }
                    )

                    continue

                log(
                    "Agent is stuck repeating a command - ending the "
                    "task with what that command produced."
                )

                if last_result["exit_code"] == 0:
                    write_final_message(
                        final_answer_from_command(value, last_result)
                    )
                    return

                ask_for_help(
                    f"""
After your reply the agent kept retrying the same command, which keeps
failing with exit code {last_result['exit_code']}:

$ {value}

{(last_result['output'] or '').strip()[:1000] or '(no output)'}
""".strip()
                )

                return

            progress.emit(f"$ {value}")

            invalid_in_a_row = 0
            command_repeats = 0

            result = run_command(value)

            last_command = value
            last_result = result

            progress.emit(describe_result(result))

            if pause_for_screen_approval(value, result):
                return

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": format_command_result(result),
                }
            )

            continue

        invalid_in_a_row += 1

        if invalid_in_a_row > MAX_INVALID_RESPONSES:

            if salvage_unmarked_response(response):
                return

            ask_for_help(
                "After your reply the agent stopped producing usable "
                "responses - none of its last "
                f"{MAX_INVALID_RESPONSES} had a COMMAND, DONE or HELP "
                "marker."
            )

            return

        history.append(
            {
                "role": "assistant",
                "content": response,
            }
        )

        history.append(
            {
                "role": "user",
                "content": invalid_format_nudge(response),
            }
        )

    ask_for_help(
        f"The agent ran {max_steps} more steps after your reply without "
        "finishing the task."
    )
