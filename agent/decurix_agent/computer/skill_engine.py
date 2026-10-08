import json

from decurix_agent import llm_client, task_context
from decurix_agent.computer import escalation, core, learning, replay, skills
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log


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
    said = skills.explanation(skill)
    summary = (skill.get("understanding") or {}).get("summary")
    where = ", ".join(skills.places(skill))
    return (f"{skill['name']} {el._quote(skill.get('title') or '', 70)} - {len(skill['steps'])} steps, run {skill.get('runs', 0)} times"
            + (f"; works in {where}" if where else "")
            + (f"; needs {needs}" if needs else "")
            + (f"; what it is: {el._quote(summary, 200)}" if summary else "")
            + (f"; the person who taught it said: {el._quote(said, 160)}" if said else ""))


def render_text(skill, budget=3500):
    """A skill as an engine reads it: what it needs, what it does, and how to run it."""

    lines = [f"Skill {skill['name']} {el._quote(skill.get('title') or '', 80)}: {len(skill['steps'])} steps, "
             f"learned from {skill.get('demos', 1)} demonstration(s), run {skill.get('runs', 0)} times."]

    where = skills.places(skill)
    if where:
        lines.append(f"Works in: {', '.join(where)} - it does its task there and nowhere else; a request "
                     "for the same kind of work in another app or site is not this skill.")

    said = skills.explanation(skill)
    if said:
        lines.append("The person who taught it said: " + el._quote(said, 600)
                     + " - that is how they want this task done; follow it.")

    understood = learning.understood_text(skill)
    if understood:
        lines.append(understood)
    for correction in skill.get("corrections") or []:
        lines.append(f"A correction after an earlier run: {correction['text']}")

    needed = skills.required(skill)
    if needed:
        lines.append("Needs: " + ", ".join(
            f"{p['name']} ({p['label']}" + ("; a list, items separated by ';'" if p.get("list") else "") + ")" for p in needed))

    gated = sum(1 for step in skill["steps"] if step.get("consequential"))
    if gated:
        lines.append(f"{gated} step(s) stop for human approval every run.")

    args = "".join(f' --param {p["name"]}="' + ("ITEM 1; ITEM 2; ..." if p.get("list") else "...") + '"'
                   for p in needed)
    footer = f"Run it with no model: gui do {skill['name']}{args} [--from N]"
    if any(p.get("list") for p in needed):
        # Found live: an engine read a two-line PO and ran the skill with the
        # first line only, twice. A list is every item, in one run.
        footer += ("\nA list value is every item of the document, in its order, all lists the same length - "
                   "one run enters them all; never give only the first item.")

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


FITS_SYSTEM = (
    "You decide whether a taught procedure can do a request. The procedure was demonstrated in one app or "
    "site (where it works) and replays clicks there. The same kind of work asked for in a DIFFERENT app, "
    "site or system (another desktop app, another web app, one the request names or says is open) is not this "
    "procedure's to do. Reply with JSON only: {\"same_system\": true|false, \"requested_system\": \"<the "
    "app or site the request is for, or empty>\"}. When the request names no system, it is the same."
)


def other_system(skill, request):
    """
    The app or site the request is for, when that is not where the skill works;
    "" when it is, when it cannot be told, or with no hosted model to ask.
    A procedure demonstrated in one application must not open it over the
    application requested by the user and save the result in the wrong place.
    """

    request = (request or "").strip()
    where = skills.places(skill)
    if not request or not where or not llm_client.available():
        return ""
    prompt = (f"Procedure: {skill.get('title') or skill['name']}\nWorks in: {', '.join(where)}\n"
              f"What it does: {(skills.explanation(skill) or skill.get('goal') or '')[:400]}\n\nRequest:\n{request[:1500]}")
    try:
        message, _ = llm_client.chat([{"role": "system", "content": FITS_SYSTEM}, {"role": "user", "content": prompt}],
                                     max_tokens=200, response_format={"type": "json_object"})
        answer = json.loads(message.get("content") or "{}")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"skill_engine: could not tell which system {skill['name']} is for: {exc}")
        return ""
    if answer.get("same_system") is False:
        return str(answer.get("requested_system") or "another app").strip()[:80] or "another app"
    return ""


def run(skill, params=None, start=1):
    """
    Run a ready skill with no model and say how it ended, in replay.run's shape.
    A skill that is not ready, is missing a value the recording could not
    supply, or is not where it began (when a person asked for that to be
    checked) does nothing and says why.
    """

    if skill.get("status") != skills.READY:
        return _refusal(skill, "it was never finished being taught - a person confirms a skill in the chat with /learn")

    elsewhere = other_system(skill, task_context.current_request())
    if elsewhere:
        return _refusal(skill, f"this request is for {elsewhere}, and the skill works in "
                               f"{', '.join(skills.places(skill)) or 'another app'} - do the task in {elsewhere} with gui "
                               "instead (the skill's steps show what the work involves)")

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

    # The engine running this is itself the model, and there is no person to
    # ask - so only healing's own rung: a control that has plainly been
    # renamed is found, and anything less halts as it always did. Nothing is
    # kept from it: a skill is rewritten by a person's correction or by a run
    # skill_task did, never by an engine's.
    program, expanded = skills.expand(skill, values)
    result = replay.run(program, params=expanded, start=start,
                        ladder=escalation.Ladder(ask_model=None, goal=skill.get("title") or "", total=len(program["steps"])))

    if result["outcome"] in replay.SUCCESS:
        skills.note_run(skill["name"])

    return result
