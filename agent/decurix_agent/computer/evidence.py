import json
import time
from pathlib import Path

from decurix_agent import config, task_context
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


def live_text(limit=4000):
    """
    The text the fields on the screen hold now, read from the apps themselves:
    exact, where small print in a picture is not. Found by the GUI benchmark:
    asked whether every "colour" in a file had become "color", the check read
    the screenshot as showing all three changed when the third was not. Each
    field's whole text (a secret field's never), the window with the keyboard
    first; "" when the screen cannot be read.
    """

    try:
        from decurix_agent.computer import core

        observation = core.observe(remember=False)
    except Exception as exc:
        log(f"evidence: could not read the screen's text: {exc}")
        return ""

    return fields_text(observation, limit)


def fields_text(observation, limit=4000, offscreen=False):
    """What the fields of one look at the screen hold, as text (see live_text); "" when none holds anything."""

    from decurix_agent.computer import core, privacy
    from decurix_agent.computer import elements as el

    out = [f"Window with the keyboard: {observation.get('active_title') or '(none)'}"]
    active = core.active_group(observation)
    fields = [e for e in observation.get("elements", [])
              if e.get("role") in core.TYPING_ROLES and (e.get("value") or "").strip()
              and (offscreen or "offscreen" not in e.get("states", [])) and not privacy.secret_field(e)]
    fields.sort(key=lambda e: e.get("group") != active)
    for e in fields:
        text, withheld = privacy.withhold_text(e, e["value"])
        if withheld or not text:
            continue
        label = f"{e['role']} {el._quote(e.get('name') or '', 60)}"
        out.append(f"{label} holds:" + ("\n" + text.strip() if "\n" in text.strip() else " " + text.strip()))
    text = "\n".join(out)
    return text[:limit] + (" ..." if len(text) > limit else "") if len(out) > 1 else ""


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

NOTES_DIR = config.DECURIX_DIR / "computer" / "evidence"
NOTES_KEEP_SECONDS = 2 * 24 * 3600
MAX_NOTE = 18000
MAX_OBSERVED = 40
# How much of what an action changed on the screen is kept beside it (a search's answer fits; a page's new contents do not).
MAX_CHANGE = 220


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


# How much of one look at the screen is kept as a note.
MAX_LOOK = 2000
MAX_FIELDS = 8000
MAX_WORDS = 7000
# What a page says without it being a field: a saved record shows its rows and totals as plain words.
READ_ROLES = ("text", "link", "cell", "gridcell", "heading", "status", "alert", "rowheader", "columnheader", "listitem")


def words_text(observation, limit=MAX_WORDS):
    """
    The words the front window's page shows that are not fields, in the page's
    order, the part out of view included; "" when it shows none. Found live:
    once a record is saved its rows and totals are no longer fields but plain
    words, so a look that kept only fields left the check unable to confirm a
    correct record ("does not show the entered row or the total").
    """

    from decurix_agent.computer import core

    active = core.active_group(observation)
    out, last = [], None
    for e in observation.get("elements", []):
        # (Which window is in front cannot always be told - an app that is a page in a window of its own is
        # titled otherwise than its page: then every window's words are kept. Found live: nothing was.)
        if (active is not None and e.get("group") != active) or e.get("role") not in READ_ROLES:
            continue
        name = " ".join((e.get("name") or "").split())
        if not name or name == last:
            continue
        out.append(name[:200])
        last = name
    text = " | ".join(out)
    return text[:limit] + (" ..." if len(text) > limit else "")


def note_look(shown, observation=None):
    """
    Keep what a tool's look at the screen read, for the task being worked on:
    the list of windows and elements exactly as the engine was shown it (read
    only values are in it as text, which a list of fields would miss). Found
    live: a worker read a saved record's date and total off its page (`gui
    observe`), closed the page as the task asks, and reported both - and the
    check, shown the closed screen and a list of actions, could not confirm a
    value nobody had kept. A look is evidence only if what it saw is written
    down. Never raises.
    """

    # Every field of the page with what it holds, the part scrolled out of sight included: a total at the foot of
    # a long form is on the page when its top is on the screen. Found live: a correct record could not be
    # confirmed because the look that read it listed only the part in view.
    held = words = ""
    if observation is not None:
        try:
            held = fields_text(observation, MAX_FIELDS, offscreen=True)
            words = words_text(observation)
        except Exception as exc:
            log(f"evidence: could not list the fields of a look: {exc}")
    if (shown and shown.strip()) or held or words:
        note("a look at the screen then. What every field of the page held (in view or not):\n" + (held or "(none)")
             + "\nThe words the page showed outside its fields, in its order (in view or not):\n" + (words or "(none)")
             + "\nThe part in view, as the tool listed it:\n" + (shown or "").strip()[:MAX_LOOK])


def _prune():
    cutoff = time.time() - NOTES_KEEP_SECONDS
    for path in NOTES_DIR.glob("*.jsonl"):
        try:
            if path.stat().st_mtime < cutoff:
                path.unlink()
        except OSError:
            pass


def entered(task_id=None):
    """
    What a task's tools put into fields, oldest first: the text of each typing
    that went through, and each option selected. A text that was withheld (a
    secret) is not among them.
    """

    task_id = task_id or task_context.current()
    if not task_id:
        return []
    try:
        _, events = recording.load(recording.session_for_task(task_id))
    except (FileNotFoundError, OSError, ValueError):
        return []
    return [str(value) for event in events if event.get("outcome") in (None, "done")
            for value in (event.get("text") if event.get("kind") == "type" else None,
                          event.get("option") if event.get("kind") == "select" else None) if value]


def pressed(task_id=None):
    """The names of the controls a task's tools pressed and that went through, oldest first."""

    task_id = task_id or task_context.current()
    if not task_id:
        return []
    try:
        _, events = recording.load(recording.session_for_task(task_id))
    except (FileNotFoundError, OSError, ValueError):
        return []
    return [str((event.get("element") or {}).get("name") or "") for event in events
            if event.get("outcome") in (None, "done") and event.get("kind") in ("click", "double_click")
            and (event.get("element") or {}).get("name")]


def read(task_id=None):
    """Everything a task's tools read off the screen, as one text ("" when nothing was kept)."""

    task_id = task_id or task_context.current()
    if not task_id:
        return ""
    out = []
    try:
        with open(NOTES_DIR / f"{task_id}.jsonl", encoding="utf-8") as f:
            for line in f:
                try:
                    out.append(json.loads(line)["text"])
                except (ValueError, KeyError):
                    continue
    except OSError:
        pass
    return "\n".join(out)


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
            # ...and what the screen did in answer, which is what a check made along the way found: a search
            # whose result ("No Results found") was missing here left the final check unable to see that the
            # duplicate check was made, and it paged a person about a correct bill (found live, 2026-10-13).
            change = " ".join(str(event.get("change") or "").split())
            after = f" -> {change[:MAX_CHANGE]}{'…' if len(change) > MAX_CHANGE else ''}" if change else ""
            lines.append(f"action: {summary} ({event.get('outcome') or 'done'}){after}")
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
