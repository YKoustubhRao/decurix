import os
import re
import threading

from flowera_agent import state


# ============================================================
# Which task this desktop is working on
#
# The control plane keeps a record of every task it sends (Task ->
# Execution -> Steps -> Interventions -> Outcome) and names each one with
# an ID it hands the desktop alongside the message. Everything the desktop
# reports for that task - each progress line, each on-screen action, the
# reply that ends it - carries the ID back, so the record is attributed
# exactly rather than guessed from timing.
#
# Harness-agnostic, like progress.py and messaging.py: the watcher sets the
# ID when it dispatches a message, and every engine's reports pick it up.
#
# The ID lives in two places. state.current_task_id is the agent process's
# own copy. CURRENT_FILE is for the processes that report steps without
# sharing that memory: the `gui` command (run by the Ollama loop, or by
# TheBot's soul daemon, which is not even a child of this process) and the
# MCP server Claude Code starts.
# ============================================================

CURRENT_FILE = os.environ.get("FLOWERA_TASK_FILE", "/tmp/flowera-current-task")

# The shape the control plane issues ("t" + 16 hex digits). Anything else in
# a file name or the task file is ignored rather than reported.
_TASK_ID = re.compile(r"^t[0-9a-f]{16}$")

# Where a task ID sits in a command file's name: 20260913_101500_ab12cd34.t<id>.txt
_FILENAME_TASK_ID = re.compile(r"\.(t[0-9a-f]{16})\.txt$")

_lock = threading.Lock()


def valid(task_id):
    return bool(task_id) and bool(_TASK_ID.match(task_id))


def command_filename(stem, task_id):
    """The name a received message is saved under, carrying its task ID."""

    if valid(task_id):
        return f"{stem}.{task_id}.txt"

    return f"{stem}.txt"


def task_id_from_filename(name):
    """The task ID a command file's name carries, or ""."""

    match = _FILENAME_TASK_ID.search(name or "")

    return match.group(1) if match else ""


def start(task_id):
    """
    Make task_id the task every report from here on belongs to.

    An empty ID (a message from a control plane too old to send one) clears
    it, so reports go out without one and the control plane attributes them
    to whatever it knows this desktop is running.
    """

    task_id = task_id if valid(task_id) else ""

    with _lock:
        state.current_task_id = task_id

        try:
            tmp = CURRENT_FILE + ".tmp"
            with open(tmp, "w", encoding="utf-8") as f:
                f.write(task_id)
            os.replace(tmp, CURRENT_FILE)
        except OSError:
            # Only the out-of-process reporters lose the ID, and the control
            # plane still attributes their steps to the live task.
            pass


def current():
    """The task being worked on, as far as this process can tell, or ""."""

    if state.current_task_id:
        return state.current_task_id

    try:
        with open(CURRENT_FILE, encoding="utf-8") as f:
            task_id = f.read().strip()
    except OSError:
        return ""

    return task_id if valid(task_id) else ""
