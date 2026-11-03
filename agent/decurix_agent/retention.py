"""
What labels a run is kept until the control plane has it (W0.8 of the plan of 2026-11-03; PLAN-P2 D12, C8).

A run is labelled once it has an outcome a person or a check gave it: its final report went out (`final`), or a person
replied on it (`reply`). Its evidence - the notes of what the tools read (computer/evidence.py) and its recording
(computer/recording.py) - used to be pruned by age (notes after 2 days) and by size (the oldest recordings past 1 GB)
whatever the run's state, so the very runs that label a skill's or a model's work could vanish before anyone exported
them (plan rule 4: nothing that labels a run is deleted). Retention is now by label, not age:

    ~/.decurix/runs/<task>/LABELLED   written here, empty: the run has a label
    ~/.decurix/runs/<task>/EXPORTED   written by the control plane after it pulled the run's bundle (R9): the
                                      execution id and the manifest's SHA-256, one line each

A labelled run that is not yet exported is held: neither prune deletes anything of it. Over budget with only held runs
left, nothing is deleted, and that is said (a log line, at most hourly) - the disk fills visibly instead of labels
vanishing silently, and the export clears it. Exported runs are pruned as before.

None of this ever raises into a task: what cannot be marked is logged, and what cannot be read is held.
"""

import hashlib
import json
import os
import time
from pathlib import Path

from decurix_agent import config, task_context
from decurix_agent.logging_utils import log

LABELLED = "LABELLED"
EXPORTED = "EXPORTED"
MANIFEST = "run.json"
MANIFEST_SCHEMA = "decurix.run-manifest/v1"
LABELS = ("final", "reply")
# How often the "kept past the budget" line is said at most, across processes (the `gui` command is a process per run).
WARN_EVERY_SECONDS = 3600


def runs_root():
    return config.DECURIX_DIR / "runs"


def _dir(task_id):
    return task_context.run_dir(task_id)


def read_manifest(task_id):
    """The run's run.json as a dict ({} when it is not there or cannot be read)."""

    folder = _dir(task_id)
    if folder is None:
        return {}
    try:
        data = json.loads((folder / MANIFEST).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _write_manifest(task_id, data):
    """run.json, whole or not at all: a temporary file of this writer's own, flushed to disk, then put in place (two
    processes writing at once never share a temporary file, and a crash never leaves it half written)."""

    import tempfile
    folder = _dir(task_id)
    folder.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=MANIFEST + ".", suffix=".tmp", dir=str(folder))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(json.dumps(data, sort_keys=True, ensure_ascii=False))
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, folder / MANIFEST)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


# ------------------------------------------------------------------ the run's manifest (PLAN-P2 §5.3, R1)

_agent_sha = []


def agent_sha():
    """
    "sha256:<hex>" of the agent's code as this process loaded it: SHA-256 over the sorted (relative path, SHA-256 of
    the file) of the decurix_agent tree (/opt/decurix_agent on a desktop), .pyc and __pycache__ left out. Computed once
    per process (app.main asks at start), so a tree copied in later is not mistaken for what is running.
    """

    if not _agent_sha:
        _agent_sha.append(tree_sha(Path(__file__).resolve().parent))
    return _agent_sha[0]


def tree_sha(root):
    """"sha256:<hex>" over the sorted (relative path, SHA-256 of the file) of a folder, .pyc and __pycache__ aside."""

    root = Path(root)
    digest = hashlib.sha256()
    for path in sorted(p for p in root.rglob("*") if p.is_file() and p.suffix != ".pyc"
                       and "__pycache__" not in p.parts):
        try:
            data = path.read_bytes()
        except OSError:
            continue
        digest.update(f"{path.relative_to(root).as_posix()}\0{hashlib.sha256(data).hexdigest()}\n".encode("utf-8"))
    return "sha256:" + digest.hexdigest()


