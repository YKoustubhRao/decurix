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

COMMAND_DIR = Path("/home/headless/command")
HELPTEXT_DIR = Path("/home/headless/helptext")
FINAL_DIR = Path("/home/headless/final")
WORK_DIR = Path("/home/headless/work")
MEMORY_DIR = Path("/home/headless/memory")

COMMAND_DIR.mkdir(parents=True, exist_ok=True)
HELPTEXT_DIR.mkdir(parents=True, exist_ok=True)
FINAL_DIR.mkdir(parents=True, exist_ok=True)
WORK_DIR.mkdir(parents=True, exist_ok=True)
MEMORY_DIR.mkdir(parents=True, exist_ok=True)
