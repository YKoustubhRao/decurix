"""
branches.py - the tools a worker builds for itself: what the model did to get a stopped replay going again, compiled
into a short program of its own, kept beside the person's taught steps (never in them), tried by code the next time the
same stop comes, and trusted only after code has seen it work.
"""

import copy
import hashlib
import time

from decurix_agent.computer import replay


# ============================================================
# Branches: a repair, once paid for, becomes code
#
# A taught task replays with no model until a step stops it: a list with no entry for this month's new customer, a
# form a dialog is in front of, a spot on the screen kept only as a point. Since 2026-11-11 the model is given that one
# stop to fix (repairs.py, a bounded repair) and the replay carries on after it. What the model pressed and typed for
# the fix is in the task's recording; this module compiles it into a CANDIDATE branch of the skill, keyed by the stop's
# shape - where it stopped (before the step acted, after it acted without its result, at a step left to a person), the
# step's kind and its control's role and name, and the step's stable id - never by the words of a reason.
#
# The next time a run stops with the same shape, the candidate runs first, through replay.run (so every press goes
# through core.act: the approval gate, the pre-save comparison, the read-back), and its effect is checked by code: the
# stopped step then goes through with its own recorded result, or the result that never appeared is now on the screen.
# A candidate is PROMOTED after PROMOTE_AFTER later runs in which it worked so (each a different task); a failure
# DEMOTES it, and the model repairs again, which may compile a new version. At most MAX_COMPILES versions are ever
# compiled for one shape, and the last MAX_VERSIONS are kept. The person's steps are never rewritten.
#
# What a branch may hold (research: ASI's "1553" overfitting, OpenAdapt's scope rule; owner's invariants):
#   - only presses, typing, keys, choices and scrolls on controls known by role and name - no bare point, no opening of
#     an address, no upload, no drag, no step a person did by hand, nothing typed that the recorder withheld;
#   - only in the applications the person's own steps work in;
#   - every value typed or chosen that equals one of the source run's values is that value's PARAMETER (the stopped
#     step's own value, or another value of the same item), and one that is in the run's documents but is no value of
#     the run refuses the candidate: a branch must never type one document's text into the next;
#   - a control whose name holds one of the run's values refuses it too (it would be that document's control);
#   - its committing presses are marked consequential: a commit whose result never appears is never pressed again.
# A branch carries no approval of its own. A press of it is judged by the gate like any engine's (core.act), and a
# press the gate holds stops the branch: the model then makes that press, and the person's card decides.
# ============================================================

CANDIDATE, PROMOTED, DEMOTED = "candidate", "promoted", "demoted"

# Later runs (each another task) in which a candidate worked, checked by code, before it is promoted.
try:
    from decurix_agent.computer.experience import PROMOTE_AFTER
except ImportError:  # pragma: no cover - experience always imports
    PROMOTE_AFTER = 2

MAX_COMPILES = 3        # versions ever compiled for one shape: then the model repairs it each time
MAX_VERSIONS = 3        # versions kept per shape (newest last)
MAX_BRANCH_STEPS = 25   # a fix longer than this is a task of its own, not a branch
MIN_LITERAL = 3         # shorter typed text is not compared with the run's documents (a "1" is in every one)

ALLOWED_KINDS = {"click", "double_click", "right_click", "type", "key", "select", "scroll"}
NEEDS_CONTROL = {"click", "double_click", "right_click", "type", "select"}


def _norm(text):
    return " ".join(str(text or "").split())


def _low(text):
    return _norm(text).lower()


# ------------------------------------------------------------
# The stop's shape
# ------------------------------------------------------------

def shape(step, result):
    """
    The structural shape of a replay stop at `step` (result: replay.run's): {"when", "kind", "role", "name", "id"}.
    `when` is "before" (the step did not act: its control was not found or the action failed), "after" (it acted and
    its recorded result never appeared: replay resumes from the next step) or "manual" (a step left to a person).
    """

    target = step.get("target") or {}
    if step.get("kind") == "manual":
        when = "manual"
    elif result.get("resume_from") is not None and result.get("resume_from") == (step.get("n") or 0) + 1:
        when = "after"
    else:
        when = "before"
    return {"when": when, "kind": str(step.get("kind") or ""), "role": str(target.get("role") or ""),
            "name": _norm(target.get("name")), "id": str(step.get("id") or step.get("n") or "")}


