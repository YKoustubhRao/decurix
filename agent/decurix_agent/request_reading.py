"""
A request read by the hosted model: is it a task this desktop was taught, and
with which values?

computer/mimic.py answers that with no model, by comparing the request word by
word with the ticket a task was recorded for. That is all a desktop on a small
local model has, and it is brittle in exactly the way words are: found live
(2026-10-04), `... send akshit gupta "decurix auto say hello"` against a task
taught as `... send sarthak gawade "hello"` was split at the shared word
"hello", so the worker searched for `akshit gupta "decurix auto say` and sent
"hello". A model that reads the sentence does not make that mistake, and it
also recognises the task when it is worded another way ("message Akshit on
LinkedIn saying ...").

So where a hosted model is there, it reads the request: it is shown the tasks
taught here - what each is for, what the person said about it, and each input
with the value the demonstration typed as an example of WHICH thing it is -
and answers which task this is (or none) and what to type for each input.

What the model says is checked before it is used, with no model:
  - the task must be one it was shown;
  - a value must come from the request. One that is nowhere in the request,
    for an input whose demonstrated value was in the ticket's own words, is a
    guess - it is dropped and the person is asked for it;
  - a low-confidence match is no match: the engine does the request.

No model, or a model that fails: None, and the caller falls back to mimic.py.
"""

import json
import re

from decurix_agent import lessons, llm_client, study
from decurix_agent.computer import mimic, skills
from decurix_agent.logging_utils import log

MAX_CANDIDATES = 12
MIN_CONFIDENCE = 0.7
MAX_VALUE_CHARS = 2000

# Words every sentence has: sharing one says nothing about a request being a
# taught task (a lesson's summary is prose, and holds most of them).
COMMON = {
    "is", "are", "was", "be", "it", "its", "this", "that", "these", "those", "to", "of", "in", "on", "at", "by", "with",
    "from", "into", "as", "and", "or", "not", "no", "if", "then", "so", "do", "does", "did", "what", "which", "who",
    "how", "when", "where", "why", "i", "my", "we", "our", "your", "their", "them", "they", "he", "she", "him", "her",
    "one", "all", "any", "some", "there", "here", "have", "has", "had", "will", "should", "must", "up", "out", "new",
}

READ_SYSTEM ="""A person sends a request to a computer worker. The worker was taught some tasks by demonstration: a person did each once on the screen and explained it. You decide whether the request is one of those tasks done again - possibly with other values, possibly worded differently - and you read from the request what to type for each of the task's inputs.

Choosing the task:
- It is a taught task only when it asks for the same thing done in the same app or site, start to finish. Only the inputs' values may differ.
- It is NOT one when it asks for something else, for the same kind of work in a different app or site, or for more than the task does (a second job before or after it). Then answer null: the worker's general engine handles the request, and may still use the taught task as one part.
- If two tasks fit, choose the one whose inputs the request fills best.

Reading the values:
- Give each input exactly the text to type, read from the request. Each input shows the value the demonstration typed: that is an example of WHICH thing the input is (a person's name, a message, a search), never the answer.
- Text the request puts in quotes is one value: take it whole, word for word, without the quotes. Never split it, shorten it, or move part of it into another input.
- Keep the person's own spelling, capitals and punctuation. Do not correct, translate or embellish.
- An input the demonstration typed in another form than its ticket said (a date written as 05/03/2026 for "5 March") takes this request's value in the demonstration's form.
- An input marked "stays the same" keeps its example unless the request clearly gives another value.
- An input the request does not give may come from what the person told the worker before (listed under "The person has said before", when there is any) - only when that plainly says what to use in this case, and then word for word.
- Otherwise an input the request does not give, and that does not stay the same, is null - the person will be asked. Never invent a value and never reuse the example for it.
- An input marked "list" takes every item the request gives, separated by "; ".

Reply with JSON only:
{"task": "<id of the taught task, or null>", "values": {"<input name>": "<value or null>"}, "why": "<one sentence>", "confidence": 0.0-1.0}"""


def available():
    return llm_client.available()


# ============================================================
# What the model is shown
# ============================================================

