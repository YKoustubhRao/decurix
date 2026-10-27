"""
Resolving a record by its identifier.

A document names a party (a customer, a supplier, a person) as it likes - a trade name, a title before the name, a
short form - while the application holds that party under its own name. What the two share is an identifier: a tax
or registration number, an account number, an email. Found live (2026-10-26, twice): a bill named its customer
otherwise than the books did; the worker searched the books by the bill's name only, found nothing, and made a second
record holding the same tax number. A rule in prose did not land; this is the tool behaviour and the code gate instead.

`gui select REF "<name as the document writes it>" --id <identifier>` searches a list by the identifier first
(core._select_custom): an entry whose text holds it is THE match, whatever its name. The outcome is kept as one
resolution fact per identifier in the task's evidence notes (evidence.note), one line each:

    resolved <id>: matched "<entry name>" by identifier
    resolved <id>: no record holds it (searched: ...; name searches: ...; offered: ...)
    resolved <id>: ambiguous - <n> records hold it: "<a>", "<b>"
    resolved <id>: unanswered - <where> showed nothing for it (searched: ...)

core._unsearched_record reads them back (resolution): a save that makes a new record holding an identifier is let
through only when the fact says none holds it; refused naming the match when one does; refused until resolved when
there is no fact. Nothing here decides anything about names: a name is what the model reads; the identifier is what
the code checks.
"""

import re

from decurix_agent import task_context
from decurix_agent.computer import evidence

LEAD = "resolved "
_FACT = re.compile(r'^resolved (?P<id>.+?): (?:matched "(?P<name>.*)" by identifier$|(?P<none>no record holds it)\b|'
                   r"(?P<ambiguous>ambiguous) - |(?P<unanswered>unanswered) - )")

# An identifier's digits alone are searched for only when there are this many: fewer are any number on the page.
MIN_DIGITS = 6
# How many of its last characters are searched for after the whole: a list that indexes a number's end, a column that
# shows it cut, a form that keeps only the end.
TAILS = (8, 6)
# How many offered entries one fact line names.
MAX_OFFERED = 12

# Roles whose text is an entry a person could choose: a list's option, a menu's item, a row's link.
ENTRY_ROLES = ("option", "menuitem", "listitem", "link")
# Roles whose text is read as part of a row of records (a table's cells, a line of text).
ROW_ROLES = ENTRY_ROLES + ("cell", "gridcell", "rowheader", "row", "text", "statictext")
# A line that says what the search was for, or that it found nothing: it holds the identifier and is no record.
_NOT_A_RECORD = re.compile(r"(?i)\b(?:no|zero)\s+(?:results?|matches?|records?|entries|items)\b|\bresults?\s+for\b|"
                           r"\bsearch(?:ed|ing)?\s+for\b|\bnot\s+found\b|\bcreate\b|\badd\s+new\b")


def key(value):
    """An identifier as compared: its letters and digits only, in lower case - spaces, dashes and dots never make another."""

    return re.sub(r"[\s\-./,:;()_]+", "", str(value or "")).lower()


def digits(value):
    """The digits of a value, in order."""

    return re.sub(r"\D", "", str(value or ""))


def holds(text, identifier):
    """Whether `text` holds the identifier (key for key: spaces, dashes and case apart)."""

    k = key(identifier)
    return len(k) >= 3 and k in key(text)


def searches(identifier):
    """
    What to type into a list to find a record by its identifier, most specific first: the whole; its last 8 and 6
    characters; its digits alone when it has MIN_DIGITS or more. An email: the whole, then its part before the @.
    """

    ident = " ".join(str(identifier or "").split())
    if not ident:
        return []
    tries = [ident]
    if "@" in ident:
        local = ident.split("@", 1)[0]
        if len(local) >= 4:
            tries.append(local)
    else:
        for n in TAILS:
            if len(ident) > n:
                tries.append(ident[-n:])
        d = digits(ident)
        if len(d) >= MIN_DIGITS and d != ident:
            tries.append(d)
    return list(dict.fromkeys(t for t in tries if len(t) >= 3))


def _quote(text, limit=90):
    text = " ".join(str(text).split())
    if len(text) > limit:
        text = text[:limit - 1] + "…"
    return '"' + text.replace('"', "'") + '"'


