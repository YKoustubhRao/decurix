import json
import os
import time
import uuid
from pathlib import Path

from flowera_agent import config, progress, task_context
from flowera_agent.logging_utils import log


# ============================================================
# The audit trail
#
# Every on-screen action - done, failed, or stopped for approval - is kept
# twice:
#
# - on the desktop's own disk, in full: one JSON line in
#   ~/audit/actions.jsonl plus full-resolution before/after PNGs under
#   ~/audit/frames/. /home/headless is the desktop's persistent volume, so
#   this survives restarts and image rebuilds, and it is the record to mine
#   later (what the screen looked like, what was done, what changed).
# - in the chat, as a step of the running turn, with half-size screenshots,
#   through progress.emit_action - the control plane's live record a human
#   can expand under the answer.
#
# Neither write may fail an action. The action has already happened by the
# time it is recorded; losing its record is logged, never raised.
# ============================================================

AUDIT_DIR = Path(os.environ.get("FLOWERA_AUDIT_DIR") or (Path.home() / "audit"))

# How many actions' frames to keep on disk. The JSON lines are kept whole;
# at ~200 KB per full-size frame this is what bounds the disk.
KEEP_ACTIONS = int(os.environ.get("FLOWERA_AUDIT_KEEP", "2000"))

THUMBNAIL_SCALE = 2


def _prune(frames):
    try:
        files = sorted(frames.glob("*.png"))
    except OSError:
        return

    excess = len(files) - KEEP_ACTIONS * 2
    for path in files[:max(0, excess)]:
        try:
            path.unlink()
        except OSError:
            pass


def record(entry, before=None, after=None):
    """
    Persist one action and report it as a chat step. `entry` is the action's
    description; `before`/`after` are screen.Frame objects (either may be None).
    Returns the record id.
    """

    record_id = time.strftime("%Y%m%dT%H%M%S") + "-" + uuid.uuid4().hex[:8]

    entry = dict(entry)
    entry["id"] = record_id
    entry["time"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    entry["desktop_id"] = config.DESKTOP_ID
    entry["task_id"] = task_context.current()

    before_small = after_small = None

    try:
        frames = AUDIT_DIR / "frames"
        frames.mkdir(parents=True, exist_ok=True)

        for label, frame in (("before", before), ("after", after)):
            if frame is None:
                continue
            path = frames / f"{record_id}-{label}.png"
            path.write_bytes(frame.png())
            entry[f"{label}_frame"] = str(path)

        with open(AUDIT_DIR / "actions.jsonl", "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, ensure_ascii=False) + "\n")

        _prune(frames)
    except OSError as exc:
        log(f"Could not write the audit record for {record_id}: {exc}")

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
        },
        before_small,
        after_small,
    )

    return record_id
