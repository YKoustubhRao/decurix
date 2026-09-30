"""
Documents and spreadsheets for back-office work: read what arrived (a PDF, a
scan, an Excel export, a Word file, an email), build what goes out (a new
workbook, a combined PDF), and update an established Excel model in place.

The model reads; this module never guesses. There is deliberately no
"parse a purchase order" here: a hosted model reading the document's own text
- or, for a scan, its pages as pictures - does that better than any regex,
and says when a field is not there instead of inventing a default.

Updating an existing workbook is done on its XML (`update_workbook`), not by
loading and re-saving it with openpyxl: openpyxl drops charts, images and
pivot caches it does not understand, so "refresh the MIS model" would quietly
destroy the model's charts. Only the cells named change; the workbook is told
to recalculate when opened.
"""

import json
import os
import re
import shutil
import subprocess
import tempfile
import zipfile
from email import policy
from email.parser import BytesParser
from pathlib import Path

# How much of one document is handed to the model as text.
MAX_TEXT_CHARS = int(os.environ.get("DECURIX_DOC_TEXT_CHARS", "30000"))
# A scanned document is shown as pictures, at most this many pages at once.
MAX_PAGE_IMAGES = 6
# Below this many characters of text per page a PDF is taken to be a scan.
SCANNED_CHARS_PER_PAGE = 40
# How many rows of each sheet reading a workbook shows by default.
DEFAULT_ROWS = 60

IMAGE_TYPES = {".png", ".jpg", ".jpeg", ".webp", ".gif"}
TEXT_TYPES = {".txt", ".md", ".csv", ".tsv", ".json", ".xml", ".html", ".htm", ".log"}


class OfficeError(Exception):
    pass


def _path(path):
    p = Path(os.path.expanduser(str(path or ""))).resolve()
    if not str(path or "").strip():
        raise OfficeError("no file path was given")
    return p


def _clip(text, limit=MAX_TEXT_CHARS):
    if len(text) <= limit:
        return text
    return text[:limit] + f"\n\n[... {len(text) - limit} more characters not shown - ask for fewer pages or a smaller range]"


# ------------------------------------------------------------------
# Reading
# ------------------------------------------------------------------

def read_document(path, pages=None, as_images=False):
    """
    Read one file for the model. Returns {"text": str, "images": [paths]}.

    `pages` ("1-3", "2") limits a PDF; `as_images` shows its pages as pictures
    even when they carry text (a table whose layout matters, a stamp, a
    signature). A PDF with next to no text is a scan and is shown as pictures
    anyway.
    """

    p = _path(path)
    if not p.exists():
        raise OfficeError(f"{p} does not exist")

    if p.is_dir():
        return {"text": _list_dir(p), "images": []}

    ext = p.suffix.lower()

    if ext == ".pdf":
        return _read_pdf(p, pages, as_images)
    if ext in IMAGE_TYPES:
        return {"text": f"{p.name}: an image, shown below.", "images": [str(p)]}
    if ext in (".xlsx", ".xlsm"):
        return {"text": read_workbook(p), "images": []}
    if ext == ".docx":
        return {"text": _clip(_read_docx(p)), "images": []}
    if ext == ".eml":
        return {"text": _clip(_read_eml(p)), "images": []}
    if ext in TEXT_TYPES or _looks_textual(p):
        return {"text": _clip(p.read_text(encoding="utf-8", errors="replace")), "images": []}

    raise OfficeError(f"{p.name}: cannot read a {ext or 'extension-less'} file - convert it first, or open it on the screen")


def _looks_textual(p):
    try:
        chunk = p.read_bytes()[:2048]
    except OSError:
        return False
    return b"\x00" not in chunk


def _list_dir(p):
    lines = [f"Folder {p}:"]
    for child in sorted(p.iterdir()):
        kind = "folder" if child.is_dir() else f"{child.stat().st_size} bytes"
        lines.append(f"  {child.name}  ({kind})")
    return "\n".join(lines)


def _page_range(pages, count):
    if not pages:
        return 1, count
    m = re.fullmatch(r"\s*(\d+)\s*(?:-\s*(\d+))?\s*", str(pages))
    if not m:
        raise OfficeError(f"pages must look like '2' or '1-3', not {pages!r}")
    first = max(1, int(m.group(1)))
    last = min(count, int(m.group(2) or m.group(1)))
    if first > last:
        raise OfficeError(f"the PDF has {count} pages")
    return first, last


def _pdf_page_count(p):
    out = subprocess.run(["pdfinfo", str(p)], capture_output=True, text=True, timeout=60)
    m = re.search(r"^Pages:\s+(\d+)", out.stdout, re.M)
    if not m:
        raise OfficeError(f"{p.name} could not be opened as a PDF: {out.stderr.strip()[:200]}")
    return int(m.group(1))


