from decurix_agent.computer import replay


# ============================================================
# Workflow: a recording as subtasks a model can follow
#
# UI-Mate (Tencent) turns a recorded demonstration into a workflow of subtasks - each
# a short goal, an explicitly checkable completion criterion, and its ordered action
# steps described WITHOUT pixel coordinates, "advisory rather than executable" - and
# hands it to a model as in-context guidance, under one rule: the live screen is
# authoritative. The model follows a demonstrated step while it still applies,
# supplies what the recording left out, and re-plans where the task differs.
# Reported effect of one demonstration: strict success 17% -> 35% on a benchmark of
# real desktop workflows (UI-Mate technical report). AgentRR calls the same thing a
# high-level experience: the general procedure beside the exact low-level replay.
#
# UI-Mate has a vision-language model annotate and segment the recording. Decurix
# already knows what that model would say, because it was watching the screen when the
# action happened: a run of steps in one window or site is a phase (replay._phases),
# and what a step should leave on the screen is what it expected (replay.mine_checks).
# So a subtask here is a phase, its completion criterion is the last thing its steps
# expected, and nothing is generated - it reads the same every time, and works with no
# model at all. What it does not have is UI-Mate's intent-level goal sentence; a
# subtask's goal is the place and what was done there.
# ============================================================

MAX_GOAL_STEPS = 3


def _advice(step):
    """A step as guidance: what to do, never where on the screen."""

    kind = step["kind"]

    if kind == "manual":
        return f"a person does this by hand ({step.get('reason') or 'a private step'})"

    line = step["summary"]

    # A position is a place on one screen at one size; the guidance is what it was for.
    if step.get("point") and not step.get("target"):
        line = f"{kind.replace('_', ' ')} the control shown in the recording" + \
               (" (recognised by a picture of it)" if step.get("look") else " (no more is known than its position)")

    if step.get("consequential"):
        line += " - stops for a person's approval"
    if step.get("optional"):
        line += " - only sometimes needed"

    return line


def _short(step):
    """A step in a few words, for a subtask's goal."""

    if step["kind"] == "manual":
        return "a step a person does by hand"
    if step.get("point") and not step.get("target"):
        return f"{step['kind'].replace('_', ' ')} a control"

    return step["summary"].split(" - ")[0]


def build(program):
    """
    A program's subtasks: [{"n", "place", "goal", "steps", "done_when", "first", "last", "gated"}].
    `steps` are advisory descriptions, `done_when` what the screen should show when
    the subtask is finished (empty when nothing in it left anything checkable).
    """

    groups = []

    for step in program.get("steps") or []:
        phase = step.get("phase") or 1

        if not groups or groups[-1]["phase"] != phase:
            groups.append({"phase": phase, "place": step.get("phase_label") or "the screen", "steps": []})

        groups[-1]["steps"].append(step)

    subtasks = []

    for n, group in enumerate(groups, 1):
        steps = group["steps"]
        said = [_short(s) for s in steps[:MAX_GOAL_STEPS]]
        goal = f"In {group['place']}: " + "; ".join(said) + (f"; and {len(steps) - MAX_GOAL_STEPS} more" if len(steps) > MAX_GOAL_STEPS else "")

        # What the subtask should end with: the last thing any of its steps expected.
        done = next(([replay.describe_check(c) for c in s["expect"][:2]] for s in reversed(steps) if s.get("expect")), [])

        subtasks.append({
            "n": n,
            "place": group["place"],
            "goal": goal,
            "steps": [_advice(s) for s in steps],
            "done_when": done,
            "first": steps[0]["n"],
            "last": steps[-1]["n"],
            # A boundary is worth knowing about before it is reached, not on arriving.
            "gated": sum(1 for s in steps if s.get("consequential")),
        })

    return subtasks


CONTRACT = (
    "This is what a person once did, for guidance. It is advisory: the screen in front of you is what is "
    "true. Follow a step while it still fits, do what the recording left out, and work it out afresh where "
    "the task differs. A part is finished when its 'done when' is on the screen, not when its steps have been tried."
)


def render(program, current=None):
    """
    The workflow as a model reads it, in UI-Mate's shape: every subtask's goal with a
    done / current / upcoming marker, and the action steps of the current one only.
    `current` is a subtask number (default: the first).
    """

    subtasks = build(program)
    if not subtasks:
        return ""

    current = current or 1
    title = program.get("title") or program.get("session") or "a recorded task"
    lines = [f"Workflow: {title}", CONTRACT, ""]

    for sub in subtasks:
        mark = "x" if sub["n"] < current else (">" if sub["n"] == current else " ")
        done = "; ".join(sub["done_when"]) if sub["done_when"] else "nothing here can be confirmed on the screen"
        gate = f"  [{sub['gated']} step(s) stop for a person's approval]" if sub["gated"] else ""
        lines.append(f"[{mark}] {sub['n']}. {sub['goal']}  (done when: {done}){gate}")

        if sub["n"] == current:
            lines.extend(f"      {i}. {step}" for i, step in enumerate(sub["steps"], 1))

    params = program.get("params") or []
    if params:
        def describe(p):
            if p.get("secret"):
                return "secret - never in a recording, must be given"
            if p.get("default") is None and p.get("example") is not None:
                return f"changes every run; recorded as {p['example']!r}"
            return f"recorded as {p.get('default')!r}"

        lines += ["", "Values it takes: " + ", ".join(f"{p['name']} ({describe(p)})" for p in params)]

    return "\n".join(lines)
