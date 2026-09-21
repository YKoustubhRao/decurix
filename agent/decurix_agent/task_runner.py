import os
import re

from decurix_agent import config, progress, state
from decurix_agent.commands import dangerous_reason, run_command
from decurix_agent.computer import approval
from decurix_agent.logging_utils import log
from decurix_agent.messaging import (
    ask_for_query,
    backend_failure_note,
    write_final_reply,
)
from decurix_agent.ollama_client import (
    ask_agent,
    build_system_messages,
    ollama_request,
)
from decurix_agent.parser import parse_agent_response


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
PREMATURE_QUERY_NUDGE = """
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


# What a HELP gets answered with, once, when it arrives straight after a
# command that SUCCEEDED (exit 0).
#
# Measured live on qwen2.5-coder:3b: asked "how many files are in /etc?",
# it ran `ls -la /etc | wc -l`, got exit 0 and "123", and replied
# HELP "It seems there was an error with the command" - paging a human
# about a task that was already finished. A HELP that follows a success
# and asks for nothing only a person can give is a mislabelled answer far
# more often than a real block, so it is shown the output again, once.
SUCCESS_QUERY_NUDGE = """
Nothing failed: your last command finished with exit code 0 and printed:

{output}

If that answers the task, reply now with DONE: followed by the answer.
If there is more to do, send the next COMMAND.
Only reply HELP again if the human really has to act - to choose between
options, give you something only they have, or approve a consequential
action.
""".strip()


# What a HELP gets answered with when it arrives straight after a command
# that FAILED, and the agent has not yet tried another way.
#
# Measured live: "how much free disk space do I have?" became
# `df -h | grep "root"`, which matched nothing (exit 1, no output), and the
# next reply was HELP "The command failed to execute. Please check the path
# or permissions" - a person paged because a filter was too narrow. One
# failed command is information, not a blocker: the agent gets two more
# tries with the failure in front of it before a human is asked.
FAILURE_QUERY_NUDGE = """
Your last command did not work: it finished with exit code {code} and
printed:

{output}

Do not ask the human yet. Try a DIFFERENT command that gets the same
information - a simpler one, without the filter or pipe that may have
hidden the output, or a different tool. Send it now with COMMAND.
Only reply HELP if the human really has to act.
""".strip()

# How many failure pushbacks one task gets.
MAX_FAILURE_NUDGES = 2


# A HELP that is genuinely asking for something only a person can give.
# Such a HELP is always delivered, never pushed back on (same list as
# TheBot's operator, so the two harnesses escalate the same things).
# Approval, secrets and payments: only a person can give these, and asking for
# them before touching anything is what the safety rules require.
APPROVAL_RE = re.compile(
    r"\b(approv|(your|need(ed)?|ask(ing)?|grant(ing)?|without)\s+permission"
    r"|permission\s+to|consent|confirm|authori[sz]|password|passphrase"
    r"|token|api[ -]?key|credential|secret|log ?in|sign ?in|2fa|otp"
    r"|payment|pay\b|purchase|send(ing)?\s+(an?\s+)?(e-?mail|message|sms)"
    r"|publish|push)",
    re.IGNORECASE,
)

# Choices between alternatives - also only a person's to make, but a HELP of
# this kind is only credible once the agent has looked at something.
CHOICE_RE = re.compile(
    r"\b(which\s+(one|of)|choose|pick|prefer|decide|should\s+i|do\s+you\s+want)",
    re.IGNORECASE,
)


class _Either:
    """A pattern that matches when any of its parts does."""

    def __init__(self, *patterns):
        self.patterns = patterns

    def search(self, text):
        for pattern in self.patterns:
            found = pattern.search(text)
            if found:
                return found
        return None


# What after-a-command pushbacks leave alone: approvals and choices.
HUMAN_ONLY_RE = _Either(APPROVAL_RE, CHOICE_RE)


# A HELP that is the agent's own safety brake on something destructive or
# irreversible. Never argued with either: measured live, "delete every file in
# /etc" drew the correct HELP "Are you sure you want to delete all files in
# /etc?", the premature-query pushback then talked the model out of it, and
# what came back was `DONE: I am ready to proceed.` - the question that
# protects the machine was thrown away.
CONSEQUENTIAL_RE = re.compile(
    r"\b(are\s+you\s+sure|irreversible|permanent(ly)?|destructive|"
    r"delet(e|ing)|remov(e|ing)|erase|wipe|overwrit(e|ing)|format(ting)?\s+(the\s+)?(disk|drive|partition)|"
    r"drop\s+(the\s+)?(table|database)|shut\s?down|reboot|"
    r"cannot\s+be\s+undone|can'?t\s+be\s+undone)",
    re.IGNORECASE,
)


def is_safety_brake(text):
    """Whether a HELP is asking a human for something only a human can give."""

    return bool(APPROVAL_RE.search(text or "") or CONSEQUENTIAL_RE.search(text or ""))


# How many replies in a row may miss the output contract before the loop
# stops arguing about format and just uses what it was given.
MAX_INVALID_RESPONSES = 3


def invalid_format_nudge(response):
    """
    Ask for the same answer back, with a marker on the front.

    Quoting the model's own words matters more than it looks. The generic
    version of this reply - a restatement of the three markers, with no
    reference to what was actually said - was measured against the real
    model on the real failure below: it answered a correct, marker-less
    "241" with a HELP request asking the human whether they needed
    anything further. The same conversation, nudged with the opening of
    its own reply quoted back, answered "DONE: 241".
    """

    opening = " ".join((response or "").split())[:60]

    if "```" in (response or ""):
        return f"""