def _read_pdf(p, pages, as_images):
    if not shutil.which("pdftotext"):
        raise OfficeError("pdftotext is not installed on this desktop (poppler-utils)")

    count = _pdf_page_count(p)
    first, last = _page_range(pages, count)

    out = subprocess.run(
        ["pdftotext", "-layout", "-f", str(first), "-l", str(last), str(p), "-"],
        capture_output=True, text=True, timeout=120,
    )
    text = out.stdout.strip()
    shown = last - first + 1
    scanned = len(text) < SCANNED_CHARS_PER_PAGE * shown

    header = f"{p.name}: PDF, {count} page(s); pages {first}-{last}."
    images = []

    if as_images or scanned:
        last_img = min(last, first + MAX_PAGE_IMAGES - 1)
        images = _pdf_pages_as_png(p, first, last_img)
        header += f" Pages {first}-{last_img} are shown as pictures below"
        header += " (it is a scan - read it from the pictures)." if scanned else "."
        if last_img < last:
            header += f" Ask for pages {last_img + 1}-{last} to see the rest."

    body = "" if scanned else "\n\n" + _clip(text)
    return {"text": header + body, "images": images}


def _pdf_pages_as_png(p, first, last):
    out_dir = Path(tempfile.mkdtemp(prefix="decurix-pages-"))
    subprocess.run(
        ["pdftoppm", "-r", "110", "-png", "-f", str(first), "-l", str(last), str(p), str(out_dir / "page")],
        capture_output=True, timeout=180, check=False,
    )
    return [str(x) for x in sorted(out_dir.glob("page*.png"))]


def _read_docx(p):
    try:
        import docx  # python3-docx
    except ImportError:
        return _docx_text_from_xml(p)

    d = docx.Document(str(p))
    parts = [para.text for para in d.paragraphs if para.text.strip()]
    for n, table in enumerate(d.tables, 1):
        parts.append(f"\n[Table {n}]")
        for row in table.rows:
            parts.append(" | ".join(cell.text.strip() for cell in row.cells))
    return f"{p.name}:\n" + "\n".join(parts)


def _docx_text_from_xml(p):
    with zipfile.ZipFile(p) as z:
        xml = z.read("word/document.xml").decode("utf-8", errors="replace")
    xml = re.sub(r"</w:p>", "\n", xml)
    return f"{p.name}:\n" + re.sub(r"<[^>]+>", "", xml)


def _read_eml(p):
    """An email file: its headers, its text, and its attachments saved beside it."""

    msg = BytesParser(policy=policy.default).parsebytes(p.read_bytes())
    lines = [f"From: {msg['from']}", f"To: {msg['to']}", f"Date: {msg['date']}", f"Subject: {msg['subject']}", ""]

    body = msg.get_body(preferencelist=("plain", "html"))
    if body is not None:
        text = body.get_content()
        if body.get_content_type() == "text/html":
            text = re.sub(r"<[^>]+>", " ", text)
        lines.append(text.strip())

    saved = []
    out_dir = p.with_suffix("")
    for part in msg.iter_attachments():
        name = safe_filename(part.get_filename() or "attachment")
        out_dir.mkdir(exist_ok=True)
        target = out_dir / name
        target.write_bytes(part.get_payload(decode=True) or b"")
        saved.append(str(target))

    if saved:
        lines.append("\nAttachments saved:\n" + "\n".join("  " + s for s in saved))

    return "\n".join(lines)


def safe_filename(name):
    name = os.path.basename(str(name)).strip().replace("\x00", "")
    name = re.sub(r"[^\w.\- ()&+,@]", "_", name)
    return name[:150] or "file"


# ------------------------------------------------------------------
# Workbooks: reading
# ------------------------------------------------------------------

def _openpyxl():
    try:
        import openpyxl
    except ImportError as exc:
        raise OfficeError("openpyxl is not installed on this desktop (python3-openpyxl)") from exc
    return openpyxl


