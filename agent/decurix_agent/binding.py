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
from decurix_agent.computer import constraints
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


# A folder a request names ("the invoices are also in ~/work/clients/november/"): a path with no extension.
FOLDER_RE = re.compile(r"(?<![\w/])((?:~|/home/headless|/tmp)/[^\s'\"<>|;,]+?)/?(?=[\s'\"<>|;,]|\.\s|\.$|$)")
# How many files of named folders a run reads, beside the files it names one by one.
MAX_FOLDER_FILES = 8
DOCUMENT_SUFFIXES = (".pdf", ".docx", ".doc", ".xlsx", ".xls", ".csv", ".txt", ".png", ".jpg", ".jpeg")


def folder_files(text):
    """The document files of the folders a request names (each folder's own files, not its subfolders'), in order, at
    most MAX_FOLDER_FILES. Found in the learning curves (2026-11-17): a filing whose request said "the invoices are also
    in <folder>" had its values read from nothing, and three parts of the taught steps went to the model by hand."""

    found = []
    for match in FOLDER_RE.finditer(text or ""):
        folder = os.path.expanduser(match.group(1).rstrip(".)"))
        if not os.path.isdir(folder):
            continue
        try:
            names = sorted(os.listdir(folder))
        except OSError:
            continue
        for name in names:
            path = os.path.join(folder, name)
            if (os.path.isfile(path) and not name.startswith((".", "~$")) and name.lower().endswith(DOCUMENT_SUFFIXES)
                    and path not in found):
                found.append(path)
            if len(found) >= MAX_FOLDER_FILES:
                return found
    return found


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


def fill(skill, params, context, strict=False, relaxed=(), folders=False):
    """
    ({name: value}, {name: evidence}, rows) for the params the run's context shows;
    ({}, {}, None) when there is no model, nothing to read, or the model fails.

    strict: keep only what this run's request and documents hold (as written, or the same amount) - for a run the
    model does itself, which reads its sources with its own tools. Found live (2026-10-24): a request that named no
    document came back with an invoice series made from the client's initials and the month, a count to match and two
    table rows, none of them anywhere; passed on as the run's values they read as the person's, were typed, and the
    final check then held the work to them over the person's own "don't invent".

    relaxed: names kept even when strict - values the steps choose from the application's own list (a financial year,
    a quarter, a return period), read from what the run was given (December -> "Quarter 3 (Oct - Dec)"): the plan shows
    them to the person, and the replay can only choose an entry the list has. Found in the learning curves (2026-11-16):
    a run driven part by part handed the model three parts only for a financial year the request implies.
    """

    files = referenced_files(context)
    if folders:
        # (folders: the documents of the folders the request names are read too - a run driven part by part.)
        files += [p for p in folder_files(context) if p not in files]
    answer, texts = _ask(skill, params, context, files)
    if answer is None:
        return ({}, {}, None)
    values, evidence = _clean(params, answer, texts, strict=strict, relaxed=relaxed)
    rows = _rows(skill, params, answer, texts, values, evidence, strict=strict)
    return (values, evidence, rows)


def fill_document(skill, params, context, path, relaxed=()):
    """({name: value}, {name: evidence}) for `params` from the one document at `path` - only what it holds, but for the
    `relaxed` names (values a step chooses from the application's own list); ({}, {}) when there is no model or it
    fails."""

    answer, texts = _ask(skill, params, context, [path], rows=False)
    if answer is None:
        return ({}, {})
    return _clean(params, answer, texts, strict=True, relaxed=relaxed)


# How many documents one run's lists are read from at once (the gathered documents of a month; a page each).
MAX_GATHERED = 12

# The item of a list for a document that shows that field's place empty (an inter-state bill's CGST row): the step that
# types it is left out for that item (skills.expand). Given only where the document's own text shows the empty place
# (left_blank).
EMPTY_ITEM = constraints.EMPTY_ITEM

