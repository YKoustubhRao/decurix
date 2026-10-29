"""
Practice sources for Simulate (docs/simulation.md).

The control plane drafts a practice suite for a taught skill or recording and
runs this same agent in a disposable practice desktop. This module exports
what it pins, for http_api.py:

  GET /practice-sources                  every skill and recording, with the facts
                                         the control plane classifies them by (no bytes)
  GET /skills/<name>/practice-source     the skill JSON exactly as saved, its SKILL.md
                                         and workflow, and those facts
  GET /recordings/<id>/practice-source   meta.json and events.jsonl exactly as saved,
                                         the workflow, the start, end and window-change
                                         frames, and those facts

Files are the saved bytes, base64 (what Go's []byte reads), keyed by their path
under the practice desktop's $HOME. privacy.py withheld secrets when they were
saved; nothing here adds anything back.

DECURIX_PRACTICE=1 marks a practice desktop. It is plumbing only: the worker is
not told it is practising, so no prompt text depends on it, and it is not a
harness branch (harness.py stays the only file that branches on
AGENT_HARNESS). A practice desktop needs no other change here: sends were never
the agent's (an email is only a draft, and in practice the broker keeps it as
practice evidence), and the control plane copies in no launchers or host apps.
"""

import base64
import json
import os
from urllib.parse import urlsplit

from decurix_agent.computer import launchers, lint, recording, replay, skillmd, skills, workflow

# What one source may weigh (the control plane's MaxPracticeSourceBytes).
MAX_SOURCE_BYTES = 4 * 1024 * 1024
# The most screen frames a recording's source carries.
MAX_FRAMES = 6


def active():
    """Whether this agent runs in a practice desktop."""

    return os.environ.get("DECURIX_PRACTICE", "").strip() == "1"


def _b64(data):
    return base64.b64encode(data).decode("ascii")


def _host_app(window):
    """Whether a window is a program that runs on the person's own computer (a "-host" launcher)."""

    try:
        entry = launchers.for_window(window) if window else None
        if entry is None:
            return False
        argv = launchers.argv(entry)
        return bool(argv) and os.path.basename(argv[0]).endswith("-host")
    except Exception:  # noqa: BLE001 - a launcher that cannot be read is no host app
        return False


def facts(steps, program=None):
    """
    What the control plane classifies a source by: the web hosts and schemes its
    steps visit, whether it has a consequential or by-hand step, whether it uses
    a host app, its lint grade and its length. No values, no bytes.
    """

    hosts, schemes, windows = [], [], []
    consequential = manual = False
    for step in steps or []:
        url = step.get("url") or ""
        if url:
            parts = urlsplit(url)
            if parts.scheme in ("http", "https") and parts.hostname:
                if parts.hostname not in hosts:
                    hosts.append(parts.hostname)
                if parts.scheme not in schemes:
                    schemes.append(parts.scheme)
        consequential = consequential or bool(step.get("consequential"))
        manual = manual or step.get("kind") == "manual"
        window = step.get("window") or ""
        if window and window not in windows:
            windows.append(window)
    grade = ""
    if program is not None:
        try:
            grade = lint.lint(program).get("grade") or ""
        except Exception:  # noqa: BLE001 - the grade is advice
            grade = ""
    return {"hosts": hosts, "schemes": schemes, "consequential": consequential, "manual": manual,
            "host_app": any(_host_app(w) for w in windows), "grade": grade, "steps": len(steps or [])}


def _skill_program(skill):
    try:
        return skills.program(skill)
    except Exception:  # noqa: BLE001 - a looped skill has no single program; its steps still say what it does
        return None


