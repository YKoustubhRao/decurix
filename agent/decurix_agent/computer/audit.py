import hashlib
import json
import os
import re
import time
import uuid

from decurix_agent import config, progress, task_context
from decurix_agent.computer import privacy, recording
from decurix_agent.logging_utils import log


# ============================================================
# The audit trail
#
# Every on-screen action - done, failed, or stopped for approval - is kept
# twice:
#
# - in the desktop's recordings (recording.py), as one event of the session
#   for the task it belongs to, with full-resolution before/after frames, the
#   element it touched and what it changed. /home/headless is the desktop's
#   persistent volume, so this survives restarts and image rebuilds; and it
#   is the format a person's demonstration is recorded in, so a task an agent
#   finished is read, exported and replayed exactly like one.
# - in the chat, as a step of the running turn, with half-size screenshots,
#   through progress.emit_action - the control plane's live record a human
#   can expand under the answer.
#
# What reaches either copy has already been through privacy.py (core.act
# builds the entry that way); the frames are masked here, over the boxes of
# the secret fields the caller found on screen.
#
# Neither write may fail an action. The action has already happened by the
# time it is recorded; losing its record is logged, never raised.
# ============================================================

THUMBNAIL_SCALE = 2

# Soul's turn, set on every command the operator runs (PLAN-P2 C3): which model call chose this action. The shape is
# C1's; anything else is not kept.
TURN_RE = re.compile(r"^[A-Za-z0-9:._-]{1,64}$")

# What an action was resolved against, kept beside it (R1): the active window's elements, at most this many and this
# many bytes, in the recording's obs/ folder by the hash of what they hold (an unchanged screen is stored once).
MAX_OBS_ELEMENTS = 2000
MAX_OBS_BYTES = 256 * 1024
OBS_KEYS = ("role", "name", "value", "states", "box")

# Set by replay.py around each step it runs, so that step's event says which
# recording, and which step of it, the action came from.
origin = None


def turn():
    """Soul's turn this process runs in (DECURIX_TURN_ID), or "" when none or not of C1's shape."""

    value = os.environ.get("DECURIX_TURN_ID", "").strip()
    return value if TURN_RE.match(value) else ""


def _observation_doc(observation):
    """The active window's elements of an observation, as kept: a secret field's value withheld, all scrubbed."""

    from decurix_agent.computer import core
    elements = observation.get("elements") or []
    group = core.active_group(observation)
    if group is not None:
        elements = [e for e in elements if e.get("group") == group] or elements
    kept = []
    for element in elements[:MAX_OBS_ELEMENTS]:
        item = {k: element.get(k) for k in OBS_KEYS if element.get(k) not in (None, "", [])}
        if privacy.secret_field(element) and "value" in item:
            item["value"] = None
            item["withheld"] = "secret field"
        kept.append(item)
    doc = {"window": observation.get("active_title") or "", "url": observation.get("page_url") or "",
           "elements": kept}
    return privacy.scrub(doc)


def keep_observation(session_id, observation):
    """
    Keep what an action was resolved against in the recording (obs/<sha256[:16]>.json) and return that name's stem,
    or "" when nothing was kept. Never raises: a failure drops the observation and says so in the log.
    """

    if not isinstance(observation, dict):
        return ""
    try:
        doc = _observation_doc(observation)
        if not isinstance(doc, dict) or "held_back" in doc:
            return ""
        data = json.dumps(doc, ensure_ascii=False, sort_keys=True).encode("utf-8")
        while len(data) > MAX_OBS_BYTES and doc["elements"]:
            doc["elements"] = doc["elements"][:len(doc["elements"]) // 2]
            doc["cut"] = True
            data = json.dumps(doc, ensure_ascii=False, sort_keys=True).encode("utf-8")
        if len(data) > MAX_OBS_BYTES:
            return ""
        name = hashlib.sha256(data).hexdigest()[:16]
        folder = recording._dir(session_id) / "obs"
        path = folder / f"{name}.json"
        if not path.exists():
            folder.mkdir(parents=True, exist_ok=True)
            tmp = folder / f".{name}.{uuid.uuid4().hex[:6]}.tmp"
            tmp.write_bytes(data)
            os.replace(tmp, path)
            from decurix_agent.computer import broker
            broker.mirror(path, data)  # (broker mode: the caller's copy too)
        return name
    except Exception as exc:  # noqa: BLE001 - decoration: the action has happened, its observation is only dropped
        log(f"Could not keep the observation of an action: {exc}")
        return ""


def record(entry, before=None, after=None, masks=(), observation=None):
    """
    Record one action and report it as a chat step. `entry` is the action's
    description; `before`/`after` are screen.Frame objects (either may be
    None); `masks` are the [x, y, w, h] boxes of secret fields, painted over
    in both; `observation` is the look the action was resolved against (kept
    in the recording's obs/, named on the event as "obs"). The event also
    carries soul's "turn" when this command runs in one. Returns the record id.
    """

    record_id = time.strftime("%Y%m%dT%H%M%S") + "-" + uuid.uuid4().hex[:8]
    task_id = task_context.current()

    entry = dict(entry)
    entry["id"] = record_id
    entry.setdefault("actor", "agent")
    entry["desktop_id"] = config.DESKTOP_ID
    entry["task_id"] = task_id
    if origin:
        entry["replay_of"] = dict(origin)
    if turn():
        entry["turn"] = turn()
    if observation is not None:
        kept = keep_observation(recording.session_for_task(task_id), observation)
        if kept:
            entry["obs"] = kept

    try:
        if masks:
            before = before.masked(masks) if before is not None else None
            after = after.masked(masks) if after is not None else None
    except Exception as exc:
        # Keeping them unmasked is not an option: drop the frames instead.
        log(f"Could not mask the frames of {record_id}, dropping them: {exc}")
        before = after = None

    session_id = recording.session_for_task(task_id)
    event = entry

    try:
        event = recording.append(
            session_id, entry, before=before, after=after,
            # Withheld before it is cut: a cut can split a secret so it no longer reads as one (review 2026-11-03,
            # D-01: the title was the raw request). recording.append scrubs the meta as well.
            defaults={"task_id": task_id, "title": privacy.safe_text(task_context.current_request(full=True))[:500]},
        )
    except (OSError, ValueError) as exc:
        log(f"Could not write the recording of {record_id}: {exc}")

    before_small = after_small = None

    try:
        before_small = before.png(THUMBNAIL_SCALE) if before is not None else None
        after_small = after.png(THUMBNAIL_SCALE) if after is not None else None
    except Exception as exc:
        log(f"Could not encode audit screenshots: {exc}")

    progress.emit_action(
        entry.get("summary") or entry.get("kind", "action"),
        {
            "id": record_id,
            "kind": entry.get("kind"),
            "target": entry.get("target"),
            "backend": entry.get("backend"),
            "outcome": entry.get("outcome"),
            "change": entry.get("change"),
            "reason": entry.get("reason"),
            "session": session_id,
            "index": event.get("i"),
        },
        before_small,
        after_small,
    )

    return record_id
