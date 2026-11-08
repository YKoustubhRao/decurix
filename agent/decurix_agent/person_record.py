"""
What the control plane recorded as a person's word, read from the control plane - never from a file the worker's own
user can write (review round 10, R10-B1: a row forged into `field-answers.json`, or a line appended to `<task>.said`,
backed the worker's own guessed pick; the skill file's steps are partly model-written).

Three reads, each of what the control plane keeps as a human judgement (H-PY-18 asks CP for the desktop-scoped reads):

- answers(task): the person's own judgements on this desktop's task (an answer to a question it asked), each with the
  question it answered. A reply the watcher kept in `<task>.said` backs a pick only when its judgement id is one of
  these and its text's hash is that judgement's: the text used is the control plane's, never the file's.
- taught(skill): the person's own judgements on this desktop's tasks that taught or ran that skill.
- worker_words(task): the person's own answers on any task of this desktop's worker, newest first and bounded
  (?scope=worker): a rule taught in an earlier task ("region 07 goes to fund FND-3") backs the value the worker
  derives from it in a later one, since the value stands in the rule's words. Read once per task. Never the worker's
  files or notes.
- field_answers(): what a person answered about which entry a field takes, kept as the control plane's learned rows
  (kind "field_answer", derived from that answer's judgement), each with the judgement's own text - an option backs a
  later pick only when it is picked out by that text (records._kept_answer).
- field_units(): what a person answered about whether a field takes its figures in a unit (kind "field_unit", derived
  from their reply to the save's numbered unit question), each with that judgement's text and question - the answer
  is used only when that question holds the question's sentence and the reply is the row's option (unit_answers).

Who counts as the person: a judgement whose actor kind is "person", or "scripted" - the eval's scripted expert, whose
replies a practice desktop's control plane (the practice broker) keeps and serves in a person's place. Never a worker's
or a program's, never one derived from another (what a desktop kept), never a secret one.

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
FIELD_UNIT = "field_unit"
TIMEOUT_SECONDS = 5
# How long one read is reused: one task's checks read it many times; a failed read is kept as long, so a control plane
# that does not answer costs one wait, not one per pick.
CACHE_SECONDS = 60
MAX_ANSWER_BYTES = 2 * 1024 * 1024
# The actor kinds whose own words are the person's (above).
PERSON_KINDS = frozenset({"person", "scripted"})
# How many of the worker's earlier answers are read (newest first): the control plane's default for the read.
WORKER_WORDS_LIMIT = 100
# The earlier tasks' answers the final check is shown (worker_said), newest first.
WORKER_SAID_LIMIT = 12

_cache = {}
_lock = threading.Lock()


def text_hash(text):
    """The hash a kept reply carries of its text (the text with its spacing collapsed)."""

    return hashlib.sha256(" ".join(str(text or "").split()).encode("utf-8")).hexdigest()


def _get(path, query, fresh=False):
    """The control plane's JSON answer to a GET, or None (unreachable, refused, not there, not JSON). Cached; `fresh`
    reads it again."""

    url = config.CONTROL_PLANE_URL.rstrip("/") + path + "?" + urllib.parse.urlencode(sorted(query.items()))
    now = time.monotonic()
    with _lock:
        kept = _cache.get(url)
        if kept and now - kept[0] < CACHE_SECONDS and not fresh:
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
        _worker_read["task"] = None


def _persons(rows, task_id=None):
    """The rows that are a person's own judgement (not derived from another), on `task_id` when given."""

    out = []
    for row in rows if isinstance(rows, list) else []:
        if not isinstance(row, dict) or not isinstance(row.get("text"), str) or not row.get("id"):
            continue
        actor = row.get("actor") if isinstance(row.get("actor"), dict) else {}
        if actor.get("kind") not in PERSON_KINDS or row.get("derived_from") or row.get("secret"):
            continue
        if task_id and row.get("task_id") != task_id:
            continue
        out.append(row)
    return out


def judgements(task_id, fresh=False):
    """The person's own judgements on this desktop's task, as the control plane keeps them ([] when none or unread).
    `fresh`: read again rather than from the cache (a reply that came after the cached read)."""

    if not task_context.valid(task_id):
        return []
    answer = _get(JUDGEMENTS_PATH, {"task": task_id}, fresh=True) if fresh else _get(JUDGEMENTS_PATH, {"task": task_id})
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


_worker_read = {"task": None}


