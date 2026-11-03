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
# How much of the request every process sees (current_request): the checks a command makes (office email's recipients,
# the screen's typed values) read the request's folders and files from it. Found (series 16, 2026-10-21): cut at 500
# characters, a request naming its folder later in the text named none, and the checks read every file of the desktop.
REQUEST_CHARS = 20000

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


def start(task_id, request=None, continues=""):
    """
    Make task_id the task every report from here on belongs to.

    An empty ID (a request from a control plane too old to send one) clears
    it, so reports go out without one and the control plane attributes them
    to whatever it knows this desktop is running. `request` is what the task
    asked for, kept beside the ID so a recording of the task can be titled
    with it by whichever process records the task's first action.
    `continues`: the task this one carries on (a change the person sent after
    that task was reported) - its drafts are this task's to replace.
    """

    task_id = task_id if valid(task_id) else ""

    with _lock:
        state.current_task_id = task_id

        writes = [(CURRENT_FILE, task_id), (CURRENT_FILE + ".stage", ""),  # a new task runs no stage yet
                  (CURRENT_FILE + ".continues", continues if valid(continues) else "")]
        if task_id and _since_record()[0] != task_id:
            # When the task began (kept across a resume of the same task): what was there before it is the person's.
            writes.append((CURRENT_FILE + ".since", f"{task_id} {time.time():.3f}"))
        if request is not None:
            writes.append((CURRENT_FILE + ".request", str(request)[:REQUEST_CHARS]))
            # The files the request brought, whole: the request above is cut at 500 characters, and its paths come
            # last. What the screen layer checks a chosen record against (core._in_documents).
            from decurix_agent import attachments
            writes.append((CURRENT_FILE + ".files", "\n".join(attachments.files(str(request)))))

        for path, value in writes:
            _write(path, value)


def _write(path, value):
    """Replace a file the out-of-process reporters read, whole."""

    try:
        tmp = path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            f.write(value)
        os.replace(tmp, path)
    except OSError:
        # Only the out-of-process reporters lose it, and the control
        # plane still attributes their steps to the live task.
        pass


