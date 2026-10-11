"""
Filling a taught skill's values from the documents of this run.

A skill learned from a demonstration knows *where* its values go - "the
Customer field", "the Quantity column, once per item" - and one example of
each, what the person typed while teaching it. When the same workflow comes
back with a new document (the next PO, the next invoice, the next new
joiner's sheet), the values are in that document. Without a model the skill
asks a person for each of them; with the hosted model (llm_client) this reads
them from the request and the files it names, using each example to know
*which* thing to read - never as the answer.

What it will not do: invent a value. Anything the documents do not show is
left out and the skill asks for it exactly as before; every value it does
fill comes with where it was read, which the run reports. The values then go
through the same checks as a person's (constraints, list lengths), and every
consequential step still stops for approval.
"""

import json
import os
import re

from decurix_agent import llm_client
from decurix_agent.logging_utils import log
from decurix_agent.tools import office

# How many files one run may read to fill its values, and how much of each.
MAX_FILES = 4
MAX_FILE_CHARS = 12000
MAX_IMAGES = 6

# A path as people and triggers write them: absolute, or under ~.
# On a native Windows worker it is a drive path (C:\...\work\inbox\7\po.pdf).
PATH_RE = re.compile(r"(?<![\w/])((?:(?:~|/home/headless|/tmp)/|[A-Za-z]:[\\/])[^\s'\"<>|;,]+\.[A-Za-z0-9]{2,5})")

SYSTEM = (
    "You fill in the values a taught procedure needs, from the documents of one run. "
    "For each value you are given: its name, the field it is typed into, and the value typed when the "
    "procedure was taught - an EXAMPLE of the kind of thing to read (for instance a customer name, a PO "
    "number, a quantity), never the answer. A value marked list takes one entry per item (line) of the "
    "document, in the document's order, all lists the same length. Read values exactly as the documents "
    "show them (keep codes and numbers as written). A date is the exception: the example shows the form "
    "the field takes, so write the document's date in the example's form - the same order of day, month "
    "and year, digits where the example has digits, the same separators (example 01-10-2026, document "
    "04-Oct-2026: answer 04-10-2026). Found live: a date typed as the document wrote it was refused by "
    "its field, and the record was saved with today's date. A value's \"meaning\", when given, is what "
    "the person who taught the procedure said about it; follow it. "
    "A value marked lookup is not typed and left: it is searched for in a list of the application's own "
    "records, and the record found is what gets entered. For such a value give the document's whole wording "
    "of the thing, exactly and unshortened (the full line or name as the document writes it) - never a "
    "shortened or reworded form, and never a guess at what the application calls it: the search matches the "
    "application's record inside the document's words, and words left out cannot be matched (found live: "
    "\"Office Mesh Chairs\" was given for a line reading \"High Back Ergonomic Lumbar Support Office Mesh "
    "Chairs\", and the record \"Ergonomic Office Chair\" could no longer be found in it). "
    "A value marked file is a file to upload: its answer is the full path of one of this run's "
    "files (listed in the request), the one that plays the part the example did - e.g. the new offer "
    "letter where an offer letter was uploaded; several files are separated by ';'. "
    "If a document does not show a value, leave it out - do not guess, and never return the "
    "example itself unless the document really says it. "
    "When \"repeating rows\" are described, those fields were filled once per entry of the document (once per "
    "line of a table, say) and the demonstration's document had its own number of entries. Give them under "
    "\"rows\": one object per entry of THIS document, in the document's order, each with every field of a row - "
    "as many rows as the document has entries, be it one or ten, never padded or cut to the demonstration's "
    "number - and leave those names out of \"values\". "
    'Reply with JSON only: {"values": {"<name>": "<value>" or ["<item value>", ...]}, "rows": [{"<field name>": "<value>", ...}, ...], '
    '"evidence": {"<name>": "<which document, which label or line>"}, "missing": ["<name>", ...]}'
)


def available():
    return llm_client.available()


def referenced_files(text):
    """The existing files a request names, in order, at most MAX_FILES."""

    found = []
    for match in PATH_RE.finditer(text or ""):
        path = os.path.expanduser(match.group(1).rstrip(".)"))
        if os.path.isfile(path) and path not in found:
            found.append(path)
        if len(found) == MAX_FILES:
            break
    return found


def _describe(param, meaning="", lookup=False):
    line = {
        "name": param["name"],
        "field": param.get("label") or param["name"],
        "example": param.get("example") or "",
        "list": bool(param.get("list")),
    }
    if meaning:
        line["meaning"] = str(meaning)[:300]
    if lookup:
        line["lookup"] = True
    if param.get("file"):
        line["file"] = True
    if param.get("list") and param.get("examples"):
        line["examples"] = param["examples"]
    if param.get("hint"):
        line["hint"] = param["hint"]
    return line


def fill(skill, params, context, with_rows=False):
    """
    ({name: value}, {name: evidence}) for the params the run's context shows;
    ({}, {}) when there is no model, nothing to read, or the model fails.
    """

    if not params or not available():
        return ({}, {}, None) if with_rows else ({}, {})

    files = referenced_files(context)
    parts = [{"type": "text", "text": _prompt(skill, params, context)}]
    texts = [context or ""]
    images = 0

    for path in files:
        try:
            doc = office.read_document(path)
        except (office.OfficeError, OSError, ValueError) as exc:
            parts.append({"type": "text", "text": f"\n[{path}: could not be read - {exc}]"})
            continue

        texts.append(doc["text"])
        parts.append({"type": "text", "text": f"\n--- Document {path} ---\n{doc['text'][:MAX_FILE_CHARS]}"})
        for image in doc["images"]:
            if images == MAX_IMAGES:
                break
            parts.append(llm_client.image_part(path=image))
            images += 1

    try:
        message, _ = llm_client.chat(
            [{"role": "system", "content": SYSTEM}, {"role": "user", "content": parts}],
            max_tokens=3000, response_format={"type": "json_object"},
        )
        answer = json.loads(message.get("content") or "{}")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"binding: could not fill values for {skill.get('name')}: {exc}")
        return ({}, {}, None) if with_rows else ({}, {})

    values, evidence = _clean(params, answer, texts)
    rows = _rows(skill, params, answer, texts, values, evidence)
    return (values, evidence, rows) if with_rows else (values, evidence)


