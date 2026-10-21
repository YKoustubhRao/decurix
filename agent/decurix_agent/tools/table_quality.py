"""
Facts about a table's rows that a total depends on, stated by office read - never a judgement.

Found live (2026-10-20, round 5 P5): a monthly sales export repeated the week 8-14 August (35 rows, an export glitch);
the worker summed it, and August revenue, every regional total, variance and the commentary's "biggest variances"
were inflated. Reading the file, nothing had pointed at the repeat. Now a table read says, in a line or two: exact
duplicate rows (how many, where, the dates they cover), rows that repeat except for an ID-like column, and - across the
period files a task reads - date ranges that overlap.
"""

import csv
import json
import re
from datetime import datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path

ID_HEADER = re.compile(r"(?i)\b(?:id|no\.?|number|#|ref(?:erence)?|code|invoice|voucher|receipt|txn|transaction)\b")
DATE_HEADER = re.compile(r"(?i)\bdate\b")
_DATE_FORMATS = ("%d-%m-%Y", "%d/%m/%Y", "%Y-%m-%d", "%d-%m-%y", "%d/%m/%y", "%d %b %Y", "%d-%b-%Y", "%m/%d/%Y")
MAX_SHOWN = 5


def _date(value):
    if isinstance(value, datetime):
        return value.date()
    text = str(value or "").strip()
    for fmt in _DATE_FORMATS:
        try:
            return datetime.strptime(text, fmt).date()
        except ValueError:
            continue
    return None


def _number(value):
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return Decimal(str(value))
    text = re.sub(r"[,\s₹$€£]|Rs\.?", "", str(value or ""))
    if not re.fullmatch(r"-?\d+(?:\.\d+)?", text):
        return None
    try:
        return Decimal(text)
    except InvalidOperation:
        return None


def _runs(numbers):
    """Consecutive runs of row numbers, as "176-210" or "212"."""

    out, start, prev = [], None, None
    for n in sorted(numbers):
        if start is None:
            start = prev = n
        elif n == prev + 1:
            prev = n
        else:
            out.append(f"{start}-{prev}" if prev != start else f"{start}")
            start = prev = n
    if start is not None:
        out.append(f"{start}-{prev}" if prev != start else f"{start}")
    return out


