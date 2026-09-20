import json
import os
import threading
import time
import urllib.error
import urllib.request

from flowera_agent import config, usage
from flowera_agent.identity import get_identity_context
from flowera_agent.logging_utils import log
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
KEEP_ALIVE = os.environ.get("OLLAMA_KEEP_ALIVE", "60m")
STOP_SEQUENCES = ["\nCommand execution result:"]
NUM_PREDICT = int(os.environ.get("OLLAMA_NUM_PREDICT", "1024"))
NUM_BATCH = int(os.environ.get("OLLAMA_NUM_BATCH", "512"))


def get_optimal_threads():
    """
    Determine the optimal thread count for CPU inference.
    Reserves at least 1-2 cores for host OS responsiveness and desktop interactivity,
    and caps threads to physical cores on hyperthreaded CPUs to prevent AVX2/cache thrashing.
    """
    env_threads = os.environ.get("OLLAMA_NUM_THREAD")
    if env_threads:
        try:
            val = int(env_threads)
            if val > 0:
                return val
        except ValueError:
            pass

    logical = os.cpu_count() or 4
    if logical > 4:
        max_allowed = logical - 2
    elif logical > 2:
        max_allowed = logical - 1
    else:
        max_allowed = 1

    # Try reading physical cores from /proc/cpuinfo (Linux) to avoid hyperthreading contention
    try:
        core_ids = set()
        with open("/proc/cpuinfo", "r") as f:
            phys_id = 0
            for line in f:
                if line.startswith("physical id"):
                    phys_id = line.split(":", 1)[1].strip()
                elif line.startswith("core id"):
                    core_id = line.split(":", 1)[1].strip()
                    core_ids.add((phys_id, core_id))
        if core_ids:
            return max(1, min(len(core_ids), max_allowed))
    except Exception:
        pass

    return max(1, max_allowed)


def is_openai_endpoint(url):
    """
    Check if the URL points to an OpenAI-compatible endpoint (like vLLM or llama-server).
    """
    u = url.rstrip("/")
    return u.endswith("/v1") or os.environ.get("INFERENCE_FORMAT", "").lower() in ("openai", "vllm")


NUM_CTX = 8192


def model_options():
    """
    The options every request to the model carries.

    Ollama treats a change in num_ctx, num_thread or num_batch as a
    different runner and reloads the model - minutes on a CPU host. The
    preload in ensure_model_resident must therefore send exactly what a real
    request does, which is why both build it here.
    """

    return {
        # Caps the KV-cache Ollama allocates for this model.
        "num_ctx": NUM_CTX,
        # Structured marker generation temperature
        "temperature": 0.2,
        # Pin threads to physical cores to avoid cache thrashing on CPU
        "num_thread": get_optimal_threads(),
        # Prevent hallucinated chattering across turns
        "num_predict": NUM_PREDICT,
        # AVX2 batch processing size
        "num_batch": NUM_BATCH,
        # Stop sequence so the model stops immediately after its turn
        "stop": STOP_SEQUENCES,
    }


# How long a cold model load may take before the request gives up. Measured
# on the 8 GB Windows host this ships on: 170-310 s, because memory the VM
# has not touched recently comes back from Windows at 4-16 MB/s. It is a
# wait for a machine, not an error - so it gets its own generous budget
# instead of eating into the request's.
LOAD_WAIT_SECONDS = int(os.environ.get("OLLAMA_LOAD_WAIT_SECONDS", "900"))

# How often a wait for the model says so in the chat.
LOAD_ANNOUNCE_SECONDS = 30

# A model seen resident this recently is not asked about again.
_RESIDENT_RECHECK_SECONDS = 20
_resident_at = 0.0


def _get_json(path, timeout=5):
    request = urllib.request.Request(config.OLLAMA_URL.rstrip("/") + path)

    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def model_resident():
    """
    Whether the model is loaded in Ollama at the context size requests use
    (GET /api/ps). Unreadable counts as not resident: preloading a model
    that is already there is harmless.
    """

    try:
        models = _get_json("/api/ps").get("models") or []
    except Exception:
        return False

    want = config.OLLAMA_MODEL.removesuffix(":latest")

    for entry in models:
        names = {
            (entry.get("name") or "").removesuffix(":latest"),
            (entry.get("model") or "").removesuffix(":latest"),
        }

        if want in names:
            return entry.get("context_length") in (None, 0, NUM_CTX)

    return False


