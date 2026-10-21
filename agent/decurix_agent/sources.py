"""
A figure the result presents as a source's own, against what that source states.

A reconciliation, a register or a summary often carries a line such as "Closing balance per bank statement" or
"Total as per ledger". Found live (2026-10-20, round 5, real Excel): a bank reconciliation arrived at "Closing
balance per bank statement" = -14,140.00, the bank statement's own last balance was 398,360.00, and the report "shows
the final closing balance per bank statement as -14,140.00" passed every check: nothing compared the two.

Decided in code only where it is precise: the label names a source that is exactly one of the request's input files,
and that file plainly states a figure of that kind (the last value of its Balance column, a row labelled Closing
balance / Opening balance / Total). Everything else is handed to the judge as evidence: what the inputs state, beside
what the result says. No application or business is special here.
"""

import csv
import io
import os
import re
from decimal import Decimal, InvalidOperation
from pathlib import Path

from decurix_agent import attachments, config, shared
from decurix_agent.logging_utils import log

# A line of the result that says it is a source's figure: "Closing balance per bank statement", "Balance as per
# cash book", "Total per ledger:".
PER_SOURCE = re.compile(
    r"^\s*(?:(?P<kind>closing|opening|ending|final|starting)\s+)?(?P<what>balance|total)\s+(?:as\s+)?per\s+"
    r"(?:the\s+|our\s+)?(?P<src>[a-z][a-z0-9 .'&/-]{1,40}?)\s*:?\s*$", re.I)
# A request whose result is a reconciliation or rests on balances: the judge is shown what the inputs state.
BALANCE_WORK = re.compile(r"\breconcil\w*|\b(?:closing|opening)\s+balance|\bbalance\s+(?:as\s+)?per\b", re.I)
INPUT_KINDS = {".csv", ".tsv", ".xlsx", ".xlsm", ".txt"}
NAMED_FILE = re.compile(r"\b[\w][\w.-]*\.(?:csv|tsv|xlsx|xlsm|txt)\b", re.I)
_STOP = {"the", "our", "a", "an", "of", "as", "per", "my", "their", "its", "and", "file", "sheet"}
TOLERANCE = Decimal("0.01")
MAX_ROWS = 20000


def _number(value):
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, (int, float)):
        return Decimal(str(value))
    text = str(value).strip().replace(",", "")
    negative = text.startswith("(") and text.endswith(")")
    text = text.strip("()").replace("Rs.", "").replace("INR", "").strip(" ₹$€£")
    if text.endswith(("Cr", "CR")):
        text = text[:-2].strip()
    elif text.endswith(("Dr", "DR")):
        text, negative = text[:-2].strip(), not negative
    try:
        found = Decimal(text)
    except InvalidOperation:
        return None
    return -found if negative else found


def _words(text):
    return [w for w in re.findall(r"[a-z]+", (text or "").lower()) if w not in _STOP]


def _money(value):
    return f"{value:,.2f}"


def _rows(path):
    """The rows of an input file as lists of cell values: a CSV's lines, or every sheet of a workbook."""

    suffix = Path(path).suffix.lower()
    if suffix in (".xlsx", ".xlsm"):
        import openpyxl
        book = openpyxl.load_workbook(str(path), data_only=True, read_only=True)
        try:
            out = []
            for sheet in book.worksheets:
                for n, row in enumerate(sheet.iter_rows(values_only=True)):
                    if n >= MAX_ROWS:
                        break
                    out.append(list(row))
                out.append(None)  # a sheet's end
            return out
        finally:
            book.close()
    text = Path(path).read_text(encoding="utf-8-sig", errors="replace")
    dialect = "excel-tab" if suffix == ".tsv" or text.count("\t") > text.count(",") else "excel"
    return [row for row in csv.reader(io.StringIO(text), dialect=dialect)][:MAX_ROWS]


