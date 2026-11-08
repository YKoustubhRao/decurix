import json
import os
import threading
import time
import urllib.error
import urllib.request

from decurix_agent import config, soul_call, usage
from decurix_agent.logging_utils import log


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

    from decurix_agent import progress, state

    # Said in the chat only while someone is waiting on a task. This also runs
    # as the agent starts (the backend is waited for before any request), and
    # a step with no task opens a turn of its own: found live, a worker that
    # had just been created showed "Working on: <the last request>" on its
    # card, over a model load nobody had asked anything of.
    say = progress.emit if state.current_task else (lambda line: None)

    started = time.monotonic()
    log(f"Model {config.OLLAMA_MODEL} is not loaded - loading it.")
    say(
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
            say(f"Still loading the model ({int(now - started)}s)...")

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

    num_predict caps the reply.
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


def ask_once(system, prompt, *, purpose):
    """
    One question to the model, with no loop and no history: what the
    computer layer's grounder and judge (computer/escalation.py) use. Keep the
    system text fixed from call to call - Ollama's prompt cache carries it.
    `purpose` (what the question is for) is logged with the call: no proxy sees
    a local model's calls, so the log is their only record of it.
    """

    log(f"ollama: one question ({purpose})")
    messages = [
        {"role": "system", "content": system},
        {"role": "user", "content": prompt},
    ]
    # Soul asks the model when it can (soul_call, plan rule 3): through its own Ollama provider, its concurrency limit
    # (one call at a time on a CPU host, so this question no longer evicts the prefix soul's own turn had loaded) and
    # its trace - which, for a local model the proxy never sees, is the only record of the call. The same options as
    # ollama_request's: num_ctx 8192, temperature 0.2, NUM_PREDICT, STOP_SEQUENCES.
    if soul_call.usable():
        ensure_model_resident()  # a cold model is loaded first, patiently and out loud, as ollama_request does
        try:
            answer = soul_call.call("local", messages, purpose=purpose, max_tokens=NUM_PREDICT, temperature=0.2,
                                    stop=STOP_SEQUENCES, timeout=REQUEST_TIMEOUT_SECONDS)
        except soul_call.Unusable:
            pass  # asked the way it always was, below
        except soul_call.Failed as exc:
            raise RuntimeError(f"Ollama request failed (through soul): {exc}")
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"Could not reach soul for the model: {exc}")
        else:
            spent = answer.get("usage") or {}
            usage.set_model(config.OLLAMA_MODEL)
            usage.add(input_tokens=spent.get("prompt_tokens"), output_tokens=spent.get("completion_tokens"),
                      cost_usd=0)  # counted here, once: soul counts a call it makes for the agent in no job
            return answer["text"]
    return ollama_request(messages)