def soul_sha():
    """"sha256:<hex>" of the soul binary, or "" when there is none."""

    try:
        stat = os.stat(config.THEBOT_SOUL_PATH)
        key = (config.THEBOT_SOUL_PATH, stat.st_mtime_ns, stat.st_size)
        if _soul_sha.get("key") == key:
            return _soul_sha["sha"]
        digest = hashlib.sha256()
        with open(config.THEBOT_SOUL_PATH, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                digest.update(block)
        _soul_sha.update(key=key, sha="sha256:" + digest.hexdigest())
        return _soul_sha["sha"]
    except (OSError, AttributeError):
        return ""


# soul's hash, kept while the binary's path, time and size stay the same (it is read at every final report).
_soul_sha = {}


def _parts(task_id):
    """The run's parts that exist, as paths under ~/.decurix (C8)."""

    root = config.DECURIX_DIR
    found = {}
    for name, rel in (("recording", f"recordings/task-{task_id}"), ("notes", f"computer/evidence/{task_id}.jsonl"),
                      ("ledger", f"computer/evidence/{task_id}.records.json"),
                      ("soul_trace", f"ai/traces/{task_id}.jsonl")):
        if (root / rel).exists():
            found[name] = rel
    return found


def write_manifest(task_id):
    """
    Write ~/.decurix/runs/<task>/run.json (decurix.run-manifest/v1): the task, the desktop, the code that ran (agent
    and soul by hash), the harness and brain, when, the parts of the run that exist, and why it is labelled. Never
    raises: a manifest that cannot be written is logged, and the control plane says the part is missing.
    """

    if not task_context.valid(task_id):
        return None
    try:
        data = read_manifest(task_id)
        started = task_context.since() if task_id == task_context.current() else None
        data.update({
            "schema": MANIFEST_SCHEMA, "task_id": task_id, "desktop_id": config.DESKTOP_ID,
            "agent_sha": agent_sha(), "soul_sha": soul_sha(), "harness": config.AGENT_HARNESS,
            "brain": getattr(config, "BRAIN", ""), "parts": _parts(task_id), "ended_at": int(time.time()),
            "labelled_by": list(data.get("labelled_by") or []),
        })
        if started and "started_at" not in data:
            data["started_at"] = int(started)
        _write_manifest(task_id, data)
        return data
    except Exception as exc:  # noqa: BLE001 - decoration for the task
        log(f"retention: could not write the run's manifest for {task_id}: {exc}")
        return None


def mark_labelled(task_id, why):
    """The run of `task_id` has a label (`why`: final or reply): its evidence is held until exported. Never raises."""

    folder = _dir(task_id)
    if folder is None:
        return False
    try:
        folder.mkdir(parents=True, exist_ok=True)
        # The execution labelled (the control plane's id for this attempt, when it sent one): an earlier attempt's
        # export does not cover it (review round 2, B8). Written anew every time, so its time is the latest label's.
        (folder / LABELLED).write_text(task_context.execution_id(task_id), encoding="utf-8")
        data = read_manifest(task_id) or {"schema": MANIFEST_SCHEMA, "task_id": task_id}
        if why in LABELS and why not in (data.get("labelled_by") or []):
            data["labelled_by"] = list(data.get("labelled_by") or []) + [why]
            _write_manifest(task_id, data)
        return True
    except Exception as exc:  # noqa: BLE001 - decoration for the task; said, never raised
        log(f"retention: could not mark the run of {task_id} labelled: {exc}")
        return False


def labelled(task_id):
    folder = _dir(task_id)
    return folder is not None and (folder / LABELLED).exists()


def exported(task_id):
    """Whether the control plane has pulled the run's bundle (it wrote EXPORTED, C8)."""

    folder = _dir(task_id)
    if folder is None:
        return False
    try:
        return bool((folder / EXPORTED).read_text(encoding="utf-8").strip())
    except OSError:
        return False


def held(task_id):
    """
    Whether the run's evidence must be kept: labelled, and its latest label not yet covered by an export. An export
    covers a label when it names the same execution (when both name one) and was written after it: a later attempt
    under the same task (a retry, a reply after the export) is held again until its own export (review round 2, B8).
    A run that cannot be read is held.
    """

    if not task_context.valid(task_id):
        return False
    try:
        if not labelled(task_id):
            return False
        if not exported(task_id):
            return True
        folder = _dir(task_id)
        mark = (folder / LABELLED).read_text(encoding="utf-8").strip()
        done = (folder / EXPORTED).read_text(encoding="utf-8").split("\n", 1)[0].strip()
        if mark and done and mark != done:
            return True
        return (folder / LABELLED).stat().st_mtime_ns > (folder / EXPORTED).stat().st_mtime_ns
    except Exception:  # noqa: BLE001 - when in doubt, keep
        return True


# Past this many bytes of recordings, with labelled runs still waiting for their export, the desktop takes no new work
# and says why (D12: it fills visibly, and never deletes a label to make room). DECURIX_HELD_CEILING_MB, default three
# times the recordings' budget.
def ceiling_bytes():
    from decurix_agent.computer import recording
    try:
        return int(os.environ.get("DECURIX_HELD_CEILING_MB", "")) * 1024 * 1024
    except ValueError:
        return 3 * recording.MAX_BYTES


def over_ceiling():
    """A sentence saying the desktop is full of runs not yet exported, when it is; "" otherwise. Never raises."""

    try:
        from decurix_agent.computer import recording
        found = recording.sessions()
        total = sum(recording._size(meta) for meta in found)
        if total <= ceiling_bytes():
            return ""
        waiting = sum(1 for meta in found if held(str(meta.get("task_id") or "")
                                                  or (meta["id"][5:] if meta["id"].startswith("task-") else "")))
        if not waiting:
            return ""
        return (f"This desktop is full: {total // (1024 * 1024)} MB of recordings, {waiting} of them runs the control "
                "plane has not taken yet, so nothing is deleted and no new work is started. Check that the control "
                "plane is pulling this desktop's runs (its run records), then send the request again.")
    except Exception as exc:  # noqa: BLE001
        log(f"retention: could not measure the disk ceiling: {exc}")
        return ""


def warn_kept(count, what):
    """Say, at most every WARN_EVERY_SECONDS, that `count` of `what` are kept past the disk budget for export."""

    marker = runs_root() / ".kept-warned"
    try:
        if time.time() - marker.stat().st_mtime < WARN_EVERY_SECONDS:
            return
    except OSError:
        pass
    line = (f"{count} {what} are kept past the disk budget because their runs are not yet exported to the control "
            "plane")
    log(line)
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.touch()
    except OSError:
        pass
    try:
        from decurix_agent import progress
        progress.emit(line + ".")
    except Exception:  # noqa: BLE001 - progress is decoration
        pass