def stated(path):
    """
    What an input file plainly states: {"opening": (value, where), "closing": (value, where), "totals": [(value,
    where)]}, each absent when the file does not state it. A Balance column's last value is its closing balance (its
    first is not an opening balance: that comes before the first line); a row labelled Opening/Closing balance or Total
    states that figure.
    """

    out = {"totals": []}
    try:
        rows = _rows(path)
    except Exception as exc:  # noqa: BLE001 - a file that cannot be read states nothing
        log(f"sources: could not read {path}: {exc}")
        return out
    balance_col, last = None, None
    for n, row in enumerate(rows, 1):
        if row is None:
            balance_col = None
            continue
        cells = ["" if c is None else c for c in row]
        texts = [str(c).strip().lower() for c in cells]
        if balance_col is None and not any(_number(c) is not None for c in cells):
            # A header row is words only: "Closing balance, 1500" is a figure, not a column's name.
            for i, t in enumerate(texts):
                if t in ("balance", "closing balance", "running balance", "balance (inr)", "balance amount"):
                    balance_col = i
                    break
            if balance_col is not None:
                continue
        elif balance_col is not None and balance_col < len(cells):
            value = _number(cells[balance_col])
            if value is not None:
                last = (value, f"the last line of its {rows_header(rows, balance_col, n)} column, row {n}")
        label = next((t for t in texts if re.search(r"[a-z]", t)), "")
        numbers = [v for v in (_number(c) for c in cells) if v is not None]
        if not numbers:
            continue
        if re.match(r"^(closing|ending)\s+balance\b", label):
            out["closing"] = (numbers[-1], f"its \"{label}\" row, row {n}")
        elif re.match(r"^opening\s+balance\b", label):
            out["opening"] = (numbers[-1], f"its \"{label}\" row, row {n}")
        elif re.match(r"^(grand\s+)?total\b", label):
            out["totals"].append((numbers[-1], f"its \"{label}\" row, row {n}"))
    if "closing" not in out and last is not None:
        out["closing"] = last
    return out


def rows_header(rows, column, before):
    """The header text of a column, as the file writes it."""

    for row in rows[:before]:
        if row is not None and column < len(row) and isinstance(row[column], str) and row[column].strip():
            return row[column].strip()
    return "Balance"


def inputs(request, made=(), fetch=None):
    """
    The request's input files, found where they are: [{"name", "path", "about"}]. `about` is what identifies the file
    for a label to name it: its name's words and the words the request puts beside it ("bank_oct.csv (the bank
    statement)"). A file the task made is not an input.
    """

    text = attachments.text(request or "")
    made_names = {Path(str(m)).name.lower() for m in made}
    found = {}
    for path in attachments.files(request or ""):
        if Path(path).suffix.lower() in INPUT_KINDS and Path(path).name.lower() not in made_names:
            found[Path(path).name.lower()] = str(path)
    from decurix_agent import produced
    folders = [h for h in produced._host_paths(text) if not Path(produced._leaf(h)).suffix]
    # Only the folders this request names (or its own files): another task's copy of a same-named file is not its input.
    from decurix_agent import payees
    roots = payees.named_folders(request, [config.WORK_DIR])
    for name in dict.fromkeys(NAMED_FILE.findall(text)):
        key = name.lower()
        if key in found or key in made_names:
            continue
        for place in (config.WORK_DIR / name, *(p for root in roots for p in Path(root).glob(f"**/{name}"))):
            if Path(place).is_file():
                found[key] = str(place)
                break
        if key in found:
            continue
        for folder in folders:
            host = folder.rstrip("\\/") + "\\" + name
            local = shared.to_local(host)
            if local and Path(local).is_file():
                found[key] = local
                break
            copies = produced.copy_from_host([host], fetch=fetch)
            if copies:
                found[key] = str(copies[0])
                break
    out = []
    for key, path in found.items():
        name = Path(path).name
        beside = re.search(re.escape(name) + r"\s*\(([^)]{1,80})\)", text, re.I)
        about = " ".join(_words(Path(name).stem.replace("_", " ").replace("-", " ")) + _words(beside.group(1) if beside else ""))
        out.append({"name": name, "path": path, "about": about})
    return out


