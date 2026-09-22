import re

from decurix_agent.computer import replay, volatility


# ============================================================
# Lint: will this recording replay, before anything is run
#
# openadapt-flow grades a compiled bundle for the gaps that make a replay
# untrustworthy ("lint" says what is missing, "certify" refuses on it), and
# AgentRR generates "check functions" for an experience - the preconditions and
# invariants that bound what a replay may do. Decurix's replay already refuses
# to guess (replay.py) and its skills ask when stuck (skill_task.py); what a
# person had no way to see, until a run stopped, was how much a recording
# leaves to luck. This reads a compiled program - no screen, no model - and
# says so, step by step:
#
#   risk    a replay will halt here, or act on a hope: a step with nothing to
#           find its target by, a click on a coordinate with no picture of it,
#           a step that cannot be undone and leaves nothing to check
#   warn    it will probably work and may not: a target named for the moment
#           it was recorded ("Invoice INV-1043"), most steps unchecked
#   info    something to know: a value it must be given, a step for a person
#
# The grade is the worst finding: `ready`, `rehearse` (run it once, watching)
# or `risky`. It is advice; nothing here stops a replay, and a replay that
# meets what a finding warned of halts exactly as it always did.
# ============================================================

# A finding's level...
RISK = "risk"
WARN = "warn"
INFO = "info"

# ...and the grade a program gets from its worst one.
READY = "ready"
REHEARSE = "rehearse"
RISKY = "risky"

GRADES = {INFO: READY, WARN: REHEARSE, RISK: RISKY}
SEVERITY = {INFO: 0, WARN: 1, RISK: 2}

# Below this share of steps with something to check, the run is mostly taking
# the screen's word for it.
MIN_COVERAGE = 0.6

MAX_LISTED = 6


def _finding(level, code, message, steps=None, fix=None):
    finding = {"level": level, "code": code, "message": message}
    if steps:
        finding["steps"] = steps
    if fix:
        finding["fix"] = fix
    return finding


def _steps_text(numbers):
    """"Step 3" or "Steps 3, 4 and 2 more": worded so what follows reads for one step or many."""

    shown = ", ".join(str(n) for n in numbers[:MAX_LISTED])
    more = f" and {len(numbers) - MAX_LISTED} more" if len(numbers) > MAX_LISTED else ""
    return ("Step " if len(numbers) == 1 else "Steps ") + shown + more


def _volatile_name(name):
    """Whether a control's name reads as a moment or a record rather than a label."""

    if not name:
        return False

    return bool(re.search(r"\d", name)) or volatility.classify(name) not in (None, "too_short")