LISTS_NOTE = (
    "This run has SEVERAL documents, listed below in order, and the procedure is done once per document: every value "
    "marked \"list\" takes one item per document, in the documents' order, so that item N of every list belongs to "
    "document N. Read each document's own values from that document only. Two kinds of document are LEFT OUT of every "
    "list and named under \"left_out\", so the lists stay aligned over the documents that are entered by the procedure: "
    "(1) \"skip\": the person's rules (below) say it is not to be entered at all - cancelled, already in the books, not "
    "of the kind the task enters, a duplicate of another; (2) \"by_hand\": it IS to be entered, but it does not fit the "
    "procedure's shape as taught - a different number of lines, a tax or a field the taught fields do not cover, a "
    "value you cannot read - so it will be entered separately by hand. A field the document shows with its place EMPTY "
    "for its item (a tax row or column there with no amount, as a bill charging another tax leaves it) takes "
    "the item \"(empty)\" in that list: the procedure then leaves that field empty for that document. A party, ledger or other "
    "thing the procedure chooses from the application's own lists is read as the document names it: whether it is "
    "already in the application, under which exact name, or has to be made first is for the procedure to find on the "
    "screen, and is never a reason for \"by_hand\" or \"skip\". Never force a "
    "document into the lists with made-up items, and never write \"(empty)\" for a value the document gives or does "
    "not show a place for. Give no \"rows\": the procedure's shape (its number of lines "
    "and rows) is fixed as taught, and a document with another shape is \"by_hand\". Add to the JSON: "
    '"left_out": [{"document": "<path>", "kind": "skip" or "by_hand", "why": "<in a few words>"}].\n'
)


def fill_lists(skill, params, context, files, rules=""):
    """
    The values of a run done once per document - the loop's lists, one item per document - read from `files` in one
    call: ({name: value}, {name: evidence}, rows, left_out). left_out: [{"document", "kind", "why"}] - kind "skip" for
    a document the person's rules (`rules`, their words) say not to enter, "by_hand" for one to enter that does not
    fit the taught shape. A list whose items do not count one per entered document is left out of the values (the
    work reads it itself) rather than typed misaligned. ({}, {}, None, []) when there is no model or it fails.
    """

    files = files[:MAX_GATHERED]
    note = LISTS_NOTE + (f"The person's rules for this task:\n{rules}\n" if rules else "")
    note += "This run's documents, in order:\n" + "\n".join(f"{n + 1}. {p}" for n, p in enumerate(files))
    answer, texts = _ask(skill, params, context, files, note=note, rows=False)
    if answer is None:
        return ({}, {}, None, [])
    # No rows across documents: the per-document shape is the taught one (found live 2026-11-02: the rows of ONE
    # document were written over the lists, one list losing its items and a field getting another's amount).
    answer.pop("rows", None)
    left_out = [{"document": str(x.get("document") or ""), "why": str(x.get("why") or "")[:200],
                 "kind": "by_hand" if str(x.get("kind") or "").strip().lower() == "by_hand" else "skip"}
                for x in (answer.get("left_out") or []) if isinstance(x, dict) and x.get("document")]
    left_out = _left_out_paths(left_out, files)
    entered = len(files) - len({x["document"] for x in left_out})
    values, evidence = _clean(params, answer, texts)
    gone = {x["document"] for x in left_out}
    entered_files = [f for f in files if f not in gone]
    for param in params:
        if not (param.get("list") and param["name"] in values):
            continue
        items = split_items(values[param["name"]])
        if EMPTY_ITEM in items and len(items) == len(entered_files) and not all(
                left_blank(param.get("label") or param["name"], _document_text(entered_files[i]))
                for i, item in enumerate(items) if item == EMPTY_ITEM):
            # "(empty)" only where the document itself shows the field's place with nothing in it.
            log(f"binding: {param['name']} has an empty item its document does not show empty - left for the work to read")
            values.pop(param["name"])
            evidence.pop(param["name"], None)
    for param in params:
        if param.get("list") and param["name"] in values and len(split_items(values[param["name"]])) != entered:
            log(f"binding: {param['name']} has {len(split_items(values[param['name']]))} items for {entered} documents - left for the work to read")
            values.pop(param["name"])
            evidence.pop(param["name"], None)
    return (values, evidence, None, left_out)