def table_facts(header, rows, first_row=2):
    """
    {"lines": [facts], "duplicates": [index of a repeated row], "date_range": (min, max) or None,
     "totals": {column: (with every row, without the repeated rows)}, "loud": bool} for a table whose header is `header`
    and whose data rows are `rows` (row `first_row` of the file is rows[0]). "loud": its repeated rows are most likely a
    copy, not real repeats - the table has an identifier or a date column (a record repeats itself only by mistake), or
    the repeats are a whole block copied in order. Exact repeats in a table with neither (attendance marks, stock
    counts, cash sales) are normal: said quietly (independent review, 2026-10-21).
    """

    header = [str(h or "").strip() for h in header]
    width = len(header)
    cleaned = [tuple(str(c if c is not None else "").strip() for c in (list(r) + [""] * width)[:width]) for r in rows]
    cleaned = [r for r in cleaned if any(r)]
    seen, dups = {}, []
    for i, r in enumerate(cleaned):
        if r in seen:
            dups.append(i)
        else:
            seen[r] = i
    lines = []
    date_col = next((j for j, h in enumerate(header) if DATE_HEADER.search(h)), None)
    dates = [d for d in (_date(r[date_col]) for r in cleaned) if d] if date_col is not None else []
    if dups:
        where = ", ".join(_runs([first_row + i for i in dups])[:MAX_SHOWN])
        span = ""
        if date_col is not None:
            dd = [d for d in (_date(cleaned[i][date_col]) for i in dups) if d]
            if dd:
                span = f", dated {min(dd).isoformat()} to {max(dd).isoformat()}"
        firsts = [seen[cleaned[i]] for i in dups]
        order = ""
        if len(dups) > 1 and firsts == sorted(firsts) and firsts[-1] - firsts[0] == len(firsts) - 1:
            order = f" - the same rows, in the same order, as rows {first_row + firsts[0]}-{first_row + firsts[-1]}"
        lines.append(f"{len(dups)} row(s) are exact copies of earlier rows: rows {where}{span}{order}. A total over "
                     "this table counts them twice.")
    # Rows that repeat but for an ID-like column (a re-numbered copy).
    id_cols = [j for j, h in enumerate(header) if ID_HEADER.search(h) and j != date_col]
    if id_cols:
        rest = {}
        near = []
        dup_set = set(dups)
        for i, r in enumerate(cleaned):
            key = tuple(c for j, c in enumerate(r) if j not in id_cols)
            if not any(key):
                continue
            if key in rest and i not in dup_set and cleaned[rest[key]] != r:
                near.append(i)
            rest.setdefault(key, i)
        if near:
            names = ", ".join(header[j] for j in id_cols)
            lines.append(f"{len(near)} row(s) repeat an earlier row in everything but {names}: rows "
                         + ", ".join(_runs([first_row + i for i in near])[:MAX_SHOWN]) + ".")
    block = False
    if len(dups) > 1:
        firsts = [seen[cleaned[i]] for i in dups]
        block = firsts == sorted(firsts) and firsts[-1] - firsts[0] == len(firsts) - 1 and dups[-1] - dups[0] == len(dups) - 1
    loud = bool(dups) and (date_col is not None or bool(id_cols) or block)
    totals = {}
    for j, h in enumerate(header):
        values = [_number(r[j]) for r in cleaned]
        if j == date_col or not h or sum(v is not None for v in values) < max(2, len(cleaned) // 2):
            continue
        all_sum = sum((v for v in values if v is not None), Decimal(0))
        without = all_sum - sum((values[i] for i in dups if values[i] is not None), Decimal(0))
        totals[h] = (all_sum, without)
    # The same per group of a few-valued text column (a region, a product line): a repeated block lands in groups, and
    # the outputs show those (round 5 P5, 2026-10-21: West Aug 5,600,437 with the repeated week, 5,193,603 without).
    # Kept for every table (a month with no repeats still adds to a quarter's group sums).
    groups = {}
    dup_set = set(dups)
    for g, gh in enumerate(header):
        values = {r[g] for r in cleaned}
        if g == date_col or not gh or not 2 <= len(values) <= 30 or any(_number(v) is not None for v in values):
            continue
        for h in totals:
            j = header.index(h)
            for value in values:
                rows_in = [i for i, r in enumerate(cleaned) if r[g] == value]
                a = sum((_number(cleaned[i][j]) or Decimal(0) for i in rows_in), Decimal(0))
                b = sum((_number(cleaned[i][j]) or Decimal(0) for i in rows_in if i not in dup_set), Decimal(0))
                groups[f"{gh}={value}|{h}"] = (a, b)
    return {"lines": lines, "duplicates": dups, "date_range": (min(dates), max(dates)) if dates else None,
            "totals": totals, "groups": groups, "loud": loud}


def csv_table(path):
    """(header, table_facts) of a CSV/TSV file, or None when it is not a table."""

    p = Path(path)
    with open(p, encoding="utf-8-sig", errors="replace", newline="") as handle:
        rows = list(csv.reader(handle, delimiter="\t" if p.suffix.lower() == ".tsv" else ","))
    if len(rows) < 3 or len(rows[0]) < 2:
        return None
    return rows[0], table_facts(rows[0], rows[1:])


def csv_facts(path):
    """table_facts of a CSV/TSV file, or None when it is not a table."""

    try:
        table = csv_table(path)
    except OSError:
        return None
    return table[1] if table else None


def sheet_table(sheet):
    """(header, table_facts) of a worksheet whose first row is its header (openpyxl, values), or None."""

    rows = [list(r) for r in sheet.iter_rows(values_only=True)]
    if len(rows) < 3 or sum(c is not None for c in rows[0]) < 2:
        return None
    return rows[0], table_facts(rows[0], rows[1:])


# ------------------------------------------------------------ several period files read in one task

def _ranges_file():
    from decurix_agent import config, task_context
    return config.DECURIX_DIR / f"table-ranges-{task_context.current() or 'none'}.json"


def _known():
    try:
        return json.loads(_ranges_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def _record(header, facts):
    """What is kept of a read table: its columns' signature, its repeats and its totals with and without them."""

    return {"sig": "|".join(str(h or "").strip().lower() for h in header),
            "repeated": len(facts.get("duplicates") or []), "loud": bool(facts.get("loud")),
            "totals": {k: [str(a), str(b)] for k, (a, b) in (facts.get("totals") or {}).items()},
            "groups": {k: [str(a), str(b)] for k, (a, b) in (facts.get("groups") or {}).items()}}


def overlap_lines(path, header, facts):
    """Note what this table holds (its date range, its totals with and without repeated rows), and say which tables
    read earlier in this task, with the same columns, have an overlapping date range."""

    if not facts:
        return []
    record = _record(header, facts)
    sig = record["sig"]
    store = _ranges_file()
    known = _known()
    out = []
    lo = hi = None
    if facts.get("date_range"):
        lo, hi = (d.isoformat() for d in facts["date_range"])
        for other, info in known.items():
            if other == str(path) or info.get("sig") != sig or not info.get("lo"):
                continue
            if lo <= info["hi"] and info["lo"] <= hi:
                out.append(f"Its dates ({lo} to {hi}) overlap those of {Path(other).name} ({info['lo']} to "
                           f"{info['hi']}), read earlier with the same columns.")
    known[str(path)] = {"sig": sig, "lo": lo, "hi": hi, **record}
    try:
        store.parent.mkdir(parents=True, exist_ok=True)
        store.write_text(json.dumps(known), encoding="utf-8")
    except OSError:
        pass
    return out


_FIGURE = re.compile(r"(?<![\w.])-?\d{1,3}(?:,\d{2,3})+(?:\.\d+)?(?![\w])|(?<![\w.,])-?\d{4,}(?:\.\d+)?(?![\w,])")
_INPUT_CACHE = {}


def input_record(paths):
    """
    The record office read keeps of tables ({path: {"sig", "repeated", "totals", "groups"}}), made here from the
    request's own input tables - CSV/TSV files and each workbook sheet whose header is row 1 - whatever the worker read
    them with. Cached by file and its modification time. Found live (round 5 P5 on pc3, 2026-10-21, series 11): the
    worker read the exports with python, office read never saw them, and the inflated quarter passed.
    """

    out = {}
    for path in paths or ():
        p = Path(str(path))
        suffix = p.suffix.lower()
        if suffix not in (".csv", ".tsv", ".xlsx", ".xlsm"):
            continue
        try:
            key = (str(p), p.stat().st_mtime_ns)
        except OSError:
            continue
        if key not in _INPUT_CACHE:
            tables = []
            try:
                if suffix in (".csv", ".tsv"):
                    table = csv_table(p)
                    if table:
                        tables.append((str(p), *table))
                else:
                    import openpyxl
                    book = openpyxl.load_workbook(p, data_only=True, read_only=True)
                    try:
                        for sheet in book.worksheets:
                            table = sheet_table(sheet)
                            if table:
                                tables.append((f"{p}#{sheet.title}", *table))
                    finally:
                        book.close()
            except Exception:  # noqa: BLE001 - a table that cannot be read is not judged
                tables = []
            _INPUT_CACHE[key] = {name: _record(header, facts) for name, header, facts in tables if facts}
        out.update(_INPUT_CACHE[key])
    return out


def inflated_totals(produced_text, report_text, inputs=(), detail=False):
    """
    Figures in what a task made that are a total including rows a source table repeats - one table's, or the sum of
    the same-column tables. One sentence each, whatever the report says about the repeat: a word list switched this off
    twice in round 5 (series 10, 11) while the figure stayed inflated (harness audit, 2026-10-21) - the finding stays
    whenever the figure is the with-repeats total, and the person decides (long_task: "Needs you"). The source tables
    are the request's own inputs (`inputs`, read here) and whatever office read noted in the task. `report_text` is
    kept for the callers; it decides nothing. With `detail`, [(sentence, loud)]: loud when a table it counts holds an
    identifier or a date column, or repeats a whole block in order (table_facts "loud") - else a quiet advisory line
    (independent review, 2026-10-21: exact repeats are normal in attendance, stock counts and cash sales).
    """

    known = dict(_known())
    known.update(input_record(inputs))
    groups = {}
    for path, info in known.items():
        groups.setdefault(info.get("sig"), []).append((path, info))
    wanted = {}
    for members in groups.values():
        if not any(info.get("repeated") for _, info in members):
            continue
        columns = set().union(*(set(info.get("totals") or {}) for _, info in members))
        for column in columns:
            per = [(path, info) for path, info in members if column in (info.get("totals") or {})]
            with_all = sum(Decimal(info["totals"][column][0]) for _, info in per)
            without = sum(Decimal(info["totals"][column][1]) for _, info in per)
            repeated = [Path(p).name for p, info in per if info.get("repeated")]
            loud = any(info.get("loud") for _, info in per if info.get("repeated"))
            if with_all != without and len(per) > 1:
                wanted[with_all] = (without, column, repeated, loud)
            for path, info in per:
                a, b = (Decimal(x) for x in info["totals"][column])
                if info.get("repeated") and a != b:
                    wanted[a] = (b, column, [Path(path).name], bool(info.get("loud")))
        # Per group (a region, a product line): one file's, and the same group summed over the same-column files.
        keys = set().union(*(set(info.get("groups") or {}) for _, info in members))
        for key in keys:
            name, column = key.split("|", 1)
            per = [(path, info) for path, info in members if key in (info.get("groups") or {})]
            for path, info in per:
                a, b = (Decimal(x) for x in info["groups"][key])
                if a != b:
                    wanted[a] = (b, f"{column} for {name}", [Path(path).name], bool(info.get("loud")))
            if len(per) > 1 and len(per) == len([m for m in members if column in (m[1].get("totals") or {})]):
                with_all = sum(Decimal(info["groups"][key][0]) for _, info in per)
                without = sum(Decimal(info["groups"][key][1]) for _, info in per)
                if with_all != without:
                    changed = [(p, info) for p, info in per if info["groups"][key][0] != info["groups"][key][1]]
                    wanted[with_all] = (without, f"{column} for {name}", [Path(p).name for p, _ in changed],
                                        any(info.get("loud") for _, info in changed))
    out = {}
    for text, value, step in _figures(produced_text):
        for total, (without, column, files, loud) in wanted.items():
            tolerance = max(Decimal("0.5"), step / 2)
            # (Only a figure that tells the two apart: a rounded one the right total would round to as well is none.)
            if abs(value - total) <= tolerance and abs(value - without) > tolerance:
                said = (f"{text} is the {column} total including the rows {', '.join(files)} repeats (exact copies of "
                        f"earlier rows); without them it is {without:,}")
                out[said] = out.get(said, False) or loud
    return list(out.items()) if detail else list(out)


_SCALED = re.compile(r"(?i)(?<![\w.])(-?\d+(?:\.\d+)?)\s*(lakhs?|lacs?|l|crores?|cr|mn|million)\b")
_SCALE = {"lakh": 10**5, "lakhs": 10**5, "lac": 10**5, "lacs": 10**5, "l": 10**5, "crore": 10**7, "crores": 10**7,
          "cr": 10**7, "mn": 10**6, "million": 10**6}


def _figures(text):
    """(as written, value in units, how finely written) for every figure: plain (36,368,806) and in lakh/crore
    (363.69 lakh, 3.64 Cr) - a deck says the quarter in lakh."""

    out = []
    for t in set(_FIGURE.findall(text or "")):
        try:
            out.append((t, Decimal(t.replace(",", "")), Decimal(1)))
        except InvalidOperation:
            continue
    for m in _SCALED.finditer(text or ""):
        number, unit = m.group(1), m.group(2).lower()
        factor = Decimal(_SCALE[unit])
        decimals = len(number.split(".")[1]) if "." in number else 0
        out.append((m.group(0), Decimal(number) * factor, factor / (10 ** decimals)))
    return out


def report(path, header, facts):
    """The "Data check:" lines office read puts first, or ""."""

    lines = list((facts or {}).get("lines") or []) + overlap_lines(path, header, facts)
    if not lines:
        return ""
    return "Data check (row numbers as in the file, the header is row 1): " + " ".join(lines) + "\n"
