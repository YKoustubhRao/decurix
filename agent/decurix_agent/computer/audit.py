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

# Set by replay.py around each step it runs, so that step's event says which
# recording, and which step of it, the action came from.
origin = None


def record(entry, before=None, after=None, masks=()):
    """
    Record one action and report it as a chat step. `entry` is the action's
    description; `before`/`after` are screen.Frame objects (either may be
    None); `masks` are the [x, y, w, h] boxes of secret fields, painted over
    in both. Returns the record id.
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
            defaults={"task_id": task_id, "title": privacy.safe_text(task_context.current_request())[:500]},
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
