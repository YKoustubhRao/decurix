"""Stack-based atomizer flavour, shared by both pipelines: instead of
deciding one atomic instruction live per step (the existing "live" flavour
-- atomizer.py's next_step / fast_step.py's decide / ext_codegen.py's
decide_and_act, all called fresh every step with no upfront plan), this runs
ONCE at the start of a task and decides the FULL ordered list of atomic
sub-instructions up front, held as a stack.

Each pipeline's own per-step call (fast_step.decide_stack_step /
ext_codegen.decide_stack_step) still grounds the CURRENT top-of-stack
instruction in the live page -- a plan can't know the live DOM -- but no
longer has to decide WHAT the next instruction is, cutting the planning call
down to one per task instead of one per step. It can still react to
something the plan couldn't have known about (a cookie banner, an
unexpected redirect, a login wall) via push_before: a new instruction
inserted ahead of the current one, addressed by THIS turn's action, leaving
the original instruction on the stack to retry next turn.

Supersedes legacy/planner.py's upfront-splitting idea (page-agnostic, no
reactive element, and never actually wired into the live pipeline) by
keeping the reactive capability the live atomizer already has, while
cutting per-step planning calls to zero for the common case.
"""
import json

import models

PLAN_SCHEMA = {
    "type": "object",
    "properties": {
        "steps": {
            "type": "array",
            "items": {"type": "string"},
        },
    },
    "required": ["steps"],
}

PLAN_SYSTEM = """You are the upfront planner for a browser automation agent. Given the full task \
the user asked for, break it down into an ORDERED list of atomic, single-action steps -- each one \
achievable as ONE browser action (a single navigate, click, type, search, or similar) -- in the \
exact order they need to happen.

Rules:
- Each step should be small enough to execute as one action. Don't combine "search for X" and \
"click the first result" into one step -- those are two steps.
- A search box's fill-and-submit is exactly ONE action, never two: write it as a single step \
("Search for X"), never as separate "Type X into the search box" + "Submit the search" steps -- \
the execution stage has a dedicated one-shot search action for this, and splitting it in two \
tends to fail (the second step goes looking for a submit button that often isn't visibly \
clickable right after typing, when pressing Enter in the already-focused field was all that was \
needed).
- Preserve the task's own order and intent; don't add steps nobody asked for, and don't skip \
steps that are implied but not explicit.
- If a step is genuinely a question or extraction ("what's the price", "download all PDFs"), keep \
it as one step -- the step-execution stage decides the details once it's actually on the page.
- You do NOT know what the page will actually look like -- don't invent specifics you can't know \
yet (a selector, an exact URL beyond what the task itself specifies). Just describe WHAT needs to \
happen, in order; grounding it in the real page happens later, one step at a time.
- Output between 1 and 20 steps. If the task is already a single atomic action, output just that \
one step."""


def plan(model_key, full_task, max_tokens=500):
    """Returns a list of instruction strings, in execution order (steps[0]
    executes first). Raises ValueError if the model's response can't be
    parsed into a non-empty list -- same hard-fail-let-the-caller-pause
    contract as the other decide calls."""
    messages = [
        {"role": "system", "content": PLAN_SYSTEM},
        {"role": "user", "content": f"Full task: {full_task}"},
    ]
    raw = models.chat(model_key, messages, max_tokens=max_tokens, response_format=PLAN_SCHEMA)
    try:
        parsed = json.loads(raw)
    except (json.JSONDecodeError, TypeError):
        raise ValueError(f"planner returned unparseable output: {raw!r}")
    steps = [s.strip() for s in (parsed.get("steps") or []) if s and s.strip()]
    if not steps:
        raise ValueError(f"planner returned no steps: {raw!r}")
    return steps
