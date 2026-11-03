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
            # Withheld (C14): the copy outlives the task in /tmp, and every process reads it.
            writes.append((CURRENT_FILE + ".request", _withheld(str(request), task_id)[:REQUEST_CHARS]))
            # The files the request brought, whole: the request above is cut at 500 characters, and its paths come
            # last. What the screen layer checks a chosen record against (core._in_documents).
            from decurix_agent import attachments
            writes.append((CURRENT_FILE + ".files", "\n".join(attachments.files(str(request)))))

        for path, value in writes:
            _write(path, value)


def _withheld(text, task_id=None):
    """withhold(text) for the current task (the copies this module keeps)."""

    return withhold(text, task_id)


def withhold(text, task_id=None):
    """
    text with the task's noted secret values withheld wherever they stand (a message the person marked secret is one
    whole value: a long one is withheld whole, not word by word), then every secret remembered so far
    (privacy.withhold_known). Never the text when it cannot be checked.
    """

    from decurix_agent.computer import privacy
    try:
        text = str(text if text is not None else "")
        flat = " ".join(text.split())
        for value in sorted(secret_values(task_id), key=len, reverse=True):
            if value in text:
                text = text.replace(value, "[withheld]")
            elif value == flat:
                text = "[withheld]"
        return privacy.withhold_known(text)
    except Exception:  # noqa: BLE001 - fail closed
        return privacy.HELD_BACK


# The secret values each task's requests and replies gave (C14), in this process's memory only - never on disk: the
# job to soul names them (thebot_harness), so soul keeps none of them in what it stores and shows.
_secret_values = {}


def note_secret_values(task_id, values):
    """Keep `values` as the task's secret values: in this process (the job to soul names them) and as they are for
    every process of the desktop (privacy.remember_literals: withheld wherever they stand, cleared at the task's end)."""

    if not valid(task_id):
        return
    from decurix_agent.computer import privacy
    values = privacy.clean_values(values)
    with _lock:
        kept = _secret_values.setdefault(task_id, [])
        for value in values:
            if value not in kept:
                kept.append(value)
    privacy.remember_literals(values, task_id)


def secret_values(task_id=None):
    """The secret values the task's requests and replies gave: as noted in this process, and as kept for the desktop
    (which outlives an agent restart within the task)."""

    from decurix_agent.computer import privacy
    task_id = task_id or current()
    return list(dict.fromkeys(list(_secret_values.get(task_id, ())) + privacy.literals(task_id)))


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
        tmp.write_text(json.dumps({"task_id": task_id, "request": _withheld(str(request or ""), task_id)[:500],
                                   "since": time.time()}), encoding="utf-8")
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


_EXECUTION_ID = re.compile(r"^[A-Za-z0-9_-]{4,64}$")


def set_execution(task_id, execution_id):
    """Keep the control plane's id of the attempt this task is on (sent on /request), for its run's label (C8)."""

    if isinstance(execution_id, str) and _EXECUTION_ID.match(execution_id):
        _keep_context(task_id, execution_id=execution_id)


def execution_id(task_id=None):
    """The control plane's id of the task's current attempt, or "" when it sent none."""

    value = _context(task_id or current()).get("execution_id") if (task_id or current()) else ""
    return value if isinstance(value, str) and _EXECUTION_ID.match(value) else ""


def valid_judgement(judgement_id):
    return isinstance(judgement_id, str) and bool(_JUDGEMENT_ID.match(judgement_id))


def set_reply_judgement(task_id, judgement_id):
    """Keep the control plane's judgement id of the latest message to the task (C6). A message that carries none (or a
    malformed one) clears it: no later answer may cite an earlier reply's judgement (review round 2, B7)."""

    _keep_context(task_id, reply_judgement=judgement_id if valid_judgement(judgement_id) else "")


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


# Two date orders, kept apart (review round 2, B4): the order the task's SOURCE documents write their digit dates in,
# and the order the APPLICATION shows them in. A US vendor's bill (month first) entered in a day-first org's books is
# both at once. Either may be unknown; values.compare reads each side under its own.
SIDES = ("source", "app")


def answered_order(task_id, side):
    """The order a person's answer set for this side of the task ({"order", "judgement"}), or None."""

    kept = (_context(task_id).get("answered_date_order") or {}).get(side) if task_id else None
    if isinstance(kept, dict) and kept.get("order") in DATE_ORDERS and _JUDGEMENT_ID.match(str(kept.get("judgement"))):
        return kept
    return None


