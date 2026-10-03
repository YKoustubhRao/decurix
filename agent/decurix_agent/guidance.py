# ============================================================
# A taught task handed to the engine
#
# While a skill is still being learned (experience.stage), or when its replay
# got stuck, the engine does the task with the skill as its guide
# (skill_task._by_model). What it is told - the procedure, this run's
# values, how to look at the demonstration - is kept here for the life of the
# task, and every harness puts it into what its model reads (thebot_harness's
# standing context, azure_harness's system message). The same record is what
# messaging's final gate checks the engine's "done" against before anyone is
# told it succeeded (skill_task._gate).
#
# Module state, like record_task's ticket: one task at a time runs, and a new
# task (watchers) clears it.
#
# One part of it is also kept in a file: this run's values. The engine works
# the screen through `gui`, a process of its own, and when it replays the
# recording or the skill it was handed (`gui replay ID`, `gui do NAME`) the
# replay must type this run's values - not the ones the person typed when
# they recorded it. Found live: asked for an order for "Globex" in Germany,
# matched to a recording of one for "Initech" in Japan, the engine ran
# `gui replay` on the recording as it was and saved a second order for
# Initech before it did the one it was asked for.
# ============================================================

import json

from decurix_agent import config, task_context

_current = None

RUN_FILE = config.DECURIX_DIR / "computer" / "guided-run.json"


def begin(skill, task, text, values=None, mode="model", **extra):
    """Hand `skill` to the engine for `task`; `text` is what its model is told."""

    global _current
    _current = dict({"skill": skill, "task": task, "text": text, "values": dict(values or {}), "mode": mode,
                     "attempt": 1}, **extra)

    run = extra.get("run") or {}
    _keep({
        "task_id": task_context.current(),
        "skill": skill,
        # A task done from a recording nobody has taught yet (skill_task's one-off).
        "recording": (run.get("mimic") or {}).get("id") if run.get("one_off") else None,
        "values": dict(values or {}),
    })

    return _current


def get():
    return _current


def text():
    return (_current or {}).get("text") or ""


def end():
    global _current
    _current = None

    try:
        RUN_FILE.unlink()
    except OSError:
        pass


def _keep(record):
    try:
        RUN_FILE.parent.mkdir(parents=True, exist_ok=True)
        RUN_FILE.write_text(json.dumps(record), encoding="utf-8")
    except (OSError, TypeError, ValueError):
        pass


def run_values(skill=None, recording=None):
    """
    This run's values for the skill or recording an engine is about to replay:
    what it was handed for the task it is on now, or {} when it was handed
    nothing, something else, or for another task. Read from any process.
    """

    try:
        kept = json.loads(RUN_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}

    if not isinstance(kept, dict) or not kept.get("task_id") or kept.get("task_id") != task_context.current():
        return {}

    if (skill and kept.get("skill") == skill) or (recording and kept.get("recording") == recording):
        values = kept.get("values")
        return dict(values) if isinstance(values, dict) else {}

    return {}


def with_run_values(given, params, skill=None, recording=None):
    """
    `given` (what an engine passed a replay) with this run's own value for
    every parameter of `params` it did not give. A value the engine gave is
    kept: it may be working one item of a list.
    """

    mine = run_values(skill=skill, recording=recording)
    names = {p.get("name") for p in params or []}
    out = dict(given or {})

    for name, value in mine.items():
        if name in names and name not in out and value not in (None, ""):
            out[name] = value

    return out
