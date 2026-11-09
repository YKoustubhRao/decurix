"""
phase.py - which part of a taught task the model is doing now, when that part only collects (skill_task's gather
phase): the task, and the controls the part's taught steps commit with. Written by skill_task in the agent, read by the
gui command (core._phase_refusal) in another process, as the record ledger is. Found live (2026-11-09): told to collect
a month's bills, the model entered all five vouchers by hand in that part.
"""

import json

from decurix_agent import config

PHASE_FILE = config.DECURIX_DIR / "computer" / "phase.json"


def write(task_id, phase, commits=()):
    """Note the part of a taught task being done now; None clears it. Never raises."""

    from decurix_agent.computer import privacy
    try:
        if not phase:
            PHASE_FILE.unlink(missing_ok=True)
            return
        PHASE_FILE.parent.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(PHASE_FILE, {"task": task_id, "phase": phase, "commits": [str(c) for c in commits]},
                               atomic=True, shapes=False)
    except OSError as exc:
        from decurix_agent.logging_utils import log
        log(f"Could not note the task's phase: {exc}")


def read():
    """The noted part: {"task", "phase", "commits"}, or {} when none (or it cannot be read)."""

    try:
        noted = json.loads(PHASE_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return noted if isinstance(noted, dict) else {}
