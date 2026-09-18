from flowera_agent.computer import core, learning, replay, skills
from flowera_agent.computer import elements as el
from flowera_agent.computer.errors import ComputerError


# ============================================================
# Skills, for an engine
#
# What an engine (Ollama, TheBot, Claude) is given to find and run a skill the
# desktop was taught (computer/learning.py): `gui skills` / `gui skill` / `gui
# do`, and the same as MCP tools. A person teaches and corrects a skill in the
# chat (skill_task.py); an engine has no such channel of its own, so where
# skill_task asks a person, this refuses in words the engine can act on - it
# says what is missing, and the engine raises it with the human the way it
# raises anything else, then runs again with the answer.
#
# A run goes through the same replay.run, and so the same approval gate and
# audit trail, as everything else. It never rewrites a skill: a correction is
# something a person makes, in the chat, and a run outside the chat has no one
# to make it.
# ============================================================


def summary_line(skill):
    """One line for a skill in a list."""

    needs = ", ".join(p["name"] for p in skills.required(skill))
    return (f"{skill['name']} {el._quote(skill.get('title') or '', 70)} - {len(skill['steps'])} steps, run {skill.get('runs', 0)} times"
            + (f"; needs {needs}" if needs else ""))


def render_text(skill, budget=3500):
    """A skill as an engine reads it: what it needs, what it does, and how to run it."""

    lines = [f"Skill {skill['name']} {el._quote(skill.get('title') or '', 80)}: {len(skill['steps'])} steps, "
             f"learned from {skill.get('demos', 1)} demonstration(s), run {skill.get('runs', 0)} times."]

    if skill.get("done_when"):
        lines.append("It is finished when: " + el._quote(skill["done_when"], 240))

    needed = skills.required(skill)
    if needed:
        lines.append("Needs: " + ", ".join(
            f"{p['name']} ({p['label']}" + ("; a list, items separated by ';'" if p.get("list") else "") + ")" for p in needed))

    gated = sum(1 for step in skill["steps"] if step.get("consequential"))
    if gated:
        lines.append(f"{gated} step(s) stop for human approval every run.")

    args = "".join(f' --param {p["name"]}="..."' for p in needed)
    footer = f"Run it with no model: gui do {skill['name']}{args} [--from N]"

    lines.append(learning.overview(skill, budget=max(200, budget - sum(len(x) + 1 for x in lines) - len(footer) - 20)))
    lines.append(footer)

    return "\n".join(lines)


def _refusal(skill, reason, missing=()):
    """A run that did not start, shaped like replay.run's answer so callers print it the same way."""

    return {
        "outcome": replay.HALTED_BEFORE_EFFECT,
        "text": f"Did not run {el._quote(skill.get('title') or skill['name'], 80)}: {reason}",
        "reason": reason,
        "step": None,
        "resume_from": None,
        "missing": list(missing),
        "done": 0, "verified": 0, "unverified": 0, "skipped": 0,
        "total": len(skill["steps"]),
        "session": f"skill-{skill['name']}",
    }


def run(skill, params=None, start=1):
    """
    Run a ready skill with no model and say how it ended, in replay.run's shape.
    A skill that is not ready, is missing a value the recording could not
    supply, or is not where it began (when a person asked for that to be
    checked) does nothing and says why.
    """

    if skill.get("status") != skills.READY:
        return _refusal(skill, "it was never finished being taught - a person confirms a skill in the chat with /learn")

    values = skills.initial_values(skill, params)

    missing = [p for p in skills.needed_from(skill, start) if not values.get(p["name"])]
    if missing:
        wanted = ", ".join(f"{p['name']} ({p['label']})" for p in missing)
        return _refusal(skill, f"it needs {wanted} - ask the human for the value, then pass it with --param", [p["name"] for p in missing])

    problem = skills.list_problem(skill, values)
    if problem:
        return _refusal(skill, problem, [p["name"] for p in skills.list_params(skill)])

    checks = learning.start_checks(skill) if start == 1 else []
    if checks:
        try:
            observation = core.observe()
        except ComputerError as exc:
            return _refusal(skill, f"the screen could not be read to check where to begin: {exc}")

        if replay.unmet(checks, observation):
            return _refusal(skill, f"it begins from {', '.join(replay.describe_check(c) for c in checks)}, but "
                                   f"{replay._screen_brief(observation)}. Put the screen where it begins, or ask the human")

    program, expanded = skills.expand(skill, values)
    result = replay.run(program, params=expanded, start=start)

    if result["outcome"] in replay.SUCCESS:
        skills.note_run(skill["name"])

    return result
