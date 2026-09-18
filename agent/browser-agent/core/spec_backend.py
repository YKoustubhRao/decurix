"""Lifecycle + chat_stream for the speculative-decoding backend: a separate
llama-server process running Qwen3-14B as target with Qwen3-0.6B as draft
(same pair verified/staged in ../spec-decode/, now wired in as a selectable
model here). Reuses Ollama's own GGUF blobs directly -- no re-download.

Ollama and this llama-server process both want the whole 16GB card, so only
one can be resident at a time: ensure_started() evicts every Ollama-resident
model before booting llama-server, and models.py calls stop_if_running()
before every plain-Ollama call so switching back also frees the VRAM this
process was holding.

Context is kept at 40960 (same as every other model in this agent, since
codegen/atomizer/reasoning all feed the full cleaned page HTML) via q8_0
KV-cache quantization rather than the default f16 -- f16 at this context
length would need ~6GB for the target alone, which doesn't leave room for
the 9.3GB of Q4 weights plus the draft model on a 16GB card.
"""
import json
import subprocess
import time

import requests

LLAMA_SERVER = r"C:\Users\dshek\tools\llama.cpp-b10948\llama-server.exe"
TARGET_GGUF = r"C:\Users\dshek\.ollama\models\blobs\sha256-a8cc1361f3145dc01f6d77c6c82c9116b9ffe3c97b34716fe20418455876c40e"
DRAFT_GGUF = r"C:\Users\dshek\.ollama\models\blobs\sha256-7f4030143c1c477224c5434f8272c662a8b042079a0a584f0a27a1684fe2e1fa"
PORT = 8091
CTX_SIZE = 40960
DRAFT_MAX = 5
OLLAMA = "http://127.0.0.1:11434"

_proc = None


def _health():
    try:
        r = requests.get(f"http://127.0.0.1:{PORT}/health", timeout=2)
        return r.status_code == 200
    except Exception:
        return False


def is_running():
    global _proc
    return _proc is not None and _proc.poll() is None and _health()


def stop_if_running():
    global _proc
    if _proc is not None and _proc.poll() is None:
        _proc.terminate()
        try:
            _proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            _proc.kill()
    _proc = None


def _free_ollama_vram():
    try:
        resp = requests.get(f"{OLLAMA}/api/ps", timeout=5)
        resp.raise_for_status()
        names = [m["name"] for m in resp.json().get("models", [])]
    except Exception:
        return
    for name in names:
        try:
            requests.post(f"{OLLAMA}/api/generate", json={"model": name, "keep_alive": 0}, timeout=10)
        except Exception:
            pass


def _wait_for_free_vram(min_free_mib=11000, timeout_s=15):
    """Best-effort: give Ollama's unload a moment before grabbing the same
    VRAM ourselves. If it never frees in time we still try to start --
    llama-server's own CUDA allocation failure is a clear enough error, and
    the caller (models.chat_stream) already surfaces exceptions as a paused
    step_failed rather than needing this to be airtight."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            out = subprocess.run(
                ["nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits"],
                capture_output=True, text=True, timeout=5,
            )
            if int(out.stdout.strip().splitlines()[0]) >= min_free_mib:
                return
        except Exception:
            return
        time.sleep(1)


def ensure_started():
    global _proc
    if is_running():
        return
    stop_if_running()
    _free_ollama_vram()
    _wait_for_free_vram()

    _proc = subprocess.Popen(
        [LLAMA_SERVER,
         "--model", TARGET_GGUF, "--model-draft", DRAFT_GGUF,
         "--spec-type", "draft-simple",  # NOT optional: --model-draft alone loads the
                                          # draft weights but never wires them into
                                          # generation ("no implementations specified for
                                          # speculative decoding" in the logs, every slot
                                          # stays "speculative":false) without this
         "--gpu-layers", "all", "--gpu-layers-draft", "all",
         "--ctx-size", str(CTX_SIZE), "--spec-draft-n-max", str(DRAFT_MAX),
         "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
         # the draft gets its OWN full-context KV cache too (ctx_dft=yes) --
         # quantize it as well, or VRAM gets so tight (measured: 199MB free
         # out of 16GB) that Windows' WDDM driver likely starts silently
         # paging GPU memory to system RAM, which is disastrous for latency
         "--spec-draft-type-k", "q8_0", "--spec-draft-type-v", "q8_0",
         "--jinja", "--reasoning", "off",  # match Ollama's "think": False -- codegen/atomizer
         "--port", str(PORT)],             # need the plain-text format, not a <think> block first
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    deadline = time.time() + 90
    while time.time() < deadline:
        if _proc.poll() is not None:
            out = _proc.stdout.read()
            _proc = None
            raise RuntimeError(f"speculative-decoding server exited during startup:\n{out[-2000:]}")
        if _health():
            return
        time.sleep(1)
    stop_if_running()
    raise RuntimeError("speculative-decoding server did not become healthy within 90s")


def chat_stream(messages, max_tokens=400, temperature=0, response_format=None):
    """Same {"type": "chunk"/"final", "text": ...} shape as models.chat_stream
    (chunk carries the incremental piece, matching that function's existing
    contract) so callers don't need to care which backend they're on."""
    ensure_started()
    body = {
        "model": "qwen3-spec",
        "messages": messages,
        "stream": True,
        "stream_options": {"include_usage": True},
        "temperature": temperature,
        "max_tokens": max_tokens,
    }
    if response_format is not None:
        # llama-server's OpenAI-compatible endpoint takes the JSON schema
        # nested under response_format.json_schema.schema (started with
        # --jinja, so it can grammar-constrain like Ollama's `format` does).
        body["response_format"] = {
            "type": "json_schema",
            "json_schema": {"name": "step", "schema": response_format, "strict": True},
        }
    resp = requests.post(f"http://127.0.0.1:{PORT}/v1/chat/completions", json=body,
                          timeout=180, stream=True)
    resp.raise_for_status()

    full = ""
    usage = {}
    for line in resp.iter_lines():
        if not line or not line.startswith(b"data: "):
            continue
        payload = line[len(b"data: "):]
        if payload.strip() == b"[DONE]":
            break
        data = json.loads(payload)
        if data.get("usage"):
            usage = data["usage"]
        for choice in data.get("choices", []):
            piece = choice.get("delta", {}).get("content") or ""
            if piece:
                full += piece
                yield {"type": "chunk", "text": piece}
    yield {"type": "final", "text": full, "usage": usage}
