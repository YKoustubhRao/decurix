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

import json
import os
import time

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
    folder = _dir(task_id)
    folder.mkdir(parents=True, exist_ok=True)
    tmp = folder / (MANIFEST + ".tmp")
    tmp.write_text(json.dumps(data, sort_keys=True, ensure_ascii=False), encoding="utf-8")
    os.replace(tmp, folder / MANIFEST)


def mark_labelled(task_id, why):
    """The run of `task_id` has a label (`why`: final or reply): its evidence is held until exported. Never raises."""

    folder = _dir(task_id)
    if folder is None:
        return False
    try:
        folder.mkdir(parents=True, exist_ok=True)
        (folder / LABELLED).touch()
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
    """Whether the run's evidence must be kept: labelled and not yet exported. A run that cannot be read is held."""

    if not task_context.valid(task_id):
        return False
    try:
        return labelled(task_id) and not exported(task_id)
    except Exception:  # noqa: BLE001 - when in doubt, keep
        return True


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
