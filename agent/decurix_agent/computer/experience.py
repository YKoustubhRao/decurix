import time


# ============================================================
# Experience: what a skill learns from being run
#
# Teaching (learning.py) gives a skill what a person's demonstrations and
# answers could say. Running it teaches it more, and this is where that goes:
# the second half of Orby's "observe, learn, automate" loop, where the system
# improves from its own runs instead of waiting to be re-taught.
#
# The rule for keeping any of it is the one skill_task already had for a
# person's correction, made general: **a lesson is kept only if the run that
# taught it then succeeded** (replay.SUCCESS). A repair that did not lead
# anywhere is not remembered.
#
# What a successful run leaves behind, per step (keyed by the step's stable
# id, since its number changes when a step is dropped):
#
#   locate       where the control was and which controls stood around it, kept
#                fresh, so healing.rank can tell a moved control from another one
#   alternates   names the control is known by now. A repair the *screen*
#                confirmed (the step's expected result appeared) is kept at
#                once; one nothing confirmed must repeat PROMOTE_AFTER times
#                first. (A control a person names is a correction, kept by
#                skill_task._retarget, which files the old name here too.)
#   needed       the last HISTORY runs: did this step need a repair or a person?
#
# and per skill a short history of run outcomes. From `needed`, a step that
# has needed help in most recent runs is reported as fragile - the skill
# telling its owner that it is worth teaching again, before it stops working.
#
# Nothing here weakens a guard: an alias only feeds healing.rank, so a step
# found through one is still held to the same score, lead and strictness, and
# still goes through core.act's approval gate.
# ============================================================

HISTORY = 5
RUN_HISTORY = 10
ALIAS_LIMIT = 5
PROMOTE_AFTER = 2
FRAGILE_AT = 3


def _record(skill, key):
    experience = skill.setdefault("experience", {"runs": [], "steps": {}})
    return experience["steps"].setdefault(str(key), {"runs": 0, "needed": [], "pending": {}})


def _step(skill, key):
    return next((s for s in skill.get("steps") or [] if str(s.get("id") or s.get("n")) == str(key)), None)


def _promote(step, role, name):
    """Add a name a step's control is known by. True when it was new."""

    known = {a.get("name") for a in step.get("alternates") or []} | {(step.get("target") or {}).get("name")}

    if not name or name in known:
        return False

    alternates = (step.get("alternates") or []) + [{"role": role, "name": name}]
    step["alternates"] = alternates[-ALIAS_LIMIT:]
    return True


def commit(skill, ladder, outcome, success, asked=None):
    """
    Fold a finished run into a skill: what to keep (only when `success`), the
    step history either way. `asked` is the ids of steps a person had to be
    asked about. Returns the lines added to the skill's "learned" list (the
    caller saves).
    """

    experience = skill.setdefault("experience", {"runs": [], "steps": {}})
    learned = []

    repaired = {h["id"]: h for h in ladder.healed}
    asked = {str(k) for k in asked or []}

    experience["runs"] = (experience["runs"] + [{
        "at": int(time.time()), "outcome": outcome, "healed": len(ladder.healed), "judged": len(ladder.judged),
    }])[-RUN_HISTORY:]

    for key, seen in ladder.seen.items():
        record = _record(skill, key)
        step = _step(skill, key)
        record["runs"] += 1
        record["needed"] = (record["needed"] + [1 if key in repaired or str(key) in asked else 0])[-HISTORY:]

        if not success or step is None:
            continue

        heal = repaired.get(key)

        # Where a step's control is now, once it has done its job: a control
        # found as recorded, or a repaired one the screen then confirmed.
        if heal is None or key in ladder.confirmed:
            if seen.get("box"):
                step["locate"] = {"box": seen["box"], "screen": seen.get("screen"), "anchors": seen.get("anchors") or []}

        if heal is None:
            record["pending"] = {}
            continue

        name = heal["now"]
        if key in ladder.confirmed:
            confirmed = True
        else:
            record["pending"][name] = record["pending"].get(name, 0) + 1
            confirmed = record["pending"][name] >= PROMOTE_AFTER

        if confirmed and _promote(step, heal["role"], name):
            record["pending"].pop(name, None)
            learned.append(f"step {step['n']}: \"{heal['was']}\" is also found as \"{name}\" ({heal['how']}, "
                           + ("confirmed on screen" if key in ladder.confirmed else f"seen in {PROMOTE_AFTER} runs") + ")")

    for key in asked - set(ladder.seen):
        record = _record(skill, key)
        record["needed"] = (record["needed"] + [1])[-HISTORY:]

    if learned:
        skill.setdefault("learned", []).extend(learned)
        skill["version"] = skill.get("version", 1) + 1

    return learned


def health(skill):
    """
    How well a skill holds up: {"fragile": [{"n", "summary", "needed", "of"}],
    "drifting": [...same], "recent": [outcomes]}. Fragile is a step that needed
    a repair or a person in FRAGILE_AT of its last HISTORY runs.
    """

    experience = skill.get("experience") or {}
    fragile, drifting = [], []

    for key, record in (experience.get("steps") or {}).items():
        step = _step(skill, key)
        needed = sum(record.get("needed") or [])
        of = len(record.get("needed") or [])

        if step is None or not needed:
            continue

        entry = {"n": step["n"], "summary": step.get("summary", ""), "needed": needed, "of": of}
        (fragile if needed >= FRAGILE_AT else drifting).append(entry)

    return {"fragile": sorted(fragile, key=lambda e: e["n"]), "drifting": sorted(drifting, key=lambda e: e["n"]),
            "recent": [r["outcome"] for r in experience.get("runs") or []]}


def advice(skill):
    """A sentence for the end of a run when its skill has started to wear, else ''."""

    fragile = health(skill)["fragile"]

    if not fragile:
        return ""

    steps = ", ".join(f"step {e['n']} ({e['needed']} of its last {e['of']} runs)" for e in fragile[:3])
    return (f" {steps} keep needing a repair. It still works, but the screen has moved on from the recording - "
            "record it again and `/learn` it to make it steady.")