def key(found):
    """The key a shape is kept under in skill["branches"]."""

    return "|".join(found.get(k) or "" for k in ("when", "kind", "role", "name", "id"))


# ------------------------------------------------------------
# Compiling a repair
# ------------------------------------------------------------

class _NoFrames:
    """compile_program reads a frame only for a bare point's picture; a branch keeps no bare point."""

    def png(self, name):
        return None


def model_events(events, since=0):
    """The actions the model itself took in a recording from event index `since`: done (or not marked), not a
    replay's (audit's replay_of), not a note or a look."""

    out = []
    for event in events or []:
        if (event.get("i") or 0) < since or event.get("replay_of"):
            continue
        if event.get("kind") in (None, "note", "observe", "verify", "read"):
            continue
        if event.get("outcome") not in ("done", None):
            continue
        out.append(event)
    return out


def _same_control(a, b):
    ta, tb = a.get("target") or {}, b.get("target") or {}
    return (a.get("kind") == b.get("kind") and (ta.get("role") or "") == (tb.get("role") or "")
            and _low(ta.get("name")) == _low(tb.get("name")))


def _apps(skill):
    return {s.get("app") for s in skill.get("steps") or [] if s.get("app")}


def _hosts(skill):
    import urllib.parse
    hosts = set()
    for s in skill.get("steps") or []:
        for url in (s.get("url"), s.get("open")):
            if url and "://" in str(url):
                hosts.add(urllib.parse.urlsplit(str(url)).netloc.lower())
    return hosts


def _host(url):
    import urllib.parse
    return urllib.parse.urlsplit(str(url)).netloc.lower() if url and "://" in str(url) else ""


def _item_of(name):
    """(base, n) of an item's parameter "<base>__<n>" (skills.expand), else (name, None)."""

    base, sep, n = str(name or "").rpartition("__")
    return (base, int(n)) if sep and n.isdigit() else (str(name or ""), None)


def _bind(literal, values, halted_param):
    """How a typed or chosen literal is bound to the run: {"halted": True}, {"item": base}, {"name": name}, or None
    when it equals none of the run's values."""

    want = _low(literal)
    if not want:
        return None
    if halted_param and _low(values.get(halted_param)) == want:
        return {"halted": True}
    _, n = _item_of(halted_param)
    named = [name for name, value in values.items() if isinstance(value, str) and _low(value) == want]
    for name in named:
        base, m = _item_of(name)
        if n is not None and m == n:
            return {"item": base}
    if named:
        return {"name": named[0]}
    return None


def _holds_value(text, values):
    """The run's values (of MIN_LITERAL characters or more) that `text` holds, longest first."""

    low = _low(text)
    found = [v for v in values.values() if isinstance(v, str) and len(_norm(v)) >= MIN_LITERAL and _low(v) in low]
    return sorted(set(found), key=len, reverse=True)


