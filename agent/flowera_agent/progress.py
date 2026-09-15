import base64
import json
import queue
import threading
import urllib.request

from flowera_agent import config, task_context
from flowera_agent.logging_utils import log


# ============================================================
# Live progress
#
# Every harness reports what it is doing, as it does it, through emit(),
# which POSTs one line to the control plane's /api/agent/events. That is
# what lets the chat show the same step-by-step view whichever engine is
# driving the desktop - so this module, like memory_context.py and
# messaging.py, stays harness-agnostic and never asks which one is running.
#
# Strictly best-effort. Progress is decoration; a control plane that is
# slow, restarting or absent must never slow down or fail a real task.
# Lines are handed to one background sender thread and every failure is
# swallowed after a single log line, so emit() never blocks the work it
# is describing and never raises into a harness.
# ============================================================

MAX_TEXT = 2000

# Bounded on purpose: TheBot can produce log lines far faster than an HTTP
# round trip to the control plane, and a queue that grew without limit would
# turn a burst of progress into unbounded memory plus a sender falling ever
# further behind the task it is narrating. Dropping is the right failure.
MAX_PENDING = 256

TIMEOUT_SECONDS = 5

_pending = queue.Queue(maxsize=MAX_PENDING)
_sender_lock = threading.Lock()
_sender_started = False


def _post(item):
    payload = {
        "desktop_id": config.DESKTOP_ID,
        "vm_name": config.DESKTOP_NAME,
    }

    if isinstance(item, dict):
        payload.update(item)
    else:
        payload["text"] = item

    request = urllib.request.Request(
        config.CONTROL_PLANE_URL.rstrip("/") + "/api/agent/events",
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )

    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS):
            pass
    except Exception as exc:
        log(f"Could not report progress: {exc}")


def _sender():
    while True:
        item = _pending.get()

        if isinstance(item, threading.Event):
            item.set()
        else:
            _post(item)


def _ensure_sender():
    global _sender_started

    with _sender_lock:
        if _sender_started:
            return

        threading.Thread(target=_sender, daemon=True).start()
        _sender_started = True


def emit(text):
    """
    Report one line of live activity for the task running right now.

    Safe to call from anywhere, including inside a task thread: it returns
    immediately and never raises.
    """

    if not text:
        return

    text = str(text).strip()

    if not text:
        return

    if len(text) > MAX_TEXT:
        text = text[:MAX_TEXT] + "…"

    _ensure_sender()

    try:
        # The task is read now, not when the line is sent: by then the next
        # task may already have started.
        _pending.put_nowait({"text": text, "task_id": task_context.current()})
    except queue.Full:
        # The sender is behind. Losing a progress line is always better
        # than blocking the task that produced it.
        pass


def emit_action(text, action, before_png=None, after_png=None, task_id=None):
    """
    Report one on-screen action as a step, with its before and after
    screenshots, for the chat's audit trail.

    Same contract as emit(): never blocks, never raises. Screenshots are the
    heavy part of a step, so when the sender is already behind they are the
    first thing dropped - the step itself still arrives, and the full-size
    frames are on the desktop's own disk either way (computer/audit.py).
    `task_id` names the task outright, for a reporter that knows it better
    than the current-task file does (a demonstration recorded for a task).
    """

    if not text:
        return

    item = {
        "text": str(text).strip()[:MAX_TEXT],
        "action": action,
        "task_id": task_id if task_id is not None else task_context.current(),
    }

    if _pending.qsize() < MAX_PENDING // 4:
        if before_png:
            item["before_png"] = base64.b64encode(before_png).decode("ascii")
        if after_png:
            item["after_png"] = base64.b64encode(after_png).decode("ascii")

    _ensure_sender()

    try:
        _pending.put_nowait(item)
    except queue.Full:
        pass


def flush(timeout=TIMEOUT_SECONDS):
    """
    Wait, at most `timeout` seconds, for every line emitted so far to be sent.

    Called just before a turn's reply goes out, so the reply cannot overtake
    the last lines of the work behind it - TheBot logs its final step in the
    same instant it finishes. The task is already over by then, so this wait
    slows nothing down, and it is bounded, so a stalled control plane still
    gets the reply.
    """

    with _sender_lock:
        if not _sender_started:
            return

    marker = threading.Event()

    try:
        _pending.put_nowait(marker)
    except queue.Full:
        return

    marker.wait(timeout)