def read_workbook(path, sheet=None, cells=None, max_rows=DEFAULT_ROWS):
    """
    A workbook as text: every sheet (or one) with its size, then its cells as
    a grid with their addresses. A formula shows as `=SUM(B2:B9) -> 1200`
    (the value Excel last saved). Charts and tables are listed with the
    ranges they read, which is what has to change when rows are added.
    """

    openpyxl = _openpyxl()
    p = _path(path)

    formulas = openpyxl.load_workbook(str(p), data_only=False)
    values = openpyxl.load_workbook(str(p), data_only=True)

    names = formulas.sheetnames
    if sheet:
        if sheet not in names:
            raise OfficeError(f"no sheet {sheet!r}; the sheets are {names}")
        names = [sheet]

    out = [f"{p.name}: sheets {formulas.sheetnames}"]

    for name in names:
        ws_f, ws_v = formulas[name], values[name]
        out.append(f"\n=== Sheet {name!r}: used range {ws_f.dimensions} ({ws_f.max_row} rows x {ws_f.max_column} columns) ===")

        if cells:
            rows = ws_f[cells]
            if not isinstance(rows, tuple) or (rows and not isinstance(rows[0], tuple)):
                rows = (rows if isinstance(rows, tuple) else (rows,),)
        else:
            last = min(ws_f.max_row, max_rows)
            rows = ws_f.iter_rows(min_row=1, max_row=last)
            if ws_f.max_row > last:
                out.append(f"(first {last} rows shown; ask for cells like 'A{last + 1}:H{min(ws_f.max_row, last + 100)}' for more)")

        for row in rows:
            shown = []
            for c in row:
                if c.value is None:
                    continue
                v = c.value
                if isinstance(v, str) and v.startswith("="):
                    cached = ws_v[c.coordinate].value
                    shown.append(f"{c.coordinate}={v} -> {cached!r}")
                else:
                    shown.append(f"{c.coordinate}={v!r}")
            if shown:
                out.append("  ".join(shown))

    out.extend(_workbook_objects(p))
    return _clip("\n".join(out))


def _workbook_objects(p):
    """Charts, tables and defined names, with the ranges they refer to."""

    lines = []
    with zipfile.ZipFile(p) as z:
        for name in sorted(z.namelist()):
            if re.match(r"xl/charts/chart\d+\.xml$", name):
                xml = z.read(name).decode("utf-8", errors="replace")
                refs = sorted(set(re.findall(r"<(?:\w+:)?f>([^<]+)</(?:\w+:)?f>", xml)))
                title = " ".join(re.findall(r"<(?:\w+:)?t>([^<]*)</(?:\w+:)?t>", xml))[:80]
                lines.append(f"chart {name.rsplit('/', 1)[-1]} {title!r} reads: {', '.join(refs) or '(no ranges)'}")
            elif re.match(r"xl/tables/table\d+\.xml$", name):
                xml = z.read(name).decode("utf-8", errors="replace")
                m = re.search(r'<table\b[^>]*\bdisplayName="([^"]+)"[^>]*\bref="([^"]+)"', xml)
                if m:
                    lines.append(f"table {m.group(1)} covers {m.group(2)}")
        if "xl/workbook.xml" in z.namelist():
            wb = z.read("xl/workbook.xml").decode("utf-8", errors="replace")
            for dn, ref in re.findall(r'<definedName\b[^>]*\bname="([^"]+)"[^>]*>([^<]*)</definedName>', wb):
                if not dn.startswith("_xlnm"):
                    lines.append(f"name {dn} = {ref}")
    if lines:
        lines.insert(0, "\nObjects in the workbook (update their ranges with replace_refs when rows are added):")
    return lines


# ------------------------------------------------------------------
# Workbooks: creating
# ------------------------------------------------------------------

SHEET_KEYS = {"name", "rows", "cells", "header", "widths", "number_format"}