def left_blank(label, text):
    """
    Whether a document shows a field's place with nothing in it: the label's most telling word (the one on the fewest of
    the document's lines - "CGST" of "CGST amount", not "amount") stands on at least one line, every line holding it is
    a table row whose cells the text marks ("|" or a tab between them), and no cell of such a row after the word's own
    holds a figure besides a rate written as a percentage ("| CGST @ 9% | |": an empty amount cell). Decided by
    structure, never by which words a field may have. Found live (2026-11-09): an inter-state bill's empty CGST and SGST
    rows sent a whole month of vouchers to the model by hand. A line whose cells the text does not mark is no proof of
    an empty place, and the value is left to be read (review of freeze 2, A11).
    """

    lines = (text or "").splitlines()
    words = {w for w in re.findall(r"[A-Za-z]{3,}", label or "")}

    def holding(word):
        pattern = re.compile(r"(?<![A-Za-z])" + re.escape(word) + r"(?![A-Za-z])", re.I)
        return pattern, [line for line in lines if pattern.search(line)]

    found = [(w,) + holding(w) for w in words]
    if not found or any(not ls for _w, _p, ls in found):
        return False  # a word of the field the document never writes: it shows no place for that field
    # The rarest word, and every word as rare (a tie is not settled by which comes first: "CGST" and "amount" on one
    # line each, the "amount" line a sentence of words, must not stand for the CGST row's figure).
    fewest = min(len(ls) for _w, _p, ls in found)

    def row_cells(line):
        cells = re.split(r"[ \t]*[|\t][ \t]*", line.strip(" "))
        return cells if len(cells) >= 2 else None

    # The rarest words whose every line is a row with marked cells: the field's place in a table. A rarest word that
    # stands in a sentence (a total's "amount in words") is no place of the field.
    placed = [(pattern, held) for _w, pattern, held in found
              if len(held) == fewest and all(row_cells(line) for line in held)]
    if not placed:
        return False  # no structured read of the field's place: not proved empty
    for pattern, held in placed:
        for line in held:
            cells = row_cells(line)
            at = next(i for i, cell in enumerate(cells) if pattern.search(cell))
            if at == len(cells) - 1:
                return False  # the word's cell is the row's last: no place after it to be empty
            after = " ".join(cells[at + 1:])
            own = pattern.sub(" ", cells[at])
            if re.search(r"\d", re.sub(r"\d+(?:\.\d+)?\s*%", " ", after + " " + own)):
                return False
    return True


def _one_path(document, files):
    """The one file of `files` the model's `document` names: the path itself, else the one file with that file name.
    None when it names none, or more than one (two folders holding the same file name)."""

    if document in files:
        return document
    leaf = document.replace("\\", "/").rsplit("/", 1)[-1].casefold()
    named = [f for f in files if str(f).replace("\\", "/").rsplit("/", 1)[-1].casefold() == leaf]
    return named[0] if len(named) == 1 else None


def _left_out_paths(left_out, files):
    """
    Each left-out entry mapped to exactly one of the run's files (its path then stands for it), once. An entry naming
    no file, or more than one, is dropped: the run's own consumers compare paths exactly, and a loose match ("1.pdf" in
    ".../11.pdf") listed one document both as "already entered" and as "enter by hand" (review 2026-11-03, E-34).
    """

    out, seen = [], set()
    for entry in left_out:
        path = _one_path(entry["document"], files)
        if path is None:
            log(f"binding: left-out {entry['document']!r} names no one document of this run - not left out")
            continue
        if path in seen:
            continue
        seen.add(path)
        out.append(dict(entry, document=path))
    return out


