import json
import re

from decurix_agent import config, task_context


# ============================================================
# The refs an engine was shown
#
# A ref is a name for an element the engine has seen in a look or an
# action's result. Found live (2026-10-16): after Excel's Insert tab was
# pressed, the result listed six of the 35 controls that appeared,
# "PivotTable" (e15895) ... "Shapes" (e15900) "(+29 more)", and the engine
# pressed e15909 - a number it made up from that run - for the column chart
# it wanted. It was "Insert Pie or Doughnut Chart". A ref nothing printed is a
# guess, and a guess on a screen is a click on something else; the gui command
# refuses one, for the task that is running.
# ============================================================

REF = re.compile(r"\be\d+\b")
MAX_REFS = 20000
# How a ref was named where it was printed: a look's line (e12 button "Save") or an action's result (button "Save" (e12)).
LISTED = re.compile(r'^(e\d+) ([a-z]+) "((?:[^"\\]|\\.)*)"', re.M)
DESCRIBED = re.compile(r'\b([a-z]+) "((?:[^"\\]|\\.)*)" \((e\d+)\)')
MAX_NAMES = 4000


def _path():
    return config.DECURIX_DIR / "computer" / "shown-refs.json"


def _read():
    try:
        with open(_path(), encoding="utf-8") as f:
            record = json.load(f)
        return record if isinstance(record, dict) else {}
    except (OSError, ValueError):
        return {}


def note(text):
    """Remember every ref in what the gui command printed, for the task being worked on."""

    task = task_context.current()
    refs = REF.findall(text or "")
    if not task or not refs:
        return
    record = _read()
    known = record.get("refs", []) if record.get("task") == task else []
    merged = list(dict.fromkeys(known + refs))[-MAX_REFS:]
    names = record.get("names", {}) if record.get("task") == task else {}
    for ref, role, name in LISTED.findall(text):
        names[ref] = [role, name]
    for role, name, ref in DESCRIBED.findall(text):
        names[ref] = [role, name]
    if len(names) > MAX_NAMES:
        names = dict(list(names.items())[-MAX_NAMES:])
    try:
        _path().parent.mkdir(parents=True, exist_ok=True)
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(_path(), {"task": task, "refs": merged, "names": names}, atomic=True)
    except OSError:
        pass


def unseen(ref):
    """
    Whether a ref was never shown during the task being worked on. Only a
    task that has been shown refs can say: with nothing on record (no task,
    a task that has not looked yet) nothing is refused.
    """

    task = task_context.current()
    if not task:
        return False
    record = _read()
    if record.get("task") != task or not record.get("refs"):
        return False
    return ref not in set(record["refs"])


def was(ref):
    """The role and name a ref had where it was shown in this task, or None."""

    record = _read()
    if record.get("task") != task_context.current():
        return None
    named = (record.get("names") or {}).get(ref)
    return tuple(named) if isinstance(named, list) and len(named) == 2 else None


def named(ref):
    """[role, name] a ref was printed with in this task, else None (shown-refs.json "names")."""
    record = _read()
    names = record.get("names", {}) if isinstance(record, dict) else {}
    got = names.get(ref)
    return list(got) if isinstance(got, list) and len(got) == 2 else None
