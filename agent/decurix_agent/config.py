import os
import socket
import stat
import sys
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

# The local chat model. The control plane always passes OLLAMA_MODEL to a
# desktop (internal/ollama's default, devstral-small-2:24b, unless .env names
# another), so this fallback only matters for an agent started by hand; it
# matches the control plane's default, as soul's and start_agent_terminal.sh's do.
OLLAMA_MODEL = os.environ.get(
    "OLLAMA_MODEL",
    "devstral-small-2:24b"
)

# The engine that drives tasks: TheBot, the only one since 2026-11-07. A
# desktop created before then may carry another AGENT_HARNESS (ollama, claude,
# azure); that is kept as CREATED_HARNESS, for harness.py to say so in the log,
# and the desktop runs TheBot all the same.
CREATED_HARNESS = os.environ.get("AGENT_HARNESS", "thebot").strip().lower() or "thebot"
AGENT_HARNESS = "thebot"

# The model TheBot reasons with (desktop.Brain on the control plane): "ollama"
# (the shared local model), or a hosted one - "claude", "azure" or "openai"
# (any OpenAI-compatible server the control plane was given) - reached
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

# The soul binary (/opt/soul, built from agent/thebot; started as a
# background daemon by start_agent_terminal.sh when AGENT_HARNESS=thebot)
# listens on this port inside the container - not exposed outside it, same
# as vm_server.py's own :8081.
THEBOT_URL = os.environ.get(
    "THEBOT_URL",
    "http://127.0.0.1:4900"
)
# The soul binary itself (start_agent_terminal.sh starts /opt/soul): a run's manifest names it by its hash.
THEBOT_SOUL_PATH = os.environ.get("DECURIX_SOUL_PATH", "/opt/soul")

# Where the control plane reaches this agent. A desktop container is a machine
# of its own, so every agent there has :8081 on every interface. Native Windows
# workers all share one machine: the control plane gives each a port of its
# own, on loopback (a listener on every interface makes Windows Firewall ask
# the person a question).
AGENT_BIND = os.environ.get("DECURIX_AGENT_BIND", "0.0.0.0")
AGENT_PORT = int(os.environ.get("DECURIX_AGENT_PORT", "8081"))

# The secret the control plane gave this desktop at creation (internal/desktop/agenttoken.go): it sends it as
# "Authorization: Bearer <token>" on every call to :8081 and to soul, and http_api.py refuses a call without it.
# It keeps out other desktops on the shared Docker network and web pages in this desktop's browser, and the worker
# itself: the entrypoint takes it out of the environment and leaves it in a file for this process alone
# (DECURIX_AGENT_TOKEN_FILE), which take_agent_token reads and deletes once this process is non-dumpable, so the
# worker's commands (the same Linux user) cannot read it from its /proc or memory. The environment is still read
# first: a desktop started without the entrypoint (a test container) and the tests set it there.
AGENT_TOKEN_ENV = "DECURIX_AGENT_TOKEN"
AGENT_TOKEN_FILE_ENV = "DECURIX_AGENT_TOKEN_FILE"
_TOKEN_MAX = 4096
_taken_token = ""


def no_dump():
    """Make this process non-dumpable (prctl PR_SET_DUMPABLE 0): its /proc files and memory become root's, so another
    process of this user cannot read its environment or memory, ptrace it or dump it. Not inherited by what it runs
    (a new program is dumpable again). True when done; False off Linux or when the call failed."""

    if not sys.platform.startswith("linux"):
        return False
    try:
        import ctypes

        libc = ctypes.CDLL(None, use_errno=True)
        return libc.prctl(4, 0, 0, 0, 0) == 0     # PR_SET_DUMPABLE, SUID_DUMP_DISABLE
    except (OSError, AttributeError):
        return False


def take_agent_token(path=None):
    """Read this process's token from its file (DECURIX_AGENT_TOKEN_FILE), delete the file, and keep the token for
    agent_token(). Called once at start, after no_dump(). A link, a file that is not a regular one, or an unreadable
    one gives no token (the API then refuses every call but /health), and the file is deleted all the same. Returns
    whether a token was taken."""

    global _taken_token
    path = path if path is not None else os.environ.get(AGENT_TOKEN_FILE_ENV, "")
    if not path:
        return False
    token = ""
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            if stat.S_ISREG(os.fstat(fd).st_mode):
                token = os.read(fd, _TOKEN_MAX).decode("ascii", "replace").strip()
        finally:
            os.close(fd)
    except OSError:
        token = ""
    try:
        os.unlink(path)
    except OSError:
        pass
    _taken_token = token
    return bool(token)


def agent_token():
    """This desktop's agent token, or "" when it was created without one. The environment's (read each time, so a test
    can set it), else the one take_agent_token took."""

    return os.environ.get(AGENT_TOKEN_ENV, "").strip() or _taken_token


def agent_auth_headers():
    """The header an in-container caller of :8081 or soul sends: {} when this desktop has no token."""

    token = agent_token()
    return {"Authorization": "Bearer " + token} if token else {}

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
# into the engine's standing context. Nothing prunes MEMORY_DIR, and a
# linked/shared desktop or a specialized child spawned from a parent can
# copy another desktop's whole memory history in at once, so this is what
# keeps that system prefix from growing without bound as memory
# accumulates - unlike identity, whose byte length is fixed.
# Ollama's num_ctx is a fixed 8192 everywhere (ollama_client.NUM_CTX) and
# prompt evaluation, not generation, is what
# makes a CPU-only desktop feel slow, so this budget matters the same way
# DECURIX_GUI_TEXT_CHARS does for a `gui` command's output.
MEMORY_CONTEXT_CHARS = int(os.environ.get("DECURIX_MEMORY_CONTEXT_CHARS", "6000"))

try:
    for _folder in (COMMAND_DIR, QUERYTEXT_DIR, FINAL_DIR, WORK_DIR, MEMORY_DIR):
        _folder.mkdir(parents=True, exist_ok=True)
except OSError:
    pass