def compile_candidate(skill, halted, values, events, inputs="", since=0):
    """
    (candidate, why): the model's actions since event `since` compiled into a branch for a stop at `halted` (a step of
    the run's written-out program), or (None, why not). `values` are the run's values (the written-out program's),
    `inputs` the text of the run's request and documents (what a literal must not be copied from).
    """

    done = model_events(events, since)
    if not done:
        return None, "the repair pressed nothing: the stop cleared by itself (a slow screen), nothing to keep"
    program = replay.compile_program({"id": "repair", "started_at": time.time()}, done, frames=_NoFrames())
    steps = program.get("steps") or []
    by_n = {p["step"]: p for p in program.get("params") or []}
    # The model did the stopped step itself as its last action: the replay does that step, not the branch.
    while steps and _same_control(steps[-1], halted):
        steps.pop()
    if not steps:
        return None, "the repair only did the stopped step itself: nothing else to keep"
    if len(steps) > MAX_BRANCH_STEPS:
        return None, f"the repair took {len(steps)} actions - a task of its own, not a branch"

    apps, hosts = _apps(skill), _hosts(skill)
    halted_param = halted.get("param")
    inputs_low = _low(inputs)
    params, binds, out, renamed = [], {}, [], {}
    for step in steps:
        kind = step.get("kind")
        if kind not in ALLOWED_KINDS:
            return None, f"it {kind.replace('_', ' ')}s, which a branch never does"
        target = step.get("target") or {}
        if kind in NEEDS_CONTROL and not (target.get("role") and (target.get("name") or target.get("path"))):
            return None, "it pressed a spot no control was known by"
        if step.get("app") and apps and step["app"] not in apps:
            return None, f"it worked in {step['app']}, which the person's steps never use"
        if _host(step.get("url")) and hosts and _host(step.get("url")) not in hosts:
            return None, "it worked on a site the person's steps never use"
        if _holds_value(target.get("name"), values):
            return None, f"it pressed a control named for this run's own value ({_norm(target.get('name'))[:60]})"
        each = {k: copy.deepcopy(v) for k, v in step.items() if k not in ("event", "locate", "look", "param")}
        param = by_n.get(step["n"])
        if kind in ("type", "select"):
            if param is None or param.get("secret") or param.get("default") is None:
                return None, "it typed something the recording withheld"
            literal = param["default"]
            how = _bind(literal, values, halted_param)
            if how is None and len(_norm(literal)) >= MIN_LITERAL and _low(literal) in inputs_low:
                return None, (f"it typed {_norm(literal)[:40]!r}, which is in this run's documents but is none of its "
                              "values - kept, it would be typed into the next run's record as it stands")
            name = f"b{len(params) + 1}"
            params.append({"name": name, "label": param.get("label") or name, "default": None if how else literal,
                           "secret": False, "reason": None, "step": step["n"], "constraint": None,
                           "choice": bool(param.get("choice"))})
            if how:
                binds[name] = how
            each["param"] = name
            if kind == "select":
                each.pop("option", None)
        # A result check that names one of the run's values follows the parameter bound to it, or is dropped (a check
        # on another document's text would fail for the right reason or pass for the wrong one).
        if param is not None:
            renamed[param["name"]] = each.get("param")
        checks = []
        for check in each.get("expect") or []:
            if check.get("param"):
                if check["param"] not in renamed:
                    continue  # (a value check on a field no step of the branch types)
                check = dict(check, param=renamed[check["param"]])
            text = " ".join(str(check.get(k) or "") for k in ("name", "value"))
            held = _holds_value(text, values)
            if not held:
                checks.append(check)
                continue
            if check.get("type") not in ("element", "window", "active_window"):
                continue
            field = "name" if check["type"] == "element" else "value"
            for v in held:
                bound = next((b for b, how in binds.items()
                              if _low(_resolve_for_compile(how, values, halted_param)) == _low(v)), None)
                if bound is None:
                    break
                check = dict(check, **{field: _replace_ci(str(check.get(field) or ""), v, "{" + bound + "}")})
            else:
                checks.append(check)
        each["expect"] = checks
        if replay._step_commits(each) or each.get("consequential"):
            each["consequential"] = True
        out.append(each)

    for i, step in enumerate(out, 1):
        old = step["n"]
        step["n"] = i
        step["id"] = f"b{i}"
        for p in params:
            if p["step"] == old:
                p["step"] = i
        step["summary"] = replay._summary(step)
    candidate = {"steps": out, "params": params, "binds": binds,
                 "summary": "; ".join(s["summary"] for s in out)[:600]}
    return candidate, ""


def _resolve_for_compile(how, values, halted_param):
    if how.get("halted"):
        return values.get(halted_param) or ""
    if how.get("item"):
        _, n = _item_of(halted_param)
        return values.get(f"{how['item']}__{n}") or ""
    return values.get(how.get("name")) or ""


def _replace_ci(text, old, new):
    low, at = text.lower(), text.lower().find(str(old).lower())
    return text if at < 0 or not old else text[:at] + new + text[at + len(old):]


# ------------------------------------------------------------
# Keeping and choosing
# ------------------------------------------------------------

def entry(skill, k):
    """The branches kept for shape key `k` on a skill ({"shape", "versions", "compiles"}), or None."""

    return (skill.get("branches") or {}).get(k)


def best(skill, k):
    """The version to try for a stop of shape `k`: the newest promoted, else the newest candidate; None when none."""

    found = entry(skill, k)
    if not found:
        return None
    versions = found.get("versions") or []
    for status in (PROMOTED, CANDIDATE):
        live = [v for v in versions if v.get("status") == status]
        if live:
            return live[-1]
    return None


