"""
A form field's unit, as the person answered it: asked once for that field of that application, then kept.

A field whose label states no unit (an application's tax table column "Rate", with no "%") cannot be compared with a
figure written with one ("18%" against the "18" it holds): values.compare answers None ("unit"), and the save waits on
the person (computer/records.presave). Before this module that wait came back on every task, once per task. Now the
save puts one numbered question of its own to the person, naming the field, the application and the unit:

    Before I save: the field "Rate" on erp.example/app/sales-taxes-and-charges-template holds 18 and the record says
    18%, and the field's label does not say its unit. Reply 1 if the field "Rate" on erp.example/app/sales-taxes-
    and-charges-template takes its figures in %, or 2 if it does not.

Like the date-order question (task_context.ask_date_order) it goes out alone, in place of the engine's next question
(messaging.ask_for_query), and only the reply to its own query, whose whole text is one option's number, settles it
(watchers.answer_waiting). What backs the answer is never this module's own note of it: it is the person's judgement
as the control plane recorded it (person_record), whose question (the intervention's request) holds the sentence
"Reply 1 if the field ... takes its figures in ...". In the task that asked, that judgement is read back on the task
(person_record.judgements); for every later task it is a learned row of kind "field_unit" derived from it
(person_record.field_units), which the control plane keeps only when the same sentence stands in the question the
person answered and their reply is the option the row says (internal/httpapi checkFieldUnit). A control plane that
cannot be read backs nothing: the save waits and the person is asked (fail closed).

The field is named by its structural identity: the application (the page's host), the form (the page's path without
its last part when it has more than one - a collection's page, not one record's name or number) and the field's label
words (computer/records.label_words), all equal. No host (an application on the screen that is not a web page) is
answered for the task alone, never kept for later. Nothing here knows any application, field or unit.
"""

import json
import re

from decurix_agent import task_context

QUESTION = "unit_question"
ANSWERED = "unit_answered"
OPTIONS = {"1": True, "2": False}
MAX_LABEL = 120
MAX_UNIT = 16
NO_HOST = "the application on the screen"


# ------------------------------------------------------------------ the field

def place_of(url):
    """(host, form) of a page's address: its host (lower case) and its path without its last part when the path has
    more than one ("/app/tax-template/new-1" is the form "/app/tax-template"). ("", "") for none."""

    import urllib.parse
    try:
        parts = urllib.parse.urlsplit(str(url or "").strip())
    except ValueError:
        return "", ""
    host = (parts.netloc or "").lower()
    steps = [p for p in (parts.path or "").split("/") if p]
    if len(steps) > 1:
        steps = steps[:-1]
    return host, ("/" + "/".join(steps)) if steps and host else ""


def _label(label):
    return " ".join(str(label or "").split())[:MAX_LABEL]


def _unit(unit):
    unit = " ".join(str(unit or "").split())
    return unit if unit and len(unit) <= MAX_UNIT and not re.search(r"\s", unit) else ""


def sentence(label, host, form, unit):
    """The sentence whose "1" or "2" is the person's answer: the control plane checks the same words
    (internal/httpapi fieldUnitSentence) before it keeps the answer."""

    place = f"{host}{form}" if host else NO_HOST
    return f'Reply 1 if the field "{_label(label)}" on {place} takes its figures in {_unit(unit)}, or 2 if it does not.'


def _same_field(a_label, b_label):
    from decurix_agent.computer import records
    words = records.label_words(a_label)
    return bool(words) and words == records.label_words(b_label)


# ------------------------------------------------------------------ the question

def ask(task_id, label, host, form, unit, shown, wanted):
    """Keep the question "does this field take its figures in <unit>" for the task (a save waits on it). None when
    there is nothing to ask (no label, no unit, no task). The same field's question is kept, never asked twice."""

    label, unit = _label(label), _unit(unit)
    if not label or not unit or not task_context.valid(task_id) or asked_already(task_id, label, host, form, unit):
        return None
    question = {"label": label, "host": str(host or ""), "form": str(form or "") if host else "", "unit": unit,
                "shown": " ".join(str(shown).split())[:40], "wanted": " ".join(str(wanted).split())[:40]}
    found = task_context._context(task_id).get(QUESTION)
    if isinstance(found, dict) and all(found.get(k) == question[k] for k in ("label", "host", "form", "unit")):
        return found
    task_context._keep_context(task_id, **{QUESTION: question})
    return question


def question(task_id=None):
    """The unit question a save of the task waits on, or None."""

    task_id = task_id or task_context.current()
    found = task_context._context(task_id).get(QUESTION) if task_id else None
    return found if isinstance(found, dict) and found.get("label") and found.get("unit") else None


def question_text(q):
    """The question as the person reads it."""

    place = f"{q['host']}{q.get('form') or ''}" if q.get("host") else NO_HOST
    return (f"Before I save: the field \"{q['label']}\" on {place} holds {q.get('shown', '')} and the record says "
            f"{q.get('wanted', '')}, and the field's label does not say its unit. "
            + sentence(q["label"], q.get("host", ""), q.get("form", ""), q["unit"]))


def mark_asked(task_id, query_id):
    """The unit question went out as the query `query_id`: only its reply answers it."""

    q = question(task_id)
    if q and isinstance(query_id, str) and query_id:
        task_context._keep_context(task_id, **{QUESTION: dict(q, query_id=query_id)})


def unmark_asked(task_id):
    """The reply picked neither option: the question is still open, and goes out again when next asked."""

    q = question(task_id)
    if q:
        task_context._keep_context(task_id, **{QUESTION: {k: v for k, v in q.items() if k != "query_id"}})


