"""
What the organisation told this worker to know, read from the control plane.

The control plane keeps rules, facts, corrections and lessons scoped to the whole organisation, a role, one worker or
one task, each with where it came from (internal/knowledge, docs/skillhub.md). fetch() asks for the ones this worker
is given - org first, then its roles, then its own, then the task's - as lines ready for a model's context:
"Rule (org): ...". Three places read them: a skill run's taught context (skill_task._taught), the record ledger's given
sources (computer/records.given_sources), and every TheBot request's standing context (standing_context, sent by
thebot_harness in the job's `context` field, the same for every call of a task, never in the operator's fixed prompt).

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


def cached(worker_id=None, max_age=CACHE_SECONDS, task_id=None):
    """
    fetch()'s lines for this worker (and task, when given: the task's own items come last), reused for max_age
    seconds. A failed fetch is not kept, so the next call tries again. Each task has its own entry; entries past
    max_age are dropped as new ones are kept, so a long-lived desktop does not keep every task's lines.
    """

    worker = worker_id if worker_id is not None else getattr(config, "DESKTOP_ID", "")
    key = (worker, task_id or "")
    now = time.monotonic()
    with _cache_lock:
        kept = _cache.get(key)
        if kept and now - kept[0] < max_age:
            return list(kept[1])

    lines = fetch(worker, task_id or None)
    if lines:
        with _cache_lock:
            for old in [k for k, (at, _) in _cache.items() if now - at >= CACHE_SECONDS]:
                del _cache[old]
            _cache[key] = (now, lines)
    return list(lines)


# How much of a request's standing context the organisation's lines may take (characters). The context goes with
# every model call of a job, so a long list costs on every turn; past this the rest is named as left out, not dropped
# silently.
MAX_CONTEXT_CHARS = 6000

CONTEXT_HEADER = ("What the organisation told this worker to know: rules, facts, corrections and lessons that people "
                  "wrote, for the organisation, this worker's roles, this worker and this task, broad first - a "
                  "narrower line refines a broader one. Hold to them where they apply; they are principles, never a "
                  "value for this task's fields, which come from the request, its files and the screen.")


def _withhold(line):
    """line with the task's known secrets, and any it spells out, withheld (computer/privacy.py); "" when it cannot be
    checked, so nothing unchecked reaches a model's context."""

    try:
        from decurix_agent.computer import privacy
        held = privacy.withhold_known(line)
    except Exception:  # noqa: BLE001 - fail closed
        return ""
    return "" if held == getattr(privacy, "HELD_BACK", None) else held


def standing_context(lines, skip="", max_chars=MAX_CONTEXT_CHARS):
    """
    The organisation's lines as one block for a request's standing context (thebot_harness._build_context): secrets
    withheld, a line already in `skip` (a taught task's procedure that carries them) left out, at most max_chars of
    lines, and a closing note saying how many more were not shown. "" when there is nothing to say.
    """

    kept, used, left = [], 0, 0
    for line in lines or ():
        line = _withhold(line)
        if not line or (skip and line in skip):
            continue
        if used + len(line) + 3 > max_chars:
            left += 1
            continue
        kept.append("- " + line)
        used += len(line) + 3
    if not kept:
        return ""
    out = [CONTEXT_HEADER] + kept
    if left:
        out.append(f"({left} more of the organisation's lines were left out for length; ask the person when a rule "
                   "may be among them.)")
    return "\n".join(out)


def as_text(worker_id=None, task_id=None):
    """fetch()'s lines joined one a line, or "" - what a prompt section takes."""

    return "\n".join(fetch(worker_id, task_id))
