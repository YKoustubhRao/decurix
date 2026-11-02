import os
import socket
from pathlib import Path


# ============================================================
# Configuration
# ============================================================

DESKTOP_ID = os.environ.get("DESKTOP_ID", "")

DESKTOP_NAME = os.environ.get(
    "DESKTOP_NAME",
    socket.gethostname()
)

CONTROL_PLANE_URL = os.environ.get(
    "CONTROL_PLANE_URL",
    "http://host.docker.internal:8080"
)

OLLAMA_URL = os.environ.get(
    "OLLAMA_URL",
    "http://decurix-ollama:11434"
)

OLLAMA_MODEL = os.environ.get(
    "OLLAMA_MODEL",
    "qwen2.5-coder:3b"
)

AGENT_HARNESS = os.environ.get(
    "AGENT_HARNESS",
    "ollama"
).strip().lower()

# The model TheBot reasons with (desktop.Brain on the control plane): "ollama"
# (the shared local model), or a hosted one - "claude" or "azure" - reached
# through the control plane's /api/llm proxy, which holds the key. Desktops
# created before brains existed have none, which is the local model.
BRAIN = os.environ.get("DECURIX_BRAIN", "ollama").strip().lower() or "ollama"
BRAIN_MODEL = os.environ.get("DECURIX_BRAIN_MODEL", "").strip()
HOSTED_BRAIN = BRAIN in ("claude", "azure", "openai")

# Set only for a desktop spawned as another desktop's specialized child (see
# httpapi.createDesktopHandler / desktop.Manager.Create) - "" for an ordinary,
# standalone desktop.
DESKTOP_SPECIALIZATION = os.environ.get("DESKTOP_SPECIALIZATION", "").strip()
DESKTOP_PARENT_ID = os.environ.get("DESKTOP_PARENT_ID", "").strip()

# TheBot harness only: the soul binary (agent/soul, started as a
# background daemon by start_agent_terminal.sh when AGENT_HARNESS=thebot)
# listens on this port inside the container - not exposed outside it, same
# as vm_server.py's own :8081.
THEBOT_URL = os.environ.get(
    "THEBOT_URL",
    "http://127.0.0.1:4900"
)

# Where the control plane reaches this agent. A desktop container is a machine
# of its own, so every agent there has :8081 on every interface. Native Windows
# workers all share one machine: the control plane gives each a port of its
# own, on loopback (a listener on every interface makes Windows Firewall ask
# the person a question).
AGENT_BIND = os.environ.get("DECURIX_AGENT_BIND", "0.0.0.0")
AGENT_PORT = int(os.environ.get("DECURIX_AGENT_PORT", "8081"))

# Everything the agent keeps for itself lives under one hidden directory, so
# the home a human browses on the desktop holds only the work directory.
# start_agent_terminal.sh moves these in from where older desktops kept them.
_default_base = Path("/home/headless") if Path("/home/headless").exists() else Path.home()
DECURIX_DIR = Path(os.environ.get("DECURIX_DIR", _default_base / ".decurix"))

COMMAND_DIR = DECURIX_DIR / "command"
QUERYTEXT_DIR = DECURIX_DIR / "querytext"
FINAL_DIR = DECURIX_DIR / "final"
MEMORY_DIR = DECURIX_DIR / "memory"
WORK_DIR = Path(os.environ.get("DECURIX_WORK_DIR", _default_base / "work"))

# How much persisted worker memory (every *.txt file under MEMORY_DIR,
# concatenated - see memory_context.get_memory_context) is worth injecting
# into every harness's system prompt. Nothing prunes MEMORY_DIR, and a
# linked/shared desktop or a specialized child spawned from a parent can
# copy another desktop's whole memory history in at once, so this is what
# keeps that system prefix from growing without bound as memory
# accumulates - unlike SYSTEM_PROMPT/identity, whose byte length is fixed.
# Ollama's num_ctx is a fixed 8192 everywhere (see ollama_client's
# build_system_messages) and prompt evaluation, not generation, is what
# makes a CPU-only desktop feel slow, so this budget matters the same way
# DECURIX_GUI_TEXT_CHARS does for a `gui` command's output.
MEMORY_CONTEXT_CHARS = int(os.environ.get("DECURIX_MEMORY_CONTEXT_CHARS", "6000"))

try:
    for _folder in (COMMAND_DIR, QUERYTEXT_DIR, FINAL_DIR, WORK_DIR, MEMORY_DIR):
        _folder.mkdir(parents=True, exist_ok=True)
except OSError:
    pass