def split_items(text):
    """The items of a list value, as skills.split_items splits them (imported late: skills imports this module)."""

    from decurix_agent.computer import skills
    return skills.split_items(text)


def _ask(skill, params, context, files, note="", rows=True):
    """One model reading of `files` for `params`: (the JSON answer, the texts it read) or (None, texts)."""

    if not params or not available():
        return None, [context or ""]

    parts = [{"type": "text", "text": _prompt(skill, params, context, rows=rows) + ("\n" + note if note else "")}]
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
        office.drop_pages(doc["images"])  # (read into the parts above: the pictures are used up)

    if any(_is_date_param(p) for p in params):
        parts.append({"type": "text", "text": "\n" + _date_note(skill, texts)})

    try:
        message, _ = llm_client.chat(
            [{"role": "system", "content": SYSTEM}, {"role": "user", "content": parts}],
            max_tokens=3000 if len(files) <= MAX_FILES else 8000, response_format={"type": "json_object"},
            purpose="py.binding.fill",
        )
        answer = json.loads(message.get("content") or "{}")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"binding: could not fill values for {skill.get('name')}: {exc}")
        return None, texts
    return (answer if isinstance(answer, dict) else {}), texts


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


def _prompt(skill, params, context, rows=True):
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
        _rows_text(skill, params) if rows else "",
        "The request that started this run (it may include an email, and it names the files attached):",
        (context or "")[:6000],
    ])


def _clean(params, answer, texts=(), strict=False, relaxed=()):
    """Only values of the params asked for, in the shape each takes (strict: only those the texts hold - fill - but for
    the `relaxed` names)."""

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
            # (EMPTY_ITEM is what the document leaves empty: kept as it is, and held by its own document in fill_lists.)
            written = [v if v == EMPTY_ITEM else as_written(v, param.get("example"), texts) for v in items]
            if any(v != EMPTY_ITEM and drops_number(v, param.get("example")) for v in written):
                log(f"binding: {name} - an item has no number where the taught one had; left for the work to read")
                continue
            if strict and not all(v == EMPTY_ITEM or held_by(v, texts) for v in written):
                log(f"binding: {name} - not every item is in what this run was given; left for the work to read")
                continue
            values[name] = "; ".join(written)
        else:
            if isinstance(value, (list, dict)):
                continue
            notes = []  # what the harness says of this value, each whole
            written = as_written(str(value).strip(), param.get("example"), texts)
            if drops_number(written, param.get("example")):
                log(f"binding: {name} = {written!r} has no number where the taught {param.get('example')!r} had; "
                    "left for the work to read")
                continue
            if strict and name not in relaxed and not held_by(written, texts):
                log(f"binding: {name} = {written!r} is in nothing this run was given; left for the work to read")
                continue
            if sign_differs(written, texts):
                # Held by its amount (held_by is sign-blind on purpose), but this run's documents write it only with
                # the other sign: said to the check, never decided here (review 2026-11-03 rework, E-45).
                log(f"binding: {name} = {written!r} - the documents show this amount only with the other sign; kept, and said")
                notes.append(OTHER_SIGN)
            if numbered_like_example(written, param, texts):
                log(f"binding: {name} = {written!r} numbers this run's words the way the taught "
                    f"{param.get('example')!r} did, and nothing of this run says it - kept, and said")
                notes.append(NUMBERED)
            if name in relaxed and not held_by(written, texts):
                # Chosen from the application's list, read for this run (the same financial year as the teaching's is
                # often right): kept, and said - the plan shows it, the review before anything is filed checks it.
                notes.append(WORKED)
            elif is_example(written, param.get("example")) and not held_by(written, texts) and not (
                    re.search(r"[^\W\d_]{3,}", written) and words_held(written, texts)):
                # The person's own value from when they taught it, which nothing of this run says.
                log(f"binding: {name} = {written!r} is the taught example and this run's documents do not hold it - left out")
                continue
            if not words_held(written, texts) and not notes:
                # Said, never dropped (independent review, 2026-10-22): a value worked out from the documents is
                # written otherwise than they write it (28-Sep-2026 from 28/09/2026, "Private Limited" from "Pvt Ltd")
                # and is right. Found live (2026-10-22) the other way: a portal username made up from the
                # client's name, "srilakshmielev" - so the person and the check see that it is not the documents' own.
                log(f"binding: {name} = {written!r} is not written so in this run's documents - kept, and said")
                notes.append(WORKED)
            if notes:
                # Every note the harness makes on a value is kept whole, beside the model's own words (cut short): a
                # second note never overwrites the first (OTHER_SIGN was lost to NUMBERED), and no cut takes the end
                # of a mark the final check looks for (skill_task._assumed_note).
                old = why.get(name) if isinstance(why, dict) else None
                said = "; ".join(notes)
                evidence[name] = f"{str(old)[:150]} ({said})" if old else said
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