def _prompt(skill, params, context):
    from decurix_agent.computer import skills

    meanings = (skill.get("understanding") or {}).get("inputs") or {}
    chosen = skills.chosen_from_list(skill)
    return "\n".join([
        f"Procedure: {skill.get('title') or skill.get('name')}",
        f"Its goal: {skill.get('goal') or skill.get('done_when') or '(not stated)'}",
        "Values to fill:",
        json.dumps([_describe(p, meanings.get(p["name"]), p["name"].split("__")[0] in chosen) for p in params],
                   ensure_ascii=False, indent=1),
        "",
        _rows_text(skill, params),
        "The request that started this run (it may include an email, and it names the files attached):",
        (context or "")[:6000],
    ])


def _clean(params, answer, texts=()):
    """Only values of the params asked for, in the shape each takes."""

    wanted = {p["name"]: p for p in params}
    values, evidence = {}, {}
    raw = answer.get("values") if isinstance(answer, dict) else None
    why = answer.get("evidence") if isinstance(answer, dict) else None

    for name, value in (raw or {}).items():
        param = wanted.get(name)
        if param is None or value in (None, "", []):
            continue

        if param.get("list"):
            items = value if isinstance(value, list) else [value]
            items = [str(v).strip() for v in items if str(v).strip()]
            if not items or any(";" in v or "\n" in v for v in items):
                continue  # an item holding the separator cannot be written as a list value
            values[name] = "; ".join(as_written(v, param.get("example"), texts) for v in items)
        else:
            if isinstance(value, (list, dict)):
                continue
            values[name] = as_written(str(value).strip(), param.get("example"), texts)

        if isinstance(why, dict) and why.get(name):
            evidence[name] = str(why[name])[:200]

    return values, evidence


def as_written(value, example, texts):
    """
    The document's own spelling of an all-capitals value, when the example was
    not in capitals. Found live: a PO's letterhead read "WOOD CORNER" and its
    signature line "Wood Corner"; the model took the letterhead and the
    quotation was saved for a customer spelled like no record. A value the
    documents only ever write in capitals (a code, "ACME") is kept as it is.
    """

    letters = [c for c in value if c.isalpha()]
    if len(letters) < 2 or value != value.upper():
        return value
    if example and str(example) == str(example).upper():
        return value
    words = value.split()
    pattern = re.compile(r"(?<!\w)" + r"\s+".join(re.escape(w) for w in words) + r"(?!\w)", re.IGNORECASE)
    for text in texts:
        for match in pattern.finditer(text or ""):
            found = " ".join(match.group(0).split())
            if found != found.upper():
                return found
    return value


def report(evidence, values):
    """One line per filled value, for the run's report."""

    return "\n".join(f"- {name} = \"{values[name]}\" ({evidence.get(name) or 'from the documents'})"
                     for name in values)


def _group(skill, params):
    """The skill's repeated part, when the values asked for include it; else None."""

    from decurix_agent.computer import skills

    group = skills.repeated_rows(skill) if skill.get("params") else None
    asked = {p["name"] for p in params}
    if group is None or not any(name in asked for row in group["rows"] for name in row):
        return None
    return group


def _rows_text(skill, params):
    group = _group(skill, params)
    if group is None:
        return ""
    return ("Repeating rows: the fields " + ", ".join(f"{name} ({label})" for name, label in zip(group["rows"][0], group["fields"]))
            + f" are one row, filled once per entry of the document; the demonstration's document had {len(group['rows'])} "
            "entries (its other rows are " + "; ".join(", ".join(row) for row in group["rows"][1:]) + "). Give this "
            "document's entries under \"rows\", each with the names " + ", ".join(group["rows"][0]) + ".\n")


def _rows(skill, params, answer, texts, values, evidence):
    """
    Puts the document's rows into `values` under each row's names (the
    demonstration's own for its rows, made after the first row's beyond them)
    and returns {"count", "demo", "fields", "names": [[row names]], "base": {name: first-row name}}, or None.
    """

    from decurix_agent.computer import skills

    group = _group(skill, params)
    rows = answer.get("rows") if isinstance(answer, dict) else None
    if group is None or not isinstance(rows, list) or not rows:
        return None

    first = group["rows"][0]
    by_name = {p["name"]: p for p in skill.get("params") or []}
    names, base = [], {}
    why = answer.get("evidence") if isinstance(answer.get("evidence"), dict) else {}
    for n, row in enumerate(r for r in rows if isinstance(r, dict)):
        mine = skills.row_names(group, n)
        names.append(mine)
        for field, name in zip(first, mine):
            base[name] = field
            value = row.get(field)
            if value in (None, "", []) or isinstance(value, (list, dict)):
                continue
            values[name] = as_written(str(value).strip(), (by_name.get(field) or {}).get("example"), texts)
            evidence[name] = str(why.get(name) or why.get(field) or f"entry {n + 1} of the document")[:200]
    # A row the document does not have is not a value of this run.
    for row in group["rows"][len(names):]:
        for name in row:
            values.pop(name, None)
            evidence.pop(name, None)
    return {"count": len(names), "demo": len(group["rows"]), "fields": group["fields"], "names": names, "base": base}
