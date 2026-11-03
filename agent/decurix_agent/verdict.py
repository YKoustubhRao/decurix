"""
The structured verdict every outcome carries (R6 of the plan of 2026-11-03; PLAN-P2 §5.2, D5, C5): decurix.verdict/v1.

The final callback said only `checked` (a boolean) and the control plane read the rest out of the report's words - a
stop by its first word, the evidence by its headings. A verdict says it as data:

    {"schema": "decurix.verdict/v1",
     "verdict": "done" | "not_done" | "unchecked" | "stopped",
     "by": which gate gave it,
     "why": "...",
     "basis": the kinds of check that passed (code, ledger, screen, model, person),
     "checks": [{"id", "result": pass | fail | not_made, "basis", "detail", "refs": [...]}],
     "evidence": ["Checked on the screen: ..."]}

Rules: `done` needs at least one check that passed and none that failed or was not made - except a `legacy` verdict,
derived from a report's checked=True by a path that has not said yet what it checked (messaging.write_final_reply): it
has no checks and no basis, so it never counts towards trust (experience.INDEPENDENT), and the list of paths that
still make one may only shrink (agent/tests/test_verdict.py). `stopped` means the person said no and the work stopped
there: never `done`. `checked` on the callback is always `verdict == "done"`.

Every string goes through privacy.scrub and is cut to MAX_TEXT; at most MAX_CHECKS checks and MAX_REFS refs each.
"""

from decurix_agent.computer import privacy

SCHEMA = "decurix.verdict/v1"
DONE, NOT_DONE, UNCHECKED, STOPPED = "done", "not_done", "unchecked", "stopped"
VERDICTS = (DONE, NOT_DONE, UNCHECKED, STOPPED)
BY = ("verify", "skill_gate", "long_task", "replay", "workflow", "worker", "none", "legacy")
PASS, FAIL, NOT_MADE = "pass", "fail", "not_made"
RESULTS = (PASS, FAIL, NOT_MADE)
BASES = ("code", "ledger", "screen", "model", "person")
MAX_CHECKS = 64
MAX_REFS = 16
MAX_TEXT = 600


def _text(value):
    """A string for the verdict: scrubbed of secrets, one line of words, at most MAX_TEXT characters."""

    text = privacy.scrub(" ".join(str(value if value is not None else "").split()))
    return (text if isinstance(text, str) else privacy.HELD_BACK)[:MAX_TEXT]


def check(id, result, basis, detail="", refs=()):  # noqa: A002 - the schema's field name
    """One check and its result: `result` pass, fail or not_made; `basis` the kind of check (code, ledger, ...)."""

    return {"id": _text(id)[:120], "result": result, "basis": basis, "detail": _text(detail),
            "refs": [_text(r) for r in list(refs or ())[:MAX_REFS]]}


def make(verdict, by, why="", checks=(), evidence=(), basis=()):
    """
    A decurix.verdict/v1. `basis` defaults to the bases of the checks that passed; a `basis` given is added to them
    (a person's confirmation has no check of its own).
    """

    checks = list(checks or ())[:MAX_CHECKS]
    passed = {c.get("basis") for c in checks if c.get("result") == PASS}
    return {"schema": SCHEMA, "verdict": verdict, "by": by, "why": _text(why),
            "basis": sorted(b for b in passed | set(basis or ()) if b in BASES),
            "checks": checks, "evidence": [_text(e) for e in list(evidence or ())[:MAX_CHECKS] if str(e or "").strip()]}