def drops_number(value, example):
    """
    Whether the taught example carries a number that `value` has none of: the value is what the thing means, not how
    it is written here. Found live (2026-11-01): taught with a folder "6. SEPTEMBER" among numbered siblings, a run
    was handed "December" from the request's words; the work made a folder "December" beside "8. NOVEMBER", and both
    workers did the same. Left out, the work reads the name off the screen, as the run without a skill did.
    """

    return bool(re.search(r"\d", str(example or ""))) and not re.search(r"\d", str(value or ""))


# What the fill says of a value it did not read as written (binding.report shows it; the final check weighs it as an
# assumption, skill_task._assumed_note).
WORKED = "not written so in this run's documents - worked out, or a guess: check it"
NUMBERED = ("its number is in nothing this run was given - the taught example's numbering, assumed: where the screen "
            "or the person names it another way, theirs is right")

# A date with its month in words: 12-Nov-2026, 3 September 2026, Sep 3, 2026.
NAMED_DATE = re.compile(r"(?<!\w)(?:\d{1,2}[\s./-]*[^\W\d_]{3,9}\.?[\s./,-]*\d{2,4}|[^\W\d_]{3,9}\.?\s+\d{1,2},?\s+\d{4})(?!\w)")


def numbered_like_example(value, param, texts):
    """
    Whether `value` is this run's words numbered the way the taught example was, by a number nothing of this run
    gives. Found live (2026-11-02, two runs of four): taught with a name made of a number and a word, the fill gave
    another run the same pattern with its own word and a number of its own working; the place it names was rightly
    called otherwise on the screen, the work followed the screen, and the final check held the run to the worked-out
    name and asked a person. Such a value is kept - the numbering is often right (a series the screen continues, a
    code that goes with a name) - and said, so the check weighs it as the assumption it is. A date is exempt (it is
    rewritten in the example's form on purpose), and so is a number the example itself carries.
    """

    example = str(param.get("example") or "")
    v = str(value or "")
    if _is_date_param(param) or DIGIT_DATE.search(v) or NAMED_DATE.search(v) or not any(texts):
        return False
    if not (re.search(r"\d", example) and re.search(r"[^\W\d_]{3,}", example)):
        return False
    if not (re.search(r"\d", v) and re.search(r"[^\W\d_]{3,}", v)):
        return False
    if re.findall(r"\d+", v) == re.findall(r"\d+", example):
        return False
    return not held_by(v, texts)


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
        # Held by its amount, whatever the sign either side writes: a credit typed as -500 is the document's "500",
        # and a discount the field takes as 500 is the document's "-500". Review 2026-11-03 (E-45): the minus was
        # dropped from the texts only, so a negative value was never held, and strict fill dropped every one.
        return any(_plain(n) == plain.lstrip("-") for n in re.findall(r"\d[\d,]*(?:\.\d+)?", joined))
    if re.search(r"(?<!\w)" + re.escape(plain) + r"(?!\w)", joined) is not None:
        return True
    if date_in(value, texts):
        return True
    return wrapped_in(plain, texts)