def _read(suffix=""):
    """What CURRENT_FILE (or the file of its `suffix`) holds, stripped, or "" when it cannot be read."""

    try:
        with open(CURRENT_FILE + suffix, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


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


def is_active():
    """Whether a task has been started and not finished (a question it waits on included)."""

    return _active_path().exists()


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
        return str(state.current_task)[:REQUEST_CHARS]

    return _read(".request")


def continues():
    """The task the current one carries on (task_context.start's `continues`), or ""."""

    task_id = _read(".continues")
    return task_id if valid(task_id) else ""


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

    task_id = _read()
    return task_id if valid(task_id) else ""


# The long request's stage running now ("<run id>:<stage id>" and its attempt), for the processes a stage's engine
# starts (the `office email` command): a draft made during a stage is that stage's, and its attempt's. Found live
# (round 5 P1, 2026-10-20): the stage tag lived only in the agent's memory, so `office email` - another process -
# never saw it, and a re-check's drafts sat beside the first attempt's instead of replacing them.
def _stage_path():
    return CURRENT_FILE + ".stage"


def set_stage(scope, attempt=0):
    """Mark the stage running now (scope "" clears it)."""

    _write(_stage_path(), json.dumps({"scope": scope, "attempt": int(attempt or 0)}) if scope else "")


def current_stage():
    """(scope, attempt) of the stage running now, from any process, or ("", 0)."""

    try:
        with open(_stage_path(), encoding="utf-8") as f:
            data = json.loads(f.read() or "{}")
        return str(data.get("scope") or ""), int(data.get("attempt") or 0)
    except (OSError, ValueError, TypeError):
        return "", 0


# ============================================================
# What the control plane said about a task, beside its request
#
# The control plane's /request body carries more than the words (PLAN-P2 C6): the org's date order and, for a person's
# reply, the judgement that reply is on the control plane. Both are kept in the task's run folder
# (~/.decurix/runs/<task>/context.json, PLAN-P2 C8), on the persistent volume, so every process of the desktop reads
# them - the `gui` command compares a form's dates on the save path, in a process of its own.
# ============================================================

DATE_ORDERS = ("day", "month")
# The control plane's judgement ids (C6). Anything else is ignored, never kept.
_JUDGEMENT_ID = re.compile(r"^j_[A-Za-z0-9]{6,32}$")


def run_dir(task_id):
    """The task's run folder (~/.decurix/runs/<task>), whether or not it exists; None for an invalid id."""

    return config.DECURIX_DIR / "runs" / task_id if valid(task_id) else None


def _context(task_id):
    folder = run_dir(task_id)
    if folder is None:
        return {}
    try:
        data = json.loads((folder / "context.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _keep_context(task_id, **fields):
    """Merge fields into the task's context.json. Never raises: what cannot be kept is only not known later."""

    folder = run_dir(task_id)
    if folder is None:
        return
    with _lock:
        data = _context(task_id)
        data.update(fields)
        try:
            folder.mkdir(parents=True, exist_ok=True)
            tmp = folder / "context.json.tmp"
            tmp.write_text(json.dumps(data, sort_keys=True), encoding="utf-8")
            os.replace(tmp, folder / "context.json")
        except OSError:
            pass


def set_date_order(task_id, order):
    """Keep the org's date order the control plane sent with a request (C6); anything but "day"/"month" is ignored."""

    if order in DATE_ORDERS:
        _keep_context(task_id, date_order=order)


def set_reply_judgement(task_id, judgement_id):
    """Keep the control plane's judgement id of the latest person's reply to the task (C6); a malformed one is ignored."""

    if isinstance(judgement_id, str) and _JUDGEMENT_ID.match(judgement_id):
        _keep_context(task_id, reply_judgement=judgement_id)


def reply_judgement(task_id=None):
    """The judgement id of the latest person's reply to the task (the current one by default), or ""."""

    value = _context(task_id or current()).get("reply_judgement")
    return value if isinstance(value, str) and _JUDGEMENT_ID.match(value) else ""


def set_skill(name, version, task_id=None):
    """The taught skill driving the task (R7): its name and version, sent on the task's outcome (C5). "" clears it."""

    task_id = task_id or current()
    if not task_id:
        return
    try:
        version = int(version or 0)
    except (TypeError, ValueError):
        version = 0
    _keep_context(task_id, skill={"name": str(name), "version": version} if name else None)


def skill(task_id=None):
    """{"name", "version"} of the taught skill that drove the task, or None."""

    found = _context(task_id or current()).get("skill") if (task_id or current()) else None
    if isinstance(found, dict) and isinstance(found.get("name"), str) and found["name"]:
        return {"name": found["name"], "version": int(found.get("version") or 0)}
    return None


def sent_date_order(task_id=None):
    """The date order the control plane sent for the task, or None."""

    value = _context(task_id or current()).get("date_order")
    return value if value in DATE_ORDERS else None


def request_date_marker(request):
    """"day" when the request writes dates DD/MM, "month" when MM/DD, None when it does not say."""

    from decurix_agent import attachments
    text = attachments.text(request or "")
    if re.search(r"(?i)\bDD[/.\-]MM\b", text):
        return "day"
    if re.search(r"(?i)\bMM[/.\-]DD\b", text):
        return "month"
    return None


# How many of the request's files are read for their dates, and how much of each (the text layer only: no OCR, which
# is too slow on the save path).
_ORDER_FILES = 6
_ORDER_CHARS = 200000
_derived = {}


def _files_text(paths):
    from decurix_agent.tools import office
    texts = []
    for path in list(paths)[:_ORDER_FILES]:
        try:
            if os.path.splitext(str(path))[1].lower() in office.IMAGE_TYPES:
                continue
            texts.append(office.read_document(path)["text"][:_ORDER_CHARS])
        except Exception:  # noqa: BLE001 - a file that cannot be read says nothing about dates
            continue
    return texts


def date_order(task_id=None):
    """
    The order the current task's digit dates are written in, "day" (DD/MM) or "month" (MM/DD), or None when nothing
    says. In this order: what the control plane sent on /request (the org's, C6); the request's own DD/MM or MM/DD
    marker; then the dates in the request and the text of its files that cannot be read the other way (28/09/2026 is
    day first: binding.date_order), when they all agree. What is derived is kept for the task, so the `gui` process
    that checks a save does not read the files again.
    """

    import hashlib

    task_id = task_id or current()
    sent = sent_date_order(task_id) if task_id else None
    if sent:
        return sent
    request = current_request() if not task_id or task_id == current() else ""
    if not request:
        return None
    # Kept by the request it was read from, so a task id used again (a test's, a resumed task's) never reads another
    # request's order.
    key = (task_id, hashlib.sha256(request.encode("utf-8", "replace")).hexdigest()[:16])
    if key in _derived:
        return _derived[key]
    kept = _context(task_id).get("derived_date_order") if task_id else None
    if isinstance(kept, dict) and kept.get("request") == key[1] and kept.get("order") in DATE_ORDERS + ("none",):
        _derived[key] = None if kept["order"] == "none" else kept["order"]
        return _derived[key]
    order = request_date_marker(request)
    if order is None:
        from decurix_agent import attachments, binding
        files = current_files() if task_id and task_id == current() else []
        order = binding.date_order([attachments.text(request)] + _files_text(files or attachments.files(request)))
    _derived[key] = order
    if task_id:
        _keep_context(task_id, derived_date_order={"request": key[1], "order": order or "none"})
    return order
