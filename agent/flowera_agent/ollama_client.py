import json
import urllib.error
import urllib.request

from flowera_agent import config
from flowera_agent.logging_utils import log
from flowera_agent.memory_context import get_memory_context
from flowera_agent.readiness import wait_for_http_ok


# ============================================================
# Ollama
# ============================================================

def ollama_request(messages):
    """
    Call the local Ollama server.
    """

    payload = {
        "model": config.OLLAMA_MODEL,
        "messages": messages,
        "stream": False,
    }

    body = json.dumps(payload).encode("utf-8")

    url = config.OLLAMA_URL.rstrip("/") + "/api/chat"

    request = urllib.request.Request(
        url,
        data=body,
        headers={
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(
            request,
            timeout=300,
        ) as response:

            raw = response.read()

            result = json.loads(
                raw.decode("utf-8")
            )

            return result["message"]["content"]

    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"Could not reach Ollama at {config.OLLAMA_URL}: {exc}"
        )

    except Exception as exc:
        raise RuntimeError(
            f"Ollama request failed: {exc}"
        )


def wait_for_ollama():
    """
    Wait until Ollama becomes available.

    Runs on a background thread (see app.py and harness.wait_for_backend),
    so it never gates the agent's own :8081 listener - it only decides when
    queued work may start.
    """

    wait_for_http_ok(
        config.OLLAMA_URL.rstrip("/") + "/api/tags",
        f"Ollama at {config.OLLAMA_URL}",
    )


# ============================================================
# Agent reasoning
# ============================================================

SYSTEM_PROMPT = """
You are the autonomous agent running inside a Flowera desktop VM.

You receive a task from a human.

Your job is to actually perform the task.

You have access to a Flowera shell.

IMPORTANT OUTPUT RULE:

Every response MUST begin with exactly ONE of:

COMMAND:
DONE:
HELP:

Do not write anything before the control signal.

Valid response formats:

COMMAND:
<one shell command>

DONE:
<short final answer>

HELP:
<specific blocking problem>

Rules:

1. Use COMMAND when you need to perform an action on the Flowera desktop.

2. After a COMMAND is executed, you will receive its output.

3. If the command fails, reason about the failure and try to fix it.

4. Do not repeatedly execute the same failed command.

5. Use HELP ONLY when you are genuinely blocked and cannot
continue without information or action from the human/control plane.

6. A normal question, greeting, conversation, explanation, or
identity question is NOT a HELP situation.

7. If the human asks something like:
   "hello"
   "who are you?"
   "what can you do?"
   "how are you?"
   answer using DONE.

8. HELP must ONLY be used when you need something from the human
to continue the task.

9. Examples of valid HELP situations:
   - You need the user to choose between two files.
   - You need a password or confirmation.
   - A required resource is unavailable.
   - You cannot determine what the user wants and cannot safely
     proceed.
   - An external system requires human intervention.

10. When the task is complete, use DONE.

11. Do not claim a task is complete unless it actually is.

12. Prefer small, verifiable commands.

13. Never invent command output.

14. Never use HELP merely to ask a conversational question.

15. Never use HELP merely because the task is a question.

16. Never output both DONE and HELP.

17. The control signal must be the FIRST thing in your response.

18. IMPORTANT: Always use HELP - even if you are technically able to
proceed - before: making or approving any payment or financial
transaction, sending or approving any outgoing message on behalf of
the human (WhatsApp, email, SMS, or any other messaging platform), or
taking any other action with real-world consequences that you are not
fully confident about. Describe exactly what you want to do and why,
and wait for explicit human approval before proceeding.

Examples:

Human:
hello, who are you?

Correct:
DONE:
I am a virtual machine assistant running inside your Flowera desktop VM.

Human:
List the files in /tmp.

Correct:
COMMAND:
ls -la /tmp

Human:
Delete the project.

If you need clarification about which project:
HELP:
Which project directory should I delete?

Human:
Delete /home/headless/work/project.

Correct:
COMMAND:
rm -rf /home/headless/work/project

After verifying deletion:
DONE:
The project was deleted successfully.
"""


def ask_agent(task, history):
    messages = [
        {
            "role": "system",
            "content": SYSTEM_PROMPT,
        }
    ]

    memory_context = get_memory_context()
    if memory_context:
        messages.append(
            {
                "role": "system",
                "content": (
                    "Persistent worker memory for this desktop:\n\n"
                    f"{memory_context}"
                ),
            }
        )

    messages.extend(history)

    messages.append(
        {
            "role": "user",
            "content": task,
        }
    )

    return ollama_request(messages)
