"""
Facts about a long request's stage result that code decides where it is precise (long_task.stage_finished).

- removed_residue: a record the person removed mid-run (an amendment the model read as removing it: "exclude bill
  <ref>") whose figures may still be inside a total. Found live (2026-10-20): the bill's row left the register, but a
  summary sheet's total stayed the register's total plus that bill's figure.

Nothing here knows an application, a tax or a customer: tables, figures and names. Which of these facts may fail a
stage is long_task.CHECK_MATURITY's (docs/harness-audit.md): only domain-free invariants block; the rest is evidence.
"""

import re
from pathlib import Path

from decurix_agent.logging_utils import log
from decurix_agent.provenance import _squash as _ref_key
from decurix_agent.sources import _money as _fmt

TABLE_KINDS = {".xlsx", ".xlsm", ".csv", ".tsv"}

# A record's reference in words: letters, then digits (AB-0101, INV/2026/12, PO 7781).
RECORD_ID = re.compile(r"\b[A-Z][A-Z0-9]{1,9}(?:[-/][A-Z0-9]{1,10}){1,3}\b")
MONEY = re.compile(r"(?<![\d.])\d{1,3}(?:,\d{2,3})+(?:\.\d{1,2})?(?![\d])|(?<![\d.,])\d{3,}\.\d{2}(?![\d])")


def _number(text):
    try:
        return float(str(text).replace(",", "").strip())
    except (TypeError, ValueError):
        return None


def _header_at(rows, within=10):
    """Where a table's header is: the first of its first rows with two or more filled cells, or None."""

    return next((i for i, row in enumerate(rows[:within]) if sum(1 for c in row if c not in (None, "")) >= 2), None)


def _refs(row):
    """The item references (ITEM_ID) a row's cells hold, sorted."""

    return sorted({r for c in row if c not in (None, "") for r in ITEM_ID.findall(str(c))})


def _status_cells(made):
    """(label, header, row, column, text) of each filled status/action cell of the made tables."""

    for label, header, rows in tables_of(made):
        cols = [i for i, h in enumerate(header) if STATUS_COLUMN.search(h or "")]
        for row in rows:
            for i in cols:
                cell = str(row[i]) if i < len(row) and row[i] not in (None, "") else ""
                if cell:
                    yield label, header, row, i, cell


# ============================================================
# A removed record still inside a total
# ============================================================

def removed_records(amendments):
    """
    [record reference] that the person's changes took out of the work, in order: what the model read each change as
    removing ("removed", whole records - a line of a record is "removed_lines" and never here) less what a later change
    put back ("restored"). long_task.classify keeps only references the person's own message writes. An amendment with
    no such field removes nothing: no word list reads the person's words (harness review, 2026-10-21 - "Don't remove
    INV-2026-001 after all" and "Exclude the freight line from PO-7781's total" were both read as removals).
    """

    out = []
    for amendment in amendments or []:
        for ref in amendment.get("removed") or []:
            if _ref_key(ref) and _ref_key(ref) not in {_ref_key(r) for r in out}:
                out.append(str(ref))
        restored = {_ref_key(r) for r in amendment.get("restored") or []}
        out = [r for r in out if _ref_key(r) not in restored]
    return out


def names_in(name, text):
    """Whether `text` names `name` as a whole token, separators aside ("PO-1" in "po1_bill", "PO 1.pdf"), never as part
    of a longer one: "po1" is not in "po12.pdf". Found in review (2026-11-02): a substring match took another record's
    file."""

    chars = [c for c in str(name or "").lower() if c.isalnum()]
    if not chars:
        return False
    pattern = r"(?<![a-z0-9])" + r"[^a-z0-9]*".join(re.escape(c) for c in chars) + r"(?![a-z0-9])"
    return bool(re.search(pattern, str(text or "").lower()))


def record_figures(reference, documents):
    """The money figures of a removed record, from the input document that is that record (its name or its text)."""

    from decurix_agent import payees
    figures = set()
    for doc in documents:
        text = ""
        if names_in(reference, Path(doc).stem):
            text = payees._doc_text(doc)
        if not text:
            continue
        for found in MONEY.findall(text):
            value = _number(found)
            if value and value >= 100:
                figures.add(round(value, 2))
    return figures


def _sheets(path):
    """[(sheet name, [[cell values]])] of a workbook (values as stored), or of a CSV as one sheet."""

    path = Path(path)
    if path.suffix.lower() in {".xlsx", ".xlsm"}:
        import openpyxl
        wb = openpyxl.load_workbook(path, data_only=True, read_only=True)
        try:
            return [(ws.title, [list(r) for r in ws.iter_rows(values_only=True)]) for ws in wb.worksheets]
        finally:
            wb.close()
    import csv
    with open(path, encoding="utf-8", errors="replace", newline="") as handle:
        return [(path.name, [row for row in csv.reader(handle, delimiter="\t" if path.suffix.lower() == ".tsv" else ",")])]


