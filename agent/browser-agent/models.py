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

# Same convention as flowera_agent/thebot's soul: OLLAMA_URL is set on the
# container to the control plane's host (http://host.docker.internal:11434);
# outside a container (e.g. running this standalone) it just falls back to
# the local machine's own Ollama.
OLLAMA = os.environ.get("OLLAMA_URL", "http://127.0.0.1:11434")

MODELS = {
    "devstral": {"label": "Devstral (24B)", "name": "devstral-small-2:24b", "vision": True},
    "qwen3": {"label": "Qwen3 (14B)", "name": "qwen3:14b", "vision": False},
    "gemma": {"label": "Gemma 4 (26B)", "name": "gemma4:26b", "vision": False},
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
    """True if calling this model right now would evict a different one."""
    target = MODELS[model_key]["name"]
    resident = resident_model_names()
    return bool(resident) and target not in resident


def chat(model_key, messages, images=None, num_ctx=DEFAULT_NUM_CTX, temperature=0, max_tokens=400):
    full = ""
    for chunk in chat_stream(model_key, messages, images=images, num_ctx=num_ctx,
                              temperature=temperature, max_tokens=max_tokens):
        if chunk["type"] == "final":
            full = chunk["text"]
    return full


def chat_stream(model_key, messages, images=None, num_ctx=DEFAULT_NUM_CTX, temperature=0, max_tokens=400):
    """Yields {"type": "chunk", "text": <piece>} as tokens arrive, then one final
    {"type": "final", "text": <full text>} once Ollama reports done -- lets the
    caller show generation happening live instead of a silent wait."""
    cfg = MODELS[model_key]
    if images:
        messages = [*messages[:-1], {**messages[-1], "images": images}]

    resp = requests.post(
        f"{OLLAMA}/api/chat",
        json={
            "model": cfg["name"],
            "messages": messages,
            "stream": True,
            "think": False,  # qwen3/gemma4 are hybrid-reasoning models; we want plain output
            "options": {"num_ctx": num_ctx, "temperature": temperature, "num_predict": max_tokens},
        },
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
