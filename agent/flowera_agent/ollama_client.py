import json
import os
import urllib.error
import urllib.request

from flowera_agent import config, usage
from flowera_agent.identity import get_identity_context
from flowera_agent.memory_context import get_memory_context
from flowera_agent.readiness import wait_for_http_ok


# ============================================================
# Ollama
# ============================================================

# How long one request may take. Generous on purpose: the machines this
# runs on are usually CPU-only, where a few hundred tokens of prompt take
# tens of seconds to evaluate before a single token is generated.
REQUEST_TIMEOUT_SECONDS = int(
    os.environ.get("OLLAMA_TIMEOUT_SECONDS", "300")
)

# How long Ollama keeps this model resident after answering.
#
# This is the single biggest thing standing between a usable agent and an
# unusable one on CPU. Ollama's default is 5 minutes, after which it
# evicts the model AND the prompt cache built alongside it. The next step
# of the same task then pays to load the weights again and to re-evaluate
# the entire system prompt from scratch - measured at ~14 tokens/sec on an
# ordinary laptop, which is ~45 seconds before the model has read anything
# new. Staying resident means every step after the first only evaluates
# the tokens that were actually added.
KEEP_ALIVE = os.environ.get("OLLAMA_KEEP_ALIVE", "30m")


def ollama_request(messages):
    """
    Call the local Ollama server.
    """

    payload = {
        "model": config.OLLAMA_MODEL,
        "messages": messages,
        "stream": False,
        "keep_alive": KEEP_ALIVE,
        "options": {
            # Caps the KV-cache Ollama allocates for this model. Left
            # unset, Ollama sizes it to the model's native context window
            # (tens of thousands of tokens), which is memory this
            # shell-command-reasoning loop never needs -
            # format_command_result already keeps individual turns well
            # under this. Keep this value stable across every call: a
            # different num_ctx makes Ollama reload the model, throwing
            # away the resident copy KEEP_ALIVE just paid to keep.
            "num_ctx": 8192,
            # Low, not zero. The output contract below is a structured
            # choice between three markers, not creative writing, and a
            # small model at its default temperature drifts out of the
            # format often enough to matter. Not zero, because a task that
            # needs a second attempt at a failed command should be able to
            # produce a different one.
            "temperature": 0.2,
        },
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
            timeout=REQUEST_TIMEOUT_SECONDS,
        ) as response:

            raw = response.read()

            result = json.loads(
                raw.decode("utf-8")
            )

            # A local model: tokens are what the call used, and it costs
            # nothing per call.
            usage.set_model(config.OLLAMA_MODEL)
            usage.add(
                input_tokens=result.get("prompt_eval_count"),
                output_tokens=result.get("eval_count"),
                cost_usd=0,
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

# Deliberately short.
#
# The previous version of this prompt was 637 tokens and spent five
# separate numbered rules (6, 7, 8, 14, 15) forbidding the same mistake -
# asking a human for help instead of answering a question. It still made
# it: "hello, whats your desktop name?" came back as a help request that
# blocked the desktop. Repetition is not what makes a small model comply,
# and on a CPU-only host every one of those tokens is re-read on every
# step of every task. What actually fixed that case was giving the agent
# the fact it was missing (see identity.py) and stating each rule once,
# where the model is deciding between the three markers.
SYSTEM_PROMPT = """
You are the autonomous agent inside a Flowera desktop VM. You do real
work on it by running real shell commands.

OUTPUT CONTRACT. Every reply is exactly one of these three, and the
marker is the very first thing you write:

COMMAND:
<one shell command>

DONE:
<your final answer to the human>

HELP:
<the one thing you need from the human>

Choosing between them:

DONE - the task is finished, or the human asked something you can
already answer (a greeting, who you are, what you can do, anything in
the facts you were given about this desktop). Answer it and stop. Never
report work you did not actually verify.

COMMAND - you need to inspect or change something on this machine. One
command per reply; you get its exit code and output back, then you reply
again. If it fails, read the error and try a different approach. Never
send a command that just failed.

HELP - you are blocked and no command can unblock you: the human has to
choose between real alternatives, supply a secret, or fix something
outside this machine. Not knowing the answer yet is not being blocked -
run a command and find out. A question is never a reason to ask for
help, and never ask the human why they want something.

THE SCREEN. This desktop also has windows: Chromium, LibreOffice, a text
editor. Work in them with the gui command, one per COMMAND:
gui observe             lists windows and elements, each with a ref like e12
gui click e12           clicks it
gui type e12 "text"     replaces a field's text
gui key ctrl+s          presses keys (enter, tab, ctrl+a ...)
gui select e12 "High"   picks a dropdown option
gui open <url|file|app> opens it
Observe first and use refs from the latest output, just the ref: gui click
e12, not the whole line. Every gui action tells you what changed. A button that sends, pays or publishes pauses for
approval on its own.

ALWAYS use HELP, even when you could proceed on your own, before making
or approving a payment, sending a message as the human (WhatsApp, email,
SMS, or anything else), or any other action with real-world consequences
you are not fully confident about. Say exactly what you intend to do and
why, then wait for approval.

Examples.

Human: hello, who are you?
DONE:
I'm the agent running on this Flowera desktop. Tell me what you need
done and I'll do it.

Human: what is in /tmp?
COMMAND:
ls -la /tmp

Human: add a user named Kim in the admin panel open in the browser.
COMMAND:
gui observe

Human: delete the project.
HELP:
There is more than one project directory here. Which should I delete?
""".strip()


def build_system_messages():
    """
    The system half of every request: the output contract, who this
    desktop is, and anything in persistent worker memory.

    Shared by perform_task and continue_after_help so both halves of a
    conversation are given the same standing context - and, just as
    importantly, so both send a byte-identical prefix. Ollama caches the
    evaluated prefix of a prompt, so a stable one is the difference
    between re-reading the whole contract on every step and re-reading
    nothing.
    """

    messages = [
        {
            "role": "system",
            "content": SYSTEM_PROMPT,
        },
        {
            "role": "system",
            "content": get_identity_context(
                f"{config.OLLAMA_MODEL} via Ollama"
            ),
        },
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

    return messages


def ask_agent(task, history):
    messages = build_system_messages()

    messages.extend(history)

    messages.append(
        {
            "role": "user",
            "content": task,
        }
    )

    return ollama_request(messages)
