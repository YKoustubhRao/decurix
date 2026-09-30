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
# ============================================================

_current = None


def begin(skill, task, text, values=None, mode="model", **extra):
    """Hand `skill` to the engine for `task`; `text` is what its model is told."""

    global _current
    _current = dict({"skill": skill, "task": task, "text": text, "values": dict(values or {}), "mode": mode,
                     "attempt": 1}, **extra)
    return _current


def get():
    return _current


def text():
    return (_current or {}).get("text") or ""


def end():
    global _current
    _current = None