Your last reply was code with no marker in front of it, so nothing has
been written or run yet.

If the human wanted a file created or a program run, do it with COMMAND -
for example:

COMMAND:
cat > hello.py <<'EOF'
print("hello")
EOF

then run it with your next COMMAND (python3 hello.py) and answer with DONE
once you have seen its real output.

If the human only wanted to see the code, reply DONE: followed by it.
""".strip()

    return f"""
Your last reply started with "{opening}", which is not one of the three
markers, so it could not be used.

Send it again, unchanged, with the right marker in front:

If that was your final answer to the human, prefix it with DONE:
If it was a shell command to run, prefix it with COMMAND:
If you need the human before you can continue, prefix it with HELP:
""".strip()


def failure_nudge(result):
    """The pushback for a HELP that follows a failed command."""

    output = (result["output"] or "").strip()[:MAX_COMMAND_OUTPUT_CHARS]

    nudge = FAILURE_QUERY_NUDGE.format(
        code=result["exit_code"],
        output=output or "(no output)",
    )

    # "python3 fib.py" failing with "No such file" means the file was never
    # written - the model described a script and then ran it. Say so, with
    # the way to write one (measured: it otherwise asked the human where the
    # script was, three times running).
    if re.search(r"no such file|can't open file|cannot open|not found", output, re.IGNORECASE):
        nudge += (
            "\n\nThe file it needs does not exist yet. Create it first with a "
            "COMMAND that writes it, for example:\n"
            "COMMAND:\ncat > name.py <<'EOF'\nprint('hello')\nEOF\n"
            "and only then run it."
        )

    return nudge


def command_only(command):
    """
    The transcript's record of a COMMAND turn: the command, and nothing
    the model wrote beside it.

    A small model writes `COMMAND: df -h` and, underneath, the `DONE:` it
    expects the command to produce - before anything has run.
    parse_agent_response rightly takes only the command, but if the raw
    reply stays in history the model reads its own guess back on the next
    turn and reports it (measured against TheBot's operator: a real `df`
    of 1007G/121G/835G was answered "1.8T total, 20G used", word for word
    the guess written beside the command).
    """

    return f"COMMAND:\n{command}"


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
    human got a query about a loop, for a question that had been
    answered correctly minutes earlier. An answer without a marker is
    still an answer; a false alarm is not.
    """

    text = (response or "").strip()

    if not text:
        return False

    # A bare "HELP" / "DONE" / "COMMAND" is a marker with nothing after it,
    # not an answer: measured live, a model that could not think of a
    # command replied "HELP" four times in a row and the human was handed
    # the single word "HELP" as the result of their request.
    if re.fullmatch(r"[\s*_`#>-]*(COMMAND|DONE|HELP)\s*:?[\s*_`]*", text, re.IGNORECASE):
        return False

    log(
        "Agent never produced a control signal - taking its last reply "
        "as the final answer."
    )

    write_final_reply(text)

    return True


