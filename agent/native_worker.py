"""
The entry point of a native Windows worker.

A worker in a Linux desktop is started by start_agent_terminal.sh, inside a
container with a display of its own. A native worker is this script, started by
the control plane (internal/desktop/native.go) on a private Win32 desktop of the
person's own computer: every program it starts - TheBot's engine below, the
commands a task runs, the apps a task opens - inherits that desktop, so none of
them has a window on the person's screen.

It is run with pythonw.exe (no console window on the worker's screen), which
leaves stdout unset; everything the agent prints goes to the worker's log.
"""

import os
import subprocess
import sys
from pathlib import Path

# The Python the Windows app ships is the "embeddable" one, whose python*._pth
# file fixes its module path: neither this script's folder nor PYTHONPATH is
# on it, so the agent package beside this file would not be found.
sys.path.insert(0, str(Path(__file__).resolve().parent))

CREATE_NO_WINDOW = 0x08000000


def open_log():
    """Send what the agent prints to <worker>/logs/agent.log."""

    logs = Path(os.environ.get("DECURIX_LOG_DIR", "."))
    logs.mkdir(parents=True, exist_ok=True)
    stream = open(logs / "agent.log", "a", buffering=1, encoding="utf-8", errors="replace")
    sys.stdout = stream
    sys.stderr = stream
    return logs


def start_soul(logs):
    """
    TheBot's engine, the same way start_agent_terminal.sh starts it: a server
    of its own that thebot_harness.py talks to, given the model the control
    plane chose. The control plane sets its port (SOUL_PORT) and where it
    keeps its stores; what it reasons with is mapped here from the variables
    every worker is given.
    """

    soul = os.environ.get("DECURIX_SOUL", "")
    if os.environ.get("AGENT_HARNESS", "") != "thebot" or not soul or not Path(soul).is_file():
        return None

    env = dict(os.environ)
    env.setdefault("LLM_PROVIDER", "ollama")
    env["OLLAMA_HOST"] = env.get("OLLAMA_URL", "http://127.0.0.1:11434")
    env["CHAT_MODEL"] = env.get("OLLAMA_MODEL", "")
    env.setdefault("THEBOT_WEB_PAGES", "2")
    env.setdefault("THEBOT_WEB_PAGE_CHARS", "2500")
    Path(env["WORKSPACE_ROOT"]).mkdir(parents=True, exist_ok=True)

    log = open(logs / "soul.log", "a", buffering=1, encoding="utf-8", errors="replace")
    process = subprocess.Popen(
        [soul],
        env=env,
        cwd=env["WORKSPACE_ROOT"],
        stdin=subprocess.DEVNULL,
        stdout=log,
        stderr=log,
        creationflags=CREATE_NO_WINDOW,
    )
    print(f"soul (TheBot) started (pid {process.pid})")
    return process


def main():
    logs = open_log()
    start_soul(logs)

    # After the log is open: importing the agent already creates its folders
    # and reads its configuration.
    from decurix_agent.app import main as serve

    serve()


if __name__ == "__main__":
    main()
