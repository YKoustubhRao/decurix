"""
What the final check holds against a task's saved records, decided in code - one function for every route that can
report a task done (review round 9, R9-B3: these checks ran on the general gate only, and a planned run, a taught
skill's run or a replay could still say done over a value no source gives).

findings() gathers, as lines a caller can show and sort by their structured `kind` (records.Finding):

- a field this task set on a saved record's form that no declaration covers (records.gate_faults);
- more records made by this task's saves than the declared records it saved: a record no declaration covers;
- one record saved twice - the same number in its own number field on two new-record saves - with or without a
  declaration (core.saved_twice);
- an entry the task picked from an application's own list into a form it saved, that nothing given backs
  (records.unbacked_pick_entries: not chosen by an identifier, not picked out by the request, its files or what a
  person authored, not a person's kept answer about that field);
- each item of a report's JUDGEMENTS list that the worker itself marked "| not backed" (the protocol's own marker).

A check that could not be made is a finding of its own kind (NOT_MADE): not checked, never passed. Never raises.
"""

import re

from decurix_agent import task_context
from decurix_agent.computer.records import (NOT_BACKED, NOT_MADE, SAVED_TWICE, UNBACKED_PICK, UNCOVERED,  # noqa: F401
                                            UNDECLARED, Finding, finding)
from decurix_agent.logging_utils import log

_JUDGEMENTS_HEAD = re.compile(r"(?im)^[ \t>*_#-]*JUDG(?:E)?MENTS?[ \t*_]*:[ \t]*(.*)$")
_JUDGEMENT_ITEM = re.compile(r"^\s*(?:[-*•]|\d{1,2}[.)])\s+(.+)$")
_NOT_BACKED = re.compile(r"(?i)\|\s*not backed\b")
_CONTROL_LINE = re.compile(r"(?im)^\s*(?:DONE|COMMAND|HELP)\s*:")


def not_backed_items(report):
    """The items of a report's JUDGEMENTS list that carry the list's own "| not backed" marker (soul's judgements.go
    protocol): a call that rests on nothing given. Words inside a call never decide it - only the marker."""

    text = str(report or "")
    head = _JUDGEMENTS_HEAD.search(text)
    if not head:
        return []
    out = []
    lines = ([head.group(1)] if head.group(1).strip() else []) + text[head.end():].splitlines()
    for line in lines:
        if _CONTROL_LINE.match(line):
            break
        item = _JUDGEMENT_ITEM.match(line)
        body = item.group(1) if item else line
        if _NOT_BACKED.search(body):
            out.append(" ".join(body.split())[:300])
    return out


def _events(task_id):
    from decurix_agent.computer import core, evidence
    if not task_id or task_id == task_context.current():
        return core._task_events_now()
    return evidence._task_events(task_id)


def findings(task_id=None, reports=(), events=None, picks=True):
    """
    Every point code holds against the task's saved records (the module's list), as records.Finding lines; [] when
    none. `reports`: the reports whose JUDGEMENTS lists are read (one text, or one per stage). `events`: the task's
    recorded actions (read when not given). `picks`: whether the entries picked from lists are checked - a replay's
    are the person's own, as recorded.
    """

    task_id = task_id or task_context.current()
    reports = [reports] if isinstance(reports, str) else list(reports or ())
    out = []
    try:
        from decurix_agent.computer import core, records
        events = _events(task_id) if events is None else events
        twice = core.saved_twice(events)
        for ref, n in twice.items():
            out.append(finding(SAVED_TWICE, f"record {ref} was saved {n} times as a new record in this task: the "
                                            "same record saved twice"))
        # (A record saved twice is said as that, not again as a record no declaration covers.)
        created = core.records_created(events) - sum(n - 1 for n in twice.values())
        out += [f if isinstance(f, Finding) else finding(UNDECLARED, f)
                for f in records.gate_faults(task_id, created=created)]
        if picks:
            records.keep_field_answers(task_id, events=events)
            for p in records.unbacked_pick_entries(task_id, events=events):
                value = p["taken"] or p["option"]
                out.append(finding(UNBACKED_PICK, f"{p['label'] or 'a field'}: {value!r} was chosen from the "
                                                  "application's own list into a saved form, and nothing given backs "
                                                  "it (not an identifier, the request, its files, nor what a person "
                                                  "said or taught)"))
    except Exception as exc:  # noqa: BLE001 - not made, never passed
        log(f"ledger_checks: the saved records could not be checked: {exc!r}")
        out.append(finding(NOT_MADE, "the saved records could not be checked against the declared ones "
                                     f"({type(exc).__name__})"))
    for report in reports:
        out += [finding(NOT_BACKED, f"the report itself says: {item}") for item in not_backed_items(report)]
    return out


def faults(found):
    """The findings that are faults (all but the checks not made)."""

    return [f for f in found if getattr(f, "kind", "") != NOT_MADE]


def not_made(found):
    """The findings that are checks not made."""

    return [f for f in found if getattr(f, "kind", "") == NOT_MADE]
