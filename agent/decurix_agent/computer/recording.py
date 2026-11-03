import hashlib
import json
import os
import re
import shutil
import struct
import time
import uuid
import zipfile
from contextlib import contextmanager
from datetime import datetime
from pathlib import Path

from decurix_agent import config, filelock
from decurix_agent.computer import privacy
from decurix_agent.logging_utils import log


# ============================================================
# Recordings
#
# One format for everything done on this desktop's screen, whoever did it: an
# agent working a task (every action, through computer/audit.py), a person
# showing how something is done (computer/demonstration.py), a recording
# being replayed (computer/replay.py). It is openadapt-flow's recording
# layout, so a recording reads, exports and replays the same way whichever of
# those made it:
#
#   ~/.decurix/recordings/<session>/
#     meta.json      what the recording is: kind, title, task, when, counts
#     events.jsonl   one action per line, in order, on the session's clock
#                    ("t" is seconds since it started): the element it
#                    touched, what it typed, what it changed
#     frames/        the before/after screenshots, named by their pixels -
#                    one action's "after" is very often the next one's
#                    "before", and is stored once
#
# A session is a task ("task-<task id>"), a day of actions no task claimed
# ("adhoc-YYYYMMDD": a gui command typed by hand), or a demonstration
# ("demo-<when>-<random>").
#
# Several processes append to one session at once - a `gui` command, Claude
# Code's MCP server, the demonstration recorder - so every change to a
# session happens under a file lock on it. What it is given has already been
# through privacy.py's helpers; every event and every meta still goes through
# privacy.scrub as it is written, so a caller that forgot one cannot leak
# (review 2026-11-03: a task's recording was titled with the raw request).
# ============================================================

ROOT = Path(os.environ.get("DECURIX_RECORDINGS_DIR") or (config.DECURIX_DIR / "recordings"))

# The disk budget. Task and hand-run recordings are pruned oldest first past
# it; a demonstration is something a person authored, and only a person
# deletes one.
MAX_BYTES = int(os.environ.get("DECURIX_RECORDINGS_MB", "1024")) * 1024 * 1024

SCHEMA = "decurix.recording/v1"

SESSION_RE = re.compile(r"^(task|adhoc|demo)-[A-Za-z0-9-]{1,64}$")
FRAME_RE = re.compile(r"^[0-9a-f]{20}\.png$")

KINDS = {"task": "task", "adhoc": "adhoc", "demo": "demonstration"}

TIME_FORMAT = "%Y-%m-%dT%H:%M:%S%z"

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"

README = """A Decurix recording.

meta.json     what was recorded: kind (task, adhoc, demonstration), title,
              task_id, started_at/ended_at (Unix seconds), screen size,
              who acted (agent, human), counts
events.jsonl  one on-screen action per line, in order. "i" is its index and
              "t" its time in seconds since the recording started. "kind" is
              click, double_click, right_click, drag, type, key, scroll,
              select, open or manual; "element" is what it acted on (role,
              name, path); "before"/"after" name its frames; "change" says
              what the action changed and "expect" is the same as data.
              A value the recording did not keep is null, with the reason
              under "withheld" - password fields and anything that looked
              like a credential are never stored. A "note" is not an action:
              a person's own written explanation of what they were doing,
              sitting at its point in the timeline like everything else -
              never replayed, and withheld the same way a typed field is if
              it looks like a credential.
program.json  the recording compiled for replay: its parameters (the text
              each typed field took) and, for every step, what the replay
              checks before it moves on.
frames/       the screenshots, named by their content. Password fields are
              painted over.
"""


def valid(session_id):
    return bool(SESSION_RE.match(session_id or ""))


def session_for_task(task_id):
    """The session an action taken for `task_id` is recorded in."""

    if task_id:
        return f"task-{task_id}"

    return "adhoc-" + time.strftime("%Y%m%d")


def new_demonstration_id():
    return "demo-" + time.strftime("%Y%m%dT%H%M%S") + "-" + uuid.uuid4().hex[:6]


