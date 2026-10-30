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
    "application's record inside the document's words, and words left out cannot be matched. "
    "A value marked file is a file to upload: its answer is the full path of one of this run's "
    "files (listed in the request), the one that plays the part the example did; several files are separated by ';'. "
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


def fill(skill, params, context, strict=False):
    """
    ({name: value}, {name: evidence}, rows) for the params the run's context shows;
    ({}, {}, None) when there is no model, nothing to read, or the model fails.

    strict: keep only what this run's request and documents hold (as written, or the same amount) - for a run the
    model does itself, which reads its sources with its own tools. Found live (2026-10-24): a request that named no
    document came back with an invoice series made from the client's initials and the month, a count to match and two
    table rows, none of them anywhere; passed on as the run's values they read as the person's, were typed, and the
    final check then held the work to them over the person's own "don't invent".
    """

    if not params or not available():
        return ({}, {}, None)

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
            texts.append(office._ocr_file(image))
            images += 1

    if any(_is_date_param(p) for p in params):
        parts.append({"type": "text", "text": "\n" + _date_note(skill, texts)})

    try:
        message, _ = llm_client.chat(
            [{"role": "system", "content": SYSTEM}, {"role": "user", "content": parts}],
            max_tokens=3000, response_format={"type": "json_object"},
        )
        answer = json.loads(message.get("content") or "{}")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"binding: could not fill values for {skill.get('name')}: {exc}")
        return ({}, {}, None)

    values, evidence = _clean(params, answer, texts, strict=strict)
    rows = _rows(skill, params, answer, texts, values, evidence, strict=strict)
    return (values, evidence, rows)


# A date written with digits only: 03/10/2026, 3.10.2026, 28-09-2026.
DIGIT_DATE = re.compile(r"(?<!\d)(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})(?!\d)")


def _is_date_param(param):
    """A value that is a date: its example looks like one, or its label says so."""

    example = str(param.get("example") or param.get("default") or "")
    return bool(re.fullmatch(r"\d{4}-\d{2}-\d{2}", example) or DIGIT_DATE.fullmatch(example)
                or re.search(r"\bdate\b", param.get("label") or param.get("name") or "", re.I))


def date_order(texts):
    """
    "day" or "month" - which comes first in the digit dates these texts write - decided only by dates that cannot be
    read the other way (28/09/2026 is day first), and only when they all agree; None otherwise.
    """

    seen = set()
    for text in texts:
        for a, b, _ in DIGIT_DATE.findall(text or ""):
            a, b = int(a), int(b)
            if a > 12 and b <= 12:
                seen.add("day")
            elif b > 12 and a <= 12:
                seen.add("month")
    return seen.pop() if len(seen) == 1 else None


def _document_text(path):
    """A document's words, its pictures read by OCR too (a scanned form has no text layer)."""

    try:
        doc = office.read_document(path)
    except (office.OfficeError, OSError, ValueError):
        return ""
    return doc["text"] + "\n" + "\n".join(office._ocr_file(image) for image in doc["images"][:3])


def _date_note(skill, texts):
    """
    What to tell the model about dates written in digits. Found live (2026-10-14): a claim form's "03/10/2026" was
    read as 10 March although the form the person taught from wrote "28/09/2026" - day first - and the record was
    saved with the wrong date.
    """

    order = date_order(texts) or date_order([_document_text(p) for p in skill.get("documents") or []])
    if order == "day":
        return ("Dates in these documents are written day first (as 28/09/2026 is 28 September): read a date such as "
                "03/10/2026 as 3 October.")
    if order == "month":
        return ("Dates in these documents are written month first (as 09/28/2026 is September 28): read a date such "
                "as 03/10/2026 as March 10.")
    return ("If a date is written with digits that could be read either way (03/10/2026) and nothing in the documents "
            "shows which comes first, leave that value out - it will be asked for.")


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