def skill_source(name):
    """A skill's practice source. Raises FileNotFoundError."""

    if not skills.valid(name):
        raise FileNotFoundError(f"no skill {name}")
    path = skills.ROOT / f"{name}.json"
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise FileNotFoundError(f"no skill {name}") from exc
    skill = json.loads(raw.decode("utf-8"))
    program = _skill_program(skill)
    files = {f".decurix/skills/{name}.json": raw}
    try:
        files[f".decurix/practice-source/{name}/SKILL.md"] = skillmd.render(skill).encode("utf-8")
    except Exception:  # noqa: BLE001 - the saved skill is the source; its rendering is a reading aid
        pass
    if program is not None:
        try:
            files[f".decurix/practice-source/{name}/workflow.txt"] = workflow.render(program).encode("utf-8")
        except Exception:  # noqa: BLE001 - a reading aid, like SKILL.md
            pass
    brief = "\n".join(t for t in (skill.get("goal") or skill.get("title") or "", skill.get("description") or "") if t)
    return {"kind": "skill", "name": name, "title": skill.get("title") or name, "brief": brief,
            "facts": facts(skill.get("steps"), program),
            "files": {k: _b64(v) for k, v in _bounded(files).items()}}


def _chosen_frames(events):
    """The start, end and window-change frames, as recording-relative paths, at most MAX_FRAMES."""

    chosen, last_window = [], None
    for index, event in enumerate(events):
        window = event.get("window")
        moved = bool(window) and window != last_window
        last_window = window or last_window
        if (index == 0 or moved) and event.get("before"):
            chosen.append(event["before"])
    if events:
        end = events[-1].get("after") or events[-1].get("before")
        if end:
            chosen.append(end)
    out = []
    for frame in chosen:
        if frame not in out:
            out.append(frame)
    if len(out) > MAX_FRAMES:
        out = out[:MAX_FRAMES - 1] + [out[-1]]
    return out


def _bounded(files):
    """The files within MAX_SOURCE_BYTES: frames are dropped first, then the rest is refused."""

    total = sum(len(v) for v in files.values())
    for key in sorted((k for k in files if "/frames/" in k), reverse=True):
        if total <= MAX_SOURCE_BYTES:
            break
        total -= len(files.pop(key))
    if total > MAX_SOURCE_BYTES:
        raise ValueError("this source is too large to practise from")
    return files


def recording_source(session_id):
    """A recording's practice source. Raises FileNotFoundError."""

    if not recording.valid(session_id):
        raise FileNotFoundError(f"no recording {session_id}")
    folder = recording.ROOT / session_id
    try:
        meta_raw = (folder / "meta.json").read_bytes()
    except OSError as exc:
        raise FileNotFoundError(f"no recording {session_id}") from exc
    try:
        events_raw = (folder / "events.jsonl").read_bytes()
    except OSError:
        events_raw = b""
    meta, events = recording.load(session_id)
    program = replay.compile_program(meta, events)
    base = f".decurix/recordings/{session_id}"
    files = {f"{base}/meta.json": meta_raw, f"{base}/events.jsonl": events_raw,
             f".decurix/practice-source/{session_id}/workflow.txt": workflow.render(program).encode("utf-8")}
    for frame in _chosen_frames(events):
        path = recording.frame_file(session_id, frame.rsplit("/", 1)[-1])
        if path is not None:
            files[f"{base}/frames/{path.name}"] = path.read_bytes()
    brief = "\n".join(t for t in (meta.get("title") or "", meta.get("description") or "") if t)
    return {"kind": "recording", "name": session_id, "title": meta.get("title") or session_id, "brief": brief,
            "facts": facts(program.get("steps"), program),
            "files": {k: _b64(v) for k, v in _bounded(files).items()}}


def sources():
    """Every skill and demonstration recording with its facts, for the page's pick list."""

    out = []
    for skill in skills.all_skills():
        name = skill.get("name") or ""
        if skills.valid(name):
            out.append({"kind": "skill", "name": name, "title": skill.get("title") or name,
                        "facts": facts(skill.get("steps"), _skill_program(skill))})
    for meta in recording.sessions():
        session_id = meta.get("id") or ""
        if not session_id.startswith("demo-"):
            continue
        try:
            m, events = recording.load(session_id)
            program = replay.compile_program(m, events)
        except Exception:  # noqa: BLE001 - one unreadable recording does not hide the rest
            continue
        out.append({"kind": "recording", "name": session_id, "title": meta.get("title") or session_id,
                    "facts": facts(program.get("steps"), program)})
    return {"sources": out}
