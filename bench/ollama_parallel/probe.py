"""
Do two desktops' prompts evict each other's cached prefix in Ollama, and does
OLLAMA_NUM_PARALLEL=2 stop that?

Each desktop sends its own system prompt. With one slot, Ollama keeps one
evaluated prefix: desktop A, then B, then A again re-reads A's prompt from
scratch - on this repo's CPU host that is minutes (see CLAUDE.md's prompt-cache
notes). Ollama reports how many prompt tokens it had to evaluate
(`prompt_eval_count`), so the answer is read off the third call, not timed.

Runs a PRIVATE Ollama on its own port against the installed models, never the
shared one the desktops use (probing that would evict their cache), with a
tiny model and a small context so it costs a few hundred MB:

    python3 bench/ollama_parallel/probe.py [--model qwen3:0.6b] [--port 11435]
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request

MODELS_DIR = "/usr/share/ollama/.ollama/models"


def call(port, model, system, ctx):
    body = json.dumps({
        "model": model, "stream": False, "keep_alive": "10m",
        "options": {"num_ctx": ctx, "num_predict": 1, "num_thread": 2, "temperature": 0},
        "messages": [{"role": "system", "content": system}, {"role": "user", "content": "Reply OK."}],
    }).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}/api/chat", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        d = json.load(r)
    return d.get("prompt_eval_count", 0), d.get("prompt_eval_duration", 0) / 1e9


def prompt(tag):
    # ~500 tokens, different from the first word on so no prefix is shared.
    line = f"You are desktop {tag}. Rule {{}}: always answer as desktop {tag}, briefly and exactly.\n"
    return "".join(line.format(i) for i in range(40))


def run(parallel, model, port, ctx):
    env = dict(os.environ, OLLAMA_HOST=f"127.0.0.1:{port}", OLLAMA_MODELS=MODELS_DIR,
               OLLAMA_NUM_PARALLEL=str(parallel), OLLAMA_MAX_LOADED_MODELS="1",
               OLLAMA_NOPRUNE="1", OLLAMA_KEEP_ALIVE="10m")
    server = subprocess.Popen(["nice", "-n", "10", shutil.which("ollama"), "serve"], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(60):
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{port}/api/version", timeout=2)
                break
            except OSError:
                time.sleep(1)
        a, b = prompt("A"), prompt("B")
        call(port, model, "warm up " + prompt("W"), ctx)  # load the model once
        results = [("A", *call(port, model, a, ctx)), ("B", *call(port, model, b, ctx)),
                   ("A again", *call(port, model, a, ctx))]
        return results
    finally:
        server.terminate()
        server.wait(timeout=30)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="qwen3:0.6b")
    ap.add_argument("--port", type=int, default=11435)
    ap.add_argument("--ctx", type=int, default=2048)
    args = ap.parse_args()

    for parallel in (1, 2):
        rows = run(parallel, args.model, args.port, args.ctx)
        print(f"OLLAMA_NUM_PARALLEL={parallel}")
        for name, tokens, secs in rows:
            print(f"  {name:8} evaluated {tokens:4} prompt tokens in {secs:5.2f}s")
        again, first = rows[2][1], rows[0][1]
        print(f"  -> A's prefix {'SURVIVED' if again < first / 2 else 'was EVICTED'} by B\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