def create_workbook(path, sheets):
    """
    A new workbook. `sheets` is a list of {"name", "rows": [[...], ...],
    "cells": {"C5": "=SUM(C2:C4)"}, "header": true, "widths": {"A": 20},
    "number_format": {"C": "#,##0.00"}}. `rows` fill from A1 down; `cells`
    set single cells by address, after the rows (a value, "=FORMULA", or
    {"formula": ...} / {"value": ...}). A string starting with "=" is a
    formula. Refuses to overwrite a file.

    A sheet given neither rows nor cells, or a key this does not read, is
    refused: live, a spec of {"cells": {...}} (before cells existed) made an
    empty sheet, the result said "Created", and the engine attached the empty
    workbook to an email and reported the task done.
    """

    openpyxl = _openpyxl()
    from openpyxl.styles import Font, PatternFill

    p = _path(path)
    if p.exists():
        raise OfficeError(f"{p} already exists - pick another name, or use update_workbook to change it")
    if not sheets:
        raise OfficeError("give at least one sheet")

    for spec in sheets:
        if not isinstance(spec, dict):
            raise OfficeError("each sheet is an object like {\"name\": \"Data\", \"rows\": [[...], ...]}")
        unknown = sorted(set(spec) - SHEET_KEYS)
        if unknown:
            raise OfficeError(f"a sheet does not take {', '.join(map(repr, unknown))}; it takes "
                              f"{', '.join(sorted(SHEET_KEYS))}")
        if "rows" not in spec and "cells" not in spec:
            raise OfficeError(f"sheet {spec.get('name') or '(unnamed)'!r} has no data: give \"rows\" "
                              "([[\"Invoice\", \"Amount\"], [\"INV-1\", 1200]]) or \"cells\" ({\"A1\": \"Invoice\"})")

    wb = openpyxl.Workbook()
    wb.remove(wb.active)

    for spec in sheets:
        ws = wb.create_sheet(str(spec.get("name") or f"Sheet{len(wb.sheetnames) + 1}")[:31])
        for row in spec.get("rows") or []:
            ws.append([_cell_value(v) for v in row])
        for ref, v in (spec.get("cells") or {}).items():
            cell = str(ref).upper().replace("$", "")
            if not re.fullmatch(r"[A-Z]{1,3}[1-9]\d*", cell):
                raise OfficeError(f"{ref!r} is not a cell address like B14")
            if isinstance(v, dict):
                if "value" in v:
                    v = v["value"]
                elif "formula" in v:
                    v = str(v["formula"] or "")
                    if v and not v.startswith("="):
                        v = "=" + v
                else:
                    raise OfficeError(f"cell {cell} has no value: give a value, \"=FORMULA\" or {{\"formula\": ...}}")
            ws[cell] = _cell_value(v)

        if spec.get("header", True) and ws.max_row >= 1:
            for c in ws[1]:
                c.font = Font(bold=True)
                c.fill = PatternFill("solid", fgColor="DDE6F0")
            ws.freeze_panes = "A2"

        for col, width in (spec.get("widths") or {}).items():
            ws.column_dimensions[col].width = float(width)

        for col, fmt in (spec.get("number_format") or {}).items():
            for cell in ws[col][1:]:
                cell.number_format = fmt

        if not spec.get("widths"):
            for column in ws.columns:
                longest = max((len(str(c.value)) for c in column if c.value is not None), default=8)
                ws.column_dimensions[column[0].column_letter].width = min(max(10, longest + 2), 60)

    p.parent.mkdir(parents=True, exist_ok=True)
    wb.save(str(p))
    # Say what is in it, so a reader of this result sees an empty or short
    # sheet for what it is instead of taking "Created" for the whole job.
    parts = []
    for ws in wb:
        filled = sum(1 for r in ws.iter_rows() for c in r if c.value not in (None, ""))
        parts.append(f"{ws.title}: {ws.max_row} rows x {ws.max_column} columns, {filled} cells filled")
    return f"Created {p} - " + "; ".join(parts) + ". Run office read on it to check the values."


def _cell_value(v):
    if isinstance(v, str):
        s = v.strip()
        if re.fullmatch(r"-?\d+(\.\d+)?", s) and not (len(s) > 1 and s.startswith("0") and "." not in s):
            return float(s) if "." in s else int(s)
    return v


# ------------------------------------------------------------------
# Workbooks: updating in place (charts survive)
# ------------------------------------------------------------------

_MAIN_NS_REL = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"