def ensure_model_resident():
    """
    Block until the model is loaded, telling the human why if it takes a
    while.

    The first request after the model was evicted (or after the machine
    was left alone for a long time) used to fail: Ollama queues it behind
    a load that takes minutes, and the request's 300 s timeout expired
    before a single token existed - "accepted the request but did not
    finish answering in time" - for a model that was simply still loading.
    The load is asked for here with an empty prompt, exactly as the control
    plane's own warm-up does, and waited for with its own budget.

    Raises RuntimeError if the model cannot be loaded at all (missing,
    server unreachable), which the caller reports like any other backend
    failure. Does nothing for an OpenAI-style endpoint, which has no
    notion of residency.
    """

    global _resident_at

    if is_openai_endpoint(config.OLLAMA_URL):
        return

    if time.monotonic() - _resident_at < _RESIDENT_RECHECK_SECONDS:
        return

    if model_resident():
        _resident_at = time.monotonic()
        return

    from flowera_agent import progress

    started = time.monotonic()
    log(f"Model {config.OLLAMA_MODEL} is not loaded - loading it.")
    progress.emit(
        "Loading the model into memory - the first request after a quiet "
        "spell takes a few minutes on this machine"
    )

    outcome = {}

    def load():
        body = json.dumps(
            {
                "model": config.OLLAMA_MODEL,
                "prompt": "",
                "keep_alive": KEEP_ALIVE,
                "stream": False,
                "options": {
                    "num_ctx": NUM_CTX,
                    "num_thread": get_optimal_threads(),
                    "num_batch": NUM_BATCH,
                },
            }
        ).encode("utf-8")

        request = urllib.request.Request(
            config.OLLAMA_URL.rstrip("/") + "/api/generate",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        try:
            with urllib.request.urlopen(request, timeout=LOAD_WAIT_SECONDS):
                pass
            outcome["ok"] = True
        except Exception as exc:
            outcome["error"] = exc

    loader = threading.Thread(target=load, daemon=True, name="model-load")
    loader.start()

    announced = started

    while loader.is_alive():
        loader.join(timeout=2)

        now = time.monotonic()

        if now - announced >= LOAD_ANNOUNCE_SECONDS:
            announced = now
            progress.emit(
                f"Still loading the model ({int(now - started)}s)..."
            )

        if now - started > LOAD_WAIT_SECONDS:
            raise RuntimeError(
                f"The model {config.OLLAMA_MODEL} did not finish loading "
                f"within {LOAD_WAIT_SECONDS} s"
            )

    if "error" in outcome:
        raise RuntimeError(
            f"Could not load {config.OLLAMA_MODEL} at "
            f"{config.OLLAMA_URL}: {outcome['error']}"
        )

    log(f"Model loaded after {int(time.monotonic() - started)}s.")
    _resident_at = time.monotonic()


def ollama_request(messages, num_predict=None):
    """
    Call the local Ollama server or configured OpenAI-compatible inference server.

    num_predict caps the reply for callers that only want the prompt read
    (see prime_prompt_cache).
    """

    if is_openai_endpoint(config.OLLAMA_URL):
        url = config.OLLAMA_URL.rstrip("/") + "/chat/completions"
        payload = {
            "model": config.OLLAMA_MODEL,
            "messages": messages,
            "temperature": 0.2,
            "max_tokens": NUM_PREDICT,
            "stop": STOP_SEQUENCES,
        }
        body = json.dumps(payload).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        api_key = os.environ.get("OLLAMA_API_KEY") or os.environ.get("OPENAI_API_KEY")
        if api_key:
            headers["Authorization"] = f"Bearer {api_key}"

        request = urllib.request.Request(url, data=body, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=REQUEST_TIMEOUT_SECONDS) as response:
                result = json.loads(response.read().decode("utf-8"))
                usage.set_model(config.OLLAMA_MODEL)
                usage_info = result.get("usage") or {}
                usage.add(
                    input_tokens=usage_info.get("prompt_tokens"),
                    output_tokens=usage_info.get("completion_tokens"),
                    cost_usd=0,
                )
                return result["choices"][0]["message"]["content"]
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"Could not reach inference server at {config.OLLAMA_URL}: {exc}"
            )
        except Exception as exc:
            raise RuntimeError(
                f"Inference request failed: {exc}"
            )

    # Native Ollama endpoint (/api/chat) with streaming and CPU thread tuning.
    #
    # A cold model is loaded first, patiently and out loud, so a slow load
    # is never mistaken for a slow answer by the timeout below.
    ensure_model_resident()

    payload = {
        "model": config.OLLAMA_MODEL,
        "messages": messages,
        "stream": True,
        "keep_alive": KEEP_ALIVE,
        "options": {
            **model_options(),
            **({"num_predict": num_predict} if num_predict else {}),
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

            content_chunks = []
            prompt_eval_count = 0
            eval_count = 0

            # Stream response chunks to avoid idle timeouts and buffer delay
            for raw_line in response:
                line = raw_line.strip()
                if not line:
                    continue
                try:
                    chunk = json.loads(line.decode("utf-8"))
                except Exception:
                    continue

                if "message" in chunk and "content" in chunk["message"]:
                    content_chunks.append(chunk["message"]["content"])

                if chunk.get("done"):
                    prompt_eval_count = chunk.get("prompt_eval_count", 0)
                    eval_count = chunk.get("eval_count", 0)
                    break

            content = "".join(content_chunks)

            usage.set_model(config.OLLAMA_MODEL)
            usage.add(
                input_tokens=prompt_eval_count,
                output_tokens=eval_count,
                cost_usd=0,
            )

            return content

    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"Could not reach Ollama at {config.OLLAMA_URL}: {exc}"
        )

    except Exception as exc:
        raise RuntimeError(
            f"Ollama request failed: {exc}"
        )


def prime_prompt_cache():
    """
    Have Ollama read this desktop's real system prompt now, while nobody is
    waiting, so the first request a person sends finds it cached.

    Measured on the host this ships on: the ~900-token prompt costs 75-200 s
    to evaluate from scratch and ~1 s once cached, and the cache survives
    exactly as long as nothing else replaces it. Reading it at startup moves
    that cost from the person's first request to the desktop's boot. It only
    ever sends the agent's own system messages - the same prefix every
    request starts with - and skips if a task is already running, so it can
    never replace the cache under one. Best-effort: never raises.
    """

    if is_openai_endpoint(config.OLLAMA_URL):
        return

    try:
        started = time.monotonic()
        ensure_model_resident()

        from flowera_agent import state

        if state.current_task:
            return

        ollama_request(
            [
                *build_system_messages(),
                {"role": "user", "content": "Reply with DONE: ready"},
            ],
            num_predict=4,
        )
        log(f"Prompt cache primed in {int(time.monotonic() - started)}s.")

    except Exception as exc:
        log(f"Could not prime the prompt cache (harmless): {exc}")


def ask_once(system, prompt):
    """
    One question to the model, with no loop and no history: what the
    computer layer's grounder and judge (computer/escalation.py) use. Keep the
    system text fixed from call to call - Ollama's prompt cache carries it.
    """

    return ollama_request([
        {"role": "system", "content": system},
        {"role": "user", "content": prompt},
    ])


def wait_for_ollama():
    """
    Wait until Ollama or the configured inference server becomes available.

    Runs on a background thread (see app.py and harness.wait_for_backend),
    so it never gates the agent's own :8081 listener - it only decides when
    queued work may start.
    """

    if is_openai_endpoint(config.OLLAMA_URL):
        health_url = config.OLLAMA_URL.rstrip("/") + "/models"
        server_desc = f"Inference server at {config.OLLAMA_URL}"
    else:
        health_url = config.OLLAMA_URL.rstrip("/") + "/api/tags"
        server_desc = f"Ollama at {config.OLLAMA_URL}"

    wait_for_http_ok(health_url, server_desc)


# ============================================================
# Agent reasoning
# ============================================================

# Deliberately short.
#
# The previous version of this prompt was 637 tokens and spent five
# separate numbered rules (6, 7, 8, 14, 15) forbidding the same mistake -
# asking a human a query instead of answering a question. It still made
# it: "hello, whats your desktop name?" came back as a query that
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
send a command that just failed. Write the command directly without
markdown code blocks (```) or backticks.

HELP - you are blocked and no command can unblock you: the human has to
choose between real alternatives, supply a secret, or fix something
outside this machine. Not knowing the answer yet is not being blocked -
run a command and find out. A question is never a reason to ask for
help, and never ask the human why they want something.

HABITS THAT KEEP COMMANDS WORKING. Keep a command simple, and read-only
when you are only looking. Do not add a grep or awk filter you have to
guess at: one that matches nothing makes the command fail with no output.
Never write DONE beside a COMMAND - you have not seen its output yet. When
the output answers the question, reply DONE with the answer in plain words,
taken from the output, in one or two short sentences. Never tell the human
to run a command or to change directory: run it yourself. To make a file,
write it with a COMMAND (cat > name <<'EOF' ... EOF), then run it.

Reliable commands: free disk space: df -h /   (read the Avail column of the
line for /). Files in a folder: ls -A DIR | wc -l. Largest file: ls -S DIR |
head -1. Public IP: curl -s ifconfig.me. Make a small file in one command:
printf 'line1\nline2\n' > name  or  python3 -c "..." for a one-off script.

THE SCREEN. This desktop also has windows: Chromium, LibreOffice, a text
editor. Work in them with the gui command, one per COMMAND:
gui observe             lists windows and elements, each with a ref like e12
gui click e12           clicks it
gui type e12 "text"     replaces a field's text
gui key ctrl+s          presses keys (enter, tab, ctrl+a ...)
gui select e12 "High"   picks a dropdown option
gui open <url|file|app> opens it
gui recordings <words>  finds a recorded demonstration of a task
gui replay <id>         runs that recording again, step by step
gui skills <words>      finds a task this desktop was taught
gui do <name> --param k=v   does it with no model (gui skill <name> says what it needs)
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

    Shared by perform_task and continue_after_query so both halves of a
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

    messages.append(
        {
            "role": "user",
            "content": task,
        }
    )

    messages.extend(history)

    return ollama_request(messages)
