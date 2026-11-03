"""
decurix.trajectory/v1: one run's record, as the control plane builds it (internal/runs, docs/trajectory.md).

A run is one execution of a task on a worker. The control plane joins the task records, the judgements, the model
proxy's calls and the bundle pulled off the desktop into one document; a desktop never writes it. This module is the
document's Python types and a validator for anything that reads one (an eval report, a training-set export, a test).

validate(doc) lists what is wrong ([] when nothing is). It checks the required keys and the enums; unknown keys are
kept and pass, so a v1 reader reads a v1.1 document. The nested verdict is checked by decurix_agent.verdict when that
module is there, else by its own enums.
"""

from typing import Any, List, Optional, TypedDict

SCHEMA = "decurix.trajectory/v1"

# The enums of the document (docs/trajectory.md).
OUTCOMES = ("done_checked", "done_unchecked", "needs_you", "stopped", "failed", "running", "queued", "")
# A step's label is its own outcome; the run's verdict is labels.run, never copied onto steps.
LABELS = ("ok", "failed", "refused", "corrected", "overridden", "unlabelled")
RUN_LABELS = ("done", "not_done", "unchecked", "stopped", "")
GATE_DECISIONS = ("not_needed", "asked", "granted", "denied", "refused")
ORIGIN_KINDS = ("person", "concierge", "schedule", "mail", "dag", "queue", "handoff", "recording", "practice", "eval",
                "unknown")
ACTOR_KINDS = ("person", "worker", "scripted", "program")
CALL_SOURCES = ("proxy", "soul_trace")
GUARDS = ("allowed", "asked", "refused")

# The verdict's own enums, for when decurix_agent.verdict is not there (PLAN-P2 §5.2).
_VERDICTS = ("done", "not_done", "unchecked", "stopped")


class Actor(TypedDict, total=False):
    id: str
    name: str
    roles: List[str]
    kind: str


class Call(TypedDict, total=False):
    i: int
    id: str
    at: str
    source: str
    provider: str
    deployment: str
    purpose: str
    turn: str
    caller: str
    status: int
    ms: int
    prompt_sha256: str
    prefix_sha256: str
    response_sha256: str
    messages: int
    usage: dict
    problems: List[str]


class Step(TypedDict, total=False):
    i: int
    at: str
    turn: str
    call: Optional[int]
    model: Optional[dict]
    prompt_sha256: str
    prefix_sha256: str
    observation: Optional[dict]
    command: Optional[dict]
    action: Optional[dict]
    gate: dict
    diff: Optional[dict]
    ledger_delta: List[dict]
    label: str


class Answer(TypedDict, total=False):
    judgement_id: str
    at: str
    text: str
    actor: Optional[Actor]


class Question(TypedDict, total=False):
    intervention_id: str
    kind: str
    asked_at: str
    text: str
    secret: bool
    answer: Optional[Answer]


class Judgement(TypedDict, total=False):
    id: str
    kind: str
    actor: Actor
    at: str
    derived_from: str
    payload: Any


class Trajectory(TypedDict, total=False):
    schema: str
    run_id: str
    task_id: str
    attempt: int
    worker: dict
    origin: dict
    skill: dict
    request: dict
    started_at: str
    ended_at: str
    status: str
    outcome: str
    verdict: Optional[dict]
    models: List[dict]
    usage: dict
    steps: List[Step]
    calls: List[Call]
    questions: List[Question]
    judgements: List[Judgement]
    ledger: List[dict]
    labels: dict
    integrity: dict


# The keys every document has, with the type each holds.
_REQUIRED = {
    "schema": str, "run_id": str, "task_id": str, "attempt": int, "worker": dict, "origin": dict, "skill": dict,
    "request": dict, "started_at": str, "ended_at": str, "status": str, "outcome": str, "models": list,
    "usage": dict, "steps": list, "calls": list, "questions": list, "judgements": list, "ledger": list,
    "labels": dict, "integrity": dict,
}

_STEP_REQUIRED = {"i": int, "at": str, "turn": str, "gate": dict, "ledger_delta": list, "label": str}


def _verdict_problems(v):
    """What is wrong with the nested verdict: decurix_agent.verdict's validate when it is there (imported only here)."""

    try:
        from decurix_agent import verdict as verdict_module  # lazy: the PY lane's module
    except ImportError:
        verdict_module = None
    if verdict_module is not None and hasattr(verdict_module, "validate"):
        return list(verdict_module.validate(v))
    if not isinstance(v, dict):
        return ["a verdict is a JSON object"]
    if v.get("verdict") not in _VERDICTS:
        return [f"verdict {v.get('verdict')!r} is not one of {', '.join(_VERDICTS)}"]
    return []