def _column_sums(rows):
    """{(column header, filter words): sum} for a table's numeric columns: all rows, and the rows where a column with a
    few repeated values (Yes/No, a status) holds each value. Total rows are left out."""

    header_at = _header_at(rows)
    if header_at is None:
        return {}
    header = [str(c or "").strip() for c in rows[header_at]]
    body = [r for r in rows[header_at + 1:] if any(c not in (None, "") for c in r)
            and not any(isinstance(c, str) and re.match(r"\s*(?:grand\s+)?total\b", c, re.I) for c in r[:3])]
    if len(body) < 2:
        return {}
    numeric = [i for i, h in enumerate(header) if h and sum(1 for r in body if i < len(r) and isinstance(_number(r[i]), float)
                                                            and not isinstance(r[i], str)) >= max(2, len(body) // 2)]
    categorical = []
    for i, h in enumerate(header):
        values = {str(r[i]).strip() for r in body if i < len(r) and isinstance(r[i], str) and r[i].strip()}
        if h and i not in numeric and 2 <= len(values) <= 5 and all(len(v) <= 20 for v in values):
            categorical.append((i, h, values))
    sums = {}
    for i in numeric:
        sums[(header[i], "")] = round(sum(_number(r[i]) or 0 for r in body if i < len(r)), 2)
        for k, h, values in categorical:
            for value in values:
                chosen = [r for r in body if k < len(r) and str(r[k]).strip() == value]
                if 2 <= len(chosen) < len(body):
                    sums[(header[i], f"{h} = {value}")] = round(sum(_number(r[i]) or 0 for r in chosen if i < len(r)), 2)
    return sums


# A cell that is a figure written as text: a CSV's every cell is text ("66700", "4,533.00", "(1,200)").
_FIGURE_TEXT = re.compile(r"^\s*[-+]?\(?\s*(?:₹|rs\.?|inr)?\s*\d[\d,]*(?:\.\d+)?\s*\)?\s*$", re.I)


def _figure(cell):
    """A cell's figure: a number as stored, or a text that is only a number (as every cell of a CSV is); else None.
    Found in review (2026-11-02): only stored numbers counted, so in a CSV no removed record ever had figures and the
    checks of removed records found nothing."""

    if isinstance(cell, bool):
        return None
    if isinstance(cell, (int, float)):
        return float(cell)
    if isinstance(cell, str) and _FIGURE_TEXT.match(cell):
        value = _number(re.sub(r"(?i)₹|rs\.?|inr|[()\s]", "", cell))
        return -value if value is not None and "(" in cell else value
    return None


def _as_figures(rows):
    """A CSV's rows with each cell that is only a number read as that number (a workbook stores them so already)."""

    return [[(_figure(c) if isinstance(c, str) and _FIGURE_TEXT.match(c) else c) for c in row] for row in rows]


def removed_still_present(amendments, made):
    """
    Sentences for each record the person removed that a made table still has as a row of its own: a cell that IS the
    record's reference (the whole cell, not a word inside a note), on a row with figures. Mechanical, and evidence for
    the judge: whether that row shows the record taken out ("cancelled", "taken out at your request", in any wording) is
    a reading, which the judge makes from the row's text quoted here. A row that only mentions the reference inside
    other text (a credit note "against" it) is not the record's own row. Found (review 2026-11-03, E-07): a word list
    decided "shown as taken out", and a correct row worded differently was refused.
    """

    removed = removed_records(amendments)
    if not removed:
        return []
    out = []
    for label, header, rows in sheet_tables(made):
        for row in rows:
            cells = [c for c in row if c not in (None, "")]
            own = {str(c).strip().lower() for c in cells}
            for reference in removed:
                if reference.strip().lower() not in own:
                    continue
                if not any(_figure(c) not in (None, 0) for c in cells if str(c).strip().lower() != reference.lower()):
                    continue  # (its row carries no figures: nothing of it can be counted)
                text = " | ".join(str(c) for c in cells)
                out.append(f"{label}: {reference} has a row of its own with figures (\"{text[:120]}\") though the "
                           "person removed it - check that the row shows it taken out and that no total counts it")
    return list(dict.fromkeys(out))[:8]


def removed_residue(amendments, made, documents):
    """
    Sentences for each figure in a made workbook that is a column's total (or its total over the rows of one kind)
    plus exactly one money figure of a record the person removed: the record may have left the table while its figures
    stayed in the total. [] when nothing was removed or nothing adds up so. EVIDENCE only: with many sums and many
    figures an unrelated total can add up the same way (review 2026-11-03, E-08: a courier charge equal to the removed
    bill's freight line refused a correct total).
    """

    removed = removed_records(amendments)
    if not removed:
        return []
    figures = {}
    for reference in removed:
        for value in record_figures(reference, documents):
            figures.setdefault(value, reference)
    if not figures:
        return []
    out = []
    for path, sheets in _table_files(made):
        if Path(str(path)).suffix.lower() in {".csv", ".tsv"}:
            sheets = [(name, _as_figures(rows)) for name, rows in sheets]
        totals = {}
        for name, rows in sheets:
            for (column, kind), value in _column_sums(rows).items():
                if value > 0:
                    totals.setdefault(value, []).append((name, column, kind))
        for name, rows in sheets:
            header_at = _header_at(rows) or 0
            header = [str(c or "").strip() for c in rows[header_at]] if rows else []
            for row in rows[header_at + 1:]:
                label = next((str(c).strip() for c in row if isinstance(c, str) and c.strip()), "")
                for i, cell in enumerate(row):
                    if isinstance(cell, str) or _number(cell) is None:
                        continue
                    value = round(float(cell), 2)
                    column = header[i] if i < len(header) else ""
                    for figure, reference in figures.items():
                        base = round(value - figure, 2)
                        places = [w for w in totals.get(base, []) if w[0] != name or w[1] != column]
                        # The same column's total first (Tax A with Tax A), when two columns add up alike.
                        where = next((w for w in places if w[1].lower() == column.lower()), places[0] if places else None)
                        if where and base > 0:
                            out.append(f"{Path(str(path)).name}: {name} \"{label}\" {column} {_fmt(value)} is "
                                       f"{where[0]}'s {where[1]}{' (' + where[2] + ')' if where[2] else ''} "
                                       f"{_fmt(base)} plus {_fmt(figure)} of {reference}, which was removed - the "
                                       "total may still include it (an arithmetic match only: check how it is made)")
                            break
    return list(dict.fromkeys(out))[:8]


def sheet_tables(paths):
    """
    [(label, header, rows)] of every table in these files: a sheet may hold several, one under another (a per-customer
    ageing, the receipts applied, a ranking), each after a blank row and its own header - read as one, a later table's
    dates and ranks were taken for the first one's parties. Found live (2026-10-20).
    """

    out = []
    for _, label, rows in _labelled_sheets(paths):
        segment = []
        for row in list(rows) + [[]]:
            if any(c not in (None, "") for c in row):
                segment.append(row)
                continue
            header_at = _header_at(segment, 4)
            if header_at is not None and len(segment) > header_at + 1:
                out.append((label, [str(c or "").strip() for c in segment[header_at]], segment[header_at + 1:]))
            segment = []
    return out


def _table_files(paths):
    """(path, [(sheet name, rows)]) of the tables among these files (_sheets); a file that cannot be read is logged and
    left out (it decides nothing here)."""

    for path in paths:
        if Path(str(path)).suffix.lower() not in TABLE_KINDS:
            continue
        try:
            sheets = _sheets(path)
        except Exception as exc:  # noqa: BLE001
            log(f"stage_facts: could not read {path}: {exc}")
            continue
        yield path, sheets


def _labelled_sheets(paths):
    """(path, label, rows) of each sheet of the tables among these files (a workbook's sheets, a CSV), values as stored;
    a file that cannot be read is logged and left out."""

    for path, sheets in _table_files(paths):
        for sheet, rows in sheets:
            yield path, f"{Path(str(path)).name} ({sheet})" if sheet != Path(str(path)).name else Path(str(path)).name, rows


def tables_of(paths):
    """[(label, header, rows)] of the tables in these files (a workbook's sheets, a CSV), values as stored."""

    return [(label, header, rows) for _, label, header, rows, _ in _tables_with_rows(paths)]


# ============================================================
# An earlier finding a re-check dropped
# ============================================================
# Found live (2026-10-20): re-run for "leave the cancelled bill out", the stage rebuilt its register and lost an
# unrelated finding - a row flagged and held showed "Included" again, while another output still held it out.

FLAG_WORDS = re.compile(r"\b(?:flag\w*|held|hold|duplicat\w*|mismatch\w*|exception\w*|ineligible|not eligible|excluded|"
                        r"disput\w*|wrong|short\w*|over-?billed|blocked|reject\w*|discrepan\w*|kept out|not claimed)\b",
                        re.I)


def earlier_findings(texts, amendments=()):
    """[(record reference, the sentence)] of exceptions, flags and holds in `texts` (a stage's carried facts and its
    previous report), leaving out records the person's changes name."""

    changed = {_ref_key(r) for a in amendments or [] for r in RECORD_ID.findall(str(a.get("text") or ""))}
    out, seen = [], set()
    for text in texts:
        for line in re.split(r"\n+|(?<=[.;])\s+", str(text or "")):
            line = " ".join(line.strip(" -*\t").split())
            if not line or not FLAG_WORDS.search(line):
                continue
            for ref in RECORD_ID.findall(line):
                key = _ref_key(ref)
                if any(c.isdigit() for c in ref) and key not in changed and key not in seen:
                    seen.add(key)
                    out.append((ref, line[:300]))
    return out


# A cell that marks its row: a flag word, a comparison ("billed at 18% vs 12%"), or a bare No (eligible: No).
_MARKS = re.compile(r"\b(?:vs\.?|versus|instead of)\b|^\s*(?:no|n)\s*$", re.I)


def _flagged(cell):
    return bool(FLAG_WORDS.search(cell) or _MARKS.search(cell))


def lost_findings(findings, made):
    """
    Sentences for each earlier finding whose record is in a made table (a cell that is its reference) with no flag, hold
    or note on any of its rows any more. A record that is nowhere decides nothing.
    """

    if not findings:
        return []
    tables = tables_of(made)
    out = []
    for ref, said in findings:
        key = _ref_key(ref)
        rows = [(label, row) for label, _header, body in tables for row in body
                if any(_ref_key(c) == key for c in row if c not in (None, ""))]
        if rows and not any(_flagged(c) for _label, row in rows for c in row if isinstance(c, str)):
            out.append(f"{ref} was flagged before (\"{said}\") and is not flagged now in {rows[0][0]} - keep every earlier "
                       "finding the change does not touch")
    return out



# ============================================================
# A status recorded for an item that another output contradicts
# ============================================================
# Found live (2026-10-20): a tracker's Action said an item was still to be assessed, while the next stage's reply
# drafts approved it as the request says - the record and the drafts disagreed, and the cross-check (figures only) did
# not see it.

ITEM_ID = re.compile(r"\b[A-Z]{1,6}[-/]?\d{2,8}(?:[-/]\d{1,6})?\b")
STATUS_COLUMN = re.compile(r"\b(?:status|action|decision|outcome|resolution|next step)\b", re.I)
# What a status says, by the earliest word of one of these kinds in it.
_KINDS = [
    ("open", re.compile(r"\b(?:assess\w*|review\w*|to (?:check|decide|confirm)|investigat\w*|tbd|to be decided|"
                        r"check against)\b", re.I)),
    ("approved", re.compile(r"\b(?:approv(?:e|ed|al granted)|accept\w*|releas\w*)\b", re.I)),
    ("for approval", re.compile(r"\b(?:for (?:your |my |the person's )?approval|pending approval|awaiting approval|"
                                r"on hold|held|hold)\b", re.I)),
    ("rejected", re.compile(r"\b(?:reject\w*|declin\w*|not approved|refus\w*)\b", re.I)),
]
# What a draft tells its recipient, in words that commit to a status.
_DRAFT_KINDS = [
    ("approved", re.compile(r"\b(?:(?:has|have) been approved|(?:is|are) approved|we (?:have )?approved|approved "
                            r"your)\b", re.I)),
    ("for approval", re.compile(r"\b(?:under review|being reviewed|for approval|pending approval|we will (?:review|"
                                r"get back))\b", re.I)),
    ("rejected", re.compile(r"\b(?:cannot approve|can ?not be approved|has been declined|is declined|rejected)\b", re.I)),
]


def _kind(text, kinds):
    best = None
    for name, pattern in kinds:
        m = pattern.search(text or "")
        if m and (best is None or m.start() < best[1]):
            best = (name, m.start())
    return best[0] if best else None


# The column whose value identifies a row's item: an order / invoice / reference number, the source message's file, a
# record id - never a product, SKU, line or category code that many rows share. Found live (2026-10-20): one product
# code on one customer's held line and on another's released line was taken for one item in two states.
KEY_COLUMN = re.compile(r"\b(?:order|invoice|so|po|reference|ref|ticket|case|record|file|message|e-?mail id)\b", re.I)
NOT_KEY_COLUMN = re.compile(r"\b(?:sku|product|item code|item|line|category|code|description|qty|quantity)\b", re.I)


_REF_FIRST = re.compile(r"\b(?:order|invoice|so|po|reference|ref|ticket|case|record)\b", re.I)


def _key_columns(header, rows):
    """
    The table's key columns (KEY_COLUMN, not NOT_KEY_COLUMN) whose every value names one item only: a value shared by
    rows that differ in another key column ("WA-1810" for two customers' chats) identifies nothing. An order or
    reference column first, the source file after.
    """

    cols = [i for i, h in enumerate(header) if h and KEY_COLUMN.search(h) and not NOT_KEY_COLUMN.search(h)
            and not STATUS_COLUMN.search(h)]

    def cell(row, i):
        return str(row[i]).strip() if i < len(row) and row[i] not in (None, "") else ""
    usable = []
    for i in cols:
        identity = {}
        ok = True
        for row in rows:
            value = cell(row, i)
            if not value:
                continue
            whole = tuple(cell(row, j) for j in cols)
            if identity.setdefault(value, whole) != whole:
                ok = False
                break
        if ok:
            usable.append(i)
    usable.sort(key=lambda i: 0 if _REF_FIRST.search(header[i]) else 1)
    return usable


def _row_keys(row, cols):
    """The row's values in the table's key columns (_key_columns), or [] when it cannot be told."""

    return [str(row[i]).strip() for i in cols if i < len(row) and row[i] not in (None, "") and str(row[i]).strip()]


def status_conflicts(made, drafts=()):
    """
    Sentences for each item whose status one output records - a status/action/decision column of a made table, the
    row's item told by its own key column (_row_key) - and a DIFFERENT output contradicts: another table's row with the
    same key, or what an email draft that names that key tells its recipient. Rows of one table never conflict with
    each other; a row whose key cannot be told is not compared. [] when nothing disagrees.
    """

    seen = {}  # key -> {where: (kind, words)}
    aliases = {}  # each of a row's key values -> the key it is filed under
    for label, header, rows in tables_of(made):
        cols = [i for i, h in enumerate(header) if STATUS_COLUMN.search(h or "")]
        if not cols:
            continue
        keyed = _key_columns(header, rows)
        for row in rows:
            keys = _row_keys(row, keyed)
            words = " | ".join(str(row[i]) for i in cols if i < len(row) and row[i] not in (None, ""))
            kind = _kind(words, _KINDS)
            if not keys or not kind:
                continue
            key = next((aliases[k] for k in keys if k in aliases), keys[0])
            for k in keys:
                aliases.setdefault(k, key)
            seen.setdefault(key, {}).setdefault(label, (kind, words[:120]))
    for draft in drafts or ():
        text = f"{draft.get('subject') or ''}\n{draft.get('body') or ''}"
        kind = _kind(text, _DRAFT_KINDS)
        if not kind:
            continue
        where = f"the draft to {', '.join(draft.get('to') or [])}"
        low = text.lower()
        for alias, key in aliases.items():
            if len(alias) >= 4 and alias.lower() in low:
                seen[key].setdefault(where, (kind, str(draft.get("subject") or "")[:80]))
    out = []
    for key, places in seen.items():
        if len(places) > 1 and len({k for k, _ in places.values()}) > 1:
            said = "; ".join(f"{where} says {kind} (\"{words}\")" for where, (kind, words) in list(places.items())[:4])
            out.append(f"{key}: {said}")
    return out[:10]


# ============================================================
# Work left as an instruction; a reference with no evidence
# ============================================================
# Found live (2026-10-20): a tracker's Action read "calculate affected units x unit price from order" - work the stage
# could do itself, left for someone else - and an automatic reply was given another email's order number.

DEFERRED = re.compile(r"\b(?:calculate|compute|work out|to be (?:calculated|computed|checked|confirmed|decided)|"
                      r"verify (?:the )?amount|determine (?:the )?amount)\b", re.I)
REFERENCE_COLUMN = re.compile(r"\b(?:order|invoice|so|po|reference|ref)\b\.?\s*(?:no|number|#|id)?\b", re.I)
_AUTO_HEADERS = re.compile(r"^(?:auto-submitted:\s*auto-|x-autoreply:|x-autorespond:|precedence:\s*auto_reply)", re.I | re.M)
_AUTO_SUBJECT = re.compile(r"^subject:\s*(?:automatic reply|auto(?:matic)?[- ]?reply|out of (?:the )?office)", re.I | re.M)


def deferred_actions(made):
    """Sentences for each status/action cell of a made table that hands work the stage could do to someone else
    ("calculate ...", "check against policy"), with its row's reference."""

    out = []
    for label, header, row, i, cell in _status_cells(made):
        if DEFERRED.search(cell):
            out.append(f"{label}: {', '.join(_refs(row)) or 'a row'} - {header[i]} says \"{cell[:140]}\"")
    return out[:10]


def unsupported_references(made, messages):
    """
    Sentences for each row of a made table that gives an automatic reply (Auto-Submitted, "Automatic reply" ...) a
    reference (an order, an invoice) its own message never mentions: a reference is recorded for an item only when the
    item's own content gives it or a lookup by it matches - never carried over from another item of the same sender.
    """

    by_name = {}
    for path in messages or ():
        try:
            by_name[Path(str(path)).name.lower()] = Path(str(path)).read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
    if not by_name:
        return []
    out = []
    for label, header, rows in tables_of(made):
        refs = [i for i, h in enumerate(header) if REFERENCE_COLUMN.search(h or "") and not STATUS_COLUMN.search(h or "")]
        if not refs:
            continue
        for row in rows:
            name = next((Path(str(c)).name.lower() for c in row if c not in (None, "")
                         and Path(str(c)).name.lower() in by_name), "")
            if not name:
                continue
            text = by_name[name]
            if not (_AUTO_HEADERS.search(text) or _AUTO_SUBJECT.search(text)):
                continue
            for i in refs:
                value = str(row[i]).strip() if i < len(row) and row[i] not in (None, "") else ""
                if value and value.lower() not in ("-", "n/a", "na", "none") and value.lower() not in text.lower():
                    out.append(f"{label}: {name} is an automatic reply, and {header[i]} says {value}, which its own "
                               "message never mentions")
    return out[:10]



# ============================================================
# Computed amounts that cannot be reproduced; a row that contradicts itself
# ============================================================
# Found live (2026-10-20): an Action stated an amount for an item that its order's quantity and price do not give -
# no working anywhere to show where it came from; and a message saying it wanted no X was filed in the category X while
# its own Action said "no X requested".

QTY_COLUMN = re.compile(r"\b(?:qty|quantity|units?|count|no\.? of)\b", re.I)
PRICE_COLUMN = re.compile(r"\b(?:unit price|price|rate|unit cost|cost per|per unit)\b", re.I)
RESULT_COLUMN = re.compile(r"\b(?:result|amount|total|value|line total|computed)\b", re.I)
SOURCE_COLUMN = re.compile(r"\b(?:source|from|where|taken from)\b", re.I)
MONEY_IN_TEXT = re.compile(r"(?:rs\.?|inr|₹)\s*([\d,]+(?:\.\d{1,2})?)", re.I)
# Which amounts a status cell states need a working is not told by the kind of amount (review 2026-11-03, E-28: a
# list of business words decided it): any money amount a status cell states is one. Evidence only, never a block.
# The factors a working may record besides quantity and price, and the columns that are no factor at all.
# A percentage factor: a column whose header says it is a percentage - whatever the percentage is of (review
# 2026-11-03, E-28: a list of tax names decided it). A working applies it on top of quantity x price, as it does a
# discount below it (reproduces); a discount column is told first (_working_columns).
PERCENT_COLUMN = re.compile(r"%|\bpct\b|\bper ?cent\b", re.I)
DISCOUNT_COLUMN = re.compile(r"\b(?:discount|disc\.?|rebate)\b", re.I)
SUBTOTAL_COLUMN = re.compile(r"\b(?:taxable|subtotal|sub-total|net|gross|base|before tax|excl\w*)\b", re.I)
IDENT_COLUMN = re.compile(r"\b(?:line|no\.?|number|row|id|code|#|days?|terms|year|month|page|sl|sr|s\.no)\b", re.I)


def _working_columns(header):
    """{kind: [column]} of a working table: qty, price, tax, discount, result, subtotal, ident, source - and the numeric
    columns of no known kind under "other"."""

    kinds = {k: [] for k in ("qty", "price", "tax", "discount", "result", "subtotal", "ident", "source")}
    for i, h in enumerate(header):
        h = str(h or "")
        if re.search(r"(?i)\bsource\b|\btaken from\b|\bread from\b", h):
            kinds["source"].append(i)  # (before the rest: "Unit Price Source" says where, it is no price)
        elif DISCOUNT_COLUMN.search(h):  # (before a percentage: "Discount %" is a discount)
            kinds["discount"].append(i)
        elif PERCENT_COLUMN.search(h):
            kinds["tax"].append(i)
        elif PRICE_COLUMN.search(h):  # (before quantity: "Unit price" is a price)
            kinds["price"].append(i)
        elif QTY_COLUMN.search(h):
            kinds["qty"].append(i)
        elif SUBTOTAL_COLUMN.search(h):
            kinds["subtotal"].append(i)
        elif RESULT_COLUMN.search(h):
            kinds["result"].append(i)
        elif SOURCE_COLUMN.search(h):
            kinds["source"].append(i)
        elif IDENT_COLUMN.search(h):
            kinds["ident"].append(i)
    return kinds


_CELL_REF = re.compile(r"\$?([A-Z]{1,3})\$?(\d+)")


def _formulas(path):
    """{sheet: {(excel row, column index): formula}} of a workbook's formula cells; {} for anything else."""

    if Path(str(path)).suffix.lower() not in {".xlsx", ".xlsm"}:
        return {}
    try:
        import openpyxl
        book = openpyxl.load_workbook(path, data_only=False, read_only=True)
        try:
            out = {}
            for sheet in book.worksheets:
                cells = {}
                for r, row in enumerate(sheet.iter_rows(values_only=True), 1):
                    for c, value in enumerate(row):
                        if isinstance(value, str) and value.startswith("="):
                            cells[(r, c)] = value
                out[sheet.title] = cells
            return out
        finally:
            book.close()
    except Exception as exc:  # noqa: BLE001 - what cannot be read is not decided
        log(f"stage_facts: could not read the formulas of {path}: {exc}")
        return {}


def _column_index(letters):
    n = 0
    for ch in letters:
        n = n * 26 + ord(ch) - 64
    return n - 1


def _commas_only_in_round(expr):
    """Whether every comma of an expression is the one separating ROUND's two arguments, at that call's own depth."""

    stack = []
    for i, ch in enumerate(expr):
        if ch == "(":
            stack.append(expr[max(0, i - 6):i] == "_round")
        elif ch == ")":
            if not stack:
                return False
            stack.pop()
        elif ch == "," and not (stack and stack[-1]):
            return False
    return not stack


def evaluate(formula, excel_row, row, formulas=None, depth=0):
    """
    The value of a simple formula from its own row's cells: + - * / ( ), ROUND, numbers and references to cells of the
    same row (a referenced formula is evaluated in turn). None for anything else - a function it does not know, a cell
    of another row, a blank - so nothing is decided on it.
    """

    if depth > 6 or not isinstance(formula, str) or not formula.startswith("="):
        return None
    formulas = formulas or {}
    failed = []

    def value(m):
        if int(m.group(2)) != excel_row:
            failed.append(m.group(0))
            return "0"
        i = _column_index(m.group(1))
        cell = formulas.get(i) or (row[i] if i < len(row) else None)
        if isinstance(cell, str) and cell.startswith("="):
            cell = evaluate(cell, excel_row, row, formulas, depth + 1)
        number = _number(cell) if not isinstance(cell, bool) else None
        if number is None:
            failed.append(m.group(0))
            return "0"
        return repr(number)

    expr = _CELL_REF.sub(value, formula[1:])
    expr = re.sub(r"(?i)\bROUND\s*\(", "_round(", expr)
    if failed or not re.fullmatch(r"[\d.\s+\-*/(),e]*", expr.replace("_round", "")):
        return None
    if not _commas_only_in_round(expr):
        return None  # a comma outside ROUND( makes a tuple, and (1,)*N would allocate without bound (E-39)
    if "**" in re.sub(r"\s", "", expr):
        return None  # a power is no formula of a row, and 9**9**9 would never finish
    try:
        return float(eval(expr, {"__builtins__": {}}, {"_round": lambda x, n=0: round(x + 0.0, int(n))}))  # noqa: S307
    except Exception:  # noqa: BLE001 - a formula it cannot work out is not decided
        return None


def _tables_with_rows(paths):
    """tables_of, with each table's file and where its header row is (to find a row's cells by their address)."""

    out = []
    for path, label, rows in _labelled_sheets(paths):
        header_at = _header_at(rows)
        if header_at is not None:
            out.append((path, label, [str(c or "").strip() for c in rows[header_at]], rows[header_at + 1:], header_at))
    return out


def working_rows(made):
    """
    One entry per row of a working table (a "Working" sheet or file, or one with a result column): its references,
    every factor it records - {"qty": [(header, n)], "price": [...], "tax": [...], "discount": [...]} - its results
    [(header, n)] (the result and any subtotal), its source text, and whether it has numbers of no known kind.
    """

    out = []
    for path, label, header, rows, header_at in _tables_with_rows(made):
        # Only a table that is the working itself: a register's amounts follow its own rules.
        if "working" not in label.lower() and not any((h or "").strip().lower() in ("result", "computed", "computed amount",
                                                                                     "working result") for h in header):
            continue
        kinds = _working_columns(header)
        if not kinds["qty"] or not kinds["price"] or not (kinds["result"] or kinds["subtotal"]):
            continue
        known = set().union(*kinds.values())
        sheet = label[label.rfind("(") + 1:-1] if label.endswith(")") else ""
        sheet_formulas = _formulas(path).get(sheet, {}) if sheet else {}
        for n, row in enumerate(rows):
            excel_row = header_at + 2 + n
            in_row = {c: f for (r, c), f in sheet_formulas.items() if r == excel_row}
            row = list(row)
            formula_cols = set()
            for c, f in in_row.items():
                formula_cols.add(c)
                if c < len(row) and _number(row[c]) is None:
                    row[c] = evaluate(f, excel_row, row, in_row)  # (no value kept by the app: worked out here)

            def numbers(cols):
                return [(str(header[i]), _number(row[i])) for i in cols if i < len(row) and _number(row[i]) is not None]
            entry = {k: numbers(kinds[k]) for k in ("qty", "price", "tax", "discount")}
            results = numbers(kinds["result"]) + numbers(kinds["subtotal"])
            if not entry["qty"] or not entry["price"] or not results:
                continue
            other = [i for i, c in enumerate(row) if i not in known and i < len(header) and _number(c) is not None
                     and not isinstance(c, bool)]
            refs = _refs(row)
            source = " ".join(str(row[i]) for i in kinds["source"] if i < len(row) and row[i] not in (None, ""))
            sources_by = [(str(header[i]), str(row[i])) for i in kinds["source"] if i < len(row) and row[i] not in (None, "")]
            items = [str(row[i]).strip() for i, h in enumerate(header)
                     if i < len(row) and re.search(r"(?i)\b(?:item|sku|product|part|article)\b", str(h or ""))
                     and _ITEM_CODE.fullmatch(str(row[i] or "").strip())]
            numbers = {str(header[i]): _number(row[i]) for i in range(min(len(row), len(header)))
                       if _number(row[i]) is not None and not isinstance(row[i], bool)}
            cells = {str(c).strip().lower() for c in row if isinstance(c, str) and c.strip()}
            formula_results = {str(header[i]) for i in formula_cols if i < len(header)}
            out.append({"label": label, "refs": refs, **entry, "results": results, "source": source,
                        "formula_results": formula_results,
                        "sources": sources_by, "items": items, "numbers": numbers, "cells": cells,
                        "unknown": bool(other)})
    return out


def _matches(value, target):
    """Whether a computed value gives the written result: to the paisa, or rounded to the rupee as written."""

    return abs(value - target) <= 0.011 or (float(target).is_integer() and abs(round(value) - target) < 0.001)


def reproduces(work, target):
    """
    Whether any combination of the factors a working row records gives `target`: a quantity column x a price column, x
    (1 + tax%) when it records a tax %, x (1 - discount%) when it records one (each also tried without, for a subtotal).
    A quantity the working marks as the one used ("payable qty") is the only quantity tried when there is one.
    """

    qtys = work["qty"]  # (every quantity: a bill amount uses the billed one, the payable amount the payable one)
    taxes = [0.0] + [t for _, t in work["tax"]]
    discounts = [0.0] + [d for _, d in work["discount"]]
    for _, q in qtys:
        for _, p in work["price"]:
            for t in taxes:
                for d in discounts:
                    # A rate of 1 or more is a percent (1 is 1%, never 100%); under 1 it is the fraction a
                    # percent-formatted cell holds (0.18 for 18%).
                    rate_t = t / 100 if t >= 1 else t
                    rate_d = d / 100 if d >= 1 else d
                    if _matches(q * p * (1 + rate_t) * (1 - rate_d), target):
                        return True
    return False


_FIGURE = re.compile(r"(?<![\w.])\d{1,3}(?:,\d{2,3})+(?:\.\d+)?(?![\d])|(?<![\w.,])\d+(?:\.\d+)?(?![\d,])")
_ADDRESS = re.compile(r"[\w.+-]+@[\w-]+(?:\.[\w-]+)+")


def read_messages(paths):
    """
    {file name (lower): {"text", "sender", "subject", "amounts"}} for the request's messages: an email decoded (its
    quoted-printable soft breaks and =E2=82=B9 joined back, every text part), anything else as text. `amounts` are the
    figures the message writes, as numbers (1,490 / 1490.00 / Rs 1,490 / 4,80,000 alike).
    """

    import email
    import email.utils
    from email import policy
    out = {}
    for path in paths or ():
        p = Path(str(path))
        try:
            raw = p.read_bytes()
        except OSError:
            continue
        sender = subject = ""
        if p.suffix.lower() == ".eml":
            try:
                message = email.message_from_bytes(raw, policy=policy.default)
                parts = [part.get_content() for part in message.walk()
                         if part.get_content_maintype() == "text" and not part.is_attachment()]
                text = "\n".join(str(x) for x in parts)
                sender = (email.utils.parseaddr(str(message.get("From") or ""))[1] or "").lower()
                subject = str(message.get("Subject") or "")
                text = subject + "\n" + text
            except Exception:  # noqa: BLE001 - an unreadable email is read as text
                text = raw.decode("utf-8", errors="replace")
        else:
            text = raw.decode("utf-8", errors="replace")
        amounts = {v for v in (_number(m) for m in _FIGURE.findall(text)) if v is not None}
        out[p.name.lower()] = {"text": text, "sender": sender, "subject": subject, "amounts": amounts}
    return out


def _tied_message(row, messages):
    """The message a table row is about: one whose file name (or its stem) a cell holds, else one whose sender a cell
    holds and whose subject a cell holds too; None when the row cannot be tied to any."""

    cells = [str(c).strip() for c in row if c not in (None, "")]
    for c in cells:
        name = Path(c).name.lower()
        if name in messages:
            return messages[name]
        for key, message in messages.items():
            if Path(key).stem == name:
                return message
    joined = " ".join(cells).lower()
    senders = {a.lower() for a in _ADDRESS.findall(joined)}
    for message in messages.values():
        subject = re.sub(r"(?i)^\s*(?:re|fwd?|fw)\s*:\s*", "", message["subject"]).strip().lower()
        if message["sender"] and message["sender"] in senders and subject and subject in joined:
            return message
    return None


def stated_by_message(amount, row, messages):
    """True when the row's own message states this amount (read, not computed); False when it states other figures
    only; None when the row cannot be tied to a message (then nothing is decided)."""

    message = _tied_message(row, messages)
    if message is None:
        return None
    return any(abs(amount - v) <= 0.005 for v in message["amounts"])


def _stated_against_working(amount, row, refs, work):
    """
    (working label, its result) when an amount a row states provably differs from its working; None when it holds or
    cannot be decided. It holds when it is any result of the one working row the row's full key ties it to, or a sum
    of one order's working rows (any column: pre-tax, tax, with tax). A reference several working rows share (an
    order's PO, a chat's id, a product code) never ties a row on its own: tied to no single row, nothing is decided.
    Found live (2026-10-20): a line's "Decision reason" states its ORDER's total with tax, and was compared with one
    line's working.
    """

    refset = set(refs)
    cells = {str(c).strip().lower() for c in row if isinstance(c, str) and c.strip()}
    # Sums over one order's rows: the rows sharing a reference, narrowed by any other cell the row shares (its customer).
    for ref in refset:
        group = [w for w in work if ref in w["refs"]]
        for narrowed in [group] + [[w for w in group if value in w["cells"]] for value in cells]:
            if not narrowed:
                continue
            for header in {h for w in narrowed for h in w["numbers"]}:
                total = sum(w["numbers"].get(header) or 0 for w in narrowed)
                if abs(total - amount) <= 0.011:
                    return None
    candidates = [w for w in work if w["refs"] and set(w["refs"]) <= refset]
    if len(candidates) != 1:
        return None
    only = candidates[0]
    # Tied by its full key: no other working row carries the same references.
    if sum(1 for w in work if set(w["refs"]) == set(only["refs"])) != 1:
        return None
    values = [v for _, v in only["results"]] + list(only["numbers"].values())
    if any(abs(v - amount) <= 0.011 for v in values):
        return None
    if only["unknown"]:
        return None  # (a working with columns of no known kind is not decided)
    return only["label"], max(v for _, v in only["results"])


def working_problems(made, sources=(), messages=()):
    """
    Sentences for computed amounts that provably do not hold - and only those: a working row none of whose recorded
    factor combinations (quantity x price, with its tax % and discount %) gives its result; a price no source table it
    names has for its references; an amount a status or action states for an item that its working does not give. A
    working whose numbers cannot be told apart (columns of no known kind) is not decided. [] when nothing is computed.
    Found live (2026-10-20): a result right with its tax rate, and one right for the quantity received rather than
    ordered, were each refused as "no combination of its factors" - a correct register, twice.
    """

    out = []
    work = working_rows(made)
    read = read_messages(messages)
    source_tables = tables_of(list(sources) + list(made))
    by_ref = {}
    for w in work:
        label, refs = w["label"], w["refs"]
        result_header, result = next(((h, v) for h, v in w["results"] if RESULT_COLUMN.search(h)), w["results"][-1])
        if result_header in w.get("formula_results", ()):
            pass  # a formula worked out from its own row: its own arithmetic, never refused on guessed factors
        elif not reproduces(w, result) and not w["unknown"]:
            q, p = w["qty"][0][1], w["price"][0][1]
            factors = "; ".join(f"{h} {v:g}" for k in ("qty", "price", "tax", "discount") for h, v in w[k])
            out.append(f"{label}: {', '.join(refs) or 'a row'} gives {result_header} {_fmt(result)}, which no "
                       f"combination of its own factors ({factors}) gives - {q:g} x {_fmt(p)} is {_fmt(q * p)}")
        for ref in refs:
            by_ref.setdefault(ref, (set(), label))[0].update(v for _, v in w["results"])
        if refs and w["source"]:
            prices = [v for _, v in w["price"]]
            named = [t for t in source_tables if names_in(Path(t[0].split(" (")[0]).stem, w["source"])]
            for t_label, header, rows in named:
                pcols = [i for i, h in enumerate(header) if PRICE_COLUMN.search(h or "") and not PERCENT_COLUMN.search(h or "")]
                if not pcols:
                    continue
                for row in rows:
                    cells = " ".join(str(c) for c in row if c not in (None, ""))
                    if not all(r in cells for r in refs):
                        continue
                    theirs = [_number(row[i]) for i in pcols if i < len(row) and _number(row[i]) is not None]
                    if theirs and not any(abs(a - b) <= 0.005 for a in theirs for b in prices):
                        out.append(f"{label}: {', '.join(refs)} uses a price of {_fmt(prices[0])}, but {t_label} "
                                   f"has {_fmt(theirs[0])}")
    for label, header, row, i, cell in _status_cells(made):
        m = MONEY_IN_TEXT.search(cell)
        if not m:
            continue
        amount = _number(m.group(1))
        refs = _refs(row)
        worked = next((by_ref[r] for r in refs if r in by_ref), None)
        if worked and amount is not None:
            tied = _stated_against_working(amount, row, refs, work)
            if tied is not None:
                out.append(f"{label}: {', '.join(refs)} - {header[i]} says {_fmt(amount)}, its working in "
                           f"{tied[0]} gives {_fmt(tied[1])}")
        elif not worked and work and refs and amount is not None:
            # Only a derived amount needs a working: one the row's own message states was read, and a row tied
            # to no message is not decided (2026-10-20: five amounts the customers' own emails state were
            # refused for "no working").
            if stated_by_message(amount, row, read) is not False:
                continue
            out.append(f"{label}: {', '.join(refs)} - {header[i]} states {_fmt(amount)} with no working "
                       "for it (quantity, price, where each was read)")
    return list(dict.fromkeys(out))[:10]


def formula_contradictions(made):
    """
    Own arithmetic (harness audit, 2026-10-21): a workbook cell whose stored figure is not what its own formula gives -
    worked out here from the cells of its own row (stage_facts.evaluate: + - * / ROUND); a formula it cannot work out,
    or a cell with no stored figure, decides nothing. Nothing here guesses what a column means.
    """

    out = []
    for path in made:
        if Path(str(path)).suffix.lower() not in {".xlsx", ".xlsm"}:
            continue
        formulas = _formulas(path)
        if not formulas:
            continue
        try:
            sheets = dict(_sheets(path))
        except Exception as exc:  # noqa: BLE001 - what cannot be read is not decided
            log(f"stage_facts: could not read {path}: {exc}")
            continue
        for sheet, cells in formulas.items():
            rows = sheets.get(sheet) or sheets.get(Path(str(path)).name) or []
            for (r, c), formula in sorted(cells.items()):
                row = list(rows[r - 1]) if r - 1 < len(rows) else []
                stored = _number(row[c]) if c < len(row) and not isinstance(row[c], bool) else None
                if stored is None:
                    continue
                in_row = {cc: f for (rr, cc), f in cells.items() if rr == r and cc != c}
                worked = evaluate(formula, r, row, in_row)
                if worked is not None and not _matches(worked, stored):
                    out.append(f"{Path(str(path)).name} ({sheet}) row {r}: the cell holds {_fmt(stored)}, its own formula "
                               f"{formula} gives {_fmt(worked)}")
    return list(dict.fromkeys(out))[:10]


# A short product code in a cell of its own ("AB-S", "PK-BOX", "XY-500").
_ITEM_CODE = re.compile(r"[A-Z][A-Z0-9]{1,7}(?:-[A-Z0-9]{1,6}){1,2}")


def unworked_amounts(made, messages=()):
    """Sentences for amounts a status/action cell states for an item (any money amount) that the row's own source
    message does not state (so the stage computed it) when no table of the made files records any working (quantity,
    price, result) at all."""

    if working_rows(made):
        return []
    read = read_messages(messages)
    out = []
    for label, header, row, i, cell in _status_cells(made):
        m = MONEY_IN_TEXT.search(cell)
        if m:
            # An amount the item's own message states was read, not computed; a row tied to no message is
            # not decided.
            amount = _number(m.group(1).rstrip("."))
            if amount is None or stated_by_message(amount, row, read) is not False:
                continue
            out.append(f"{label}: {', '.join(_refs(row)) or 'a row'} - \"{cell[:100]}\"")
    return out[:10]


CATEGORY_COLUMN = re.compile(r"\b(?:category|type|kind|class(?:ification)?)\b", re.I)


def self_contradictions(made):
    """Sentences for each row filed in a category that its own other cells deny ("Refund" with "no refund requested")."""

    out = []
    for label, header, rows in tables_of(made):
        cats = [i for i, h in enumerate(header) if CATEGORY_COLUMN.search(h or "")]
        for row in rows:
            for c in cats:
                value = str(row[c]).strip() if c < len(row) and row[c] not in (None, "") else ""
                if not value or len(value) > 40:
                    continue
                word = re.escape(value.lower())
                deny = re.compile(rf"\b(?:no {word}(?: is)? (?:requested|asked|needed|wanted)|not an? {word}\b|"
                                  rf"(?:do|does|did) not want an? {word}|without an? {word}|no {word} wanted)", re.I)
                others = " | ".join(str(x) for j, x in enumerate(row) if j != c and x not in (None, ""))
                m = deny.search(others)
                if m:
                    out.append(f"{label}: {', '.join(_refs(row)) or 'a row'} is filed as {header[c]} \"{value}\" while the "
                               f"same row says \"{m.group(0)}\"")
    return out[:10]




# ============================================================
# A summary figure against the rows its own workbook counts
# ============================================================
# Found live (2026-10-21): a summary sheet gave three totals as 0 while the same workbook's register counted them
# over its rows (a duplicate counted once, a cancelled bill and a held one left out) - and the net figure followed the 0.

_LEFT_OUT_ROW = re.compile(r"(?i)\b(?:duplicat\w*|cancel\w*|held|hold|kept out|keep\b[^;|]{0,20}\bout|blocked|excluded?|"
                           r"not eligible|ineligible|reversed|void)\b")
_TOTAL_LABEL = re.compile(r"(?i)^\s*(?:total|totals|sum|grand|eligible|blocked|net|subtotal)\b")
_GENERIC = {"total", "totals", "amount", "value", "rs", "inr", "sum", "the", "of", "this", "month", "in", "for", "and"}


def _label_words(text):
    return {w for w in re.findall(r"[a-z0-9]+", str(text or "").lower()) if w not in _GENERIC and len(w) > 1}


def summary_vs_detail(made):
    """
    Within one workbook: a summary row in one sheet (its label naming a column of another sheet's table and words its
    counted rows' statuses carry, e.g. "Eligible credit | Tax A") whose figure differs from that column summed over the rows
    the workbook itself counts there - not marked duplicate, cancelled, held, kept out or excluded. Both figures named.
    [] when no summary can be tied to one column and its counted rows (nothing is decided then).
    """

    out = []
    for path in made:
        if Path(str(path)).suffix.lower() not in (".xlsx", ".xlsm"):
            continue
        tables = tables_of([path])
        details = []
        for label, header, rows in tables:
            status = next((i for i, h in enumerate(header) if re.search(r"(?i)\bstatus\b|\bexception\b|\bremark\b|"
                                                                         r"\bnote\b", h or "")), None)
            if status is None:
                continue
            body = [r for r in rows if any(c not in (None, "") for c in r)
                    and not _TOTAL_LABEL.match(str(r[0] or "")) and status < len(r) and r[status] not in (None, "")]
            counted = [r for r in body if not _LEFT_OUT_ROW.search(str(r[status]))]
            if len(counted) < 2 or len(counted) == len(body) and not any(_LEFT_OUT_ROW.search(str(r[status])) for r in body):
                continue  # (nothing marked left out: no counted rows to tell from all)
            words = set().union(*(_label_words(r[status]) for r in counted))
            for i, h in enumerate(header):
                values = [_number(r[i]) for r in counted if i < len(r)]
                if not h or i == status or sum(v is not None for v in values) < len(counted) // 2 + 1:
                    continue
                details.append((label, str(h), _label_words(h), words, sum(v or 0 for v in values)))
        for label, header, rows in tables:
            for row in rows:
                cells = [c for c in row if c not in (None, "")]
                text = " ".join(str(c) for c in cells if not isinstance(c, (int, float)))
                figures = [c for c in cells if isinstance(c, (int, float)) and not isinstance(c, bool)]
                if not figures or _LEFT_OUT_ROW.search(text):
                    continue
                said = _label_words(text)
                for d_label, d_header, d_words, statuses, expected in details:
                    if d_label == label or not d_words or not d_words <= said:
                        continue
                    rest = said - d_words
                    if not rest or not rest <= statuses:
                        continue
                    value = figures[-1]
                    if abs(value - expected) > 0.5:
                        out.append(f"{label}: \"{text}\" is {value:,.2f}, but {d_label}'s {d_header} over the rows it "
                                   f"counts (not marked duplicate, cancelled, held or excluded) is {expected:,.2f}")
    return list(dict.fromkeys(out))[:10]



# ============================================================
# What a draft states about its recipient's own record
# ============================================================
# Found live (2026-10-21): four drafts named people no source has, or another recipient's, and dates other than each
# recipient's own - while the tracker and the portal were right. Nothing checked what a draft says.

# Header words too general to say which value a draft means: "date", "name", "amount" are anything's.
_GENERIC_HEADER_WORDS = {"date", "name", "amount", "number", "value", "total", "type", "status", "code", "email", "mail",
                         "notes", "note", "the", "and", "for", "per", "inr", "details", "full", "personal", "with", "from"}
_NAME_IN_TEXT = re.compile(r"(?<![A-Za-z])([A-Z][a-z]+(?:[ \t]+[A-Z][a-z]+){1,2})(?![A-Za-z])")
_NAME_CELL = re.compile(r"[A-Z][a-z]+(?:[ \t]+[A-Z][a-z]+){1,2}")


def _date_key(value):
    """A date cell or text as (y, m, d) readings - a set, since 02/11/2026 may be either - or an empty set."""

    import datetime
    from decurix_agent import provenance
    if isinstance(value, (datetime.date, datetime.datetime)):
        return {tuple(value.timetuple()[:3])}
    return provenance.date_readings(str(value or ""))


def _cell_kind(value):
    """What kind of value a cell holds, for finding it in a draft: "date", "number", "name" - or "" (not looked for)."""

    from decurix_agent import provenance
    if value is None or isinstance(value, bool):
        return ""
    if _date_key(value):
        return "date"
    if isinstance(value, (int, float)):
        return "number" if provenance._significant(provenance.Decimal(str(value))) >= 3 else ""
    text = " ".join(str(value).split())
    found = provenance.parse_number(text)
    if found and re.fullmatch(r"(?i)[^A-Za-z]*(?:rs\.?|inr)?[^A-Za-z]*", text) and provenance._significant(found[0]) >= 3:
        return "number"
    if _NAME_CELL.fullmatch(text):
        return "name"
    return ""


def _said_after(body, word, kind, skip):
    """[value of `kind` the draft writes within a short reach after a word of the column's header]."""

    from decurix_agent import provenance
    stem = word[:4] if len(word) >= 5 else word
    out = []
    for m in re.finditer(r"(?i)\b" + re.escape(stem) + r"\w*\b", body):
        reach = body[m.end():m.end() + 60].split("\n")[0]
        if kind == "date":
            d = provenance._DATE_IN_TEXT.search(reach)
            if d:
                out.append(d.group(0))
        elif kind == "number":
            clean = provenance._PERCENT.sub(" ", provenance._DATE_IN_TEXT.sub(" ", reach))
            for n in provenance._NUMBER.finditer(clean):
                found = provenance.parse_number(n.group(0))
                if found and provenance._significant(found[0]) >= 3:
                    out.append(n.group(0).strip())
                    break
        elif kind == "name":
            for n in _NAME_IN_TEXT.finditer(reach):
                words = [w for w in n.group(1).split() if w.lower() not in skip]
                if len(words) >= 2:
                    out.append(" ".join(words))
                    break
    return out


def _same_said(kind, said, own):
    from decurix_agent import provenance
    if kind == "date":
        return bool(_date_key(said) & _date_key(own))
    if kind == "number":
        a, b = provenance.parse_number(said), provenance.parse_number(str(own))
        return bool(a and b and provenance.same_figure(a, b))
    return " ".join(said.lower().split()) == " ".join(str(own or "").lower().split())


def draft_field_facts(drafts, tables):
    """
    [{"who", "header", "said", "own", "kind"}] for each draft tied, by its recipient's address, to exactly one row of a
    table of the request's own inputs: a value it writes right after a word of one of that row's column headers (a
    date or a person's name - as the cell holds) that differs, by meaning, from that row's cell. Which column a
    sentence means is a reading, so this is evidence (harness audit, 2026-10-21): no business word is known here.
    """

    out = []
    for draft in drafts or ():
        to = {str(a).strip().lower() for a in (draft.get("to") or [])}
        body = " ".join(str(draft.get(k) or "") for k in ("subject", "body"))
        hits = None
        for header, rows in tables:
            mine = [row for row in rows if any(str(c).strip().lower() in to for c in row if c not in (None, ""))]
            if len(mine) == 1:
                hits = (header, mine[0])
                break
            if mine:
                break  # (several rows for one address: not decided)
        if not hits:
            continue
        header, row = hits
        who = ", ".join(sorted(to))
        for i, h in enumerate(header):
            if i >= len(row) or str(row[i]).strip().lower() in to:
                continue
            kind = _cell_kind(row[i])
            words = [w for w in re.findall(r"[a-z]{3,}", str(h or "").lower()) if w not in _GENERIC_HEADER_WORDS]
            # Dates and names only: a figure near a column's word is as often a line's, a part's or a sum's (the
            # replay gate, 2026-10-21: a dispute email's billed line against the bill's total) - provenance holds them.
            if kind not in ("date", "name") or not words or re.search(r"\d", str(h or "")):
                continue
            for word in words:
                for said in _said_after(body, word, kind, set(re.findall(r"[a-z]+", str(h or "").lower()))):
                    if not _same_said(kind, said, row[i]):
                        out.append({"who": who, "header": str(h), "said": said, "own": row[i], "kind": kind})
    seen, unique = set(), []
    for f in out:
        key = (f["who"], f["header"], f["said"])
        if key not in seen:
            seen.add(key)
            unique.append(f)
    return unique[:12]
