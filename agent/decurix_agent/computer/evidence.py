import json
import time
from pathlib import Path

from decurix_agent import task_context
from decurix_agent.computer import recording, screen, visual
from decurix_agent.logging_utils import log


# ============================================================
# Evidence: pictures of what a task looked like
#
# What a model is shown when it has to judge or follow a taught task: the
# screen a step of the demonstration was done on (with the spot the person
# pressed marked), the screen the demonstration ended on, and the screen now.
# A skill's steps keep the recording and event they came from ("session",
# "event"), so any of them can be read back while the recording is on disk.
# Nothing here decides anything; verify.py and the engine do.
# ============================================================

MARK_RADIUS = 18
MARK_WIDTH = 4
MARK_RGB = (230, 30, 30)


def _event(step):
    session, index = step.get("session"), step.get("event")
    if not session or index is None:
        return None, None
    try:
        _, events = recording.load(session)
    except (FileNotFoundError, OSError, ValueError):
        return session, None
    return session, next((e for e in events if e.get("i") == index), None)


def step_frame(step, which="before"):
    """PNG bytes of the screen a step was recorded on ("before") or left ("after"), or None."""

    session, event = _event(step)
    if not event or not event.get(which):
        return None
    name = str(event[which]).rsplit("/", 1)[-1]
    try:
        path = recording.frame_file(session, name)
        return path.read_bytes() if path else None
    except OSError:
        return None


def mark(png, point):
    """The picture with a ring around `point` (x, y), or the picture unchanged when it cannot be drawn on."""

    if not png or not point:
        return png
    try:
        width, height, rgb = visual.decode_png(png)
    except ValueError:
        return png

    rgb = bytearray(rgb)
    cx, cy = int(point[0]), int(point[1])
    outer, inner = MARK_RADIUS ** 2, (MARK_RADIUS - MARK_WIDTH) ** 2

    for y in range(max(0, cy - MARK_RADIUS), min(height, cy + MARK_RADIUS + 1)):
        for x in range(max(0, cx - MARK_RADIUS), min(width, cx + MARK_RADIUS + 1)):
            d = (x - cx) ** 2 + (y - cy) ** 2
            if inner <= d <= outer:
                at = (y * width + x) * 3
                rgb[at:at + 3] = bytes(MARK_RGB)

    return screen.Frame(width, height, bytes(rgb)).png()


def step_picture(step):
    """What a step was done on, with the spot pressed marked when it was a press; None when gone."""

    png = step_frame(step, "before")
    return mark(png, step.get("point")) if png else None


def final_frame(skill):
    """The screen the demonstration ended on: the last step's "after", else its "before"."""

    for step in reversed(skill.get("steps") or []):
        png = step_frame(step, "after") or step_frame(step, "before")
        if png:
            return png
    return None


def live(scale=1):
    """The screen now as PNG bytes, or None when it cannot be read."""

    try:
        return screen.grab().png(scale=max(1, int(scale)))
    except Exception as exc:
        log(f"evidence: could not read the screen: {exc}")
        return None


# ============================================================
# What the tools saw during a task
#
# The final screen shows the end of a task, not all of it: a list of five
# questions put to ChatGPT leaves the fifth answer on the screen and the other
# four in tabs behind it. Found live: the check could only say "unsure" about
# a task that had in fact gone right - and, the time before, about one where
# the first question had reached ChatGPT garbled. So what a tool read off the
# screen (`gui ask`'s answer) is noted here per task, and every screen action
# of the task is in its recording; the check is shown both, as what was
# observed - never the worker's own claims.
# ============================================================

NOTES_DIR = Path.home() / ".decurix" / "computer" / "evidence"
NOTES_KEEP_SECONDS = 2 * 24 * 3600
MAX_NOTE = 4000
MAX_OBSERVED = 40


def note(text):
    """Keep something a tool read off the screen, for the task being worked on. Never raises."""

    task_id = task_context.current()
    if not task_id or not text:
        return
    try:
        NOTES_DIR.mkdir(parents=True, exist_ok=True)
        with open(NOTES_DIR / f"{task_id}.jsonl", "a", encoding="utf-8") as f:
            f.write(json.dumps({"at": time.time(), "text": str(text)[:MAX_NOTE]}, ensure_ascii=False) + "\n")
        _prune()
    except OSError as exc:
        log(f"evidence: could not keep a note: {exc}")


def _prune():
    cutoff = time.time() - NOTES_KEEP_SECONDS
    for path in NOTES_DIR.glob("*.jsonl"):
        try:
            if path.stat().st_mtime < cutoff:
                path.unlink()
        except OSError:
            pass


def observed(task_id=None):
    """What the tools did and read during a task, oldest first: its screen actions, then its notes."""

    task_id = task_id or task_context.current()
    if not task_id:
        return []

    lines = []
    try:
        _, events = recording.load(recording.session_for_task(task_id))
        for event in events[-MAX_OBSERVED:]:
            summary = event.get("summary") or event.get("kind")
            lines.append(f"action: {summary} ({event.get('outcome') or 'done'})")
    except (FileNotFoundError, OSError, ValueError):
        pass

    try:
        with open(NOTES_DIR / f"{task_id}.jsonl", encoding="utf-8") as f:
            for line in f.readlines()[-MAX_OBSERVED:]:
                try:
                    lines.append("read off the screen: " + json.loads(line)["text"])
                except (ValueError, KeyError):
                    continue
    except OSError:
        pass

    return lines