def format_command_result(result):
    """
    Render a command's exit code and output as the history entry the model
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

    # Say what an exit code and an empty output mean. Measured live on the
    # 3B model: `echo banana > hello.txt` succeeded silently (exit 0, no
    # output) and the next reply was the system prompt's own HELP example,
    # "There is more than one project directory here" - to a model reading
    # a blank, silence looks like failure.
    verdict = "succeeded" if result["exit_code"] == 0 else "failed"

    if not output.strip():
        output = "(no output)" + (
            " - this is normal, the command succeeded" if result["exit_code"] == 0 else ""
        )

    return (
        "Command execution result:\n\n"
        f"exit_code: {result['exit_code']} ({verdict})\n\n"
        f"output:\n{output}"
    )


MAX_RECENT_FULL_TURNS = int(os.environ.get("DECURIX_MAX_RECENT_TURNS", "12"))
MAX_HISTORY_CHARS_BEFORE_COMPACTION = int(os.environ.get("DECURIX_MAX_HISTORY_CHARS", "16000"))
OLD_COMMAND_OUTPUT_CHARS = 200


def compact_history(history, max_recent=MAX_RECENT_FULL_TURNS, max_chars=MAX_HISTORY_CHARS_BEFORE_COMPACTION):
    """
    Keep recent turns in full detail, while compressing the verbose outputs
    of older turns so total prompt size stays comfortably within num_ctx (8192)
    and RAM budgets without forgetting what commands have already been run.

    Crucially, avoid eagerly mutating history when total prompt characters are
    well within context limits. Mutating earlier messages destroys llama.cpp's
    KV prefix cache, turning ~0.5s cached turns into ~30s full-prompt evaluations on CPU.
    """
    if len(history) <= max_recent * 2:
        return history

    total_chars = sum(len(m.get("content", "")) for m in history)
    # If using default max_recent and character count is well within safety budget,
    # preserve the exact history to keep KV cache prefix intact.
    if total_chars <= max_chars and max_recent >= MAX_RECENT_FULL_TURNS:
        return history

    cutoff = len(history) - (max_recent * 2)
    compacted = []

    for i, msg in enumerate(history):
        if i >= cutoff or msg.get("role") != "user":
            compacted.append(msg)
            continue

        content = msg.get("content", "")
        if "Command execution result:\n\n" in content and "output:\n" in content:
            parts = content.split("output:\n", 1)
            header = parts[0] + "output:\n"
            body = parts[1].strip()
            if len(body) > OLD_COMMAND_OUTPUT_CHARS:
                compacted_body = (
                    body[:OLD_COMMAND_OUTPUT_CHARS]
                    + "\n... [earlier output compacted for context budget]"
                )
                compacted.append({"role": "user", "content": header + compacted_body})
            else:
                compacted.append(msg)
        else:
            compacted.append(msg)

    return compacted


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
# reason claude_harness keeps its session id: continue_after_query starts a
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
    ask_for_query(blocked, kind="approval")

    return True


# A shell command stopped by commands.dangerous_reason, waiting on a person:
# run once if they say yes, dropped if they do not.
_pending_dangerous_command = None


def pause_for_dangerous_command(command, reason):
    """
    Stop the task before a catastrophic command runs, and ask a person.

    Decided here from the command itself, not left to the model, for the same
    reason the screen gate is: a pause that depends on the model choosing to
    ask is not a pause.
    """

    global _pending_dangerous_command

    log(f"Command held for approval ({reason}): {command}")

    _pending_dangerous_command = command
    progress.emit(f"Held for approval: {command}")
    ask_for_query(
        f"""