DATE_ORDERS = ("day", "month")


def date_in(value, texts):
    """
    The date token of the texts a digit date value names, or "": the value is written the way the field takes it (the
    application's order, when known, else either), the texts as the documents write their dates (a month by name, or
    digits in the source's order). "" when the value is no date, matches no date of the texts, or its two readings match
    two different dates written there (which one is meant is not code's to say). Found live (2026-11-08): a bill's
    "09-Oct-2026" filled as "09-10-2026" - how the form takes it - was "in nothing this run was given", every bill's date
    was dropped, and the model entered every bill by hand.
    """

    from decurix_agent import task_context, values
    from decurix_agent.computer import records

    if not values.parse_date(value):
        return ""
    app = task_context.app_date_order()
    readings = {values.as_iso(value, order) for order in ((app,) if app in DATE_ORDERS else DATE_ORDERS)} - {None}
    if not readings:
        return ""
    source = task_context.source_date_order()
    found = {}
    for text in texts:
        for m in records._DATE_TOKEN.finditer(text or ""):
            iso = values.as_iso(m.group(0), source)
            if iso in readings:
                found.setdefault(iso, m.group(0))
    return next(iter(found.values())) if len(found) == 1 else ""


# How many lines of a wrapped cell's row may stand between two lines of the cell: the row's other columns, read in
# between as a PDF lays the row out.
WRAP_LINES_BETWEEN = 1


def wrapped_in(plain, texts):
    """
    Whether a value of several words stands in a text as one table cell wrapped over lines does: each of its words,
    whole and in order, within one region of the text (lines with no blank line between: a row as the reader lays it
    out), each line of the cell at most WRAP_LINES_BETWEEN lines after the last. Found live (2026-11-08): a bill's item
    "Executive A4 Printing Paper 75 GSM (Box of 10 Reams)" is read from its PDF as "Executive A4 / Printing Paper 75 /
    1 4802 20 Reams 350.00 7,000.00 / GSM (Box of 10 / Reams)" - the row's other columns between the lines of the
    cell. Bound to the region and its lines, never to a count of characters (review of freeze 2, A11: a span of
    characters tuned to one PDF let words of two rows stand for one value).
    """

    words = re.findall(r"\w+", str(plain or "").casefold())
    if len(words) < 2:
        return False
    patterns = [re.compile(r"(?<!\w)" + re.escape(w) + r"(?!\w)") for w in words]
    for text in texts:
        for region in re.split(r"\n[ \t]*\n", str(text or "").casefold()):
            lines = region.split("\n")
            if _cell_in(lines, patterns):
                return True
    return False


def _cell_in(lines, patterns):
    """Whether the words (patterns) stand in order in these lines of one region as a cell's lines do: each next word
    straight after its word on the same line (nothing but spaces and marks between), or on a later line at most
    WRAP_LINES_BETWEEN lines after it, where the cell goes on."""

    for row, line in enumerate(lines):
        for first in patterns[0].finditer(line):
            at_row, at = row, first.end()
            for pattern in patterns[1:]:
                found = None
                m = pattern.search(lines[at_row], at)
                if m and not re.search(r"\w", lines[at_row][at:m.start()]):
                    found = (at_row, m.end())
                else:
                    for r in range(at_row + 1, min(len(lines), at_row + WRAP_LINES_BETWEEN + 2)):
                        m = pattern.search(lines[r])
                        if m:
                            found = (r, m.end())
                            break
                if found is None:
                    break
                at_row, at = found
            else:
                return True
    return False


OTHER_SIGN = "this run's documents show this amount only with the other sign"


