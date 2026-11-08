"""
Methods: what a worker keeps of how a kind of task is done (docs/learning-loop.md, phase C).

A playbook keeps the way through a task nobody had to steer. A method is what a steered one teaches: the plan of the
work soul wrote before it first changed anything - the records it covers, the places facts about them are kept, what it
writes and what it commits - with what the person said about that plan. It is kept once the run's result is checked
done, and offered to the next request of the same kind (playbooks.shape) at its start: the worker reads those places
first, and the plan is not put to the person again (soul's plan.go looks for MARKER).

Values never enter a method. It is the plan's own words with every number taken out: a serial range or a month of
one run is that run's, the kinds of records, the places and the actions are the method.
"""

import json
import os
import re
import threading
import time
from pathlib import Path

from decurix_agent import config, playbooks
from decurix_agent.logging_utils import log

ROOT = Path(os.environ.get("DECURIX_METHODS_DIR") or (config.DECURIX_DIR / "methods"))
# soul's methodMarker (plan.go): a method in the goal's context means the plan is not asked again.
MARKER = "A METHOD FOR THIS KIND OF TASK"
MAX_METHODS = 200
# A number of two digits or more is one run's value (a serial, an amount, a year), never the method's.
_VALUE = re.compile(r"\d[\d,./-]*\d")
# What one method keeps of the person's words about its plan, newest last.
MAX_SAID = 8
# How much of the question a kept answer answered is kept with it.
MAX_ASKED = 600

_lock = threading.Lock()
_run = {"plan": None}


def _path(name):
    return ROOT / f"{name}.json"


def _all():
    try:
        paths = sorted(ROOT.glob("*.json"))
    except OSError:
        return []
    out = []
    for p in paths:
        try:
            with open(p, encoding="utf-8") as f:
                method = json.load(f)
        except (OSError, ValueError):
            continue
        if isinstance(method, dict) and isinstance(method.get("plan"), dict):
            out.append(method)
    return out


def _save(method):
    ROOT.mkdir(parents=True, exist_ok=True)
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(_path(method["name"]), method, atomic=True, json_kw={"indent": 1, "ensure_ascii": False})


def _without_values(text):
    """The text with every number of two digits or more taken out (one run's values)."""

    return " ".join(_VALUE.sub("…", str(text or "")).split())


def find(request):
    """The method for this kind of request, or None (the same matching as a playbook's)."""

    mine = playbooks.shape(request)
    best, score = None, 0.0
    for method in _all():
        alike = playbooks._alike(mine, method.get("shape") or [])
        if alike > score:
            best, score = method, alike
    return best if score >= playbooks.MATCH else None


def begin():
    """A new task: nothing of the last one's plan carries over."""

    with _lock:
        _run["plan"] = None


def note_job(job):
    """Take the plan of the work out of a finished engine job (soul's operator reports it as a result's `plan`)."""

    for result in (job or {}).get("results") or []:
        plan = (result.get("Meta") or {}).get("plan")
        if isinstance(plan, dict) and plan.get("scope"):
            with _lock:
                _run["plan"] = plan


def keep(request):
    """
    The task's result was checked done: keep its plan as the method for this kind of request - a new one, or the one
    kept for the kind brought up to date (the newer plan; the person's words added). Returns the method's name, or None
    when the task wrote no plan that writes or commits anything.
    """

    with _lock:
        plan = _run["plan"]
    if not plan or not (plan.get("commits") or plan.get("outputs")):
        return None
    clean = {
        "scope": _without_values(plan.get("scope")),
        "sources": [_without_values(s) for s in plan.get("sources") or [] if str(s).strip()],
        "outputs": [_without_values(s) for s in plan.get("outputs") or [] if str(s).strip()],
        "commits": [_without_values(s) for s in plan.get("commits") or [] if str(s).strip()],
    }
    # The person's answer is kept with the question it answered (the plan as it was put to them): found live
    # (2026-11-08), an answer kept alone - "I don't know that, so I can't give it to you ..." to the plan's question -
    # reached the next task's worker and its review as the person's standing word about it, and the review called a
    # value the person did give in that task made up.
    # And every other answer of the run, each with its question: found live (2026-11-08), the plan's question got "I
    # don't know", the layout came in answer to a later question, and the method kept only the first - with the plan's
    # first idea of the output - so the next task of the kind wrote the wrong file and asked nothing.
    said = []
    first = _without_values(plan.get("personSaid"))
    if first:
        said.append({"asked": _without_values(plan.get("ask"))[:MAX_ASKED], "said": first})
    try:
        from decurix_agent import guidance
        exchanges = guidance.exchanges_this_task()
    except Exception:  # noqa: BLE001 - what cannot be read is not kept
        exchanges = []
    for asked, reply in exchanges:
        entry = {"asked": _without_values(asked)[:MAX_ASKED], "said": _without_values(reply)}
        if entry["said"] and all(entry["said"] != s["said"] for s in said):
            said.append(entry)
    method = find(request)
    if method is None:
        if len(_all()) >= MAX_METHODS:
            return None
        shape = playbooks.shape(request)
        base = "-".join(shape[:6])[:60].strip("-") or "task"
        name, n = base, 2
        while _path(name).exists():
            name, n = f"{base}-{n}", n + 1
        method = {"name": name, "shape": shape, "said": [], "runs": 0}
    method["plan"] = clean
    for entry in said:
        if entry not in method.get("said", []):
            method["said"] = (method.get("said", []) + [entry])[-MAX_SAID:]
    method["runs"] = int(method.get("runs") or 0) + 1
    method["kept_at"] = time.time()
    try:
        _save(method)
    except OSError as exc:
        log(f"Methods: could not keep the method for this kind of task: {exc}")
        return None
    log(f"Methods: kept {method['name']} ({method['runs']} checked run(s))")
    return method["name"]


def context(request):
    """What the engine is told at the start of a request of a kind it has a method for, or ""."""

    method = find(request)
    if method is None:
        return ""
    plan = method["plan"]
    lines = [f"{MARKER} (learned from {method.get('runs', 1)} earlier checked run(s) of a request like this one; it "
             "names places and steps, never values - what you read now decides):"]
    lines.append(f"- It covers: {plan.get('scope')}")
    if plan.get("sources"):
        lines.append("- Read first, before you enter anything: " + "; ".join(plan["sources"]))
    if plan.get("outputs"):
        lines.append("- It writes: " + "; ".join(plan["outputs"]))
    if plan.get("commits"):
        lines.append("- Then, each with the person's OK: " + "; then ".join(plan["commits"]))
    if any(isinstance(s, dict) for s in method.get("said") or []):
        lines.append("- What the person answered on those runs, each to its own question (where an answer differs from "
                     "the plan above - what it writes, how it is laid out - the answer holds):")
    for said in method.get("said") or []:
        if isinstance(said, dict):
            # Asked then, about that run: their answer is to that question, not about this request's values.
            asked = said.get("asked") or "the plan of the work"
            lines.append(f'- Asked on an earlier run: "{asked}" - the person answered: "{said.get("said")}"')
        else:
            lines.append(f'- The person said about this plan: "{said}"')
    return "\n".join(lines)