def _clean(params, answer, texts=(), strict=False):
    """Only values of the params asked for, in the shape each takes (strict: only those the texts hold - fill)."""

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
            written = [as_written(v, param.get("example"), texts) for v in items]
            if strict and not all(held_by(v, texts) for v in written):
                log(f"binding: {name} - not every item is in what this run was given; left for the work to read")
                continue
            values[name] = "; ".join(written)
        else:
            if isinstance(value, (list, dict)):
                continue
            written = as_written(str(value).strip(), param.get("example"), texts)
            if strict and not held_by(written, texts):
                log(f"binding: {name} = {written!r} is in nothing this run was given; left for the work to read")
                continue
            if is_example(written, param.get("example")) and not held_by(written, texts) and not (
                    re.search(r"[^\W\d_]{3,}", written) and words_held(written, texts)):
                # The person's own value from when they taught it, which nothing of this run says.
                log(f"binding: {name} = {written!r} is the taught example and this run's documents do not hold it - left out")
                continue
            if not words_held(written, texts):
                # Said, never dropped (independent review, 2026-10-22): a value worked out from the documents is
                # written otherwise than they write it (28-Sep-2026 from 28/09/2026, "Private Limited" from "Pvt Ltd")
                # and is right. Found live (2026-10-22) the other way: a portal username made up from the
                # client's name, "srilakshmielev" - so the person and the check see that it is not the documents' own.
                log(f"binding: {name} = {written!r} is not written so in this run's documents - kept, and said")
                note = "not written so in this run's documents - worked out, or a guess: check it"
                old = why.get(name) if isinstance(why, dict) else None
                evidence[name] = (f"{str(old)[:150]} ({note})" if old else note)[:200]
            values[name] = written

        if isinstance(why, dict) and why.get(name) and name not in evidence:
            evidence[name] = str(why[name])[:200]

    return values, evidence


# A figure as a document writes it: one number, with the money it is in before it ("Rs. 360.00", "INR 1,250",
# "$40") and the unit it counts after it ("20 Reams", "2 Units", "1.5 kg", "360.00/-").
FIGURE = re.compile(r"^\s*(?:(?:Rs\.?|INR|USD|EUR|GBP|AED|SGD|[₹$€£])\s*)?(-?\d[\d,]*(?:\.\d+)?)\s*(?:/-|[A-Za-z][A-Za-z .()/-]{0,30})?\s*$",
                    re.IGNORECASE)
BARE_NUMBER = re.compile(r"^\s*-?\d[\d,]*(?:\.\d+)?\s*$")


def as_number(value, example):
    """
    A figure written with its unit or its currency, as the number alone, when
    the field was shown to take a bare number. Found live: a bill's "20
    Reams", "2 Chairs" and "Rs. 360.00" were read whole for fields the
    demonstration typed "1" and "40000" into; the number check refused them
    and a person was asked for figures the bill states plainly. One number,
    with nothing around it but the mark of its currency and the words of its
    unit, is that number - nothing is guessed. Anything else is left as read.
    """

    if example is None or not BARE_NUMBER.match(str(example)):
        return value
    scaled = _scaled(value or "")
    if scaled is not None:
        return scaled
    found = FIGURE.match(value or "")
    return found.group(1) if found else value


# A figure written with its scale: "$72k" is 72000, not 72 (found live, 2026-10-14: a salary of "$72k" was entered as
# 72, the "k" taken for a unit and dropped). "k" and spelled-out scales always; "m"/"bn" only after a currency mark,
# since "20 m" of cable is metres.
_SCALES = {"k": 10 ** 3, "thousand": 10 ** 3, "lakh": 10 ** 5, "lakhs": 10 ** 5, "lac": 10 ** 5, "crore": 10 ** 7,
           "crores": 10 ** 7, "cr": 10 ** 7, "million": 10 ** 6, "mn": 10 ** 6, "m": 10 ** 6, "billion": 10 ** 9,
           "bn": 10 ** 9}
_CURRENCY = r"(?:Rs\.?|INR|USD|EUR|GBP|AED|SGD|[₹$€£])"
SCALED = re.compile(rf"^\s*(?P<cur>{_CURRENCY}\s*)?(?P<n>-?\d[\d,]*(?:\.\d+)?)\s*(?P<scale>[A-Za-z]+)\.?"
                    rf"(?:\s+(?:a|per)\s+(?:year|annum|month|week|day|hour)|\s*/\s*(?:yr|year|mo|month|hr|hour))?\s*$", re.I)


def _scaled(value):
    """The number a figure with a scale word stands for, written plainly ("72000"), or None for any other figure."""

    from decimal import Decimal, InvalidOperation

    m = SCALED.match(value)
    if not m:
        return None
    scale = m.group("scale").lower()
    if scale not in _SCALES or (scale in ("m", "mn", "bn", "cr") and not m.group("cur")):
        return None
    try:
        number = Decimal(m.group("n").replace(",", "")) * _SCALES[scale]
    except InvalidOperation:
        return None
    return str(number.quantize(Decimal(1))) if number == number.to_integral_value() else format(number.normalize(), "f")