def set_answered_order(task_id, side, order, judgement_id):
    """Keep a person's answer to "which order" for one side, with the judgement it rests on (C6). False when the
    answer cannot be kept as theirs (no judgement id: a reply the control plane did not record)."""

    if side not in SIDES or order not in DATE_ORDERS or not valid(task_id):
        return False
    if not (isinstance(judgement_id, str) and _JUDGEMENT_ID.match(judgement_id)):
        return False
    answers = dict(_context(task_id).get("answered_date_order") or {})
    answers[side] = {"order": order, "judgement": judgement_id}
    _keep_context(task_id, answered_date_order=answers)
    return True


def app_date_order(task_id=None):
    """The order the application shows digit dates in: a person's answer for this task, else what the control plane
    sent on /request (the org's, C6), else None."""

    task_id = task_id or current()
    answered = answered_order(task_id, "app")
    if answered:
        return answered["order"]
    return sent_date_order(task_id) if task_id else None


def source_date_order(task_id=None):
    """The order the task's source documents write digit dates in: a person's answer for this task, else the
    request's own DD/MM or MM/DD marker, else unambiguous dates in the request and its files (_derived_order)."""

    task_id = task_id or current()
    answered = answered_order(task_id, "source")
    if answered:
        return answered["order"]
    return _derived_order(task_id)


def date_order(task_id=None):
    """The source documents' order (source_date_order): what the ledger and the checks read a document's date by."""

    return source_date_order(task_id)


def _derived_order(task_id):
    """
    "day" or "month" from the request's own marker, then from the dates in the request and the text of its files that
    cannot be read the other way (28/09/2026 is day first: binding.date_order), when they all agree; None otherwise.
    Kept for the task, so the `gui` process that checks a save does not read the files again.
    """

    import hashlib

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


# ============================================================
# The date order a save waits on (review round 2, B3)
#
# When a save cannot compare a date - a digit date whose day and month could be either way, and no order known for its
# side - the save waits and the person is asked, with both readings. The question is kept for the task here; the
# agent's next question to the person carries it (messaging.ask_for_query), and the reply that picks one of its options
# by number (watchers) sets that side's order, backed by the reply's judgement. Typing the source's own digits into the
# application and letting it choose is not an answer: the application's side is then the one asked about.
# ============================================================

def ask_date_order(task_id, side, value):
    """Keep the question "which order is <value> in" for one side of the task (a save waits on it). Its readings come
    from the value. None when there is nothing to ask (the value is not ambiguous)."""

    from decurix_agent import values
    found = values.readings(value)
    if side not in SIDES or len(found) != 2 or not valid(task_id):
        return None
    question = {"side": side, "value": " ".join(str(value).split())[:40],
                "options": {"1": "day", "2": "month"},
                "readings": {order: values.spoken(day) for order, day in found.items()}}
    current_q = _context(task_id).get("date_question")
    if isinstance(current_q, dict) and current_q.get("side") == side and current_q.get("value") == question["value"]:
        return current_q
    _keep_context(task_id, date_question=question)
    return question


def date_question(task_id=None):
    """The date-order question a save of the task waits on, or None."""

    found = _context(task_id or current()).get("date_question") if (task_id or current()) else None
    return found if isinstance(found, dict) and found.get("side") in SIDES else None


def date_question_text(question):
    """The question as the person reads it: both readings, each with the number that picks it."""

    where = "the document" if question["side"] == "source" else "the application"
    r = question["readings"]
    return (f"Before I save: {where} writes the date {question['value']}, which can be read two ways. Reply 1 if it is "
            f"{r['day']} (day first), or 2 if it is {r['month']} (month first).")


def take_date_answer(task_id, reply, judgement_id=None):
    """
    The person's reply to the date-order question, taken when its whole text is one option's number: that side's order
    is kept with the reply's judgement, and the question is settled. Anything else is not taken (the question stays).
    Returns the order taken, or None.
    """

    question = date_question(task_id)
    if not question:
        return None
    choice = " ".join(str(reply or "").split()).strip(" .")
    order = question["options"].get(choice)
    if not order:
        return None
    if not set_answered_order(task_id, question["side"], order,
                              judgement_id if judgement_id is not None else reply_judgement(task_id)):
        return None
    _keep_context(task_id, date_question=None)
    return order
