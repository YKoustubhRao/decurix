import os
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

# How many runs in a row must succeed - checked, not assumed, and with no
# person stepping in - before a skill is run as a replay with the model only
# on call. Until then the model does the task with the skill as its guide
# (skill_task._by_model): a worker still learning its range of tasks leans on
# the brain, and leans on it less once the task has shown it runs smoothly.
# One failure, or a run a person had to rescue, and it is back to learning.
# 0 trusts every skill from the start: replayed, the model only on call (what
# a bench of the replay itself wants).
try:
    TRUST_AFTER = max(0, int(os.environ.get("DECURIX_SKILL_TRUST_RUNS", "3")))
except ValueError:
    TRUST_AFTER = 3

APPRENTICE, TRUSTED = "apprentice", "trusted"

# The kinds of check that are independent of the worker's own say-so (W0.5 of the plan of 2026-11-03): the run's files
# read back by code ("code"), every record it declared read back from the application matching ("ledger"), or a
# person ("person"). Only a run checked by one of these counts towards trust. The screen ("screen": every step's
# expected result appeared) and the model looking at it ("model") are kept on the verdict, for the record, and never
# advance the streak: a replay can show each step's result and still have entered the wrong thing, and the model that
# judges is the same kind of thing that worked. A verdict written before `basis` existed has none, so every skill
# earns its trust again under this rule (PLAN-P2 D11): its old streak was counted by the rule this replaces.
INDEPENDENT = frozenset({"code", "ledger", "person"})
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


def note_verdict(skill, mode, ok, checked_by=None, helped=False, why="", tokens=None, basis=()):
    """
    Record how one run of a skill ended, for stage(): `mode` "model" (the
    engine did it, guided by the skill) or "replay"; `ok` only for a success
    that was checked - by the screen (`checked_by` "screen"), the model looking
    at it ("model") or a person ("person"); `helped` when a person had to step
    in. `basis` is every kind of check that passed (code, ledger, person,
    screen, model): only an INDEPENDENT one counts towards trust. `tokens` is
    what the run's model calls read and wrote in all (None when nobody
    counted): what economy() compares. The caller saves.
    """

    experience = skill.setdefault("experience", {"runs": [], "steps": {}})
    verdict = {
        "at": int(time.time()), "mode": mode, "ok": bool(ok), "checked_by": checked_by,
        "helped": bool(helped), "why": (why or "")[:300], "basis": sorted({str(b) for b in basis or () if b}),
    }
    if tokens is not None:
        verdict["tokens"] = int(tokens)
        # The first good run's cost outlives the short history: it is what
        # every later run is measured against.
        if ok and "first_tokens" not in experience:
            experience["first_tokens"] = int(tokens)
    experience["verdicts"] = (experience.get("verdicts", []) + [verdict])[-RUN_HISTORY:]


def economy(skill):
    """
    What a skill costs to run now against its first good run: {"first",
    "now" (the mean of its last three good runs), "saved_percent"} in tokens,
    each None until it is known. A skill that is working as intended costs
    less with use: learned with the model, then replayed without it.
    """

    experience = skill.get("experience") or {}
    first = experience.get("first_tokens")
    good = [v["tokens"] for v in experience.get("verdicts") or [] if v.get("ok") and "tokens" in v]
    # The first run is the baseline, not also the measure of now.
    recent = good[-3:] if len(good) > 1 else []
    now = round(sum(recent) / len(recent)) if recent else None
    saved = round(100 * (first - now) / first) if first and now is not None else None
    return {"first": first, "now": now, "saved_percent": saved}


def counts(verdict):
    """Whether one run's verdict counts towards trust: it succeeded, needed no person, and an independent check passed."""

    return bool(verdict.get("ok") and not verdict.get("helped") and INDEPENDENT & set(verdict.get("basis") or ()))


def clean_streak(skill):
    """How many runs in a row, most recent first, succeeded, needed no person and were checked independently."""

    streak = 0
    for verdict in reversed((skill.get("experience") or {}).get("verdicts") or []):
        if not counts(verdict):
            break
        streak += 1
    return streak


def stage(skill):
    """APPRENTICE while the skill has not yet run TRUST_AFTER times in a row cleanly, TRUSTED after."""

    return TRUSTED if clean_streak(skill) >= TRUST_AFTER else APPRENTICE


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
            "recent": [r["outcome"] for r in experience.get("runs") or []],
            "stage": stage(skill), "clean_runs": clean_streak(skill), "trust_after": TRUST_AFTER,
            "tokens": economy(skill),
            # How each recent run was checked, most recent last: what its trust rests on.
            "basis": [list(v.get("basis") or []) for v in experience.get("verdicts") or []]}


def advice(skill):
    """A sentence for the end of a run when its skill has started to wear, else ''."""

    fragile = health(skill)["fragile"]

    if not fragile:
        return ""

    steps = ", ".join(f"step {e['n']} ({e['needed']} of its last {e['of']} runs)" for e in fragile[:3])
    return (f" {steps} keep needing a repair. It still works, but the screen has moved on from the recording - "
            "record it again and `/learn` it to make it steady.")
