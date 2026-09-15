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
    "http://flowera-ollama:11434"
)

OLLAMA_MODEL = os.environ.get(
    "OLLAMA_MODEL",
    "qwen2.5-coder:3b"
)

AGENT_HARNESS = os.environ.get(
    "AGENT_HARNESS",
    "ollama"
).strip().lower()

# TheBot harness only: the soul binary (agent/soul, started as a
# background daemon by start_agent_terminal.sh when AGENT_HARNESS=thebot)
# listens on this port inside the container - not exposed outside it, same
# as vm_server.py's own :8081.
THEBOT_URL = os.environ.get(
    "THEBOT_URL",
    "http://127.0.0.1:4900"
)

# Everything the agent keeps for itself lives under one hidden directory, so
# the home a human browses on the desktop holds only the work directory.
# start_agent_terminal.sh moves these in from where older desktops kept them.
FLOWERA_DIR = Path("/home/headless/.flowera")

COMMAND_DIR = FLOWERA_DIR / "command"
QUERYTEXT_DIR = FLOWERA_DIR / "querytext"
FINAL_DIR = FLOWERA_DIR / "final"
MEMORY_DIR = FLOWERA_DIR / "memory"
WORK_DIR = Path("/home/headless/work")

COMMAND_DIR.mkdir(parents=True, exist_ok=True)
QUERYTEXT_DIR.mkdir(parents=True, exist_ok=True)
FINAL_DIR.mkdir(parents=True, exist_ok=True)
WORK_DIR.mkdir(parents=True, exist_ok=True)
MEMORY_DIR.mkdir(parents=True, exist_ok=True)
