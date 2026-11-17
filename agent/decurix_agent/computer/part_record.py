"""
The record of each part of a taught task (skills.parts): every run of a whole part and how the task it ran in ended.

A taught task is reused part by part (owner, 2026-11-16: "entire workflow may not be repeatable, but subparts of it are
repeatable ... repeated sub parts logic can be codified"). Trust in the whole skill needs whole runs that repeat exactly,
which semi-structured work never gives - one case's other quarter, one receiver to validate, a one-time code, and the
whole run counts for nothing. A part is judged on its own: it ran from its first step to its last with nothing stopping
it, and the task it ran in then ended checked done (the final check, unchanged - messaging.write_final_reply settles it).
A part with such a run behind it, and no later run that stopped or ended wrong, is proven: once the part before it
ends, it runs on by code with no model turn (skill_engine.run, chained), and the model gets the screen back only where
a part is new, varies, or stops. Correctness does not rest on this: it only decides who drives a part; whether the work
is done is still the final check's.

  ~/.decurix/skill-parts.json
    {skill: {part key: {"first", "last", "uses": [{"task", "at", "clean", "ended"}]}}}
    ended: None while the task runs, then True (checked done) or False (anything else).
"""

import hashlib
import json
import os
import threading
import time

from decurix_agent import config, filelock
from decurix_agent.logging_utils import log

PATH = config.DECURIX_DIR / "skill-parts.json"
# Runs a part needs behind it - each clean, each in a task that ended checked done, the latest ones first - before it
# runs on by code. One: a part is the person's own steps, replayed with every check a replay makes, and the task it ran
# in was then checked done; a part that later stops or ends wrong is the model's again at once.
try:
    PROVEN_AFTER = max(1, int(os.environ.get("DECURIX_PART_PROVEN_AFTER", "1")))
except ValueError:
    PROVEN_AFTER = 1
MAX_USES = 12
_lock = threading.Lock()


def key(skill, part):
    """A part's key: its span and the identity of its steps (a skill learned again is other parts)."""

    ids = [str(s.get("id") or s.get("n")) for s in skill.get("steps") or []
           if part["first"] <= (s.get("n") or 0) <= part["last"]]
    return f"{part['first']}-{part['last']}:" + hashlib.sha1("|".join(ids).encode("utf-8")).hexdigest()[:10]


def _load():
    try:
        data = json.loads(PATH.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _save(data):
    from decurix_agent.computer import privacy
    PATH.parent.mkdir(parents=True, exist_ok=True)
    privacy.write_scrubbed(PATH, data)


def _locked(change):
    """Run change(data) under the file's lock and save what it returns True for. Never raises."""

    try:
        with _lock:
            PATH.parent.mkdir(parents=True, exist_ok=True)
            with open(str(PATH) + ".lock", "a+") as guard:
                filelock.exclusive(guard)
                data = _load()
                if change(data):
                    _save(data)
    except (OSError, ValueError, TypeError) as exc:
        log(f"part_record: not kept ({exc})")


def note_use(skill, part, task_id, clean):
    """A run of a whole part (from its first step) ended: `clean` when it ran to its last step with nothing stopping it."""

    if not task_id:
        return

    def change(data):
        entry = data.setdefault(skill["name"], {}).setdefault(key(skill, part), {"first": part["first"],
                                                                                 "last": part["last"], "uses": []})
        entry["uses"] = (entry["uses"] + [{"task": task_id, "at": int(time.time()), "clean": bool(clean),
                                           "ended": None}])[-MAX_USES:]
        return True

    _locked(change)


def settle(task_id, done):
    """The task ended: `done` when it ended checked done. Every part run in it is settled so. Never raises."""

    if not task_id:
        return

    def change(data):
        changed = False
        for parts in data.values():
            for entry in parts.values():
                for use in entry.get("uses") or []:
                    if use.get("task") == task_id and use.get("ended") is None:
                        use["ended"] = bool(done)
                        changed = True
        return changed

    _locked(change)


def proven(skill, part):
    """How many runs stand behind the part: its latest settled runs, newest first, while each is clean and ended
    checked done (a run that stopped, or a task that ended otherwise, ends the count). Runs still going count for
    nothing either way."""

    entry = _load().get(skill.get("name") or "", {}).get(key(skill, part)) or {}
    count = 0
    for use in reversed(entry.get("uses") or []):
        if use.get("ended") is None:
            if not use.get("clean"):
                break  # (a run that stopped, in the task going on now: the model's again at once)
            continue
        if not (use.get("clean") and use.get("ended")):
            break
        count += 1
    return count


def is_proven(skill, part):
    return proven(skill, part) >= PROVEN_AFTER