def add_candidate(skill, found_shape, candidate, task_id=""):
    """Keep a compiled repair as a new candidate version for its shape. Returns the version, or None when the shape
    has had MAX_COMPILES compiled already (the model repairs it each time from then on)."""

    k = key(found_shape)
    branches = skill.setdefault("branches", {})
    held = branches.setdefault(k, {"shape": dict(found_shape), "versions": [], "compiles": 0})
    if held.get("compiles", 0) >= MAX_COMPILES:
        return None
    held["compiles"] = held.get("compiles", 0) + 1
    version = dict(copy.deepcopy(candidate), v=held["compiles"], status=CANDIDATE, made_at=int(time.time()),
                   source=str(task_id or ""), verified_in=[], failed_in=[], uses=0, held=0)
    version["hash"] = hashlib.sha256(repr((candidate.get("steps"), candidate.get("binds"))).encode()).hexdigest()[:12]
    held["versions"] = (held["versions"] + [version])[-MAX_VERSIONS:]
    return version


def _version(skill, k, v):
    found = entry(skill, k) or {}
    return next((x for x in found.get("versions") or [] if x.get("v") == v), None)


def note_trial(skill, k, v, ok, task_id=""):
    """
    Count one trial of version `v` of shape `k`: `ok` when code saw it work (the stopped step went through with its
    result, or the missing result appeared). Returns "promoted", "demoted" or "" (no change). A candidate is promoted
    after PROMOTE_AFTER tasks it worked in (not the one it was compiled in); one failure demotes a candidate, and a
    promoted version is demoted after two failures in a row or when more than half of three or more uses failed.
    """

    version = _version(skill, k, v)
    if version is None:
        return ""
    task = str(task_id or "")
    version["uses"] = version.get("uses", 0) + 1
    if ok:
        version["fails_in_row"] = 0
        if task and task != version.get("source") and task not in version["verified_in"]:
            version["verified_in"].append(task)
        if version["status"] == CANDIDATE and len(version["verified_in"]) >= PROMOTE_AFTER:
            version["status"] = PROMOTED
            version["promoted_at"] = int(time.time())
            return PROMOTED
        return ""
    version["failed_in"] = (version.get("failed_in") or []) + [task]
    version["fails_in_row"] = version.get("fails_in_row", 0) + 1
    failed = len(version["failed_in"])
    if version["status"] == CANDIDATE or version["fails_in_row"] >= 2 or (version["uses"] >= 3 and 2 * failed > version["uses"]):
        if version["status"] != DEMOTED:
            version["status"] = DEMOTED
            version["demoted_at"] = int(time.time())
            return DEMOTED
    return ""


def note_held(skill, k, v):
    """A trial the approval gate stopped before its press: not a failure of the branch, and not a success."""

    version = _version(skill, k, v)
    if version is not None:
        version["held"] = version.get("held", 0) + 1


def program_for(version, values, halted, skill_name=""):
    """
    (program, params) to run version `version` for a stop at `halted` with this run's `values`, or (None, why) when a
    value it is bound to is not this run's (nothing is run with a guess).
    """

    halted_param = halted.get("param")
    params = {}
    for p in version.get("params") or []:
        how = (version.get("binds") or {}).get(p["name"])
        if how is None:
            params[p["name"]] = p.get("default") or ""
            continue
        value = _resolve_for_compile(how, values, halted_param)
        if not _norm(value):
            return None, f"this run has no value for what {p['label']} was bound to"
        params[p["name"]] = str(value)
    tag = hashlib.sha256(f"{skill_name}|{version.get('hash')}".encode()).hexdigest()[:10]
    program = {"schema": "decurix.replay-program/v1", "session": f"branch-{tag}", "title": "a fix learned from a repair",
               "kind": "branch", "steps": copy.deepcopy(version.get("steps") or []),
               "params": [dict(p, default=params[p["name"]]) for p in version.get("params") or []]}
    return program, params


def describe(skill):
    """One line per kept branch, for the report and the skill's health: shape, status, uses."""

    lines = []
    for k, found in sorted((skill.get("branches") or {}).items()):
        top = (found.get("versions") or [{}])[-1]
        sh = found.get("shape") or {}
        lines.append(f"{sh.get('when')} {sh.get('kind')} {sh.get('role')} {sh.get('name')!r}: v{top.get('v')} "
                     f"{top.get('status')}, worked in {len(top.get('verified_in') or [])}, failed in "
                     f"{len(top.get('failed_in') or [])}")
    return lines