def sign_differs(value, texts):
    """Whether `value` is a bare number whose amount this run's texts hold only with the other sign ("-500" where they
    say 500, or 500 where they say -500). The fact goes to the check as evidence; nothing is refused on it."""

    plain = _plain(value)
    if not plain or not BARE_NUMBER.match(plain) or not held_by(value, texts):
        return False
    joined = " ".join(" ".join(t.split()) for t in texts if t).casefold()
    signed = []
    for m in re.finditer(r"(-?)(\d[\d,]*(?:\.\d+)?)", joined):
        # A minus right after a word ("inv-500") joins the word; it is no sign.
        minus = m.group(1) and not (m.start() and re.match(r"\w", joined[m.start() - 1]))
        signed.append(_plain(("-" if minus else "") + m.group(2)))
    return plain not in signed


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


# ============================================================
# A value the documents give by arithmetic (derive)
# ============================================================

DERIVE_SYSTEM = (
    "A task replays a person's taught steps with this run's values. These values are not written anywhere as they are, "
    "but this request's documents may give each one by simple arithmetic: a total of figures across documents (the "
    "taxable value of every invoice of a kind), a count of documents, or one figure copied - or zero (op \"none\", no "
    "figures) when no document has such a figure at all (no cess on any invoice). For each value, answer "
    "the operation and EVERY figure it uses, each with the file name it stands in, written exactly as that file writes "
    "it - or null when the documents and the request do not settle it (never a guess). JSON only: {\"values\": {NAME: "
    "{\"op\": \"sum\"|\"count\"|\"copy\"|\"none\", \"value\": \"...\", \"figures\": [{\"document\": \"file name\", \"figure\": "
    "\"...\"}], \"why\": \"which documents and why those\"} or null}}"
)
# How much of each document, and of all of them, a derivation reads.
DERIVE_DOC_CHARS = 6000
DERIVE_TOTAL_CHARS = 40000


def _amount(text):
    """A figure as a Decimal ("1,20,000.00" -> 120000.00), or None."""

    from decimal import Decimal, InvalidOperation
    cleaned = re.sub(r"[^\d.\-]", "", str(text or "").replace(",", ""))
    if not cleaned or cleaned in ("-", ".") or cleaned.count(".") > 1:
        return None
    try:
        return Decimal(cleaned)
    except InvalidOperation:
        return None


def _figures_in(text):
    """Every number a text writes, as Decimals."""

    out = set()
    for m in re.finditer(r"(?<![\w.])\d[\d,]*(?:\.\d+)?(?![\w])", str(text or "")):
        n = _amount(m.group(0))
        if n is not None:
            out.add(n)
    return out


def checked_derivation(answer, texts):
    """
    The value an answered derivation gives, or None: every figure must stand in the document it names (`texts`:
    {file name: text}), and the operation must give the value exactly - a sum of the figures, the count of them, or the
    one figure. Decided by code: the model only proposes.
    """

    if not isinstance(answer, dict):
        return None
    op, value = str(answer.get("op") or ""), _amount(answer.get("value"))
    figures = answer.get("figures") if isinstance(answer.get("figures"), list) else []
    if op == "none" and value is not None and value == 0 and not figures and str(answer.get("why") or "").strip():
        # (no document has such a figure at all - no cess, no cancelled invoice: zero, said why, and marked worked out)
        return str(answer.get("value")).strip()
    if value is None or not figures or op not in ("sum", "count", "copy"):
        return None
    amounts = []
    for f in figures:
        if not isinstance(f, dict):
            return None
        name, figure = os.path.basename(str(f.get("document") or "")), _amount(f.get("figure"))
        text = texts.get(name)
        if text is None or figure is None or figure not in _figures_in(text):
            return None
        amounts.append(figure)
    if op == "sum" and sum(amounts) == value and len(amounts) == 1:
        # (a sum of one figure is that figure. A sum of several is a choice of which documents count - the B2B invoice
        # in the B2C total - which code cannot check: the model makes it where the work is, with the screen. Found in
        # the learning curves (2026-11-17): a B2C HSN total summed in a B2B invoice, and the filing went wrong.)
        return str(answer.get("value")).strip()
    if op == "count" and value == len(amounts) and len({(os.path.basename(str(f.get("document"))), str(f.get("figure")))
                                                         for f in figures}) == len(amounts):
        return str(answer.get("value")).strip()
    if op == "copy" and len(amounts) == 1 and amounts[0] == value:
        return str(answer.get("value")).strip()
    return None