def update_workbook(path, edits=None, replace_refs=None, output_path=None):
    """
    Change cells of an existing workbook without touching anything else.

    `edits`: [{"sheet": "Data", "cell": "B14", "value": 1250.5}] - a number,
    text, true/false, null (empties the cell) or a formula ("=SUM(B2:B14)").
    `replace_refs`: [{"from": "Data!$B$2:$B$13", "to": "Data!$B$2:$B$14"}] -
    rewritten wherever charts, tables and defined names refer to them, so a
    chart takes in a month that was appended.

    Writes `output_path` (default: the file itself, after keeping the
    original as <name>.orig.xlsx the first time). Excel and LibreOffice
    recalculate every formula when the file is next opened.
    """

    src = _path(path)
    if not src.exists():
        raise OfficeError(f"{src} does not exist")
    dst = _path(output_path) if output_path else src

    with zipfile.ZipFile(src) as z:
        entries = {i.filename: z.read(i.filename) for i in z.infolist()}
        order = [i.filename for i in z.infolist()]

    sheet_files = _sheet_files(entries)
    changed = []

    by_sheet = {}
    for e in edits or []:
        sheet = e.get("sheet") or next(iter(sheet_files))
        if sheet not in sheet_files:
            raise OfficeError(f"no sheet {sheet!r}; the sheets are {list(sheet_files)}")
        cell = str(e.get("cell") or "").upper().replace("$", "")
        if not re.fullmatch(r"[A-Z]{1,3}[1-9]\d*", cell):
            raise OfficeError(f"{e.get('cell')!r} is not a cell address like B14")
        # "formula" is how a model naturally names a formula's edit; taken as
        # the value (with its "="). An edit with neither key is refused, never
        # read as "empty the cell": live, {"cell": "D2", "formula": "=SUM(..)"}
        # wiped a total and the engine reported it updated.
        if "value" in e:
            value = e["value"]
        elif "formula" in e:
            value = str(e["formula"] or "")
            if value and not value.startswith("="):
                value = "=" + value
        else:
            raise OfficeError(f"the edit of {sheet}!{cell} has no value: give \"value\" (a number, text, \"=FORMULA\", "
                              "or null to empty the cell)")
        by_sheet.setdefault(sheet, []).append((cell, value))

    for sheet, cell_edits in by_sheet.items():
        name = sheet_files[sheet]
        xml = entries[name].decode("utf-8")
        for cell, value in cell_edits:
            xml = _set_cell(xml, cell, value)
            changed.append(f"{sheet}!{cell}")
        entries[name] = _fix_dimension(xml).encode("utf-8")

    swaps = 0
    for r in replace_refs or []:
        old, new = str(r.get("from") or ""), str(r.get("to") or "")
        if not old or not new:
            raise OfficeError("each replace_refs entry needs from and to")
        # A sheet name is written quoted or bare ('Data'!$B$2 / Data!$B$2)
        # depending on who saved the file; both mean the same range. Live, a
        # model wrote the bare form, the file had the quoted one, and nothing
        # matched. Try the other spelling when the given one is not there.
        if not any(old in entries[n].decode("utf-8", "ignore") for n in entries
                   if re.match(r"xl/(charts/chart\d+|workbook|pivotCache/pivotCacheDefinition\d+)\.xml$", n)):
            old, new = _other_sheet_quoting(old), _other_sheet_quoting(new)
        for name in list(entries):
            if re.match(r"xl/(charts/chart\d+|tables/table\d+|workbook|pivotCache/pivotCacheDefinition\d+)\.xml$", name):
                text = entries[name].decode("utf-8")
                hits = text.count(old)
                if not hits and name.startswith("xl/tables/"):
                    old_local, new_local = old.split("!")[-1].replace("$", ""), new.split("!")[-1].replace("$", "")
                    text2 = text.replace(f'ref="{old_local}"', f'ref="{new_local}"')
                    hits = int(text2 != text)
                    text = text2
                else:
                    text = text.replace(old, new)
                if hits:
                    swaps += hits
                    entries[name] = text.encode("utf-8")

    # Every formula is recalculated on open, and the calculation chain (a
    # cache of formula order that no longer matches) is dropped - a stale
    # one is what makes Excel offer to "repair" a file.
    wb = entries["xl/workbook.xml"].decode("utf-8")
    if "<calcPr" in wb:
        wb = re.sub(r"<calcPr\b([^>]*?)(/?)>", lambda m: "<calcPr" + re.sub(r'\s+fullCalcOnLoad="[^"]*"', "", m.group(1)) + ' fullCalcOnLoad="1"' + m.group(2) + ">", wb, count=1)
    else:
        wb = wb.replace("</workbook>", '<calcPr fullCalcOnLoad="1"/></workbook>')
    entries["xl/workbook.xml"] = wb.encode("utf-8")

    if "xl/calcChain.xml" in entries:
        del entries["xl/calcChain.xml"]
        order.remove("xl/calcChain.xml")
        rels = entries["xl/_rels/workbook.xml.rels"].decode("utf-8")
        entries["xl/_rels/workbook.xml.rels"] = re.sub(r"<Relationship\b[^>]*calcChain[^>]*/>", "", rels).encode("utf-8")
        ct = entries["[Content_Types].xml"].decode("utf-8")
        entries["[Content_Types].xml"] = re.sub(r"<Override\b[^>]*calcChain[^>]*/>", "", ct).encode("utf-8")

    if dst == src:
        backup = src.with_name(src.stem + ".orig" + src.suffix)
        if not backup.exists():
            shutil.copy2(src, backup)

    tmp = dst.with_name(dst.name + ".tmp")
    dst.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as z:
        for name in order:
            z.writestr(name, entries[name])
    os.replace(tmp, dst)

    msg = f"Updated {dst}: {len(changed)} cell(s)"
    if changed:
        msg += " (" + ", ".join(changed[:20]) + (" ..." if len(changed) > 20 else "") + ")"
    if replace_refs:
        msg += f"; {swaps} range reference(s) rewritten"
        if not swaps:
            msg += " - none matched: read the workbook to see the exact ranges its charts use"
    return msg + ". Formulas recalculate when the file is opened."


def _other_sheet_quoting(ref):
    """Data!$B$2:$B$3 <-> 'Data'!$B$2:$B$3 (the sheet name quoted or not)."""

    sheet, bang, rest = ref.partition("!")
    if not bang:
        return ref
    if sheet.startswith("'") and sheet.endswith("'") and len(sheet) > 1:
        return sheet[1:-1] + "!" + rest
    return f"'{sheet}'!" + rest


