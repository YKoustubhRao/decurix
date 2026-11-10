"""
How bench/skills/e2e.py answers the teaching questions, as the person who demonstrated the task would.

A question is answered by its structure - the kind the lesson gave it (computer/learning.py), and for a question the
study raised (study.py), the topic of the workflow brief it is about - never by matching its words, which change with
the brain and the wording of the day. What the model understood is accepted as it stands. A kind or a topic this table
does not know fails the bench loudly instead of guessing.

The structure is read on the desktop by PROBE: the lesson's draft (saved before every question, skill_task._ask) and
its next question, which is the one just asked.
"""

import json

# Run on the desktop (PYTHONPATH=/opt): the draft being taught and the question it is waiting on.
PROBE = (
    "import glob, json, os\n"
    "from decurix_agent.computer import learning\n"
    "paths = glob.glob(os.path.expanduser('~/.decurix/skills/*.json'))\n"
    "skill = json.load(open(max(paths, key=os.path.getmtime), encoding='utf-8'))\n"
    "q = learning.next_question(skill) or {}\n"
    "u = skill.get('understanding') or {}\n"
    "g = (skill.get('gates_pending') or {}).get('unmapped') or []\n"
    "print(json.dumps({'question': dict({k: q.get(k) for k in ('id', 'kind', 'text', 'step')},\n"
    "                                    values=bool(q.get('values')), steps=bool(q.get('steps'))),\n"
    "                  'study': [{'id': x.get('id'), 'topic': x.get('topic')} for x in u.get('questions') or []],\n"
    "                  'gates_unmapped': len(g),\n"
    "                  'understood': bool(u.get('summary'))}))\n"
)

# What the person knows about the demonstrated task (a synthetic local form that saves a customer's name), one
# answer per topic of the study's brief (study.BRIEF_TOPICS).
FACTS = {
    "sources": "The customer's name comes from the request; nothing else is read.",
    "changes": "Nothing is revised or cancelled here: each request is one new customer.",
    "filing": "No files are kept; the form saves on the page.",
    "matching": "There is no lookup: every run adds the name it is given.",
    "fields": "Type the customer name exactly as the request gives it.",
    "prechecks": "Nothing to check before starting: the form is empty each time.",
    "approvals": "Saving this local form needs no OK.",
    "done": "Done when the page shows a line beginning Saved with the name.",
    "exceptions": "If no name is given, ask for it.",
}


# The person's answer to the worker's plan of the work (soul's plan.go puts it to them before its first change): the
# plan is agreed as stated, with what the person knows about where the data comes from and what to look up - the same
# facts the study's questions get. Recognised by the plan's own marker (PLAN_MARKER_PROBE), never by its words.
PLAN_ANSWER = "Yes, do it that way. " + FACTS["sources"] + " " + FACTS["matching"]

# Run on the desktop (PYTHONPATH=/opt): the marker every plan question starts with, the agent's own constant.
PLAN_MARKER_PROBE = "from decurix_agent.thebot_harness import PLAN_QUESTION; print(PLAN_QUESTION)"


def is_plan(reply, marker):
    """Whether a chat reply is the worker's plan of the work: a question that starts with the plan's marker."""

    return bool(marker) and reply.get("kind") == "query" and (reply.get("reply") or "").lstrip().startswith(marker)


class UnknownQuestion(SystemExit):
    """A question whose structure this bench has no answer for: the bench stops and says so."""


def answer(probe, asked_text=""):
    """The person's answer to the question the probe describes; asked_text is what the chat showed, which must hold
    the question's own text (else the probe read another question than the one asked)."""

    q = probe.get("question") or {}
    kind = q.get("kind")
    own = " ".join(str(q.get("text") or "").split())[:80]
    if asked_text and own and own not in " ".join(asked_text.split()):
        raise UnknownQuestion(f"the draft's next question is not the one asked:\n  draft: {own}\n  asked: {asked_text[:200]}")
    if kind == "goal":
        # With a model's understanding to check, it is accepted as stated; without one, the goal is asked outright.
        return "yes" if probe.get("understood") else "the page shows a line beginning Saved"
    if kind == "study":
        qid = str(q.get("id") or "").split(":", 1)[-1]
        topic = next((s.get("topic") for s in probe.get("study") or [] if s.get("id") == qid), None)
        if topic not in FACTS:
            raise UnknownQuestion(f"a study question about a topic this bench does not know ({topic!r}):\n{q.get('text')}")
        return FACTS[topic]
    if kind == "start":
        return "yes"
    if kind == "variable":
        return "all change" if q.get("values") else "changes"
    if kind == "optional":
        return "always"
    if kind == "approval":
        # The fixture only saves a local synthetic form.
        return "go ahead"
    if kind == "gates":
        # Which steps stop for an OK, read from an answer above and put back (learning._gates): the steps as put back
        # are right; a name the lesson found no step for is left to the gate.
        return "none" if probe.get("gates_unmapped") else "yes"
    if kind == "confirm":
        return "yes"
    raise UnknownQuestion(f"a question of a kind this bench does not know ({kind!r}):\n{json.dumps(q)[:400]}")