I was about to run this command, and I stopped because {reason}:

$ {command}

Reply "yes" to let me run exactly that, or tell me what to do instead.
""".strip(),
        kind="approval",
    )


def resume_approved_dangerous_command(history, reply):
    """
    After a person answered a held command: run it if they approved.
    Returns (command, result), or (None, None) when nothing was run.
    """

    global _pending_dangerous_command

    command = _pending_dangerous_command
    _pending_dangerous_command = None

    # A catastrophic command is released only by a clean yes: "yes but not
    # that one" is a person changing their mind, not permission.
    if (
        not command
        or not approval.is_approval(reply)
        or re.search(r"\b(but|except|instead|not|however|though|unless|only|rather)\b", reply or "", re.IGNORECASE)
    ):
        return None, None

    log("Human approved the held command - running it now.")

    progress.emit(f"$ {command}")
    result = run_command(command)
    progress.emit(describe_result(result))

    history.append({"role": "assistant", "content": f"COMMAND:\n{command}"})
    history.append({"role": "user", "content": format_command_result(result)})

    return command, result


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
    global _pending_gui_command, _pending_dangerous_command

    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_query = False
    _pending_gui_command = None
    _pending_dangerous_command = None

    log("========================================")
    log("STARTING TASK")
    log(task)
    log("========================================")

    history = []

    # Whether the agent has actually run anything yet, and whether it has
    # already been told that asking a query before trying counts as not
    # having tried. Both gate PREMATURE_QUERY_NUDGE, which fires at most
    # once per task.
    commands_run = 0
    nudged = False
    success_nudged = False
    failure_nudges = 0
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
                compact_history(history),
            )

        except Exception as exc:

            # The task is over, not paused. This used to be ask_for_query,
            # which leaves the desktop waiting on a reply - but the note
            # tells the human to send their request again, and there is
            # nothing a reply could add to a model that never answered.
            # Measured live: the human sent a new task instead, it was
            # routed to continue_after_query as the "reply" to a greeting,
            # and the finished note reported it under the greeting's Task.
            write_final_reply(
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

            write_final_reply(value)

            return

        if action == "query":

            if commands_run == 0 and not nudged and not is_safety_brake(value):

                nudged = True

                log(
                    "Agent had a query before running anything - "
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
                        "content": PREMATURE_QUERY_NUDGE,
                    }
                )

                continue

            if (
                last_result is not None
                and last_result["exit_code"] == 0
                and not success_nudged
                and not HUMAN_ONLY_RE.search(value or "")
            ):

                success_nudged = True

                log(
                    "Agent had a query right after a command succeeded - "
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
                        "content": SUCCESS_QUERY_NUDGE.format(
                            output=(
                                (last_result["output"] or "").strip()[
                                    :MAX_COMMAND_OUTPUT_CHARS
                                ]
                                or "(no output)"
                            )
                        ),
                    }
                )

                continue

            if (
                last_result is not None
                and last_result["exit_code"] != 0
                and failure_nudges < MAX_FAILURE_NUDGES
                and not HUMAN_ONLY_RE.search(value or "")
            ):

                failure_nudges += 1

                log(
                    "Agent had a query right after a command failed - "
                    "asking it to try another way first "
                    f"({failure_nudges}/{MAX_FAILURE_NUDGES})."
                )

                history.append({"role": "assistant", "content": response})

                history.append(
                    {
                        "role": "user",
                        "content": failure_nudge(last_result),
                    }
                )

                continue

            ask_for_query(value)

            return

        if action == "command":

            command = value

            if not command:
                ask_for_query(
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
                            "content": command_only(command),
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
                    write_final_reply(
                        final_answer_from_command(command, last_result)
                    )
                    return

                ask_for_query(
                    f"""
The agent kept retrying the same command, which keeps failing with exit
code {last_result['exit_code']}:

$ {command}

