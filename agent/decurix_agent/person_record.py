"""
What the control plane recorded as a person's word, read from the control plane - never from a file the worker's own
user can write (review round 10, R10-B1: a row forged into `field-answers.json`, or a line appended to `<task>.said`,
backed the worker's own guessed pick; the skill file's steps are partly model-written).

Three reads, each of what the control plane keeps as a human judgement (H-PY-18 asks CP for the desktop-scoped reads):

- answers(task): the person's own judgements on this desktop's task (an answer to a question it asked), each with the
  question it answered. A reply the watcher kept in `<task>.said` backs a pick only when its judgement id is one of
  these and its text's hash is that judgement's: the text used is the control plane's, never the file's.
- taught(skill): the person's own judgements on this desktop's tasks that taught or ran that skill.
- field_answers(): what a person answered about which entry a field takes, kept as the control plane's learned rows
  (kind "field_answer", derived from that answer's judgement), each with the judgement's own text - an option backs a
  later pick only when it is picked out by that text (records._kept_answer).

Fail closed: a control plane that cannot be read, or that has no such read yet, confirms nothing, and a pick that
rested only on the person's word is then not backed - the person is asked. Never raises.
"""

import hashlib
import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from decurix_agent import config, task_context
from decurix_agent.logging_utils import log

JUDGEMENTS_PATH = "/api/desktops/judgements"
LEARNED_PATH = "/api/desktops/learned"
FIELD_ANSWER = "field_answer"
TIMEOUT_SECONDS = 5
# How long one read is reused: one task's checks read it many times; a failed read is kept as long, so a control plane
# that does not answer costs one wait, not one per pick.
CACHE_SECONDS = 60
MAX_ANSWER_BYTES = 2 * 1024 * 1024

_cache = {}
_lock = threading.Lock()


def text_hash(text):
    """The hash a kept reply carries of its text (the text with its spacing collapsed)."""

    return hashlib.sha256(" ".join(str(text or "").split()).encode("utf-8")).hexdigest()


def _get(path, query):
    """The control plane's JSON answer to a GET, or None (unreachable, refused, not there, not JSON). Cached."""

    url = config.CONTROL_PLANE_URL.rstrip("/") + path + "?" + urllib.parse.urlencode(sorted(query.items()))
    now = time.monotonic()
    with _lock:
        kept = _cache.get(url)
        if kept and now - kept[0] < CACHE_SECONDS:
            return kept[1]
    answer = None
    try:
        with urllib.request.urlopen(url, timeout=TIMEOUT_SECONDS) as response:
            if 200 <= response.status < 300:
                raw = response.read(MAX_ANSWER_BYTES + 1)
                if len(raw) <= MAX_ANSWER_BYTES:
                    answer = json.loads(raw.decode("utf-8"))
    except urllib.error.HTTPError as exc:
        log(f"person_record: the control plane answered {exc.code} for {path}: nothing confirmed")
    except Exception as exc:  # noqa: BLE001 - fail closed
        log(f"person_record: the control plane could not be read ({type(exc).__name__}): nothing confirmed")
    with _lock:
        _cache[url] = (now, answer)
    return answer


def clear_cache():
    with _lock:
        _cache.clear()


def _persons(rows, task_id=None):
    """The rows that are a person's own judgement (not derived from another), on `task_id` when given."""

    out = []
    for row in rows if isinstance(rows, list) else []:
        if not isinstance(row, dict) or not isinstance(row.get("text"), str) or not row.get("id"):
            continue
        actor = row.get("actor") if isinstance(row.get("actor"), dict) else {}
        if actor.get("kind") != "person" or row.get("derived_from") or row.get("secret"):
            continue
        if task_id and row.get("task_id") != task_id:
            continue
        out.append(row)
    return out


def judgements(task_id):
    """The person's own judgements on this desktop's task, as the control plane keeps them ([] when none or unread)."""

    if not task_context.valid(task_id):
        return []
    answer = _get(JUDGEMENTS_PATH, {"task": task_id})
    return _persons((answer or {}).get("judgements") if isinstance(answer, dict) else None, task_id)


def answers(task_id=None):
    """
    [{"id", "asked", "text"}]: each reply this task kept (guidance.kept_replies) that the control plane confirms as a
    person's judgement on this task with the same text (its hash). The text and the question are the control plane's.
    """

    task_id = task_id or task_context.current()
    try:
        from decurix_agent import guidance
        kept = guidance.kept_replies(task_id)
    except Exception:  # noqa: BLE001
        kept = []
    if not kept:
        return []
    confirmed = {row["id"]: row for row in judgements(task_id)}
    out = []
    for row in kept:
        source = confirmed.get(row.get("judgement") or "")
        if source is None or not row.get("sha256") or text_hash(source["text"]) != row["sha256"]:
            continue
        out.append({"id": source["id"], "asked": str(source.get("asked") or ""), "text": source["text"]})
    return out


def taught(skill):
    """The person's own words on this desktop's tasks that taught or ran `skill` (texts), as the control plane keeps
    them ([] when none or unread)."""

    if not skill:
        return []
    answer = _get(JUDGEMENTS_PATH, {"skill": str(skill)})
    rows = _persons((answer or {}).get("judgements") if isinstance(answer, dict) else None)
    return [row["text"] for row in rows if not row.get("skill_name") or row.get("skill_name") == skill]


def field_answers():
    """[{"label", "host", "option", "source_text", "source_asked", "judgement"}]: the field answers the control plane
    keeps for this desktop, each with the text of the person's judgement it was derived from and the question that
    judgement answered (H-PY-19; "" until the control plane sends it, and then nothing is backed) ([] when none or
    unread)."""

    answer = _get(LEARNED_PATH, {"kind": FIELD_ANSWER})
    out = []
    for row in (answer or {}).get("learned") or [] if isinstance(answer, dict) else []:
        if not isinstance(row, dict) or row.get("kind") != FIELD_ANSWER or not row.get("derived_from"):
            continue
        source = row.get("source") if isinstance(row.get("source"), dict) else {}
        actor = source.get("actor") if isinstance(source.get("actor"), dict) else {}
        if actor.get("kind") != "person" or not isinstance(source.get("text"), str):
            continue
        try:
            said = json.loads(row.get("text") or "")
        except ValueError:
            continue
        if not isinstance(said, dict) or not said.get("label") or not said.get("option"):
            continue
        out.append({"label": str(said["label"]), "host": str(said.get("host") or ""), "option": str(said["option"]),
                    "source_text": source["text"], "source_asked": str(source.get("asked") or ""),
                    "judgement": str(row["derived_from"])})
    return out


def keep_field_answer(task_id, judgement_id, label, host, option, asked=""):
    """Keep on the control plane that the person's judgement `judgement_id` answered which entry `label` takes in the
    application at `host`: a learned row derived from it (messaging.post_learned). The thread, or None."""

    from decurix_agent import messaging
    text = json.dumps({"label": label, "host": host, "option": option}, ensure_ascii=False, sort_keys=True)
    return messaging.post_learned(FIELD_ANSWER, "", 0, asked, text, task_id=task_id, derived_from=judgement_id)
