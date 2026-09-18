"""Model abstraction over Ollama's NATIVE /api/chat endpoint (not the OpenAI shim,
which silently caps context at ~4096 and ignores num_ctx -- see doc-ui/server.py
for the same lesson learned earlier in this workspace).

Ollama owns model loading/eviction/VRAM management, so switching models in the
UI is just changing which `model` name goes in the request -- no process
juggling like the raw llama-server path Phase 1 used.
"""
import json
import os

import requests

import spec_backend

# Same convention as flowera_agent/ollama_client.py: OLLAMA_URL is set on
# every desktop container to the control plane's host
# (http://host.docker.internal:11434); outside a container (e.g. running
# this standalone for research) it falls back to the local machine's own
# Ollama.
OLLAMA = os.environ.get("OLLAMA_URL", "http://127.0.0.1:11434")

MODELS = {
    "devstral": {"label": "Devstral (24B)", "name": "devstral-small-2:24b", "vision": True},
    "qwen3": {"label": "Qwen3 (14B)", "name": "qwen3:14b", "vision": False},
    "gemma": {"label": "Gemma 4 (26B)", "name": "gemma4:26b", "vision": False},
    "qwen3-0.6b": {"label": "Qwen3 (0.6B)", "name": "qwen3:0.6b", "vision": False},
    "llama3.2-1b": {"label": "Llama 3.2 (1B)", "name": "llama3.2:1b", "vision": False},
    "qwen2.5-3b": {"label": "Qwen2.5 (3B)", "name": "qwen2.5:3b", "vision": False},
    "yi-6b": {"label": "Yi (6B)", "name": "yi:6b", "vision": False},
    "qwen2.5-coder-14b": {"label": "Qwen2.5-Coder (14B)", "name": "qwen2.5-coder:14b", "vision": False},
    "qwen2.5-coder-7b": {"label": "Qwen2.5-Coder (7B)", "name": "qwen2.5-coder:7b", "vision": False},
    "qwen3-spec": {
        "label": "Qwen3 (14B, speculative decoding w/ 0.6B draft)",
        "name": "qwen3:14b+draft:0.6b", "vision": False, "backend": "spec",
    },
}
DEFAULT_MODEL = "devstral"
# Bumped well past the earlier 8192/12288 steps: codegen now gets the page's full
# (cleaned) HTML rather than a filtered element list, specifically so nothing
# structurally relevant gets excluded before the model ever sees it (the Bing
# failure was the model guessing a URL pattern because the real "Videos" link
# didn't match our narrow selector list -- full HTML means nothing gets pre-filtered
# out that way). 40k lets most real pages fit uncut; slower prompt processing is an
# accepted tradeoff for the accuracy this buys.
DEFAULT_NUM_CTX = 40960

# Updated after every call so /stats can report it without re-querying Ollama.
LAST_USAGE = {"model": None, "prompt_tokens": None, "completion_tokens": None, "num_ctx": None}


def resident_model_names():
    """Ollama models currently loaded in VRAM, right now -- used to tell the UI
    when a call is about to trigger a swap (this 16GB card only fits one of
    these 14-26B models at a time, so switching always evicts the other)."""
    try:
        resp = requests.get(f"{OLLAMA}/api/ps", timeout=5)
        resp.raise_for_status()
        return {m["name"] for m in resp.json().get("models", [])}
    except Exception:
        return set()


def will_swap(model_key):
    """True if calling this model right now would evict a different one --
    covers both directions between Ollama and the speculative-decoding
    llama-server backend, since they fight over the same 16GB card and only
    one can be resident at a time (see spec_backend.py)."""
    cfg = MODELS[model_key]
    if cfg.get("backend") == "spec":
        return not spec_backend.is_running()
    resident = resident_model_names()
    return spec_backend.is_running() or (bool(resident) and cfg["name"] not in resident)


def chat(model_key, messages, images=None, num_ctx=DEFAULT_NUM_CTX, temperature=0, max_tokens=400,
         response_format=None):
    full = ""
    for chunk in chat_stream(model_key, messages, images=images, num_ctx=num_ctx,
                              temperature=temperature, max_tokens=max_tokens,
                              response_format=response_format):
        if chunk["type"] == "final":
            full = chunk["text"]
    return full


def chat_stream(model_key, messages, images=None, num_ctx=DEFAULT_NUM_CTX, temperature=0, max_tokens=400,
                 response_format=None):
    """Yields {"type": "chunk", "text": <piece>} as tokens arrive, then one final
    {"type": "final", "text": <full text>} once done -- lets the caller show
    generation happening live instead of a silent wait. Routes to the
    speculative-decoding llama-server backend for "qwen3-spec"; every other
    model_key goes to Ollama as before (first freeing the spec backend's VRAM
    if it's currently the one resident).

    response_format, if given, is a JSON-schema dict: Ollama's `format` param
    (and llama-server's OpenAI-compatible `response_format`) both grammar-
    constrain decoding to match it, so the model CANNOT emit invalid JSON --
    this replaces free-text line-parsing (NEXT:/REMAINING:/ACTION: etc) with a
    guaranteed-parseable response for callers that opt in."""
    cfg = MODELS[model_key]
    if cfg.get("backend") == "spec":
        for evt in spec_backend.chat_stream(messages, max_tokens=max_tokens, temperature=temperature,
                                              response_format=response_format):
            if evt["type"] == "final":
                usage = evt.get("usage") or {}
                LAST_USAGE.update({
                    "model": cfg["name"],
                    "prompt_tokens": usage.get("prompt_tokens"),
                    "completion_tokens": usage.get("completion_tokens"),
                    "num_ctx": spec_backend.CTX_SIZE,
                })
            yield {"type": evt["type"], "text": evt["text"]}
        return

    spec_backend.stop_if_running()
    if images:
        messages = [*messages[:-1], {**messages[-1], "images": images}]

    body = {
        "model": cfg["name"],
        "messages": messages,
        "stream": True,
        "think": False,  # qwen3/gemma4 are hybrid-reasoning models; we want plain output
        "options": {"num_ctx": num_ctx, "temperature": temperature, "num_predict": max_tokens},
    }
    if response_format is not None:
        body["format"] = response_format

    resp = requests.post(
        f"{OLLAMA}/api/chat",
        json=body,
        timeout=180,
        stream=True,
    )
    resp.raise_for_status()

    full = ""
    for line in resp.iter_lines():
        if not line:
            continue
        data = json.loads(line)
        piece = data.get("message", {}).get("content", "")
        if piece:
            full += piece
            yield {"type": "chunk", "text": piece}
        if data.get("done"):
            LAST_USAGE.update({
                "model": cfg["name"],
                "prompt_tokens": data.get("prompt_eval_count"),
                "completion_tokens": data.get("eval_count"),
                "num_ctx": num_ctx,
            })
            yield {"type": "final", "text": full}