def _matches(source_words, about):
    """Whether the words a label names a source with are all in what identifies an input (spaces aside)."""

    if not source_words:
        return False
    joined = "".join(about.split())
    return all(w in joined for w in source_words)


def _per_source_lines(path):
    """[(label, kind, what, source words, value, "Sheet!B7")] for the lines of a made workbook that name a source."""

    import openpyxl
    out = []
    book = openpyxl.load_workbook(str(path), data_only=True)
    for sheet in book.worksheets:
        for row in sheet.iter_rows(min_row=1, max_row=min(sheet.max_row, MAX_ROWS)):
            for i, cell in enumerate(row):
                if not isinstance(cell.value, str):
                    continue
                match = PER_SOURCE.match(cell.value)
                if not match:
                    continue
                value = next(((c, _number(c.value)) for c in row[i + 1:] if _number(c.value) is not None), None)
                if value is None:
                    continue
                kind = (match.group("kind") or "closing").lower()
                kind = "opening" if kind in ("opening", "starting") else "closing"
                out.append((cell.value.strip(), kind, match.group("what").lower(), _words(match.group("src")),
                            value[1], f"{sheet.title}!{value[0].coordinate}"))
                break
    return out


def mismatches(request, made, found_inputs=None):
    """
    Sentences for each figure a made workbook presents as a source's own that the source contradicts: the label names
    exactly one input file, that file states the figure, and the two differ. [] otherwise (decided only where it is
    precise; the judge sees the rest as evidence).
    """

    books = [p for p in made if Path(str(p)).suffix.lower() in (".xlsx", ".xlsm")]
    if not books:
        return []
    lines = []
    for book in books:
        try:
            lines += [(Path(str(book)).name,) + line for line in _per_source_lines(book)]
        except Exception as exc:  # noqa: BLE001 - a file that cannot be read decides nothing here
            log(f"sources: could not read {book}: {exc}")
    if not lines:
        return []
    known = inputs(request, made) if found_inputs is None else found_inputs
    out = []
    for book, label, kind, what, source, value, where in lines:
        candidates = [i for i in known if _matches(source, i["about"])]
        if len(candidates) != 1:
            continue
        facts = stated(candidates[0]["path"])
        if what == "total":
            if not facts["totals"] or any(abs(value - t) <= TOLERANCE for t, _ in facts["totals"]):
                continue
            expected, said = facts["totals"][-1]
        else:
            if kind not in facts:
                continue
            expected, said = facts[kind]
            if abs(value - expected) <= TOLERANCE:
                continue
        out.append(f"{book}: {where} \"{label}\" is {_money(value)}, but {candidates[0]['name']} itself states "
                   f"{_money(expected)} ({said})")
    return out


def evidence(request, made=()):
    """
    For balance work (a reconciliation, a closing balance): what each input file states, read in code, for the judge
    to set beside the result's own figures. "" when the request is not such work or no input states a figure.
    """

    if not BALANCE_WORK.search(attachments.text(request or "")):
        return ""
    parts = []
    for item in inputs(request, made):
        facts = stated(item["path"])
        said = []
        for kind in ("opening", "closing"):
            if kind in facts:
                said.append(f"{kind} balance {_money(facts[kind][0])} ({facts[kind][1]})")
        if facts["totals"]:
            said.append("total " + _money(facts["totals"][-1][0]) + f" ({facts['totals'][-1][1]})")
        if said:
            parts.append(f"{item['name']}: " + "; ".join(said))
    if not parts:
        return ""
    return ("--- what the input files themselves state (read in code; a figure the result gives as one of theirs must "
            "equal it)\n" + "\n".join(parts))
