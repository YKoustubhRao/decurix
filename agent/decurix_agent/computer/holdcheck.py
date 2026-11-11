"""
A dry check, run inside a desktop against a stored taught skill, that every press the person said needs their OK is
held for a yes when the skill is replayed - presses nothing, reads nothing from the screen.

    python3 -m decurix_agent.computer.holdcheck SKILL_NAME

For each entry of the skill's confirmed approvals (skill["approvals"]["ask"]) and each step the gate asked about while
it was taught (consequential), it says which rule holds that press on a replay - decided by the very functions the
press goes through (core._step_hold, core.skill_approvals; core._unidentified_hold for a press at a point where
nothing can be identified) - or NOT HELD. Exit 0 when every one is held; 1 when any is NOT HELD; 2 when the skill
cannot be read or has no confirmed approvals answer (every replayed save of a filled form is then held, but the
person's own list is not in force).
"""

import json
import sys

from decurix_agent.computer import audit, core, skills


def _targets(skill):
    """(label, step or None, entry or None) for every press the person said needs their OK, each once."""

    steps = skill.get("steps") or []
    by_key = {k: s for s in steps for k in (s.get("n"), s.get("id")) if k is not None}
    seen, out = set(), []
    for entry in ((skill.get("approvals") or {}).get("ask") or []):
        if not isinstance(entry, dict):
            continue
        step = by_key.get(entry.get("step")) or by_key.get(entry.get("id"))
        key = ("step", step.get("id") or step.get("n")) if step else ("name", str(entry.get("name") or ""))
        if key not in seen:
            seen.add(key)
            out.append((str(entry.get("name") or (step or {}).get("summary") or ""), step, entry))
    for step in steps:
        key = ("step", step.get("id") or step.get("n"))
        if step.get("consequential") and key not in seen:
            seen.add(key)
            out.append((str(step.get("summary") or ""), step, None))
    return out


def _how_held(skill, step, entry):
    """Which rule holds this press on a replay, in words, or None when none does."""

    if step is not None:
        saved = audit.origin
        audit.origin = {"session": "holdcheck", "step": step.get("n"), "id": step.get("id")}
        try:
            by_place = core._step_hold("replay", skill=skill)
            # (and where nothing can be identified at its point, on a page that says nothing is there)
            blind = core._unidentified_hold("click", None, None, (0, 0), {"page_url": "http://page"}, "replay",
                                            skill=skill) if step.get("point") else None
        finally:
            audit.origin = saved
        if by_place:
            return f"by its place: {by_place}" + ("; and an unidentifiable target there is held" if blind else "")
    name = " ".join(str((entry or {}).get("name") or "").split())
    if name and any(name == a.get("name") for a in (core.skill_approvals(skill) or {}).get("ask") or []):
        return f'by its control\'s name: any press of "{name}" in any run (core._person_hold)'
    return None


def check(name):
    """(exit code, report lines) for the stored skill `name`."""

    try:
        skill = skills.load(name)
    except FileNotFoundError:
        return 2, [f"no skill named {name} on this desktop"]
    if core.skill_approvals(skill) is None:
        return 2, [f"{name}: no confirmed approvals answer - every replayed save of a filled form and every press at a "
                   "point nothing can be identified is held, but the person's own list is not in force"]
    lines, code = [f"{name}: confirmed approvals"], 0
    for label, step, entry in _targets(skill):
        where = f"step {step.get('n')}" if step else "no step"
        how = _how_held(skill, step, entry)
        if how is None:
            code = 1
        lines.append(f"- {where} {json.dumps(label)}: " + (f"held {how}" if how else "NOT HELD"))
    if len(lines) == 1:
        lines.append("- (the person said no press of this task needs their OK)")
    return code, lines


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 1:
        print("usage: python3 -m decurix_agent.computer.holdcheck SKILL_NAME", file=sys.stderr)
        return 2
    code, lines = check(argv[0])
    print("\n".join(lines))
    return code


if __name__ == "__main__":
    sys.exit(main())