{(last_result['output'] or '').strip()[:1000] or '(no output)'}

It is not going to get past this on its own.
""".strip()
                )

                return

            danger = dangerous_reason(command)

            if danger:
                pause_for_dangerous_command(command, danger)
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
                    "content": command_only(command),
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

                ask_for_query(
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
    ask_for_query(
        f"""
The agent ran its full budget of {max_steps} steps without finishing
the task, having run {commands_run} command(s).

It is most likely repeating an approach that cannot work.

Please look at the steps above and tell it what to do differently.
""".strip()
    )


# ============================================================
# Query response detection
# ============================================================

def is_query_response(filename, content):
    """
    A request received while waiting for a query to be answered is
    treated as the human's response to the current blocked task.
    """

    if not state.waiting_for_query:
        return False

    lower_name = filename.lower()

    # Ignore our own generated result/query files.
    if lower_name.startswith("query_"):
        return False

    if lower_name.startswith("result_"):
        return False

    return True


def continue_after_query(query_response):
    """
    Continue the current task after a human answers a query.
    """

    if not state.current_task:
        log(
            "Received a reply to a query but no "
            "active task exists."
        )
        return

    task = state.current_task

    state.waiting_for_query = False

    log("========================================")
    log("RECEIVED HUMAN REPLY TO QUERY")
    log(query_response)
    log("========================================")

    # Continue the same task with the human's
    # response appended as context.
    #
    # We run this in the same task thread.
    history = [
        {
            "role": "user",
            "content": (
                "The human replied:\n\n"
                + query_response
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
    success_nudged = False
    failure_nudges = 0

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

    # Likewise a shell command held as catastrophic, if the person said yes.
    held_command, held_result = resume_approved_dangerous_command(history, query_response)

    if held_result is not None:
        last_command, last_result = held_command, held_result

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
                            f"Human reply:\n\n"
                            f"{query_response}\n\n"
                            f"Continue the task."
                        ),
                    },
                    *compact_history(history),
                ]
            )

        except Exception as exc:

            ask_for_query(
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

            write_final_reply(value)
            return

        if action == "query":

            if (
                last_result is not None
                and last_result["exit_code"] == 0
                and not success_nudged
                and not HUMAN_ONLY_RE.search(value or "")
            ):

                success_nudged = True

                log(
                    "Agent had a query right after a command succeeded - "
                    "showing it the output again."
                )

                history.append({"role": "assistant", "content": response})

                history.append(
                    {
                        "role": "user",
                        "content": SUCCESS_QUERY_NUDGE.format(
                            output=(
                                (last_result["output"] or "").strip()[
                                    :MAX_COMMAND_OUTPUT_CHARS
                                ]
                                or "(no output)"
                            )
                        ),
                    }
                )

                continue

            if (
                last_result is not None
                and last_result["exit_code"] != 0
                and failure_nudges < MAX_FAILURE_NUDGES
                and not HUMAN_ONLY_RE.search(value or "")
            ):

                failure_nudges += 1

                log(
                    "Agent had a query right after a command failed - "
                    "asking it to try another way first "
                    f"({failure_nudges}/{MAX_FAILURE_NUDGES})."
                )

                history.append({"role": "assistant", "content": response})
                history.append(
                    {"role": "user", "content": failure_nudge(last_result)}
                )

                continue

            ask_for_query(value)
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
                            "content": command_only(value),
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
                    write_final_reply(
                        final_answer_from_command(value, last_result)
                    )
                    return

                ask_for_query(
                    f"""
After your reply the agent kept retrying the same command, which keeps
failing with exit code {last_result['exit_code']}:

$ {value}

{(last_result['output'] or '').strip()[:1000] or '(no output)'}
""".strip()
                )

                return

            danger = dangerous_reason(value)

            if danger:
                pause_for_dangerous_command(value, danger)
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
                    "content": command_only(value),
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

            ask_for_query(
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

    ask_for_query(
        f"The agent ran {max_steps} more steps after your reply without "
        "finishing the task."
    )
