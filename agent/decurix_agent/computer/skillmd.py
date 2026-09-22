import json
import re

from decurix_agent.computer import learning, replay, skills, workflow


# ============================================================
# SKILL.md: a taught task as a skill any agent can read
#
# Understudy publishes what it was taught as a SKILL.md with three layers: an intent
# procedure (what is done, in words), route options (the preferred way to do it, a
# fallback, and the routes observed in the demonstration - direct tool, shell,
# browser, GUI), and GUI replay hints (where on the screen, last resort, re-grounded
# on every run). SKILL.md is also the Agent Skills format - a folder with a
# SKILL.md whose YAML front matter carries a `name` and a `description` - that Claude
# Code, Codex and others load, so a task taught here can be handed to any of them,
# and one that only reads skills still knows it exists and how to run it.
#
# Everything is read out of the skill; nothing is generated. The intent layer is
# workflow.py's subtasks. The routes are what Decurix can actually do with it: replay
# it with no model (`gui do`), or an engine doing the same with `gui` commands. The
# hints say how each step is found again - by role and name, never by coordinates
# unless that is all the recording had, and then checked against a picture.
# ============================================================

NAME_OK = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")
MAX_NAME = 64
MAX_DESCRIPTION = 1024


def _name(name):
    """A skill's name as Agent Skills allows it: lowercase, digits, single hyphens, at most 64."""

    cleaned = re.sub(r"-{2,}", "-", re.sub(r"[^a-z0-9-]", "-", name.lower())).strip("-")[:MAX_NAME].strip("-")
    return cleaned if NAME_OK.match(cleaned) else "decurix-skill"


def _how_found(step):
    if step["kind"] == "open":
        return "opened directly by its address - the browser route"
    if step["kind"] == "manual":
        return "not replayed: a person does it"

    if step.get("look") and not step.get("target"):
        where = "by a picture of what was around it (the recording had no control to name), and only where that picture still is"
        return where + (" - the picture is cut off by its edge, so it cannot tell apart something that differs beyond it"
                        if step["look"].get("partial") else "")
    if step.get("point") and not step.get("target"):
        return "by its position on a screen of the recorded size - nothing else is known of it"

    target = step.get("target") or {}
    if target.get("nth"):
        return f"as the {_ordinal(target['nth'])} {target.get('role', 'control')} with no name - by its place, so only as good as the page's order"
    if target.get("role") or target.get("name"):
        alt = "; also known as " + ", ".join(repr(a.get("name")) for a in step["alternates"] if a.get("name")) if step.get("alternates") else ""
        return "by its role and name, looked for again on each run" + alt

    return "typed into whatever has focus"


def _ordinal(n):
    return {1: "1st", 2: "2nd", 3: "3rd"}.get(n, f"{n}th")


def _description(skill, subtasks):
    title = skill.get("title") or skill["name"]
    parts = [f"{title.rstrip('.')}, taught by demonstration on a Decurix desktop."]

    if skill.get("done_when"):
        parts.append(f"It is finished when: {skill['done_when'].rstrip('.')}.")

    params = [p for p in skills.required(skill)]
    if params:
        parts.append("Needs " + ", ".join(p["name"] for p in params) + ".")

    parts.append(f"Use when asked to {title[:1].lower() + title[1:].rstrip('.')}.")

    # Angle brackets in the front matter can inject into the system prompt of whatever loads it.
    return " ".join(parts).replace("<", "(").replace(">", ")")[:MAX_DESCRIPTION]


