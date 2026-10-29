"""
What the organisation told this worker to know, read from the control plane.

The control plane keeps rules, facts, corrections and lessons scoped to the whole organisation, a role, one worker or
one task, each with where it came from (internal/knowledge, docs/skillhub.md). fetch() asks for the ones this worker
is given - org first, then its roles, then its own - as lines ready for a model's context: "Rule (org): ...".

Best-effort and never raises: a control plane that cannot be reached leaves the worker with what it knows locally,
and a task must never fail for want of these lines.
"""

import json
import threading
import time
import urllib.parse
import urllib.request

from decurix_agent import config
from decurix_agent.logging_utils import log

# How long one fetch may take: it runs as a task starts, so it must not hold the task up.
TIMEOUT_SECONDS = 5

# Bounds on what is taken in, whatever the control plane answers.
MAX_LINES = 200
MAX_LINE_CHARS = 4000
MAX_ANSWER_BYTES = 2 * 1024 * 1024


def fetch(worker_id=None, task_id=None):
    """The knowledge lines for this worker (and task, when given), broad to narrow; [] when there are none or on any failure."""

    worker = worker_id if worker_id is not None else getattr(config, "DESKTOP_ID", "")
    query = {"worker": worker or ""}
    if task_id:
        query["task"] = task_id
    url = config.CONTROL_PLANE_URL.rstrip("/") + "/api/knowledge?" + urllib.parse.urlencode(query)

    try:
        with urllib.request.urlopen(url, timeout=TIMEOUT_SECONDS) as response:
            if not 200 <= response.status < 300:
                log(f"Organisation knowledge: the control plane answered {response.status}")
                return []
            raw = response.read(MAX_ANSWER_BYTES + 1)
        if len(raw) > MAX_ANSWER_BYTES:
            log("Organisation knowledge: the answer was too large; using none of it")
            return []
        answer = json.loads(raw.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - best-effort by contract
        log(f"Organisation knowledge could not be read: {exc}")
        return []

    lines = answer.get("lines") if isinstance(answer, dict) else None
    if not isinstance(lines, list):
        return []

    out = []
    for line in lines[:MAX_LINES]:
        if isinstance(line, str):
            line = " ".join(line.split())[:MAX_LINE_CHARS]
            if line:
                out.append(line)
    return out


# How long cached() keeps an answer. Within it every call returns the same lines, so a run's prompt prefix stays
# byte-identical (a changed prefix makes Ollama reload) and a check called many times costs one request.
CACHE_SECONDS = 300

_cache = {}
_cache_lock = threading.Lock()


def cached(worker_id=None, max_age=CACHE_SECONDS):
    """fetch()'s lines for this worker, reused for max_age seconds. A failed fetch is not kept, so the next call tries again."""

    key = worker_id if worker_id is not None else getattr(config, "DESKTOP_ID", "")
    now = time.monotonic()
    with _cache_lock:
        kept = _cache.get(key)
        if kept and now - kept[0] < max_age:
            return list(kept[1])

    lines = fetch(key)
    if lines:
        with _cache_lock:
            _cache[key] = (now, lines)
    return list(lines)


def as_text(worker_id=None, task_id=None):
    """fetch()'s lines joined one a line, or "" - what a prompt section takes."""

    return "\n".join(fetch(worker_id, task_id))