def as_written(value, example, texts):
    return _own_spelling(as_number(value, example), example, texts)


def _plain(value):
    """A value as compared with another: spacing and case gone, and a number as its amount (165,000.00 is 165000)."""

    text = " ".join(str(value or "").split()).casefold()
    if BARE_NUMBER.match(text):
        try:
            return repr(float(text.replace(",", "")))
        except ValueError:
            pass
    return text


def is_example(value, example):
    """Whether `value` is the one typed when the procedure was taught."""

    return example not in (None, "") and _plain(value) == _plain(example)


def words_held(value, texts):
    """
    Whether a value with words in it is this run's own: held whole, or every word of three letters or more is in the
    texts ("29-Karnataka" when the documents say Karnataka). A figure is always let through - a total may be worked out.
    """

    words = [w for w in re.findall(r"[^\W\d_]{3,}", str(value or "")) if w]
    if not words or not any(texts) or held_by(value, texts):
        return True  # (no texts: nothing to tell a value by)
    joined = " ".join(" ".join(t.split()) for t in texts if t).casefold()
    return all(re.search(r"(?<!\w)" + re.escape(w.casefold()) + r"(?!\w)", joined) for w in words)


def held_by(value, texts):
    """
    Whether the texts of this run say `value`: as written (case and spacing aside), or as a number of the same amount.
    Found live (2026-10-22): asked to prepare another client's return "the way I showed you", the values
    came back as the teaching client's own - "copied from example", its taxable value and invoice numbers - and were
    typed into the other client's return. The example says which thing to read, never what it is.
    """

    plain = _plain(value)
    if not plain:
        return False
    joined = " ".join(" ".join(t.split()) for t in texts if t).casefold()
    if BARE_NUMBER.match(plain):
        return any(_plain(n) == plain for n in re.findall(r"\d[\d,]*(?:\.\d+)?", joined))
    return re.search(r"(?<!\w)" + re.escape(plain) + r"(?!\w)", joined) is not None


def _own_spelling(value, example, texts):
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
            "document's entries under \"rows\", each with the names " + ", ".join(group["rows"][0]) + ", and each with its "
            "own \"evidence\": {name: which line of the document it was read from}.\n")


def _rows(skill, params, answer, texts, values, evidence, strict=False):
    """
    Puts the document's rows into `values` under each row's names (the
    demonstration's own for its rows, made after the first row's beyond them)
    and returns {"count", "demo", "fields", "names": [[row names]], "base": {name: first-row name}}, or None.
    strict (fill): a row value only when the texts hold it, and no rows at all when they hold none of them - their
    number is then a guess too, and the check would hold the work to it.
    """

    from decurix_agent.computer import skills

    group = _group(skill, params)
    rows = answer.get("rows") if isinstance(answer, dict) else None
    if group is None or not isinstance(rows, list) or not rows:
        return None

    first = group["rows"][0]
    by_name = {p["name"]: p for p in skill.get("params") or []}
    names, base = [], {}
    held = 0
    why = answer.get("evidence") if isinstance(answer.get("evidence"), dict) else {}
    for n, row in enumerate(r for r in rows if isinstance(r, dict)):
        mine = skills.row_names(group, n)
        names.append(mine)
        for field, name in zip(first, mine):
            base[name] = field
            value = row.get(field)
            if value in (None, "", []) or isinstance(value, (list, dict)):
                continue
            written = as_written(str(value).strip(), (by_name.get(field) or {}).get("example"), texts)
            if strict and not held_by(written, texts):
                continue
            values[name] = written
            # Where a row's value was read is that row's own: the answer's evidence for a field names the first row's
            # line, and every later row's report cited that line for its own value (found live, 2026-10-30).
            own = row.get("evidence") if isinstance(row.get("evidence"), dict) else {}
            where = own.get(field) or own.get(name) or why.get(name) or (why.get(field) if n == 0 else None)
            evidence[name] = str(where or f"entry {n + 1} of the document")[:200]
            held += 1
    if strict and not held:
        log(f"binding: {len(names)} row(s) given and nothing this run was given holds any of them; left for the work")
        return None
    # A row the document does not have is not a value of this run.
    for row in group["rows"][len(names):]:
        for name in row:
            values.pop(name, None)
            evidence.pop(name, None)
    return {"count": len(names), "demo": len(group["rows"]), "fields": group["fields"], "names": names, "base": base}