def render(skill):
    """The skill as a SKILL.md: front matter, then intent, routes and GUI replay hints."""

    program = skills.program(skill)
    subtasks = workflow.build(program)
    params = skills.required(skill)
    steps = program["steps"]
    title = skill.get("title") or skill["name"]
    name = _name(skill["name"])

    front = [
        "---",
        f"name: {name}",
        f"description: {json.dumps(_description(skill, subtasks))}",
        # The spec's `metadata` is a map of strings to strings; a nested map is stringified
        # into a Python repr by the reference reader, so each key stands alone.
        "metadata:",
        '  decurix-artifact-kind: "skill"',
        f"  decurix-skill-name: {json.dumps(skill['name'])}",
        f"  decurix-version: {json.dumps(str(int(skill.get('version') or 1)))}",
        f"  decurix-demonstrations: {json.dumps(str(int(skill.get('demos') or 1)))}",
        f"  decurix-status: {json.dumps(skill.get('status') or 'draft')}",
        "---",
        "",
    ]

    body = [f"# {title}", ""]

    # ---- intent ----
    body += ["## Intent", ""]

    if skill.get("goal") and skill["goal"] != title:
        body += [skill["goal"], ""]

    if skill.get("done_when"):
        body += [f"**Done when:** {skill['done_when']}", ""]

    for check in learning.start_checks(skill):
        body += [f"**Starts from:** {replay.describe_check(check)}", ""]

    if params:
        body += ["**Inputs**", ""]
        for p in params:
            recorded = f", recorded as `{p['example']}`" if p.get("example") else ""
            kind = ("a list, one item per repetition" if p.get("list") else "changes every run")
            constraint = p.get("constraint") or {}
            checked = f"; must be {constraint['type']}" if constraint.get("strict") else ""
            body += [f"- `{p['name']}` - {p['label']}{recorded} ({kind}{checked})"]
        body += [""]

    secrets = [s for s in steps if s["kind"] == "manual"]
    if secrets:
        body += ["**By a person:** " + "; ".join(f"step {s['n']} ({s.get('reason') or 'a private step'})" for s in secrets)
                 + ". A recording never keeps a secret, so these are never in a run's inputs.", ""]

    body += ["**Procedure** (advisory - the screen in front of you is what is true)", ""]
    for sub in subtasks:
        done = "; ".join(sub["done_when"]) if sub["done_when"] else "nothing here can be confirmed on the screen"
        body += [f"{sub['n']}. **{sub['goal']}** _(done when: {done})_"]
        body += [f"   - {step}" for step in sub["steps"]]
    body += [""]

    gated = [s for s in steps if s.get("consequential")]
    if gated:
        body += ["**Needs approval** - each of these stops for a person and never proceeds on its own:", ""]
        body += [f"- step {s['n']}: {s['summary']}" for s in gated]
        body += [""]

    # ---- routes ----
    args = " ".join(f'--param {p["name"]}="..."' for p in params)
    browser = [s for s in steps if s["kind"] == "open"]
    gui = [s for s in steps if s["kind"] not in ("open", "manual")]

    body += ["## Routes", "",
             f"- **Preferred:** replay it with no model - `gui do {skill['name']}{' ' + args if args else ''}`. "
             "It finds each control again, checks each step's expected result, and stops to ask rather than guess.",
             "- **Fallback:** follow the procedure above yourself with `gui observe` / `gui click` / `gui type` / `gui verify`, "
             "checking each part's *done when* before going on."]
    observed = []
    if browser:
        observed.append(f"browser - {len(browser)} step(s) open an address directly ({', '.join(s['open'] for s in browser[:2])})")
    if gui:
        observed.append(f"GUI - {len(gui)} step(s) act on controls on the screen")
    body += [f"- **Observed in the demonstration:** {'; '.join(observed) or 'nothing that ran'}.", ""]

    # ---- gui hints ----
    body += ["## GUI replay hints", "",
             "Nothing here says where on the screen anything is. Each step is found again on every run, "
             "and a step that cannot be found stops the run - it is never clicked at a guess.", ""]
    for step in steps:
        flags = []
        if step.get("consequential"):
            flags.append("stops for approval")
        if step.get("optional"):
            flags.append("only sometimes")
        body += [f"{step['n']}. {step['summary']} - {_how_found(step)}" + (f" ({', '.join(flags)})" if flags else "")]
    body += [""]

    if skill.get("notes"):
        body += ["## Notes from the person who taught it", ""] + [f"- {n['text']}" for n in skill["notes"]] + [""]

    return "\n".join(front + body).rstrip() + "\n"