def _sheet_files(entries):
    wb = entries["xl/workbook.xml"].decode("utf-8")
    rels = entries["xl/_rels/workbook.xml.rels"].decode("utf-8")
    targets = {}
    for rel in re.findall(r"<Relationship\b[^>]*/>", rels):
        rid = re.search(r'\bId="([^"]+)"', rel)
        tgt = re.search(r'\bTarget="([^"]+)"', rel)
        if rid and tgt:
            t = tgt.group(1)
            t = t.lstrip("/") if t.startswith("/") else "xl/" + t
            targets[rid.group(1)] = t
    out = {}
    for tag in re.findall(r"<sheet\b[^>]*/>", wb):
        name = re.search(r'\bname="([^"]*)"', tag).group(1)
        rid = re.search(r'\br:id="([^"]+)"', tag) or re.search(r'\b\w+:id="([^"]+)"', tag)
        if rid and rid.group(1) in targets:
            out[_unescape(name)] = targets[rid.group(1)]
    return out


def _unescape(s):
    return s.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", '"').replace("&apos;", "'")


def _escape(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def _col_num(letters):
    n = 0
    for ch in letters:
        n = n * 26 + ord(ch) - 64
    return n


def _split(ref):
    m = re.fullmatch(r"([A-Z]+)(\d+)", ref)
    return m.group(1), int(m.group(2))


def _new_cell(ref, value, style):
    s = f' s="{style}"' if style else ""
    if value is None or value == "":
        return f'<c r="{ref}"{s}/>'
    if isinstance(value, bool):
        return f'<c r="{ref}"{s} t="b"><v>{int(value)}</v></c>'
    if isinstance(value, (int, float)):
        return f'<c r="{ref}"{s}><v>{repr(float(value)) if isinstance(value, float) else value}</v></c>'
    text = str(value)
    if text.startswith("="):
        return f'<c r="{ref}"{s}><f>{_escape(text[1:])}</f></c>'
    num = _cell_value(text)
    if not isinstance(num, str):
        return _new_cell(ref, num, style)
    return f'<c r="{ref}"{s} t="inlineStr"><is><t xml:space="preserve">{_escape(text)}</t></is></c>'


_ROW_RE = r'<row\b[^>]*?\br="{n}"[^>]*?(?:/>|>.*?</row>)'
_CELL_RE = r'<c\b[^>]*?\br="{ref}"[^>]*?(?:/>|>.*?</c>)'


def _set_cell(xml, ref, value):
    col, row = _split(ref)

    if "<sheetData/>" in xml:
        xml = xml.replace("<sheetData/>", "<sheetData></sheetData>")

    m = re.search(_ROW_RE.format(n=row), xml, re.S)
    if not m:
        new_row = f'<row r="{row}">{_new_cell(ref, value, None)}</row>'
        return _insert_row(xml, row, new_row)

    row_xml = m.group(0)
    if row_xml.endswith("/>"):
        row_xml = row_xml[:-2] + "></row>"
    row_xml = re.sub(r'\s+spans="[^"]*"', "", row_xml, count=1)

    cm = re.search(_CELL_RE.format(ref=ref), row_xml, re.S)
    if cm:
        old = cm.group(0)
        if re.search(r'<f\b[^>]*\bt="shared"[^>]*\bref=', old):
            raise OfficeError(f"{ref} holds a formula shared with other cells - change those cells too, or edit it on the screen")
        style = re.search(r'\bs="(\d+)"', old)
        row_xml = row_xml.replace(old, _new_cell(ref, value, style.group(1) if style else None), 1)
    else:
        # Keep the cells of a row in column order; a new one takes the style
        # of the cell above it when there is one.
        cells = list(re.finditer(r'<c\b[^>]*?\br="([A-Z]+)\d+"[^>]*?(?:/>|>.*?</c>)', row_xml, re.S))
        style = _style_above(xml, col, row)
        new = _new_cell(ref, value, style)
        pos = None
        for c in cells:
            if _col_num(c.group(1)) > _col_num(col):
                pos = c.start()
                break
        if pos is None:
            pos = row_xml.rfind("</row>")
        row_xml = row_xml[:pos] + new + row_xml[pos:]

    return xml[:m.start()] + row_xml + xml[m.end():]


def _style_above(xml, col, row):
    for r in range(row - 1, max(0, row - 6), -1):
        m = re.search(_ROW_RE.format(n=r), xml, re.S)
        if not m:
            continue
        c = re.search(_CELL_RE.format(ref=f"{col}{r}"), m.group(0), re.S)
        if c:
            s = re.search(r'\bs="(\d+)"', c.group(0))
            return s.group(1) if s else None
    return None


def _insert_row(xml, row, new_row):
    start = xml.find("<sheetData>") + len("<sheetData>")
    end = xml.find("</sheetData>")
    body = xml[start:end]
    for m in re.finditer(r'<row\b[^>]*?\br="(\d+)"', body):
        if int(m.group(1)) > row:
            at = start + m.start()
            return xml[:at] + new_row + xml[at:]
    return xml[:end] + new_row + xml[end:]


def _fix_dimension(xml):
    refs = re.findall(r'<c\b[^>]*?\br="([A-Z]+)(\d+)"', xml)
    if not refs:
        return xml
    max_col = max(refs, key=lambda r: _col_num(r[0]))[0]
    max_row = max(int(r[1]) for r in refs)
    min_col = min(refs, key=lambda r: _col_num(r[0]))[0]
    min_row = min(int(r[1]) for r in refs)
    return re.sub(r'<dimension\b[^>]*/>', f'<dimension ref="{min_col}{min_row}:{max_col}{max_row}"/>', xml, count=1)


# ------------------------------------------------------------------
# PDFs out
# ------------------------------------------------------------------

def combine_pdf(output_path, inputs):
    """
    One PDF from PDFs and images, in the order given - a customer's
    onboarding file assembled from its PAN card scan, GST certificate, ...
    """

    p = _path(output_path)
    if p.exists():
        raise OfficeError(f"{p} already exists - pick another name")
    if not inputs:
        raise OfficeError("give the files to combine, in order")

    try:
        from pypdf import PdfReader, PdfWriter
    except ImportError as exc:
        raise OfficeError("pypdf is not installed on this desktop (python3-pypdf)") from exc

    writer = PdfWriter()
    added = []
    tmp = Path(tempfile.mkdtemp(prefix="decurix-combine-"))

    for i, item in enumerate(inputs):
        src = _path(item)
        if not src.exists():
            raise OfficeError(f"{src} does not exist")
        ext = src.suffix.lower()
        if ext == ".pdf":
            pdf = src
        elif ext in IMAGE_TYPES:
            pdf = tmp / f"{i}.pdf"
            _image_to_pdf(src, pdf)
        elif ext in OFFICE_TYPES:
            pdf = _office_to_pdf(src, tmp / str(i))
        else:
            raise OfficeError(f"{src.name}: only PDFs, images and office documents can be combined")
        for page in PdfReader(str(pdf)).pages:
            writer.add_page(page)
        added.append(src.name)

    p.parent.mkdir(parents=True, exist_ok=True)
    with open(p, "wb") as f:
        writer.write(f)
    return f"Created {p} from {len(added)} file(s): {', '.join(added)}."


OFFICE_TYPES = {".docx", ".doc", ".odt", ".rtf", ".xlsx", ".xls", ".ods", ".pptx", ".odp"}


def _office_to_pdf(src, out_dir):
    """A Word/Excel/LibreOffice document as a PDF, laid out as LibreOffice prints it."""

    soffice = shutil.which("soffice") or shutil.which("libreoffice")
    if not soffice:
        raise OfficeError(f"{src.name}: LibreOffice is not installed on this desktop, so it cannot be made a PDF")

    out_dir.mkdir(parents=True, exist_ok=True)
    # Its own profile: a LibreOffice a person has open would otherwise take the job and exit at once.
    profile = f"-env:UserInstallation=file://{out_dir}/profile"
    r = subprocess.run([soffice, profile, "--headless", "--convert-to", "pdf", "--outdir", str(out_dir), str(src)],
                       capture_output=True, text=True, timeout=240)
    pdf = out_dir / (src.stem + ".pdf")
    if not pdf.exists():
        raise OfficeError(f"{src.name}: LibreOffice could not convert it ({(r.stderr or r.stdout).strip()[:200]})")
    return pdf


def _image_to_pdf(src, dst):
    try:
        import img2pdf
        dst.write_bytes(img2pdf.convert(str(src)))
        return
    except ImportError:
        pass
    except Exception:
        pass  # img2pdf refuses images with an alpha channel; PIL does not

    try:
        from PIL import Image
    except ImportError as exc:
        raise OfficeError("neither img2pdf nor PIL is installed on this desktop") from exc
    Image.open(src).convert("RGB").save(dst, "PDF", resolution=150)


def _ocr_file(path, limit=MAX_TEXT_CHARS):
    """Text tesseract reads off one picture, "" when it cannot run."""

    try:
        done = subprocess.run(["tesseract", str(path), "stdout", "--psm", "3"],
                              capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError):
        return ""
    text = (done.stdout or "").strip()
    return text[:limit]


# ------------------------------------------------------------------
# Command line, for the other harnesses' shell loops: `office read x.pdf`
# ------------------------------------------------------------------

USAGE = """office - documents, spreadsheets and email drafts, from the shell

  office read FILE [PAGES]              read a PDF, scan, picture, Word, Excel or .eml file
  office update BOOK.xlsx SPEC          change cells of an existing workbook, keeping its charts,
                                        formatting and everything else. SPEC (JSON, - for stdin):
                                        {"edits": [{"sheet": "Data", "cell": "B14", "value": 1250.5}],
                                         "replace_refs": [{"from": "Data!$B$2:$B$13", "to": "Data!$B$2:$B$14"}],
                                         "output_path": "optional.xlsx"}
                                        a value is a number, text, null (empties the cell) or "=FORMULA";
                                        replace_refs extends what charts, tables and names read
  office create BOOK.xlsx SPEC          a new workbook. SPEC: {"sheets": [{"name": "Sheet1", "rows": [[...]],
                                        "cells": {"C5": "=SUM(C2:C4)"}, "header": true, "widths": {"A": 20},
                                        "number_format": {"C": "#,##0.00"}}]} - rows fill from A1, cells set
                                        one address each; a sheet needs one or the other
  office combine OUT.pdf IN...          one PDF from PDFs, pictures and office documents, in order
  office email --to ADDR [--cc ADDR] --subject S [--attach FILE]... [--body TEXT | body on stdin]
                                        put an email in the Outbox for a person to review and send.
                                        It is never sent from here.
"""


def _spec(arg):
    """A JSON spec given as `-` (stdin), inline JSON, or a file."""

    import sys

    if arg == "-":
        text = sys.stdin.read()
    elif arg.lstrip().startswith(("{", "[")):
        text = arg
    else:
        text = Path(os.path.expanduser(arg)).read_text()
    try:
        spec = json.loads(text)
    except json.JSONDecodeError as exc:
        raise OfficeError(f"the spec is not valid JSON ({exc})") from exc
    if not isinstance(spec, dict):
        raise OfficeError("the spec must be a JSON object")
    return spec


def _email(argv):
    import argparse
    import sys

    from decurix_agent.tools import outbox

    parser = argparse.ArgumentParser(prog="office email", add_help=False)
    parser.add_argument("--to", action="append", default=[])
    parser.add_argument("--cc", action="append", default=[])
    parser.add_argument("--subject", default="")
    parser.add_argument("--attach", action="append", default=[])
    parser.add_argument("--body")
    args, rest = parser.parse_known_args(argv)
    if rest:
        raise OfficeError(f"office email does not take {' '.join(rest)!r}; see `office` for its flags")

    body = args.body
    if body is None:
        body = "" if sys.stdin.isatty() else sys.stdin.read()
    if not args.subject.strip():
        raise OfficeError("give the email a --subject")
    if not body.strip():
        raise OfficeError("the email has no body: pass --body or send it on stdin")

    try:
        return outbox.draft(args.to, args.subject, body, cc=args.cc, attachments=args.attach)
    except outbox.DraftError as exc:
        raise OfficeError(f"not drafted: {exc}") from exc


def main(argv=None):
    import sys

    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE)
        return 0 if argv else 2
    try:
        cmd = argv.pop(0)
        if cmd == "read":
            r = read_document(argv[0], pages=argv[1] if len(argv) > 1 else None)
            print(r["text"])
            # How pictures reach the engine depends on its model (TheBot's
            # operator sets DECURIX_OFFICE_IMAGES): "attach" - a model that
            # reads images is shown each [page image: ...] itself; "ocr" - a
            # text-only model is given what OCR reads off them instead, since
            # a path to a picture is of no use to it.
            if os.environ.get("DECURIX_OFFICE_IMAGES", "").strip() == "ocr":
                for n, img in enumerate(r["images"], 1):
                    print(f"\n--- picture {n} of {len(r['images'])}, read by OCR (check figures against the context: OCR misreads) ---")
                    print(_ocr_file(img) or "(OCR read no text here)")
            else:
                for img in r["images"]:
                    print(f"[page image: {img}]")
        elif cmd == "update":
            if len(argv) < 2:
                raise OfficeError("usage: office update BOOK.xlsx SPEC (JSON, or - to read it from stdin)")
            spec = _spec(argv[1])
            print(update_workbook(argv[0], spec.get("edits"), spec.get("replace_refs"), spec.get("output_path")))
        elif cmd == "create":
            if len(argv) < 2:
                raise OfficeError("usage: office create BOOK.xlsx SPEC (JSON, or - to read it from stdin)")
            print(create_workbook(argv[0], _spec(argv[1]).get("sheets") or []))
        elif cmd == "combine":
            print(combine_pdf(argv[0], argv[1:]))
        elif cmd == "email":
            print(_email(argv))
        else:
            print(f"unknown command {cmd!r}\n\n{USAGE}")
            return 2
    except (OfficeError, OSError, ValueError, IndexError) as exc:
        print(f"error: {exc}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