def validate(v):
    """What is wrong with a verdict, as lines ([] when it is valid). Unknown fields are allowed (a v1.1 verdict)."""

    if not isinstance(v, dict):
        return ["a verdict is a JSON object"]
    out = []
    if v.get("schema") != SCHEMA:
        out.append(f"schema is not {SCHEMA}")
    if v.get("verdict") not in VERDICTS:
        out.append(f"verdict {v.get('verdict')!r} is not one of {', '.join(VERDICTS)}")
    if v.get("by") not in BY:
        out.append(f"by {v.get('by')!r} is not one of {', '.join(BY)}")
    for key in ("why",):
        if not isinstance(v.get(key, ""), str) or len(v.get(key, "")) > MAX_TEXT:
            out.append(f"{key} is not a string of at most {MAX_TEXT} characters")
    basis = v.get("basis", [])
    if not isinstance(basis, list) or any(b not in BASES for b in basis):
        out.append(f"basis must list only {', '.join(BASES)}")
    checks = v.get("checks", [])
    if not isinstance(checks, list) or len(checks) > MAX_CHECKS:
        out.append(f"checks must be a list of at most {MAX_CHECKS}")
        checks = []
    for n, c in enumerate(checks):
        if not isinstance(c, dict):
            out.append(f"check {n} is not an object")
            continue
        if not isinstance(c.get("id"), str) or not c.get("id"):
            out.append(f"check {n} has no id")
        if c.get("result") not in RESULTS:
            out.append(f"check {n}: result {c.get('result')!r} is not one of {', '.join(RESULTS)}")
        if c.get("basis") not in BASES:
            out.append(f"check {n}: basis {c.get('basis')!r} is not one of {', '.join(BASES)}")
        for key in ("id", "detail"):
            if len(str(c.get(key) or "")) > MAX_TEXT:
                out.append(f"check {n}: {key} is longer than {MAX_TEXT}")
        refs = c.get("refs", [])
        if not isinstance(refs, list) or len(refs) > MAX_REFS or any(len(str(r)) > MAX_TEXT for r in refs):
            out.append(f"check {n}: refs must be at most {MAX_REFS} strings of at most {MAX_TEXT}")
    evidence = v.get("evidence", [])
    if not isinstance(evidence, list) or any(not isinstance(e, str) or len(e) > MAX_TEXT for e in evidence):
        out.append(f"evidence must be a list of strings of at most {MAX_TEXT}")
    if v.get("verdict") == DONE and v.get("by") != "legacy":
        results = [c.get("result") for c in checks if isinstance(c, dict)]
        if PASS not in results:
            out.append("a done verdict needs at least one check that passed")
        if FAIL in results or NOT_MADE in results:
            out.append("a done verdict has no check that failed or was not made")
    if v.get("verdict") == DONE and v.get("by") == "legacy" and (checks or basis):
        out.append("a legacy done carries no checks and no basis")
    return out


def is_done(v):
    """Whether a verdict is a valid done (what `checked` on the callback follows)."""

    return isinstance(v, dict) and v.get("verdict") == DONE and not validate(v)


def derive(checked, kind, gated=False, stopped=False, why=""):
    """
    The verdict of a report whose sender gave none (messaging.write_final_reply), so that no outcome goes without one:
    a stop is `stopped`; an error is `not_done` - by `legacy` when a check found it so (checked=True on an error is
    verify.outcome's "a check saw it not done"), else by the worker; a final with checked=True is a `legacy` done (no
    checks, no basis: it never counts towards trust); anything else - gated and not checked, or never gated - is
    `unchecked`.
    """

    if stopped:
        return make(STOPPED, "worker", why=why)
    if kind == "error":
        return make(NOT_DONE, "legacy" if checked else "worker", why=why)
    if checked:
        return make(DONE, "legacy", why=why)
    return make(UNCHECKED, "worker" if gated else "none", why=why)


def from_outcome(decided, by):
    """The verdict of verify.outcome's dict (status, why, checks, basis), given by gate `by`."""

    status = decided.get("status")
    verdict = {"done": DONE, "not_done": NOT_DONE}.get(status, UNCHECKED)
    checks = list(decided.get("checks") or [])
    if verdict == DONE and not any(c.get("result") == PASS for c in checks):
        # verify.outcome said done with nothing that passed to show for it: not a done this schema can carry.
        verdict = UNCHECKED
    return make(verdict, by, why=decided.get("why") or "", checks=checks, basis=decided.get("basis") or ())