def query_answered(task_id, query_id):
    """Whether `query_id` - the query a reply answers - is the one the unit question went out as."""

    q = question(task_id)
    return bool(q and q.get("query_id") and query_id and q["query_id"] == query_id)


def take_answer(task_id, reply, query_id=None):
    """
    The person's reply to the unit question, when it is the reply to that question's own query and its whole text is
    one option's number: True ("1", the field takes that unit) or False ("2"), and the question is settled. Anything
    else is None (the question stays). What backs the answer is the control plane's record of the reply, read back by
    `answered` - never this.
    """

    q = question(task_id)
    if not q or not query_answered(task_id, query_id):
        return None
    choice = " ".join(str(reply or "").split()).strip(" .")
    if choice not in OPTIONS:
        return None
    done = [d for d in _answered_here(task_id) if not _same_question(d, q)] + [
        {k: q[k] for k in ("label", "host", "form", "unit")}]
    task_context._keep_context(task_id, **{QUESTION: None, ANSWERED: done[-20:]})
    return OPTIONS[choice]


def _same_question(a, b):
    return all(a.get(k) == b.get(k) for k in ("label", "host", "form", "unit"))


def _answered_here(task_id):
    """The fields whose unit question this task's person answered (a note of which were asked, never what backs the
    answer: that is the control plane's record of it)."""

    found = task_context._context(task_id).get(ANSWERED) if task_id else None
    return [d for d in found if isinstance(d, dict)] if isinstance(found, list) else []


def asked_already(task_id, label, host, form, unit):
    """Whether the person answered this field's unit question on this task already (so it is not put to them again,
    even when the control plane cannot be read now to confirm what they said)."""

    want = {"label": _label(label), "host": str(host or ""), "form": str(form or "") if host else "", "unit": _unit(unit)}
    return any(_same_question(d, want) for d in _answered_here(task_id))


# ------------------------------------------------------------------ the person's recorded word

def _option(text):
    return OPTIONS.get(" ".join(str(text or "").split()).strip(" ."))


def _asked_holds(asked, label, host, form, unit):
    return sentence(label, host, form, unit) in " ".join(str(asked or "").split())


def answered(label, host, form, unit, task_id=None):
    """
    What the person said about whether this field (its label, on this application's form) takes its figures in `unit`:
    True, False, or None when they have not said or what they said cannot be read from the control plane. Their
    judgement on this task first (its question holds the sentence, its text is the option), then a learned row kept
    from such a judgement on an earlier task (host given only). Never raises.
    """

    label, unit, host, form = _label(label), _unit(unit), str(host or ""), str(form or "") if host else ""
    if not label or not unit:
        return None
    try:
        from decurix_agent import person_record
        task_id = task_id or task_context.current()
        if task_id:
            # Read again, not from the cache, when this task's person answered it and the cached read predates that.
            for fresh in (False, True) if asked_already(task_id, label, host, form, unit) else (False,):
                for row in reversed(person_record.judgements(task_id, fresh=fresh)):
                    said = _option(row.get("text"))
                    if said is not None and _asked_holds(row.get("asked"), label, host, form, unit):
                        return said
        if not host:
            return None
        for row in reversed(person_record.field_units()):
            if (row["host"] == host and row["form"] == form and row["unit"] == unit
                    and _same_field(row["label"], label) and _option(row["source_text"]) is row["takes"]
                    and _asked_holds(row["source_asked"], row["label"], host, form, unit)):
                return row["takes"]
    except Exception:  # noqa: BLE001 - what cannot be read backs nothing
        return None
    return None


_kept_now = set()


def keep(task_id=None):
    """
    Keep on the control plane each answer of this task's person to a unit question (a judgement on this task whose
    text is an option and whose question holds the sentence), as a learned row of kind "field_unit" derived from it:
    the next task's save of that field is then compared by it, and the person is not asked again. No host: kept for
    the task only. Returns how many were posted. Never raises.
    """

    try:
        from decurix_agent import person_record
        task_id = task_id or task_context.current()
        posted = 0
        for row in person_record.judgements(task_id):
            takes = _option(row.get("text"))
            found = _SENTENCE.search(" ".join(str(row.get("asked") or "").split())) if takes is not None else None
            if not found:
                continue
            label, place, unit = found.group(1), found.group(2), found.group(3)
            host, _, rest = place.partition("/")
            if place == NO_HOST or not host or " " in host:
                continue
            form = "/" + rest if rest else ""
            if not _asked_holds(row["asked"], label, host, form, unit) or (row["id"], host, form, label, unit) in _kept_now:
                continue
            _kept_now.add((row["id"], host, form, label, unit))
            person_record.keep_field_unit(task_id, row["id"], label, host, form, unit, takes, asked=row["asked"])
            posted += 1
        return posted
    except Exception:  # noqa: BLE001 - what is kept for later never costs the check
        return 0


# The sentence, read back from a question the person answered (keep). Its parts are checked again by building it.
_SENTENCE = re.compile(r'Reply 1 if the field "(.{1,%d}?)" on (\S+(?: \S+)*?) takes its figures in (\S{1,%d}), or 2 if '
                       r"it does not\." % (MAX_LABEL, MAX_UNIT))


def reset():
    """Forget which answers were posted (tests)."""

    _kept_now.clear()


def kind_json(label, host, form, unit, takes):
    """A field_unit row's text, as the control plane checks it."""

    return json.dumps({"label": _label(label), "host": host, "form": form, "unit": _unit(unit), "takes": bool(takes)},
                      ensure_ascii=False, sort_keys=True)