def _candidates():
    """Each task that could be meant, loaded: ready skills, then recordings nobody taught yet (newest first)."""

    found, taught_from = [], set()

    for candidate in sorted(mimic._candidates(), key=lambda c: (c["kind"] != "skill", -(c["when"] or 0))):
        plan = {"kind": candidate["kind"], "id": candidate["id"], "text": candidate["text"], "spans": []}

        if candidate["kind"] == "recording" and candidate["id"] in taught_from:
            continue   # its skill is already a candidate, and is what a person explained

        try:
            skill, one_off = mimic.load_skill(plan)
        except (FileNotFoundError, ValueError, OSError, KeyError) as exc:
            log(f"request reading: could not load {candidate['kind']} {candidate['id']}: {exc}")
            continue

        if not skill.get("steps"):
            continue
        if not one_off:
            taught_from.update(skill.get("sources") or [])

        found.append({"plan": plan, "skill": skill, "one_off": one_off})
        if len(found) >= MAX_CANDIDATES:
            break

    return found


def _describe(candidate):
    """One taught task as the model reads it."""

    skill, plan = candidate["skill"], candidate["plan"]
    understood = skill.get("understanding") or {}

    lines = [f"Task id: {plan['id']}",
             f"Recorded for the request: {plan['text']}"]
    if skill.get("title") and skill["title"] != plan["text"]:
        lines.append(f"Named: {skill['title']}")
    where = skills.places(skill)
    if where:
        lines.append(f"Works in: {', '.join(where)}")
    if understood.get("summary"):
        lines.append(f"What it does: {understood['summary']}")
    said = skills.explanation(skill)
    if said:
        lines.append(f"The person said: {said[:600]}")
    if candidate["one_off"]:
        lines.append("(Recorded, not yet explained by the person.)")

    inputs = []
    for param in skill.get("params") or []:
        if param.get("file"):
            continue   # an uploaded file is read from the run's attachments (binding.py), not from words
        meaning = (understood.get("inputs") or {}).get(param["name"]) or f"typed into {param.get('label') or 'a field'}"
        example = param.get("example")
        if param.get("list"):
            example = "; ".join(str(x) for x in param.get("examples") or []) or example
        marks = []
        if param.get("list"):
            marks.append("list")
        if not _varies(candidate, param):
            marks.append("stays the same")
        inputs.append(f"  - {param['name']}: {meaning}. The demonstration typed: {json.dumps(example, ensure_ascii=False)}"
                      + (f" [{', '.join(marks)}]" if marks else ""))
    lines.append("Inputs:\n" + "\n".join(inputs) if inputs else "Inputs: none")

    return "\n".join(lines)


# ============================================================
# Checking what the model said
# ============================================================

def _norm(text):
    """Text as it is compared: lower case, quotes and spacing evened out."""

    text = re.sub(r"[\"'“”‘’`]", "", str(text or "").lower())
    return " ".join(text.split())


def _tokens(text):
    return re.findall(r"\w+", _norm(text))


def _grounded(value, request):
    """Whether the request holds the value as whole words, in order: "500" is not in "pay 5000", nor "Ram" in "Ramesh"
    (review 2026-11-03, E-28: a substring passed both)."""

    want, have = _tokens(value), _tokens(request)
    if not want:
        return False
    n = len(want)
    return any(have[i:i + n] == want for i in range(len(have) - n + 1))


def _varies(candidate, param):
    """
    Whether an input changes from run to run: a person said so when they
    taught it, or - for a recording nobody has taught - the ticket itself
    named the value (what a ticket spells out is what the task is about).
    """

    if param.get("varies"):
        return True
    example = param.get("example")
    return bool(candidate["one_off"] and example is not None and _grounded(example, candidate["plan"]["text"]))


def _check_values(candidate, given, request):
    """
    The model's values as a run may use them: (values, dropped). A value is
    kept when the request holds it (as whole words, in order), when it is the demonstration's own (the
    input stays the same), or when the demonstration typed that input in
    another form than its ticket said (so this one may be adapted too).
    Anything else is a guess, and is dropped: the person is asked.
    """

    skill, ticket = candidate["skill"], candidate["plan"]["text"]
    values, dropped = {}, []

    for param in skill.get("params") or []:
        name = param["name"]
        value = (given or {}).get(name)
        example = param.get("example")

        if param.get("file"):
            continue

        varies = _varies(candidate, param)
        if varies and not param.get("varies"):
            # Only ever a skill built for this run from a recording: a value
            # its ticket named must be given, so the run asks when it is not.
            param["varies"] = True

        if value is None or not str(value).strip():
            if not varies and example is not None and not param.get("list"):
                values[name] = str(example)
            continue

        value = str(value).strip()
        if len(value) > MAX_VALUE_CHARS:
            dropped.append(name)
            continue

        items = skills.split_items(value) if param.get("list") else [value]
        same = example is not None and _norm(value) == _norm(example)
        adapted_form = example is not None and not _grounded(example, ticket)

        if all(_grounded(item, request) for item in items):
            values[name] = value
        elif same and not varies:
            values[name] = str(example)
        elif adapted_form and not same:
            values[name] = value
        else:
            dropped.append(name)

    return values, dropped