def worker_words(task_id=None):
    """
    The person's own answers on any task of this desktop's worker - a reply to a question, one that taught a rule or
    confirmed a lesson - as the control plane keeps them (texts, newest first, at most WORKER_WORDS_LIMIT; [] when none
    or unread). Read again when the task changes (else the read is reused for CACHE_SECONDS), so a later task sees
    what an earlier one was told. Only the control plane's record: never a file or note the worker's user can write.
    """

    return [row["text"] for row in _worker_rows(task_id)]


def _worker_rows(task_id=None):
    """The rows worker_words reads (the person's own answers on any task of this worker, newest first, bounded)."""

    task = task_id or task_context.current() or ""
    query = {"scope": "worker", "kind": "answer", "limit": str(WORKER_WORDS_LIMIT)}
    with _lock:
        fresh, _worker_read["task"] = _worker_read["task"] != task, task
    answer = _get(JUDGEMENTS_PATH, query, fresh=True) if fresh else _get(JUDGEMENTS_PATH, query)
    rows = _persons((answer or {}).get("judgements") if isinstance(answer, dict) else None)
    return rows[:WORKER_WORDS_LIMIT]


def worker_said(task_id=None, limit=WORKER_SAID_LIMIT):
    """
    [{"asked", "text", "task_id"}]: the person's answers on this worker's EARLIER tasks (never this one's: the task's
    own are said_this_task's), each with the question it answered, newest first, at most `limit`. For the final check:
    a rule the person taught on an earlier task holds on this one, and an answer is read with its question. Found
    live (2026-11-08): the check, shown none of them, failed a reconciliation laid out as the person had taught ("one
    row per statement line") for lacking a row they never asked for; the worker added it, and the second check passed
    the wrong file.
    """

    task = task_id or task_context.current() or ""
    out = []
    for row in _worker_rows(task_id):
        if row.get("task_id") == task:
            continue
        out.append({"asked": str(row.get("asked") or ""), "text": row["text"], "task_id": str(row.get("task_id") or "")})
        if len(out) >= limit:
            break
    return out


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
        if actor.get("kind") not in PERSON_KINDS or not isinstance(source.get("text"), str):
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


def _learned(kind):
    """[(row, its text as JSON, its source)] of the control plane's learned rows of `kind` for this desktop that are
    derived from a person's judgement whose text is sent ([] when none or unread)."""

    answer = _get(LEARNED_PATH, {"kind": kind})
    out = []
    for row in (answer or {}).get("learned") or [] if isinstance(answer, dict) else []:
        if not isinstance(row, dict) or row.get("kind") != kind or not row.get("derived_from"):
            continue
        source = row.get("source") if isinstance(row.get("source"), dict) else {}
        actor = source.get("actor") if isinstance(source.get("actor"), dict) else {}
        if actor.get("kind") not in PERSON_KINDS or not isinstance(source.get("text"), str):
            continue
        try:
            said = json.loads(row.get("text") or "")
        except ValueError:
            continue
        if isinstance(said, dict):
            out.append((row, said, source))
    return out


def field_units():
    """[{"label", "host", "form", "unit", "takes", "source_text", "source_asked", "judgement"}]: the field units the
    control plane keeps for this desktop, each with the text of the person's judgement it was derived from and the
    question that judgement answered ([] when none or unread). unit_answers.answered checks both again."""

    out = []
    for row, said, source in _learned(FIELD_UNIT):
        if not said.get("label") or not said.get("host") or not said.get("unit") or not isinstance(said.get("takes"), bool):
            continue
        out.append({"label": str(said["label"]), "host": str(said["host"]), "form": str(said.get("form") or ""),
                    "unit": str(said["unit"]), "takes": said["takes"], "source_text": source["text"],
                    "source_asked": str(source.get("asked") or ""), "judgement": str(row["derived_from"])})
    return out


def keep_field_unit(task_id, judgement_id, label, host, form, unit, takes, asked=""):
    """Keep on the control plane that the person's judgement `judgement_id` answered whether `label` on the form `form`
    of the application at `host` takes its figures in `unit`: a learned row derived from it (messaging.post_learned).
    The thread, or None."""

    from decurix_agent import messaging, unit_answers
    return messaging.post_learned(FIELD_UNIT, "", 0, asked, unit_answers.kind_json(label, host, form, unit, takes),
                                  task_id=task_id, derived_from=judgement_id)
