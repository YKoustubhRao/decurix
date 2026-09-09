from flowera_agent import state
from flowera_agent.commands import run_command
from flowera_agent.logging_utils import log
from flowera_agent.memory_context import get_memory_context
from flowera_agent.messaging import ask_for_help, write_final_message
from flowera_agent.ollama_client import SYSTEM_PROMPT, ask_agent, ollama_request
from flowera_agent.parser import parse_agent_response


# ============================================================
# Task execution
# ============================================================

def perform_task(task, task_file):
    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_help = False

    log("========================================")
    log("STARTING TASK")
    log(task)
    log("========================================")

    history = []

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

            ask_for_help(
                f"""
Ollama/agent execution failed.

Error:
{exc}
""".strip()
            )

            # Wait until a human response arrives.
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

            command = value

            if not command:
                ask_for_help(
                    "The agent generated an empty COMMAND."
                )
                return

            result = run_command(
                command
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
                    "content": (
                        "Command execution result:\n\n"
                        f"exit_code: "
                        f"{result['exit_code']}\n\n"
                        f"output:\n"
                        f"{result['output']}"
                    ),
                }
            )

            continue

        if action == "invalid":

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": """
Your response did not follow the required format.

Respond with exactly one of:

COMMAND:
<shell command>

DONE:
<final result>

HELP:
<specific blocking issue>
""".strip(),
                }
            )

            continue

    # Agent exhausted its step limit.
    ask_for_help(
        f"""
The agent reached its maximum number of
autonomous steps ({max_steps}) without completing
the task.

It may be stuck in a loop.

Please inspect the task and provide guidance.
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

    memory_context = get_memory_context()
    memory_messages = []
    if memory_context:
        memory_messages.append(
            {
                "role": "system",
                "content": (
                    "Persistent worker memory for this desktop:\n\n"
                    f"{memory_context}"
                ),
            }
        )

    max_steps = 50

    for step in range(max_steps):

        try:
            response = ollama_request(
                [
                    {
                        "role": "system",
                        "content": SYSTEM_PROMPT,
                    },
                    *memory_messages,
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
                f"""
The agent tried to continue after human help,
but Ollama failed again.

Error:
{exc}
""".strip()
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

            result = run_command(value)

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": (
                        "Command execution result:\n\n"
                        f"exit_code: "
                        f"{result['exit_code']}\n\n"
                        f"output:\n"
                        f"{result['output']}"
                    ),
                }
            )

            continue

        history.append(
            {
                "role": "assistant",
                "content": response,
            }
        )

        history.append(
            {
                "role": "user",
                "content": """
Invalid response.

Use COMMAND, DONE, or HELP.
""".strip(),
            }
        )

    ask_for_help(
        "The agent exhausted its steps after receiving human help."
    )