def derive(skill, params, context, files):
    """
    ({name: value}, {name: evidence}) for `params` this run's documents give by arithmetic (a total across invoices, a
    count, a copied figure): one model reading proposes each with the figures it uses, and code keeps only what
    checked_derivation proves. ({}, {}) when there is no model or nothing is proven. Measured in the learning curves
    (2026-11-17): a month's B2C total, written in no document, made a whole part of a filing the model's - 43 turns, 1.2M
    tokens - for one number.
    """

    if not params or not files or not available():
        return ({}, {})
    texts, used = {}, 0
    for path in files:
        if used >= DERIVE_TOTAL_CHARS:
            break
        try:
            text = office.read_document(path)["text"][:DERIVE_DOC_CHARS]
        except Exception:  # noqa: BLE001 - a document not read gives nothing
            continue
        texts[os.path.basename(path)] = text
        used += len(text)
    if not texts:
        return ({}, {})
    # Where each value goes - the page and the part of it the taught step types into ("7 - B2C (Others)"), and the
    # person's own notes: which documents count is decided by that, never by the label alone. Found in the learning
    # curves (2026-11-17): a B2C total summed in the inter-state B2C Large invoice that has a table of its own.
    steps = {s.get("n"): s for s in (skill or {}).get("steps") or []}

    def where(p):
        n = p.get("step") or 0
        before = [steps[k].get("summary") or "" for k in range(max(1, n - 5), n) if k in steps]
        here = steps.get(n) or {}
        return (f"typed at: {here.get('summary') or ''}, on \"{here.get('window') or ''}\"; the steps just before it: "
                + "; ".join(x for x in before if x))

    notes = [str(x.get("text") or "") for x in (skill or {}).get("notes") or [] if isinstance(x, dict)]
    asked = "\n".join(f"- {p['name']}: {p.get('label') or p['name']}"
                      + (f" (when taught: {p.get('example')})" if p.get("example") else "") + f" - {where(p)}"
                      for p in params)
    content = (f"The request:\n{context}\n\nThe values:\n{asked}\n\n"
               + (("What the person said when they taught it:\n" + "\n".join(f"- {n}" for n in notes[:12]) + "\n\n")
                  if notes else "")
               + "Count a document only where the value goes - a figure that belongs in another table or section of "
                 "the same form is not part of this one.\n\nThe documents:\n"
               + "\n\n".join(f"--- {name} ---\n{text}" for name, text in texts.items()))
    try:
        message, _ = llm_client.chat([{"role": "system", "content": DERIVE_SYSTEM}, {"role": "user", "content": content}],
                                     max_tokens=16000, response_format={"type": "json_object"},
                                     purpose="py.binding.derive")
        found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
        answer = (json.loads(found.group(0)) if found else {}).get("values") or {}
    except Exception as exc:  # noqa: BLE001 - not derived: the part stays the model's
        log(f"binding: the values could not be derived ({exc})")
        return ({}, {})
    values, evidence = {}, {}
    for p in params:
        proposed = answer.get(p["name"]) if isinstance(answer, dict) else None
        value = checked_derivation(proposed, texts)
        if value is None:
            if proposed:
                log(f"binding: {p['name']} proposed as {json.dumps(proposed)[:200]} - not proven, left to the model")
            continue
        values[p["name"]] = value
        figures = ", ".join(f"{f.get('figure')} ({os.path.basename(str(f.get('document')))})"
                            for f in proposed.get("figures") or [])
        evidence[p["name"]] = f"{proposed.get('op')} of {figures} - checked by code: {str(proposed.get('why') or '')[:160]}"
    return values, evidence