def _is(value, kind):
    # bool is an int in Python; a count is never a bool.
    if kind is int:
        return isinstance(value, int) and not isinstance(value, bool)
    return isinstance(value, kind)


def _actor_problems(actor, where):
    if not isinstance(actor, dict):
        return [f"{where}: actor is not an object"]
    out = []
    if not isinstance(actor.get("id"), str) or not actor.get("id"):
        out.append(f"{where}: actor has no id")
    if actor.get("kind") not in ACTOR_KINDS:
        out.append(f"{where}: actor kind {actor.get('kind')!r} is not one of {', '.join(ACTOR_KINDS)}")
    if not isinstance(actor.get("roles", []), list):
        out.append(f"{where}: actor roles is not a list")
    return out


def validate(doc):
    """What is wrong with a trajectory document, as lines; [] when it is valid. Unknown keys pass."""

    if not isinstance(doc, dict):
        return ["a trajectory is a JSON object"]
    out = []
    for key, kind in _REQUIRED.items():
        if key not in doc:
            out.append(f"{key} is missing")
        elif not _is(doc[key], kind):
            out.append(f"{key} is not a {kind.__name__}")
    if "verdict" not in doc:
        out.append("verdict is missing (null for a run from before verdicts)")
    if out:
        return out

    if doc["schema"] != SCHEMA:
        out.append(f"schema is not {SCHEMA}")
    if not doc["run_id"]:
        out.append("run_id is empty")
    if doc["outcome"] not in OUTCOMES:
        out.append(f"outcome {doc['outcome']!r} is not one of {', '.join(o for o in OUTCOMES if o)}")
    if doc["verdict"] is not None:
        out += [f"verdict: {p}" for p in _verdict_problems(doc["verdict"])]

    origin = doc["origin"]
    if origin.get("kind") not in ORIGIN_KINDS:
        out.append(f"origin kind {origin.get('kind')!r} is not one of {', '.join(ORIGIN_KINDS)}")
    if origin.get("actor") is not None:
        out += _actor_problems(origin["actor"], "origin")

    calls = doc["calls"]
    for n, c in enumerate(calls):
        if not isinstance(c, dict):
            out.append(f"call {n} is not an object")
            continue
        if c.get("source") not in CALL_SOURCES:
            out.append(f"call {n}: source {c.get('source')!r} is not one of {', '.join(CALL_SOURCES)}")
        if not isinstance(c.get("purpose", ""), str):
            out.append(f"call {n}: purpose is not a string")

    for n, s in enumerate(doc["steps"]):
        if not isinstance(s, dict):
            out.append(f"step {n} is not an object")
            continue
        for key, kind in _STEP_REQUIRED.items():
            if key not in s:
                out.append(f"step {n}: {key} is missing")
            elif not _is(s[key], kind):
                out.append(f"step {n}: {key} is not a {kind.__name__}")
        if s.get("label") not in LABELS:
            out.append(f"step {n}: label {s.get('label')!r} is not one of {', '.join(LABELS)}")
        gate = s.get("gate") if isinstance(s.get("gate"), dict) else {}
        if gate.get("decision") not in GATE_DECISIONS:
            out.append(f"step {n}: gate decision {gate.get('decision')!r} is not one of {', '.join(GATE_DECISIONS)}")
        call = s.get("call")
        if call is not None and (not _is(call, int) or not 0 <= call < len(calls)):
            out.append(f"step {n}: call {call!r} does not point into calls")
        command = s.get("command")
        if command is not None:
            if not isinstance(command, dict):
                out.append(f"step {n}: command is not an object")
            elif command.get("guard") not in GUARDS:
                out.append(f"step {n}: command guard {command.get('guard')!r} is not one of {', '.join(GUARDS)}")
        if s.get("action") is None and command is None:
            out.append(f"step {n}: neither an action nor a command")

    for n, q in enumerate(doc["questions"]):
        answer = q.get("answer") if isinstance(q, dict) else None
        if isinstance(answer, dict) and answer.get("actor") is not None:
            out += _actor_problems(answer["actor"], f"question {n}")
    for n, j in enumerate(doc["judgements"]):
        if not isinstance(j, dict) or not j.get("id"):
            out.append(f"judgement {n} has no id")
            continue
        out += _actor_problems(j.get("actor"), f"judgement {n}")

    run_label = doc["labels"].get("run", "")
    if run_label not in RUN_LABELS:
        out.append(f"labels run {run_label!r} is not one of {', '.join(r for r in RUN_LABELS if r)}")

    integrity = doc["integrity"]
    for key, kind in (("missing", list), ("worker_written", list), ("parts", dict)):
        if not isinstance(integrity.get(key), kind):
            out.append(f"integrity: {key} is not a {kind.__name__}")
    return out