def _listed(items, limit=MAX_OFFERED):
    items = [i for i in dict.fromkeys(" ".join(str(i).split()) for i in items) if i]
    if not items:
        return "none"
    shown = ", ".join(_quote(i) for i in items[:limit])
    return shown + (f" (+{len(items) - limit} more)" if len(items) > limit else "")


def matched_line(identifier, name):
    return f"{LEAD}{identifier}: matched {_quote(name)} by identifier"


def none_line(identifier, searched, name_searches, offered):
    return (f"{LEAD}{identifier}: no record holds it (searched: {_listed(searched)}; name searches: "
            f"{_listed(name_searches)}; offered: {_listed(offered)})")


def ambiguous_line(identifier, names):
    names = list(dict.fromkeys(names))
    return f"{LEAD}{identifier}: ambiguous - {len(names)} records hold it: {_listed(names)}"


def unanswered_line(identifier, searched, where):
    return f"{LEAD}{identifier}: unanswered - {where} showed nothing for it (searched: {_listed(searched)})"


def note(line):
    """Keep one resolution fact for the task being worked on. Never raises."""

    evidence.note(line)


def parse(line):
    """{"identifier", "status", "name", "line"} of a fact line, or None when the line is not one."""

    m = _FACT.match(str(line or "").strip())
    if not m:
        return None
    status = next(s for s in ("none", "ambiguous", "unanswered") if m.group(s)) if m.group("name") is None else "matched"
    return {"identifier": m.group("id"), "status": status, "name": m.group("name"), "line": line.strip()}


def facts(task_id=None):
    """The task's resolution facts, oldest first."""

    task_id = task_id or task_context.current()
    if not task_id:
        return []
    return [fact for fact in (parse(text) for text in evidence._notes(task_id)) if fact]


def resolution(task_id, identifier):
    """
    What this task resolved the identifier to, its latest fact: {"status": "matched" | "none" | "ambiguous" |
    "unanswered" | "", "name": the matched entry's name or None, "line": the fact}. status "" when nothing did.
    """

    k = key(identifier)
    latest = {"status": "", "name": None, "line": ""}
    for fact in facts(task_id):
        if key(fact["identifier"]) == k:
            latest = {"status": fact["status"], "name": fact["name"], "line": fact["line"]}
    return latest


def identifiers(task_id=None):
    """Every identifier this task resolved (passed as --id), oldest first, once each."""

    return list(dict.fromkeys(fact["identifier"] for fact in facts(task_id)))


def same_row(a, b):
    """Whether two elements lie on one line of the screen: each one's middle is within the other's height."""

    ab, bb = a.get("box"), b.get("box")
    if not ab or not bb:
        return False
    ay, by = ab[1] + ab[3] / 2, bb[1] + bb[3] / 2
    return bb[1] <= ay <= bb[1] + bb[3] and ab[1] <= by <= ab[1] + ab[3]


def holders(elements, identifier):
    """The elements whose own text holds the identifier - an entry, a cell, a line - never one that says the search found nothing."""

    return [e for e in elements if e.get("role") in ROW_ROLES and e.get("name")
            and holds(e["name"], identifier) and not _NOT_A_RECORD.search(e["name"])
            and not (e.get("role") in ("text", "statictext") and key(e["name"]) == key(identifier))]


def entry_for(holder, elements, identifier):
    """
    (the element to choose or None, the record's name) for an element holding the identifier: the holder itself when
    it is an entry; else the entry on its row (a row's link); else the row's leftmost text that is not the identifier.
    """

    if holder.get("role") in ENTRY_ROLES:
        return holder, " ".join(holder["name"].split())
    row = sorted((e for e in elements if e is not holder and e.get("name") and same_row(holder, e)
                  and e.get("role") in ROW_ROLES), key=lambda e: e["box"][0])
    entries = [e for e in row if e.get("role") in ENTRY_ROLES and not _NOT_A_RECORD.search(e["name"])]
    if entries:
        return entries[0], " ".join(entries[0]["name"].split())
    first = next((e for e in row if not holds(e["name"], identifier) and e.get("role") != "row"), None)
    return None, " ".join((first or holder)["name"].split())


def matches(elements, identifier):
    """
    The records among `elements` that hold the identifier, as [(element to choose or None, name)], one per name:
    one is the match whatever its name; two are an ambiguity; none says nothing was found here.
    """

    out, names = [], set()
    for holder in holders(elements, identifier):
        entry, name = entry_for(holder, elements, identifier)
        if name.lower() not in names:
            names.add(name.lower())
            out.append((entry, name))
    return out