def _dir(session_id):
    if not valid(session_id):
        raise ValueError(f"not a recording: {session_id!r}")
    return ROOT / session_id


# How long a writer waits for another holding a session's lock. Found in review (2026-11-03): the wait had no end, so
# one stuck process (a `gui` command killed mid-write leaves no lock, but a hung one keeps it) stalled every action.
LOCK_WAIT_SECONDS = float(os.environ.get("DECURIX_RECORDING_LOCK_S", "30"))


@contextmanager
def _locked(directory):
    directory.mkdir(parents=True, exist_ok=True)

    with open(directory / ".lock", "w") as lock:
        deadline = time.monotonic() + LOCK_WAIT_SECONDS
        while not filelock.try_exclusive(lock):
            if time.monotonic() > deadline:
                raise TimeoutError(f"recording {directory.name} is held by another writer")
            time.sleep(0.02)
        yield


def _read_meta(directory):
    try:
        return json.loads((directory / "meta.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


def write_whole(path, data):
    """Write `data` (bytes, or text as UTF-8) to `path` at once: to a .tmp file beside it, then moved over it."""

    tmp = path.with_name(path.name + ".tmp")
    if isinstance(data, bytes):
        tmp.write_bytes(data)
    else:
        tmp.write_text(data, encoding="utf-8")
    tmp.replace(path)


def _write_meta(directory, meta):
    """Write a session's meta, through privacy.scrub, and return what was written."""

    meta = privacy.scrub(meta)
    write_whole(directory / "meta.json", json.dumps(meta, ensure_ascii=False, indent=1))
    return meta


def _new_meta(session_id, started_at=None):
    kind = KINDS[session_id.split("-", 1)[0]]

    return {
        "schema": SCHEMA,
        "id": session_id,
        "kind": kind,
        "title": "",
        "description": "",
        "task_id": "",
        "desktop_id": os.environ.get("DESKTOP_ID", ""),
        "started_at": started_at or time.time(),
        "ended_at": None,
        # Only a demonstration has a lifecycle; a task's recording is simply
        # whatever its actions were.
        "status": "recording" if kind == "demonstration" else "",
        "screen": None,
        "actors": [],
        "event_count": 0,
        "frame_bytes": 0,
        "withheld_count": 0,
    }


def ensure(session_id, **fields):
    """
    A session's meta, creating the session when it does not exist. The fields
    given fill in what the meta does not say yet; nothing already set changes.
    """

    directory = _dir(session_id)

    with _locked(directory):
        meta = _read_meta(directory)
        created = meta is None

        if created:
            meta = _new_meta(session_id)

        changed = created
        for key, value in fields.items():
            if value not in (None, "") and not meta.get(key):
                meta[key] = value
                changed = True

        if changed:
            meta = _write_meta(directory, meta)

    if created:
        prune(keep=session_id)

    return meta


def update(session_id, **fields):
    """
    Change what a person edits (title, description), a lifecycle sets (status,
    ended_at, reason), or a ticket was given (files: the documents the person
    worked from, as paths on this desktop).
    """

    directory = _dir(session_id)

    if not (directory / "meta.json").exists():
        raise FileNotFoundError(f"no recording {session_id}")

    with _locked(directory):
        meta = _read_meta(directory)
        if meta is None:
            raise FileNotFoundError(f"no recording {session_id}")

        # A rename keeps what the recording was first called as its `task`:
        # the words a person typed for the ticket (`google "koustubh is
        # great"`) are what a skill taught from it is matched against
        # (mimic.plan), and a friendly name must not lose them.
        title = fields.get("title")
        if title is not None and title != meta.get("title") and meta.get("title") and not meta.get("task"):
            meta["task"] = meta["title"]

        for key in ("title", "description", "status", "ended_at", "reason", "files", "task", "proven", "misses"):
            if fields.get(key) is not None:
                meta[key] = fields[key]

        meta = _write_meta(directory, meta)

    return meta


def _store_bytes(frames, name, encode):
    path = frames / name

    if path.exists():
        return 0

    frames.mkdir(parents=True, exist_ok=True)
    data = encode()
    write_whole(path, data)

    return len(data)


def _store_frame(frames, frame):
    """
    Save a frame under a name taken from its content: (name, bytes written -
    0 when it was already there, width, height). A frame is a screen.Frame,
    or a PNG's bytes.
    """

    if isinstance(frame, (bytes, bytearray)):
        data = bytes(frame)
        name = hashlib.sha1(data).hexdigest()[:20] + ".png"
        width = height = None
        if data[:8] == PNG_SIGNATURE and len(data) >= 24:
            width, height = struct.unpack(">II", data[16:24])
        return name, _store_bytes(frames, name, lambda: data), width, height

    # Hashed on the pixels, not the PNG: a frame already stored is then never
    # encoded again, and encoding is the slow half.
    digest = hashlib.sha1(b"%dx%d:" % (frame.width, frame.height))
    digest.update(frame.rgb)
    name = digest.hexdigest()[:20] + ".png"

    return name, _store_bytes(frames, name, frame.png), frame.width, frame.height


def append(session_id, event, before=None, after=None, at=None, defaults=None):
    """
    Add one action to a session with its before/after frames (either may be
    None) and return the event as recorded: with its index "i", its time "t"
    on the session's clock, and where its frames are. `defaults` are meta
    fields for a session this call creates (a task's title).
    """

    directory = _dir(session_id)
    created = False

    with _locked(directory):
        meta = _read_meta(directory)

        if meta is None:
            meta = _new_meta(session_id, started_at=at)
            for key, value in (defaults or {}).items():
                if value:
                    meta[key] = value
            created = True

        now = at or time.time()

        event = dict(event)
        event["i"] = meta["event_count"]
        event["t"] = round(max(0.0, now - meta["started_at"]), 3)
        event["at"] = time.strftime(TIME_FORMAT, time.localtime(now))

        for label, frame in (("before", before), ("after", after)):
            if frame is None:
                continue
            name, size, width, height = _store_frame(directory / "frames", frame)
            event[label] = f"frames/{name}"
            meta["frame_bytes"] += size
            if not meta.get("screen") and width:
                meta["screen"] = [width, height]

        event = privacy.scrub(event)
        with open(directory / "events.jsonl", "a", encoding="utf-8") as f:
            f.write(json.dumps(event, ensure_ascii=False) + "\n")

        meta["event_count"] += 1

        actor = event.get("actor")
        if actor and actor not in meta["actors"]:
            meta["actors"].append(actor)

        if event.get("withheld") or (event.get("element") or {}).get("withheld"):
            meta["withheld_count"] += 1

        _write_meta(directory, meta)

    if created:
        prune(keep=session_id)

    return event


def meta(session_id):
    """A recording's meta alone, or None - for callers that do not need its events."""

    return _read_meta(_dir(session_id))


def sessions():
    """Every recording's meta, newest first."""

    try:
        directories = list(ROOT.iterdir())
    except OSError:
        return []

    found = []
    for directory in directories:
        if not valid(directory.name):
            continue
        meta = _read_meta(directory)
        if meta is not None:
            meta["id"] = directory.name
            found.append(meta)

    found.sort(key=lambda m: m.get("started_at") or 0, reverse=True)
    return found


def load(session_id):
    """(meta, events) of a recording. Raises FileNotFoundError."""

    directory = _dir(session_id)
    meta = _read_meta(directory)

    if meta is None:
        raise FileNotFoundError(f"no recording {session_id}")

    events = []

    try:
        with open(directory / "events.jsonl", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    events.append(json.loads(line))
                except ValueError:
                    # A line cut short by a process killed mid-write.
                    continue
    except FileNotFoundError:
        pass

    return meta, events


def frame_file(session_id, name):
    """The path of one of a recording's frames, or None. Never a path outside it."""

    if not FRAME_RE.match(name or ""):
        return None

    path = _dir(session_id) / "frames" / name
    return path if path.is_file() else None


def delete(session_id):
    directory = _dir(session_id)

    if not directory.is_dir():
        raise FileNotFoundError(f"no recording {session_id}")

    shutil.rmtree(directory)


def export(session_id, stream, extra=None):
    """
    Write a recording to `stream` as a zip - its own layout, `extra` files
    ({name: bytes}) and a README. The stream need not be seekable, so this
    can write straight into an HTTP response.
    """

    directory = _dir(session_id)
    load(session_id)

    with zipfile.ZipFile(stream, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr(f"{session_id}/README.txt", README)

        for name in ("meta.json", "events.jsonl"):
            path = directory / name
            if path.exists():
                archive.write(path, f"{session_id}/{name}")

        for name, data in (extra or {}).items():
            archive.writestr(f"{session_id}/{name}", data)

        frames = directory / "frames"
        if frames.is_dir():
            for path in sorted(frames.glob("*.png")):
                # PNG is already compressed; deflating it again only costs time.
                archive.write(path, f"{session_id}/frames/{path.name}", compress_type=zipfile.ZIP_STORED)


def _size(meta):
    try:
        events = (ROOT / meta["id"] / "events.jsonl").stat().st_size
    except OSError:
        events = 0
    return (meta.get("frame_bytes") or 0) + events


def prune(keep=None):
    """
    Delete the oldest task and hand-run recordings until what is left fits in
    MAX_BYTES. Demonstrations are never pruned, and neither is `keep`.
    """

    found = sessions()
    total = sum(_size(meta) for meta in found)

    if total <= MAX_BYTES:
        return

    for meta in reversed(found):
        # A task's recording the worker kept as its own way of doing that kind
        # of request (playbooks.py) is kept like a demonstration.
        if meta.get("kind") == "demonstration" or meta.get("proven") or meta["id"] == keep:
            continue

        size = _size(meta)

        try:
            shutil.rmtree(ROOT / meta["id"])
        except OSError as exc:
            log(f"Could not prune recording {meta['id']}: {exc}")
            continue

        log(f"Pruned recording {meta['id']} ({size // 1024} KB) to stay within DECURIX_RECORDINGS_MB.")

        total -= size
        if total <= MAX_BYTES:
            return


def _parse_time(value):
    try:
        return datetime.strptime(value, TIME_FORMAT).timestamp()
    except (TypeError, ValueError):
        return None


def import_legacy_audit(audit_dir):
    """
    Move an audit trail from before recordings existed (actions.jsonl and
    frames/ in `audit_dir`) into sessions, once, keeping nothing privacy.py
    would not keep today. Returns how many actions were imported.
    """

    audit_dir = Path(audit_dir)
    index = audit_dir / "actions.jsonl"

    if not index.is_file():
        return 0

    count = 0

    for line in index.read_text(encoding="utf-8", errors="replace").splitlines():
        try:
            entry = json.loads(line)
        except ValueError:
            continue

        at = _parse_time(entry.pop("time", None))
        task_id = entry.get("task_id") or ""
        day = time.strftime("%Y%m%d", time.localtime(at or time.time()))
        session_id = f"task-{task_id}" if task_id else f"adhoc-{day}"

        if not valid(session_id):
            continue

        frames = {}
        for label in ("before", "after"):
            path = entry.pop(f"{label}_frame", None)
            try:
                frames[label] = Path(path).read_bytes() if path else None
            except OSError:
                frames[label] = None

        # The old trail kept typed text verbatim, password fields included.
        _, withheld = privacy.withhold_text(entry.get("element"), entry.get("text"))
        if withheld:
            entry["text"] = None
            entry["withheld"] = {"text": withheld}
            entry.pop("summary", None)

        entry["element"] = privacy.public_element(entry.get("element"))
        entry["url"] = privacy.reduce_url(entry.get("url"))
        entry.setdefault("actor", "agent")

        append(session_id, entry, before=frames["before"], after=frames["after"], at=at)
        count += 1

    index.rename(index.with_name(index.name + ".imported"))
    shutil.rmtree(audit_dir / "frames", ignore_errors=True)

    return count