# ============================================================
# Reading
# ============================================================

def worth_reading(request):
    """
    Whether to put a request to the model at all: there is a taught task, and
    the request shares a word with one (its ticket, name, summary or where it
    works). Cheap - it decides who the watcher hands a request to.
    """

    text = (request or "").strip()
    if not text or text.startswith("/") or not available():
        return False

    mine = {w for w, _, _ in mimic._words(text)} - mimic.FILLER - COMMON
    if not mine:
        return False

    try:
        for candidate in mimic._candidates():
            about = candidate["text"]
            if candidate["kind"] == "skill":
                skill = skills.load(candidate["id"])
                about += " " + " ".join([skill.get("title") or "", (skill.get("understanding") or {}).get("summary") or "",
                                         skills.explanation(skill), " ".join(skills.places(skill))])
            if mine & ({w for w, _, _ in mimic._words(about)} - mimic.FILLER - COMMON):
                return True
    except Exception as exc:
        log(f"request reading: could not look through the taught tasks: {exc}")

    return False


class Unavailable(Exception):
    """The model service did not answer the reading of a request (read)."""


def read(request):
    """
    The request as the model reads it against what was taught here:

      {"plan": {...}, "skill": ..., "one_off": ..., "values": {...}, "dropped": [...], "why": "..."}
          it is that task, with these values;
      {"plan": None, "why": "..."}
          it is none of them - the engine does it;
      None
          no model, nothing taught, or the model could not be asked - the
          caller decides without it.
    """

    if not available():
        return None

    candidates = _candidates()
    if not candidates:
        return None

    # What the person answered in earlier tasks (lessons.py): a standing
    # instruction there ("when I give no subject, use ...") is theirs too.
    said_before = lessons.known()

    content = ("The tasks taught on this desktop:\n\n"
               + "\n\n".join(_describe(c) for c in candidates)
               + (f"\n\nThe person has said before:\n{said_before}" if said_before else "")
               + f"\n\nThe request:\n{request}")

    try:
        data = study._call(READ_SYSTEM, content, max_tokens=800)
    except llm_client.LLMError as exc:
        # The model service did not answer (after llm_client's own retries): the engine that would do the task needs
        # it as much, so the request is not handed on as one nobody taught. Found live (2026-11-08): a 502 here sent a
        # taught bill to the generic operator, whose every turn then failed the same way.
        log(f"request reading: the model could not be asked ({exc})")
        raise Unavailable(str(exc)) from exc
    except ValueError as exc:
        log(f"request reading: the model's answer could not be read ({exc})")
        return None

    why = " ".join(str(data.get("why") or "").split())[:300]
    chosen = data.get("task")
    try:
        confidence = float(data.get("confidence") or 0)
    except (TypeError, ValueError):
        confidence = 0.0

    candidate = next((c for c in candidates if c["plan"]["id"] == chosen), None)
    if candidate is None or confidence < MIN_CONFIDENCE:
        log(f"request reading: {request!r} is not a taught task ({chosen!r}, confidence {confidence:.2f}): {why}")
        return {"plan": None, "why": why}

    # Grounded in this request only: what the person said in earlier tasks is context for the model, never a source of
    # this run's values - those lessons may be another client's (review 2026-11-03, E-28).
    values, dropped = _check_values(candidate, data.get("values") if isinstance(data.get("values"), dict) else {},
                                    request)
    log(f"request reading: {request!r} is {candidate['plan']['kind']} {chosen} with {values}"
        + (f"; not taken from the model: {dropped}" if dropped else "") + f" ({why})")

    return {"plan": candidate["plan"], "skill": candidate["skill"], "one_off": candidate["one_off"],
            "values": values, "dropped": dropped, "why": why}
