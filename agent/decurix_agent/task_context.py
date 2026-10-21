import json
import os
import re
import threading
import time

from decurix_agent import config, state


# ============================================================
# Which task this desktop is working on
#
# The control plane keeps a record of every task it sends (Task ->
# Execution -> Steps -> Interventions -> Outcome) and names each one with
# an ID it hands the desktop alongside the request. Everything the desktop
# reports for that task - each progress line, each on-screen action, the
# reply that ends it - carries the ID back, so the record is attributed
# exactly rather than guessed from timing.
#
# Harness-agnostic, like progress.py and messaging.py: the watcher sets the
# ID when it dispatches a request, and every engine's reports pick it up.
#
# The ID lives in two places. state.current_task_id is the agent process's
# own copy. CURRENT_FILE is for the processes that report steps without
# sharing that memory: the `gui` command (run by the Ollama loop, or by
# TheBot's soul daemon, which is not even a child of this process) and the
# MCP server Claude Code starts.
# ============================================================

CURRENT_FILE = os.environ.get("DECURIX_TASK_FILE", "/tmp/decurix-current-task")

# The shape the control plane issues ("t" + 16 hex digits). Anything else in
# a file name or the task file is ignored rather than reported.
_TASK_ID = re.compile(r"^t[0-9a-f]{16}$")

# Where a task ID sits in a command file's name: 20260913_101500_ab12cd34.t<id>.txt
_FILENAME_TASK_ID = re.compile(r"\.(t[0-9a-f]{16})\.txt$")

_lock = threading.Lock()


def valid(task_id):
    return bool(task_id) and bool(_TASK_ID.match(task_id))


def command_filename(stem, task_id):
    """The name a received request is saved under, carrying its task ID."""

    if valid(task_id):
        return f"{stem}.{task_id}.txt"

    return f"{stem}.txt"


def task_id_from_filename(name):
    """The task ID a command file's name carries, or ""."""

    match = _FILENAME_TASK_ID.search(name or "")

    return match.group(1) if match else ""


def start(task_id, request=None):
    """
    Make task_id the task every report from here on belongs to.

    An empty ID (a request from a control plane too old to send one) clears
    it, so reports go out without one and the control plane attributes them
    to whatever it knows this desktop is running. `request` is what the task
    asked for, kept beside the ID so a recording of the task can be titled
    with it by whichever process records the task's first action.
    """

    task_id = task_id if valid(task_id) else ""

    with _lock:
        state.current_task_id = task_id

        writes = [(CURRENT_FILE, task_id), (CURRENT_FILE + ".stage", "")]  # a new task runs no stage yet
        if task_id and _since_record()[0] != task_id:
            # When the task began (kept across a resume of the same task): what was there before it is the person's.
            writes.append((CURRENT_FILE + ".since", f"{task_id} {time.time():.3f}"))
        if request is not None:
            writes.append((CURRENT_FILE + ".request", str(request)[:500]))
            # The files the request brought, whole: the request above is cut at 500 characters, and its paths come
            # last. What the screen layer checks a chosen record against (core._in_documents).
            from decurix_agent import attachments
            writes.append((CURRENT_FILE + ".files", "\n".join(attachments.files(str(request)))))

        for path, value in writes:
            try:
                tmp = path + ".tmp"
                with open(tmp, "w", encoding="utf-8") as f:
                    f.write(value)
                os.replace(tmp, path)
            except OSError:
                # Only the out-of-process reporters lose it, and the control
                # plane still attributes their steps to the live task.
                pass


def _since_record():
    try:
        with open(CURRENT_FILE + ".since", encoding="utf-8") as f:
            task_id, at = (f.read().split() + ["", ""])[:2]
        return task_id, float(at)
    except (OSError, ValueError):
        return "", None


def since():
    """When the current task began (epoch seconds), from any process; None when no task is running or it is unknown."""

    task_id, at = _since_record()
    return at if task_id and task_id == current() else None


# The task this desktop has taken on and not yet finished, kept on the
# persistent volume (CURRENT_FILE is in /tmp, which a restart empties). A
# desktop whose X session ended restarts its container on its own; the agent
# that comes back has no memory of the task, and the control plane - which
# only closes tasks on a restart it was asked for - kept showing it as running
# ("Working..." for good) and routed the next message as a reply to a question
# nobody was waiting on any more. The agent that starts reads this back and
# reports the task as interrupted (watchers._report_interrupted).
def _active_path():
    return config.DECURIX_DIR / "active-task.json"


def mark_active(task_id, request=None):
    """Remember that task_id has been started and not finished."""

    if not valid(task_id):
        clear_active()
        return

    try:
        path = _active_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps({"task_id": task_id, "request": str(request or "")[:500], "since": time.time()}),
                       encoding="utf-8")
        tmp.replace(path)
    except OSError:
        pass


def clear_active():
    """The task this desktop was working on is over."""

    try:
        _active_path().unlink()
    except OSError:
        pass


def take_interrupted():
    """The task a previous life of this agent started and never finished ({task_id, request}), forgotten once read."""

    path = _active_path()
    try:
        record = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    finally:
        clear_active()

    if not isinstance(record, dict) or not valid(record.get("task_id")):
        return None
    return record


def current_request():
    """What the current task asked for, as far as this process can tell, or ""."""

    if state.current_task_id and state.current_task:
        return str(state.current_task)[:500]

    try:
        with open(CURRENT_FILE + ".request", encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


def current_files():
    """The files the current task's request brought (paths), as far as this process can tell."""

    try:
        with open(CURRENT_FILE + ".files", encoding="utf-8") as f:
            return [line.strip() for line in f if line.strip()]
    except OSError:
        return []


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


# The long request's stage running now ("<run id>:<stage id>" and its attempt), for the processes a stage's engine
# starts (the `office email` command): a draft made during a stage is that stage's, and its attempt's. Found live
# (round 5 P1, 2026-10-20): the stage tag lived only in the agent's memory, so `office email` - another process -
# never saw it, and a re-check's drafts sat beside the first attempt's instead of replacing them.
def _stage_path():
    return CURRENT_FILE + ".stage"


def set_stage(scope, attempt=0):
    """Mark the stage running now (scope "" clears it)."""

    try:
        tmp = _stage_path() + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            f.write(json.dumps({"scope": scope, "attempt": int(attempt or 0)}) if scope else "")
        os.replace(tmp, _stage_path())
    except OSError:
        pass


def current_stage():
    """(scope, attempt) of the stage running now, from any process, or ("", 0)."""

    try:
        with open(_stage_path(), encoding="utf-8") as f:
            data = json.loads(f.read() or "{}")
        return str(data.get("scope") or ""), int(data.get("attempt") or 0)
    except (OSError, ValueError, TypeError):
        return "", 0