def lint(program):
    """
    A program's readiness: {"grade", "summary", "findings", "coverage"}.
    `coverage` counts the steps by how each is found and checked.
    """

    steps = program.get("steps") or []
    findings = []

    unreplayable, unresolvable, blind, anchored, cut_off, positional = [], [], [], [], [], []
    unidentified = []
    unchecked, unchecked_consequential, volatile, manual = [], [], [], []
    consequential = []

    for step in steps:
        n, kind = step["n"], step["kind"]

        if kind not in replay.REPLAYABLE:
            unreplayable.append(n)
            continue

        if kind == "manual":
            manual.append(n)
            continue

        target, point = step.get("target"), step.get("point")

        if step.get("unresolved"):
            unidentified.append(n)
        elif kind in replay.NEEDS_TARGET and not target and not point:
            unresolvable.append(n)
        elif point and not target:
            (anchored if step.get("look") else blind).append(n)
            if (step.get("look") or {}).get("partial"):
                cut_off.append(n)

        if target and target.get("nth"):
            positional.append(n)

        if target and _volatile_name(target.get("name")) and not step.get("alternates"):
            volatile.append(n)

        if step.get("consequential"):
            consequential.append(n)

        if not step.get("expect"):
            unchecked.append(n)
            if step.get("consequential"):
                unchecked_consequential.append(n)

    if unreplayable:
        findings.append(_finding(
            RISK, "unreplayable",
            f"{_steps_text(unreplayable)}: an action a replay cannot do, so it will stop there.",
            unreplayable, "Record that part again, or do it by hand when the replay asks."))

    if unresolvable:
        findings.append(_finding(
            RISK, "no_target",
            f"{_steps_text(unresolvable)}: nothing to find the target by (no control and no position), "
            "so the replay will stop there.",
            unresolvable, "Record it again with the control on the screen and in focus."))

    if unidentified:
        findings.append(_finding(
            RISK, "unidentified_control",
            f"{_steps_text(unidentified)}: meant a particular control that the import could not identify, "
            "so the replay stops before it - it does not type or click somewhere else instead.",
            unidentified, "Import it again from a source that kept the page's accessibility tree."))

    if blind:
        findings.append(_finding(
            RISK, "blind_click",
            f"{_steps_text(blind)}: a click on a screen position, and nothing confirms what is there - "
            "if the page has moved, it clicks the wrong thing.",
            blind, "Record it again on a busier part of the control - a plain patch cannot be recognised."))

    if unchecked_consequential:
        findings.append(_finding(
            RISK, "unchecked_consequential",
            f"{_steps_text(unchecked_consequential)}: asks for approval and leaves nothing a replay can check, "
            "so a run cannot tell whether it took effect.",
            unchecked_consequential, "Record what the screen shows afterwards (a confirmation) so it can be seen."))

    if volatile:
        findings.append(_finding(
            WARN, "volatile_target",
            f"{_steps_text(volatile)}: found by a name that looks like a moment or a record "
            "(a number or a date in it), which will differ next time.",
            volatile, "Teach it from two recordings, so what changes is learned as a variable."))

    coverage = {
        "steps": len(steps),
        "checked": len(steps) - len(unchecked) - len(manual) - len(unreplayable),
        "anchored": len(anchored),
        "blind": len(blind),
        "consequential": len(consequential),
        "manual": len(manual),
    }

    runnable = len(steps) - len(manual) - len(unreplayable)
    if runnable and coverage["checked"] / runnable < MIN_COVERAGE:
        findings.append(_finding(
            WARN, "mostly_unchecked",
            f"Only {coverage['checked']} of {runnable} steps leave something a replay can check, "
            "so most of a run takes the screen's word for it.",
            unchecked, "Look at the result of a first run before relying on it."))

    secrets = [p for p in program.get("params") or [] if p.get("secret")]
    if secrets:
        findings.append(_finding(
            INFO, "secret_params",
            "It must be given " + ", ".join(p["label"] for p in secrets)
            + " each time: the recording never keeps a secret.",
            [p["step"] for p in secrets]))

    if manual:
        findings.append(_finding(
            INFO, "manual_steps",
            f"{_steps_text(manual)}: done by a person, so a run pauses there.", manual))

    if anchored:
        findings.append(_finding(
            INFO, "anchored_clicks",
            f"{_steps_text(anchored)}: a click on a position, checked against a picture of what was around it "
            "and followed a short way if it has moved.", anchored))

    if positional:
        findings.append(_finding(
            WARN, "positional_target",
            f"{_steps_text(positional)}: found by its place among controls that look the same, because none of "
            "them has a name - if the page's order changes, it acts on a different one.",
            positional, "Give the fields labels a replay can read, or check the result of a first run."))

    if cut_off:
        findings.append(_finding(
            INFO, "partial_anchor",
            f"{_steps_text(cut_off)}: what was clicked runs past the edge of its picture, so a different control "
            "that differs only out there would look the same to the check.", cut_off))

    if not steps:
        findings.append(_finding(RISK, "empty", "There is nothing in it a replay can run."))

    grade = GRADES[max((f["level"] for f in findings), key=SEVERITY.get, default=INFO)]

    return {"grade": grade, "summary": _summary(grade, findings, coverage), "findings": findings, "coverage": coverage}


def _summary(grade, findings, coverage):
    if grade == READY:
        return "Every step can be found again and leaves something to check afterwards."

    concerns = [f for f in findings if f["level"] != INFO]
    lead = concerns[0]["message"] if concerns else ""
    head = {REHEARSE: "Run it once while watching.", RISKY: "Expect it to stop, or to act on a guess."}[grade]

    return f"{head} {lead}".strip()
