"""
The brain a benchmark desktop runs on, the way a real one is created.

Every desktop runs TheBot now; what differs is its brain (Ollama, Claude,
Azure). A benchmark that starts its own container passes `--brain azure` (or
claude) to get the environment desktop.buildDesktopEnv gives a real desktop
with that brain: TheBot's OpenAI-compatible provider pointed at a control
plane's /api/llm/<brain> proxy, which holds the key. That control plane must
be running from this tree (127.0.0.1:8080 by default, BENCH_CONTROL_PLANE to
change it) with the brain set up in its New desktop dialog.

    from brain import chosen, docker_env, forward_llm

`--broker` starts the desktop in broker mode, as a control plane run with
DECURIX_SCREEN_BROKER=1 creates one (internal/desktop/screenbroker.go): the
screen is the screen user's and the worker reaches it only through the screen
broker (docs/gate-boundary.md, "Next: the input broker").

    from brain import broker, screen_env
"""

import json
import os
import sys
import urllib.error
import urllib.request

CONTROL_PLANE = os.environ.get("BENCH_CONTROL_PLANE", "http://127.0.0.1:8080")


def chosen(argv=None):
    """
    The --brain given on the command line ("ollama", "claude" or "azure"), or
    None: the benchmark then starts its desktop exactly as it always did, on
    the old model-free settings - TheBot on the Ollama brain would load the
    local model at startup, minutes on a small host, for a run that never
    asks it anything.
    """

    argv = sys.argv if argv is None else argv
    for i, arg in enumerate(argv):
        if arg == "--brain" and i + 1 < len(argv):
            return argv[i + 1].strip().lower()
        if arg.startswith("--brain="):
            return arg.split("=", 1)[1].strip().lower()
    return None


def broker(argv=None):
    """True when --broker was given: the desktop is started in broker mode."""

    return "--broker" in (sys.argv if argv is None else argv)


def screen_env(argv=None):
    """
    `docker run -e` arguments for the screen mode: in broker mode exactly what the control plane adds to a new
    desktop's environment (desktop.screenBrokerEnv: DECURIX_SCREEN_USER=1), nothing otherwise.
    """

    return ["-e", "DECURIX_SCREEN_USER=1"] if broker(argv) else []


def status(brain):
    """What the control plane says about a hosted brain; exits when it is not set up."""

    try:
        with urllib.request.urlopen(CONTROL_PLANE + "/api/llm", timeout=10) as r:
            info = json.loads(r.read())
    except OSError as exc:
        raise SystemExit(f"--brain {brain} needs this tree's control plane at {CONTROL_PLANE}: {exc}")
    st = (info.get("providers") or {}).get(brain) or {}
    if not st.get("configured"):
        raise SystemExit(f"{brain} is not set up on {CONTROL_PLANE}: add its key in the New desktop dialog first")
    return st


def docker_env(brain, control_plane_url):
    """`docker run -e` arguments for a desktop on `brain`, calling back to control_plane_url."""

    if brain is None:
        return ["-e", "AGENT_HARNESS=ollama"]
    if brain == "ollama":
        return ["-e", "AGENT_HARNESS=thebot", "-e", "DECURIX_BRAIN=ollama"]
    if brain not in ("claude", "azure"):
        raise SystemExit(f"unknown brain {brain!r}: ollama, claude or azure")

    st = status(brain)
    env = {
        "AGENT_HARNESS": "thebot",
        "DECURIX_BRAIN": brain,
        "DECURIX_BRAIN_MODEL": st.get("model") or "",
        "DECURIX_BRAIN_VISION": "1" if st.get("vision") else "0",
        "LLM_PROVIDER": "openai",
        "OPENAI_BASE_URL": control_plane_url.rstrip("/") + "/api/llm/" + brain,
        "OPENAI_API_KEY": "held-by-the-control-plane",
        "OPENAI_MODEL": "default",
        "EMBED_PROVIDER": "ollama",
        "DECURIX_LLM": brain,
        "THEBOT_WEB_PAGES": "3",
        "THEBOT_WEB_PAGE_CHARS": "6000",
    }
    out = []
    for k, v in env.items():
        out += ["-e", f"{k}={v}"]
    return out


def forward_llm(handler, body):
    """
    Pass a desktop's /api/llm/... call on to the real control plane, for a
    benchmark whose own fake control plane catches everything else. Returns
    True when it was such a call (and has been answered).
    """

    if not handler.path.startswith("/api/llm/"):
        return False
    req = urllib.request.Request(CONTROL_PLANE + handler.path, data=body if handler.command == "POST" else None,
                                 method=handler.command, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            status, data, headers = r.status, r.read(), r.headers
    except urllib.error.HTTPError as exc:
        status, data, headers = exc.code, exc.read(), exc.headers
    except OSError as exc:
        status, data, headers = 502, str(exc).encode(), {}
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    for name in ("Retry-After", "Retry-After-Ms"):
        if headers.get(name):
            handler.send_header(name, headers.get(name))
    handler.end_headers()
    handler.wfile.write(data)
    return True
