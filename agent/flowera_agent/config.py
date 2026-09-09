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
    "qwen3:0.6b"
)

AGENT_HARNESS = os.environ.get(
    "AGENT_HARNESS",
    "ollama"
).strip().lower()

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
