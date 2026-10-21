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
import sys
import tempfile
import zipfile
from email import policy
from email.parser import BytesParser
from pathlib import Path

# Libraries the desktop image keeps in a venv of its own (python-pptx: Ubuntu has no package for it). The image's
# ENV PATH names the venv, but the agent and soul are started by the desktop session, whose PATH is its own: found
# live (2026-10-14), every deck failed with "No module named 'pptx'" in a desktop that had it. The venv's packages
# are added after the system's, so a package both have is still the system's.
OFFICE_VENV = os.environ.get("DECURIX_OFFICE_VENV", "/opt/office-venv")


def _use_office_venv(root=OFFICE_VENV):
    import glob
    for site in sorted(glob.glob(os.path.join(root, "lib", "python3*", "site-packages"))):
        if site not in sys.path:
            sys.path.append(site)


_use_office_venv()

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
    # A file the person names under their shared folder (C:\Users\me\Documents\Shared\x.xlsx) is on this desktop
    # under ~/PC: read and made there (shared.py).
    from decurix_agent import shared

    path = shared.to_local(path) or path
    p = Path(os.path.expanduser(str(path or ""))).resolve()
    if not str(path or "").strip():
        raise OfficeError("no file path was given")
    return p


def _task_made(p):
    """Whether the task running now made this file: an in-place edit then keeps no "<name>.orig" copy - that copy is
    for the person's own files, and beside the task's own output it is a stale wrong version (source_guard)."""

    try:
        from decurix_agent import source_guard
        return source_guard.made_by_task(p)
    except Exception:  # noqa: BLE001 - not knowing keeps the copy
        return False


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
        return {"text": _data_check_workbook(p) + read_workbook(p), "images": []}
    if ext in (".docx", ".pptx"):
        # A file Word or PowerPoint would refuse (or "repair") is said to be broken, never read around: the XML read
        # below tolerates what a strict reader does not (series 14 P3, 2026-10-21).
        if zipfile.is_zipfile(p):
            strict_open(p)
        return {"text": _clip(_read_docx(p) if ext == ".docx" else _read_pptx(p)), "images": []}
    if ext == ".eml":
        return {"text": _clip(_read_eml(p)), "images": []}
    if ext in (".csv", ".tsv"):
        return {"text": _data_check_csv(p) + _clip(p.read_text(encoding="utf-8", errors="replace")), "images": []}
    if ext in TEXT_TYPES or _looks_textual(p):
        return {"text": _clip(p.read_text(encoding="utf-8", errors="replace")), "images": []}

    raise OfficeError(f"{p.name}: cannot read a {ext or 'extension-less'} file - convert it first, or open it on the screen")


def strict_open(p):
    """
    Open an Office file the way a program or the person's app does - openpyxl, python-docx, python-pptx, from its
    bytes (no handle left open on a file that fails) - and raise OfficeError(broken_file) when it cannot. Nothing for
    another kind of file, or when the reader is not installed.
    """

    status, said = open_status(p)
    if status == "broken":
        raise OfficeError(said)


# The first bytes of a Compound File (an encrypted or password-protected OOXML file, or a legacy .xls/.doc).
_CFB_MAGIC = b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"
# Strict Open XML: a valid file the common readers do not read.
_STRICT_OOXML = b"http://purl.oclc.org/ooxml/"


def open_status(p):
    """
    (status, sentence) for an Office file, as a strict reader opens it (independent review, 2026-10-21 - never call a
    legitimate file broken):
      "ok"         it opens (openpyxl - with its macros kept for .xlsm/.xltm -, python-docx, python-pptx);
      "protected"  an encrypted or password-protected file: not checked;
      "unchecked"  a kind not checked (legacy .xls/.doc, CSV, a template the reader refuses as such, Strict Open XML,
                   no reader installed);
      "broken"     it does not open - the sentence says so (broken_file).
    """

    import io
    import zipfile as _zip
    p = Path(p)
    suffix = p.suffix.lower()
    if suffix not in (".xlsx", ".xlsm", ".xltx", ".xltm", ".docx", ".dotx", ".docm", ".pptx", ".potx", ".pptm"):
        return "unchecked", f"{p.name}: not a kind this check opens"
    raw = p.read_bytes()
    if raw[:8] == _CFB_MAGIC:
        return "protected", f"{p.name} is password-protected or encrypted: it was not checked"
    try:
        with _zip.ZipFile(io.BytesIO(raw)) as z:
            if any(_STRICT_OOXML in z.read(n)[:4096] for n in z.namelist()
                   if n.endswith((".xml", ".rels")) and n.count("/") <= 1):
                return "unchecked", f"{p.name} is Strict Open XML: it was not checked"
    except _zip.BadZipFile as exc:
        return "broken", broken_file(p, exc)
    except Exception:  # noqa: BLE001 - the reader below decides
        pass
    try:
        data = io.BytesIO(raw)
        if suffix in (".xlsx", ".xlsm", ".xltx", ".xltm"):
            _openpyxl().load_workbook(data, keep_vba=suffix in (".xlsm", ".xltm")).close()
        elif suffix in (".docx", ".dotx", ".docm"):
            import docx
            try:
                docx.Document(data)
            except ValueError as exc:
                if "template" in str(exc).lower() or "content type" in str(exc).lower():
                    return "unchecked", f"{p.name}: a template or a kind the reader does not take: not checked"
                raise
        else:
            import pptx
            try:
                pptx.Presentation(data)
            except ValueError as exc:
                if "template" in str(exc).lower() or "content type" in str(exc).lower():
                    return "unchecked", f"{p.name}: a template or a kind the reader does not take: not checked"
                raise
    except ImportError:
        return "unchecked", f"{p.name}: no reader here to check it"
    except Exception as exc:  # noqa: BLE001 - any other failure to open is the finding
        return "broken", broken_file(p, exc)
    return "ok", ""


def broken_file(p, exc):
    """What office says of an Office file a strict reader cannot open: broken, and how to make it whole again."""

    return (f"{Path(p).name} cannot be opened: the file is broken ({type(exc).__name__}: "
            f"{' '.join(str(exc).split())[:160]}) - Excel, Word or a program reading it will refuse it or offer to "
            "repair it. Make it again with office create / office update (or openpyxl, python-docx, python-pptx); never "
            "edit an Office file's XML with a script.")


def _data_check_csv(p):
    """The table facts a total depends on (tools/table_quality.py), as the first lines of a CSV's read; "" for none."""

    try:
        from decurix_agent.tools import table_quality
        facts = table_quality.csv_facts(p)
        if not facts:
            return ""
        import csv
        with open(p, encoding="utf-8-sig", errors="replace", newline="") as handle:
            header = next(csv.reader(handle, delimiter="\t" if p.suffix.lower() == ".tsv" else ","), [])
        return table_quality.report(p, header, facts)
    except Exception:  # noqa: BLE001 - a check that cannot be made says nothing
        return ""


def _data_check_workbook(p):
    """The same facts for each sheet of a workbook that is one table with its header in row 1."""

    try:
        from decurix_agent.tools import table_quality
        book = _openpyxl().load_workbook(str(p), data_only=True, read_only=True)
        out = []
        try:
            for sheet in book.worksheets:
                facts = table_quality.sheet_facts(sheet)
                if facts:
                    header = next(sheet.iter_rows(values_only=True), ())
                    said = table_quality.report(f"{p}#{sheet.title}", header, facts)
                    if said:
                        out.append(f"[sheet {sheet.title}] " + said)
        finally:
            book.close()
        return "".join(out)
    except Exception:  # noqa: BLE001
        return ""


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


def _pdf_text(p, first, last):
    out = subprocess.run(
        ["pdftotext", "-layout", "-f", str(first), "-l", str(last), str(p), "-"],
        capture_output=True, text=True, timeout=120,
    )
    return out.stdout


def _read_pdf(p, pages, as_images):
    if not shutil.which("pdftotext"):
        raise OfficeError("pdftotext is not installed on this desktop (poppler-utils)")

    count = _pdf_page_count(p)
    first, last = _page_range(pages, count)

    text = _pdf_text(p, first, last).strip()
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
    """
    A Word document as text, in reading order and with its structure: headings marked "#", each page break as
    "[page break]", tables in place. Read from the document's XML, so what a reader cannot see is still shown. Found
    live (2026-10-19): four letters "each on its own page" were joined by the two characters "\\f" typed as text; read
    as paragraphs only, the file looked right, and the check passed the report's word for its page breaks.
    """

    try:
        structure = docx_structure(p)
    except (KeyError, zipfile.BadZipFile, OSError, ValueError) as exc:
        raise OfficeError(f"{p.name} is not a Word document that can be read ({exc})") from exc
    head = (f"{p.name}: {structure['page_breaks']} page break(s), {structure['tables']} table(s), "
            f"{len(structure['headings'])} heading(s)")
    out = [head]
    if structure["typed_escapes"]:
        out.append("(The text holds " + ", ".join(f'"{e}"' for e in structure["typed_escapes"]) + " typed as "
                   "characters - a backslash sequence is not a page break, line break or tab in a document.)")
    out.extend(structure["lines"])
    return "\n".join(out)


_W_TEXT = re.compile(r"<w:t(?:\s[^>]*)?>(.*?)</w:t>|<w:t\s*/>|(<w:tab\s*/>)|<w:br\b([^>]*?)/?>|<w:cr\s*/>", re.S)
_W_BLOCK = re.compile(r"<w:tbl>.*?</w:tbl>|<w:p\b[^>]*/>|<w:p\b[^>]*>.*?</w:p>", re.S)
# A backslash sequence typed as text where a key was meant (\f for a page break, \n, \t, \r).
_TYPED_ESCAPE = re.compile(r"\\[fntrv]")


def _paragraph_runs(xml):
    """A paragraph's text, with its page breaks as None entries: ["text", None, "more text"]."""

    from xml.sax.saxutils import unescape

    parts, cur = [], []
    for m in _W_TEXT.finditer(xml):
        if m.group(1) is not None:
            cur.append(unescape(m.group(1), {"&quot;": '"', "&apos;": "'"}))
        elif m.group(2):
            cur.append("\t")
        elif m.group(3) is not None and 'w:type="page"' in m.group(3):
            parts.append("".join(cur))
            parts.append(None)
            cur = []
        elif m.group(0).startswith(("<w:br", "<w:cr")):
            cur.append("\n")
    parts.append("".join(cur))
    return parts


def docx_structure(p):
    """
    {"lines", "page_breaks", "tables", "headings", "typed_escapes"} of a .docx, from its XML, in reading order. A page
    break is a break of type page, a paragraph that starts a page (pageBreakBefore) or a section that starts a new page.
    """

    with zipfile.ZipFile(p) as z:
        xml = z.read("word/document.xml").decode("utf-8", errors="replace")
    body = xml.split("<w:body>", 1)[-1]
    lines, headings, breaks, tables, text_seen = [], [], 0, 0, []
    for block in _W_BLOCK.finditer(body):
        b = block.group(0)
        if b.startswith("<w:tbl>"):
            tables += 1
            lines.append(f"[Table {tables}]")
            for row in re.findall(r"<w:tr\b.*?</w:tr>", b, re.S):
                cells = ["".join(x for x in _paragraph_runs(c) if x).strip()
                         for c in re.findall(r"<w:tc\b.*?</w:tc>", row, re.S)]
                lines.append(" | ".join(cells))
                text_seen.extend(cells)
            continue
        ppr = re.search(r"<w:pPr>.*?</w:pPr>", b, re.S)
        ppr = ppr.group(0) if ppr else ""
        if re.search(r'<w:pageBreakBefore(?:\s*/>|\s+w:val="(?:1|true|on)")', ppr):
            breaks += 1
            lines.append("[page break]")
        style = re.search(r'<w:pStyle w:val="([^"]+)"', ppr)
        level = re.match(r"(?i)heading\s*(\d)", style.group(1)) if style else None
        title = style is not None and style.group(1).lower() == "title"
        for part in _paragraph_runs(b):
            if part is None:
                breaks += 1
                lines.append("[page break]")
                continue
            if not part.strip():
                continue
            text_seen.append(part)
            if level or title:
                headings.append(part.strip())
                lines.append("#" * (int(level.group(1)) if level else 1) + " " + part.strip())
            else:
                lines.append(part)
        # A section break inside a paragraph's properties starts the next section on a new page unless it says not.
        sect = re.search(r"<w:sectPr\b.*?</w:sectPr>", ppr, re.S)
        if sect and not re.search(r'<w:type w:val="(?:continuous|nextColumn)"', sect.group(0)):
            breaks += 1
            lines.append("[page break]")
    escapes = sorted(set(_TYPED_ESCAPE.findall("\n".join(text_seen))))
    return {"lines": lines, "page_breaks": breaks, "tables": tables, "headings": headings, "typed_escapes": escapes}


def _docx_text_from_xml(p):
    with zipfile.ZipFile(p) as z:
        xml = z.read("word/document.xml").decode("utf-8", errors="replace")
    xml = re.sub(r"</w:p>", "\n", xml)
    return f"{p.name}:\n" + re.sub(r"<[^>]+>", "", xml)


def _read_pptx(p):
    """Read a deck in presentation order: each slide's text, its tables row by row, its pictures, its charts'
    data and its speaker notes.

    This is content inspection; visual layout still requires opening or rendering
    the deck. Reading XML needs no optional presentation library.
    """
    import posixpath
    from xml.etree import ElementTree as ET

    ns = {"p": "http://schemas.openxmlformats.org/presentationml/2006/main",
          "a": "http://schemas.openxmlformats.org/drawingml/2006/main"}
    rid = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}id"
    try:
        with zipfile.ZipFile(p) as z:
            names = set(z.namelist())
            presentation = ET.fromstring(z.read("ppt/presentation.xml"))
            rels = ET.fromstring(z.read("ppt/_rels/presentation.xml.rels"))
            targets = {r.get("Id"): r.get("Target") for r in rels
                       if r.get("TargetMode") != "External"}
            parts = [f"{p.name}:"]
            for n, slide in enumerate(presentation.findall("p:sldIdLst/p:sldId", ns), 1):
                target = targets[slide.get(rid)]
                member = posixpath.normpath(posixpath.join("ppt", target)) if not target.startswith("/") else target.lstrip("/")
                if not member.startswith("ppt/slides/"):
                    raise ValueError("invalid slide relationship")
                root = ET.fromstring(z.read(member))
                parts.append(f"\n[Slide {n}]")
                parts.extend(_slide_lines(root))
                # What the slide's own text does not hold: its charts' data and its speaker notes (found
                # 2026-10-14: a deck read back as slide text alone could not show a chart's figures, so an
                # update of them could not be checked).
                for kind, part in _slide_parts(z, names, member):
                    if kind == "chart":
                        parts.extend(_chart_lines(ET.fromstring(z.read(part))))
                    elif kind == "notes":
                        notes = _notes_text(ET.fromstring(z.read(part)))
                        if notes:
                            parts.append("Speaker notes: " + notes)
            return "\n".join(parts)
    except (OSError, zipfile.BadZipFile, KeyError, TypeError, ValueError, ET.ParseError) as exc:
        raise OfficeError(f"{p.name}: cannot read PowerPoint presentation: {exc}") from exc


_A = "{http://schemas.openxmlformats.org/drawingml/2006/main}"
_P = "{http://schemas.openxmlformats.org/presentationml/2006/main}"
_C = "{http://schemas.openxmlformats.org/drawingml/2006/chart}"


def _paragraph_text(node):
    return "".join(t.text or "" for t in node.iter(_A + "t"))


def _slide_lines(root):
    """A slide's text in document order: paragraphs, a table as one line per row, a picture as [picture]."""

    out = []

    def walk(node):
        if node.tag == _A + "tbl":
            for row in node.iter(_A + "tr"):
                cells = [" ".join(_paragraph_text(p) for p in tc.iter(_A + "p")).strip() for tc in row.findall(_A + "tc")]
                out.append("| " + " | ".join(cells) + " |")
            return
        if node.tag == _A + "p":
            text = _paragraph_text(node)
            if text.strip():
                out.append(text)
            return
        if node.tag == _P + "pic":
            props = node.find(f"{_P}nvPicPr/{_P}cNvPr")
            said = (props.get("descr") or "") if props is not None else ""
            out.append(f"[picture{': ' + said if said else ''}]")
            return
        for child in node:
            walk(child)

    walk(root)
    return out


def _slide_parts(z, names, member):
    """(kind, part) for a slide's charts and notes, from its relationships."""

    import posixpath
    from xml.etree import ElementTree as ET

    rels = posixpath.join(posixpath.dirname(member), "_rels", posixpath.basename(member) + ".rels")
    if rels not in names:
        return []
    out = []
    for rel in ET.fromstring(z.read(rels)):
        kind = (rel.get("Type") or "").rsplit("/", 1)[-1]
        target = rel.get("Target") or ""
        if rel.get("TargetMode") == "External" or kind not in ("chart", "notesSlide"):
            continue
        part = posixpath.normpath(posixpath.join(posixpath.dirname(member), target))
        if part in names:
            out.append(("chart" if kind == "chart" else "notes", part))
    return out


def _cached(node):
    """The values a chart caches for a series' categories or values, in point order."""

    if node is None:
        return []
    points = sorted(node.iter(_C + "pt"), key=lambda pt: int(pt.get("idx") or 0))
    return [(pt.findtext(_C + "v") or "") for pt in points]


def _chart_lines(root):
    title = root.find(f".//{_C}chart/{_C}title")
    head = "Chart" + (f" \"{_paragraph_text(title)}\"" if title is not None and _paragraph_text(title).strip() else "")
    kinds = [child.tag.replace(_C, "") for plot in root.iter(_C + "plotArea") for child in plot if child.tag.endswith("Chart")]
    out = [head + (f" ({', '.join(kinds)})" if kinds else "") + ":"]
    for n, series in enumerate(root.iter(_C + "ser"), 1):
        name = " ".join(_cached(series.find(_C + "tx"))) or f"series {n}"
        categories = _cached(series.find(_C + "cat"))
        values = _cached(series.find(_C + "val")) or _cached(series.find(_C + "yVal"))
        if n == 1 and categories:
            out.append("  categories: " + ", ".join(categories))
        out.append(f"  {name}: " + ", ".join(values))
    return out


def _notes_text(root):
    """The text of a notes page's body placeholder (not its slide picture or number)."""

    lines = []
    for shape in root.iter(_P + "sp"):
        ph = shape.find(f"{_P}nvSpPr/{_P}nvPr/{_P}ph")
        if ph is not None and ph.get("type") == "body":
            lines += [_paragraph_text(p) for p in shape.iter(_A + "p") if _paragraph_text(p).strip()]
    return " / ".join(lines)


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

    strict_open(p)  # (a broken file is said to be broken, never read around - series 14 P3, 2026-10-21)
    formulas = openpyxl.load_workbook(str(p), data_only=False)
    values = openpyxl.load_workbook(str(p), data_only=True)
    # A formula written by a tool (this one, openpyxl) has no saved value until a spreadsheet program opens the file:
    # read back, every total showed "-> None". Found live (2026-10-14): a correct model was refused by the final check
    # for its "blank cached Summary values" - nothing could show what the formulas came to. They are worked out by
    # LibreOffice on a copy, and shown as calculated.
    # Which formulas have no result in the file is read from the file's own XML: a formula whose result is empty
    # text ("" for a missing price) is stored as one, and openpyxl reads it as None like a missing one.
    unstored = formulas_without_results(p)
    calculated, saved = False, values
    if unstored:
        recalculated = _recalculated_values(p, openpyxl)
        if recalculated is not None:
            values, calculated = recalculated, True

    names = formulas.sheetnames
    if sheet:
        if sheet not in names:
            raise OfficeError(f"no sheet {sheet!r}; the sheets are {names}")
        names = [sheet]

    out = [f"{p.name}: sheets {formulas.sheetnames}"]

    if unstored:
        # Said as a fault of the file, not only of this read: found live (2026-10-19), a workbook a script wrote was
        # read back here with every value worked out on the fly, reported "completed and verified" - and the file
        # handed in held no result for any of its formulas, so an import, a preview or a script reading it saw blanks.
        out.append(f"(Formulas without stored results: {len(unstored)} formula cell(s) of this file hold no saved "
                   f"result ({_ref_sample(unstored)}). A program, a preview or an import that reads the file sees them "
                   "blank. " + ("The values after -> were calculated now by LibreOffice and are NOT in the file. "
                                if calculated else "")
                   + f"Store them with: office calculate {p})")

    for name in names:
        ws_f = formulas[name]
        ws_v = values[name] if name in values.sheetnames else saved[name]
        out.append(f"\n=== Sheet {name!r}: used range {ws_f.dimensions} ({ws_f.max_row} rows x {ws_f.max_column} columns) ===")
        # How the sheet is laid out for a reader, not only what it holds: a task that says to freeze the header
        # or make it bold is checked against this. Found live (2026-10-13): a header "frozen" with row 1 selected
        # (which freezes nothing) was reported done three times - the read-back showed values only.
        bold_header = [c.coordinate for c in ws_f[1] if c.value is not None and c.font is not None and c.font.b]
        out.append(f"Frozen panes: {('rows above / columns left of ' + ws_f.freeze_panes) if ws_f.freeze_panes else 'none'}; "
                   f"bold in row 1: {', '.join(bold_header) if bold_header else 'none'}")
        # Formatting a request may ask for - a filter, a highlight, a number format - as facts of the saved file.
        try:
            out.extend(_formatting_lines(ws_f))
        except Exception as exc:  # noqa: BLE001 - the values are still worth reading when this cannot be
            out.append(f"(formatting could not be read: {str(exc)[:120]})")

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


# ============================================================
# How a sheet is formatted, as facts a check can read
#
# A request often asks for formatting, not only values: a red fill on the overdue rows, filters on the header row, a
# number format. Found live (2026-10-20, round 5, real Excel): the worker could not tell from `office read` whether
# its fill and its filter were in the saved file ("office read does not report filter state ... I could not verify the
# saved formatting"), and the file had no fill and no conditional format at all. So the read says, per sheet and in a
# few lines: the filter range, conditional-format rules with what they apply, which rows carry a fill (runs, colour
# named), and each column's number format.
# ============================================================

# Plain names for the colours a person names. A colour is named after the nearest of these.
_COLOUR_NAMES = {
    "red": (255, 0, 0), "dark red": (192, 0, 0), "light red": (255, 199, 206), "pink": (255, 192, 203),
    "orange": (255, 165, 0), "light orange": (252, 228, 214), "yellow": (255, 255, 0), "light yellow": (255, 242, 204),
    "green": (0, 176, 80), "dark green": (0, 97, 0), "light green": (198, 239, 206), "blue": (0, 112, 192),
    "light blue": (221, 235, 247), "dark blue": (31, 78, 121), "purple": (112, 48, 160), "grey": (191, 191, 191),
    "light grey": (242, 242, 242), "dark grey": (89, 89, 89), "white": (255, 255, 255), "black": (0, 0, 0),
}
MAX_FILL_CELLS = 40000
MAX_FORMAT_LINES = 12


def _colour_name(hex6):
    r, g, b = (int(hex6[i:i + 2], 16) for i in (0, 2, 4))
    return min(_COLOUR_NAMES, key=lambda n: sum((x - y) ** 2 for x, y in zip(_COLOUR_NAMES[n], (r, g, b))))


def _colour(color):
    """A colour openpyxl read, as "red FF0000" (or "theme colour 5", "indexed colour 10"), or None for none."""

    if color is None:
        return None
    kind = getattr(color, "type", None)
    if kind == "rgb":
        rgb = str(color.rgb or "")
        if len(rgb) == 8 and rgb[:2] == "00" and rgb[2:] == "000000":
            return None  # openpyxl's "no colour"
        hex6 = rgb[-6:].upper()
        if not re.fullmatch(r"[0-9A-F]{6}", hex6):
            return None
        return f"{_colour_name(hex6)} {hex6}"
    if kind == "theme":
        return f"theme colour {color.theme}" + (f" (tint {color.tint:+.2f})" if getattr(color, "tint", 0) else "")
    if kind == "indexed":
        index = color.indexed
        if index in (64, 65):  # system foreground / background: no colour of its own
            return None
        try:
            from openpyxl.styles.colors import COLOR_INDEX
            hex6 = COLOR_INDEX[index][-6:].upper()
            return f"{_colour_name(hex6)} {hex6}"
        except (ImportError, IndexError, TypeError):
            return f"indexed colour {index}"
    return None


def _fill_colour(fill):
    """The colour a cell's fill paints, or None when it has none."""

    if fill is None or getattr(fill, "fill_type", None) in (None, "none"):
        return None
    if fill.fill_type == "solid":
        return _colour(fill.fgColor) or _colour(fill.bgColor)
    return _colour(fill.fgColor) or _colour(fill.bgColor) or f"{fill.fill_type} pattern"


def _runs(numbers):
    """[2, 5, 9, 10, 11] -> "2, 5, 9-11"."""

    out, numbers = [], sorted(set(numbers))
    start = prev = None
    for n in numbers + [None]:
        if start is not None and n == prev + 1:
            prev = n
            continue
        if start is not None:
            out.append(str(start) if start == prev else f"{start}-{prev}")
        start = prev = n
    return ", ".join(out)


def _rule_text(rule):
    """One conditional-format rule: what decides it, and what it applies."""

    parts = [rule.type or "rule"]
    if getattr(rule, "operator", None):
        parts.append(rule.operator)
    formulas = [f for f in (getattr(rule, "formula", None) or []) if f]
    if formulas:
        parts.append(" / ".join(("=" + f) if not str(f).startswith("=") else str(f) for f in formulas))
    if getattr(rule, "text", None):
        parts.append(repr(rule.text))
    applies = []
    dxf = getattr(rule, "dxf", None)
    if dxf is not None:
        fill = dxf.fill
        if fill is not None:
            # A differential fill keeps its colour in bgColor for a solid pattern (Excel), sometimes in fgColor.
            colour = _colour(fill.bgColor) or _colour(fill.fgColor) or _colour(getattr(fill, "start_color", None))
            if colour:
                applies.append(f"fill {colour}")
        if dxf.font is not None:
            if _colour(dxf.font.color):
                applies.append(f"font {_colour(dxf.font.color)}")
            if dxf.font.b:
                applies.append("bold")
    if rule.type in ("colorScale", "dataBar", "iconSet"):
        applies.append(rule.type)
    return " ".join(parts) + (" -> " + ", ".join(applies) if applies else " -> (applies no fill or font)")


def sheet_formatting(ws):
    """
    The formatting facts of one sheet (openpyxl, not read-only): {"filter": range or "", "conditional": [lines],
    "fills": {colour: [rows]}, "fill_cells": n, "number_formats": {column: format}, "checked_all": bool}.
    """

    facts = {"filter": "", "conditional": [], "fills": {}, "fill_cells": 0, "number_formats": {}, "checked_all": True}
    try:
        facts["filter"] = ws.auto_filter.ref or ""
    except AttributeError:
        pass
    try:
        for cf in ws.conditional_formatting:
            ranges = str(getattr(cf, "sqref", "") or "")
            for rule in cf.rules:
                facts["conditional"].append(f"{ranges}: {_rule_text(rule)}")
    except (AttributeError, TypeError):
        pass
    seen = 0
    formats = {}
    for row in ws.iter_rows(min_row=1, max_row=ws.max_row):
        for cell in row:
            seen += 1
            if seen > MAX_FILL_CELLS:
                facts["checked_all"] = False
                break
            colour = _fill_colour(cell.fill)
            if colour:
                facts["fills"].setdefault(colour, set()).add(cell.row)
                facts["fill_cells"] += 1
            if cell.row > 1 and cell.value is not None and cell.number_format not in (None, "General"):
                formats.setdefault(cell.column_letter, {}).setdefault(cell.number_format, 0)
                formats[cell.column_letter][cell.number_format] += 1
        if not facts["checked_all"]:
            break
    facts["fills"] = {k: sorted(v) for k, v in facts["fills"].items()}
    facts["number_formats"] = {col: max(found, key=found.get) for col, found in formats.items()}
    return facts


def _formatting_lines(ws):
    """sheet_formatting as the few lines `office read` shows."""

    facts = sheet_formatting(ws)
    lines = [f"Filter: {('on ' + facts['filter']) if facts['filter'] else 'none'}"]
    if facts["conditional"]:
        shown = facts["conditional"][:MAX_FORMAT_LINES]
        lines.append("Conditional formatting: " + "; ".join(shown)
                     + (f" (+{len(facts['conditional']) - len(shown)} more)" if len(facts["conditional"]) > len(shown) else ""))
    else:
        lines.append("Conditional formatting: none")
    if facts["fills"]:
        parts = []
        for colour, rows in sorted(facts["fills"].items(), key=lambda kv: -len(kv[1]))[:6]:
            runs = _runs(rows)
            parts.append(f"{colour} fill on rows {runs if len(runs) <= 160 else runs[:160] + '...'}")
        lines.append("Fills: " + "; ".join(parts) + ("" if facts["checked_all"] else " (first cells only)"))
    else:
        lines.append("Fills: none" + ("" if facts["checked_all"] else " in the first cells"))
    if facts["number_formats"]:
        cols = list(facts["number_formats"].items())[:12]
        lines.append("Number formats: " + ", ".join(f"{c} {f}" for c, f in cols))
    return lines


def nothing_highlighted(path):
    """
    True when a workbook has no conditional-format rule on any sheet and no fill on any cell below a sheet's first row:
    a request for a fill or highlight on some rows was not applied at all. None when it cannot be told (not a workbook,
    too large to look at all of it, unreadable).
    """

    openpyxl = _openpyxl()
    try:
        wb = openpyxl.load_workbook(str(_path(path)))
    except Exception:  # noqa: BLE001 - a file that cannot be read decides nothing here
        return None
    for ws in wb.worksheets:
        facts = sheet_formatting(ws)
        if facts["conditional"] or any(row > 1 for rows in facts["fills"].values() for row in rows):
            return False
        if not facts["checked_all"]:
            return None
    return True


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

SHEET_KEYS = {"name", "rows", "cells", "header", "widths", "number_format", "charts"}
CHART_KINDS = {"bar", "column", "line", "pie"}


def _add_chart(ws, spec):
    """
    A chart on `ws` from {"type": "bar"|"column"|"line"|"pie", "title": "...", "data": "B1:B4" (with its header
    cell, which names the series), "categories": "A2:A4", "anchor": "E2"}. Found live (2026-10-13): asked for a
    chart, a worker had no tool that made one - office update edits cells - and reported the workbook with none.
    """

    from openpyxl.chart import BarChart, LineChart, PieChart, Reference
    from openpyxl.utils.cell import range_boundaries

    if not isinstance(spec, dict) or str(spec.get("type", "bar")).lower() not in CHART_KINDS or not spec.get("data"):
        raise OfficeError('a chart is {"type": "bar|column|line|pie", "title": "...", "data": "B1:B4", '
                          '"categories": "A2:A4", "anchor": "E2"}')
    kind = str(spec.get("type", "bar")).lower()
    chart = {"bar": BarChart, "column": BarChart, "line": LineChart, "pie": PieChart}[kind]()
    if kind in ("bar", "column"):
        chart.type = "col"
    chart.title = spec.get("title") or None

    def ref(text):
        try:
            min_col, min_row, max_col, max_row = range_boundaries(str(text).replace("$", "").split("!")[-1])
        except ValueError as exc:
            raise OfficeError(f"{text!r} is not a cell range like B1:B4") from exc
        if max_row > ws.max_row or max_col > ws.max_column:
            raise OfficeError(f"{text} reaches past the data on sheet {ws.title!r} ({ws.dimensions})")
        return Reference(ws, min_col=min_col, min_row=min_row, max_col=max_col, max_row=max_row)

    chart.add_data(ref(spec["data"]), titles_from_data=True)
    if spec.get("categories"):
        chart.set_categories(ref(spec["categories"]))
    ws.add_chart(chart, str(spec.get("anchor") or "E2"))


DOCUMENT_BLOCKS = {"heading", "paragraph", "bullets", "table", "page_break"}


def _table_layout(doc, texts):
    """
    A table's font size and column widths for the page: each column at least as wide as its longest word or figure,
    the rest shared by how much each holds, and the font made smaller until the figures fit. Found live
    (2026-10-14): an eight-column payslip split every figure over two lines ("1,672.7" and "4") - equal columns
    on a portrait page were narrower than the numbers.
    """

    from docx.shared import Emu

    section = doc.sections[-1]
    usable = section.page_width - section.left_margin - section.right_margin
    columns = list(zip(*texts)) if texts else []
    longest = [max([len(t) for t in col] + [1]) for col in columns]
    token = [max([len(w) for t in col for w in t.split()] + [1]) for col in columns]
    pad = 2 * 108000  # the cell's own margins, about 0.11 inch a side
    for size in (10, 9, 8, 7):
        char = size * 12700 * 0.62  # a character of a bold header at this size, in EMU
        need = [(token[j] + 1) * char + pad for j in range(len(columns))]
        if sum(need) <= usable or size == 7:
            break
    spare = max(0, usable - sum(need))
    extra = [max(0, longest[j] - token[j]) for j in range(len(columns))]
    total = sum(extra) or 1
    widths = [Emu(int(need[j] + spare * extra[j] / total)) for j in range(len(columns))]
    if not sum(extra):
        widths = [Emu(int(need[j] + spare / max(1, len(columns)))) for j in range(len(columns))]
    return size, widths


def create_document(path, blocks):
    """
    A new Word document (.docx), made with python-docx so it is always valid.
    `blocks` is a list, in order, of {"heading": "...", "level": 1},
    {"paragraph": "...", "bold": false}, {"bullets": ["...", ...]},
    {"table": [["Header", ...], ["cell", ...], ...], "header": true} and
    {"page_break": true}. Refuses to overwrite a file and refuses a block it
    does not read. Found live (2026-10-13): asked for a table of contract
    changes, a worker wrote the document's XML by hand, left out the table's
    required grid, and reported a file Word cannot read as "created and
    verified" - its own read-back had failed.
    """

    blocks = _clean_spec(blocks)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

    from docx import Document
    from docx.shared import Pt

    p = _path(path)
    if p.suffix.lower() != ".docx":
        raise OfficeError(f"{p.name}: office create makes .docx documents and .xlsx workbooks")
    if p.exists():
        raise OfficeError(f"{p} already exists - choose another name, or delete it first if that is what was asked")
    if not isinstance(blocks, list) or not blocks:
        raise OfficeError('give "blocks": a list of {"heading"}, {"paragraph"}, {"bullets"}, {"table"} or {"page_break"}')

    doc, tables, words = Document(), 0, 0
    for n, block in enumerate(blocks, 1):
        kind = [k for k in block if k in DOCUMENT_BLOCKS] if isinstance(block, dict) else []
        if len(kind) != 1:
            raise OfficeError(f"block {n}: give exactly one of {sorted(DOCUMENT_BLOCKS)}, got {block!r}"[:300])
        kind = kind[0]
        if kind == "heading":
            doc.add_heading(str(block["heading"]), level=int(block.get("level", 1)))
        elif kind == "paragraph":
            para = doc.add_paragraph()
            para.add_run(str(block["paragraph"])).bold = bool(block.get("bold"))
            words += len(str(block["paragraph"]).split())
        elif kind == "bullets":
            for item in block["bullets"]:
                doc.add_paragraph(str(item), style="List Bullet")
                words += len(str(item).split())
        elif kind == "table":
            rows = block["table"]
            if not rows or not all(isinstance(r, list) for r in rows):
                raise OfficeError(f"block {n}: a table is a list of rows, each a list of cells")
            width = max(len(r) for r in rows)
            table = doc.add_table(rows=0, cols=width)
            table.style = "Table Grid"
            texts = [["" if j >= len(row) or row[j] is None else str(row[j]) for j in range(width)] for row in rows]
            size, widths = _table_layout(doc, texts)
            table.autofit = False
            for i, row in enumerate(texts):
                cells = table.add_row().cells
                for j in range(width):
                    cells[j].text = row[j]
                    cells[j].width = widths[j]
                    for run in cells[j].paragraphs[0].runs:
                        run.font.size = Pt(size)
                        if i == 0 and block.get("header", True):
                            run.bold = True
            tables += 1
        elif kind == "page_break":
            doc.add_page_break()
    p.parent.mkdir(parents=True, exist_ok=True)
    doc.save(str(p))
    _read_docx(p)  # what is written must read back
    return f"Created {p} - {len(blocks)} blocks, {tables} table(s), {words} words of text."


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

    sheets = _clean_spec(sheets)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

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

        for chart in spec.get("charts") or []:
            _add_chart(ws, chart)

    p.parent.mkdir(parents=True, exist_ok=True)
    wb.save(str(p))
    stored = _store_results(p)
    # Say what is in it, so a reader of this result sees an empty or short
    # sheet for what it is instead of taking "Created" for the whole job.
    parts = []
    for ws in wb:
        filled = sum(1 for r in ws.iter_rows() for c in r if c.value not in (None, ""))
        parts.append(f"{ws.title}: {ws.max_row} rows x {ws.max_column} columns, {filled} cells filled"
                     + (f", {len(ws._charts)} chart(s)" if ws._charts else ""))
    return (f"Created {p} - " + "; ".join(parts) + "." + (" Formula results are calculated and saved in it." if stored else "")
            + " Run office read on it to check the values.")


# A float as a program prints its rounding error: a run of 0s or 9s, then a digit or two, at the end of its digits
# ("7640.220000000001", "40123.60000000001", "1.9999999999999998"). Found live (round 5 P2 on pc3, 2026-10-21, series
# 12): such figures went into a workbook through office, and the stage was refused twice for them.
_FLOAT_ARTIFACT = re.compile(r"(?<![\d.])(-?\d+\.\d*?)(0{6,}|9{6,})\d{1,2}(?![\d.])")


def clean_number(value):
    """A float written as its rounding error, put back to the figure it is (7640.220000000001 -> 7640.22); any other
    value as it is - a figure that really has many digits (0.333333333333) is never changed."""

    if not isinstance(value, float) or isinstance(value, bool):
        return value
    m = _FLOAT_ARTIFACT.fullmatch(repr(value))
    if not m:
        return value
    decimals = len(m.group(1).split(".")[1])
    cleaned = round(value, decimals)
    return cleaned if abs(cleaned - value) <= 1e-9 * max(1.0, abs(value)) else value


def clean_text(text):
    """A text with such figures in it ("Rs 40123.60000000001") written with the figure it is ("Rs 40123.60")."""

    if not isinstance(text, str) or not _FLOAT_ARTIFACT.search(text):
        return text

    def fix(m):
        try:
            value = float(m.group(0))
        except ValueError:
            return m.group(0)
        cleaned = clean_number(value)
        if cleaned is value:
            return m.group(0)
        decimals = max(len(m.group(1).split(".")[1]), 2 if m.group(2).startswith("0") and len(m.group(1).split(".")[1]) == 1
                       else 0)
        return f"{cleaned:.{decimals}f}"
    return _FLOAT_ARTIFACT.sub(fix, text)


def _clean_spec(spec):
    """A spec for a file (rows, cells, blocks, slides, edits) with every float and every text cleaned of a float's
    rounding error, at any depth; dict keys stay as they are (a key may be a placeholder or a cell address)."""

    if isinstance(spec, float):
        return clean_number(spec)
    if isinstance(spec, str):
        return clean_text(spec)
    if isinstance(spec, list):
        return [_clean_spec(x) for x in spec]
    if isinstance(spec, tuple):
        return tuple(_clean_spec(x) for x in spec)
    if isinstance(spec, dict):
        return {k: _clean_spec(v) for k, v in spec.items()}
    return spec


def _cell_value(v):
    if isinstance(v, float):
        return clean_number(v)
    if isinstance(v, str):
        s = v.strip()
        if re.fullmatch(r"-?\d+(\.\d+)?", s) and not (len(s) > 1 and s.startswith("0") and "." not in s):
            return clean_number(float(s)) if "." in s else int(s)
        return clean_text(v)
    return v


# ------------------------------------------------------------------
# Workbooks: updating in place (charts survive)
# ------------------------------------------------------------------

_MAIN_NS_REL = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"


def update_workbook(path, edits=None, replace_refs=None, output_path=None, add_sheets=None):
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

    # New sheets first, so the edits below can fill them too. Found live (2026-10-14): asked to add an Exceptions
    # and a By Account sheet to an existing model, a worker had no way to and made a second workbook instead.
    added = []
    for sheet in add_sheets or []:
        added.append(_add_sheet(entries, order, sheet))

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
        if not backup.exists() and not _task_made(src):
            shutil.copy2(src, backup)

    tmp = dst.with_name(dst.name + ".tmp")
    dst.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as z:
        for name in order:
            z.writestr(name, entries[name])
    os.replace(tmp, dst)
    stored = _store_results(dst)

    msg = f"Updated {dst}: " + (f"added sheet(s) {', '.join(added)}; " if added else "") + f"{len(changed)} cell(s)"
    if changed:
        msg += " (" + ", ".join(changed[:20]) + (" ..." if len(changed) > 20 else "") + ")"
    if replace_refs:
        msg += f"; {swaps} range reference(s) rewritten"
        if not swaps:
            msg += " - none matched: read the workbook to see the exact ranges its charts use"
    return msg + (". Formula results are recalculated and saved in it." if stored else ". Formulas recalculate when the file is opened.")


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


def _col_letter(n):
    letters = ""
    while n:
        n, rem = divmod(n - 1, 26)
        letters = chr(65 + rem) + letters
    return letters


def _split(ref):
    m = re.fullmatch(r"([A-Z]+)(\d+)", ref)
    return m.group(1), int(m.group(2))


_SHEET_REL = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"
_SHEET_TYPE = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"


def _add_sheet(entries, order, sheet):
    """A new sheet in the workbook's XML, filled from {"name", "rows": [[...]], "cells": {"A1": ...}}; returns its name."""

    if not isinstance(sheet, dict):
        raise OfficeError('each of add_sheets is {"name": ..., "rows": [[...]] and/or "cells": {"A1": ...}}')
    unknown = set(sheet) - {"name", "rows", "cells"}
    if unknown:
        raise OfficeError(f"add_sheets takes name, rows and cells - not {', '.join(sorted(unknown))}. Nothing was written.")
    name = str(sheet.get("name") or "").strip()
    if not name or len(name) > 31 or re.search(r"[\\/?*\[\]:]", name):
        raise OfficeError(f"{name!r} is not a sheet name (1-31 characters, none of \\ / ? * [ ] :)")
    if name.lower() in (n.lower() for n in _sheet_files(entries)):
        raise OfficeError(f"the workbook already has a sheet {name!r} - change its cells with edits instead")
    if not sheet.get("rows") and not sheet.get("cells"):
        raise OfficeError(f"the new sheet {name!r} has neither rows nor cells. Nothing was written.")

    xml = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n'
           '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData/></worksheet>')
    for r, row in enumerate(sheet.get("rows") or [], 1):
        for c, value in enumerate(row, 1):
            if value not in (None, ""):
                xml = _set_cell(xml, f"{_col_letter(c)}{r}", value)
    for ref, value in (sheet.get("cells") or {}).items():
        if isinstance(value, dict):
            value = value.get("formula") or value.get("value")
            if isinstance(value, str) and value and not value.startswith("=") and "formula" in sheet["cells"][ref]:
                value = "=" + value
        xml = _set_cell(xml, str(ref).upper().replace("$", ""), value)

    n = 1
    while f"xl/worksheets/sheet{n}.xml" in entries:
        n += 1
    part = f"xl/worksheets/sheet{n}.xml"
    entries[part] = _fix_dimension(xml).encode("utf-8")
    order.append(part)

    rels = entries["xl/_rels/workbook.xml.rels"].decode("utf-8")
    ids = {int(i) for i in re.findall(r'\bId="rId(\d+)"', rels)}
    rid = f"rId{max(ids | {0}) + 1}"
    rels = rels.replace("</Relationships>", f'<Relationship Id="{rid}" Type="{_SHEET_REL}" Target="worksheets/sheet{n}.xml"/></Relationships>')
    entries["xl/_rels/workbook.xml.rels"] = rels.encode("utf-8")

    wb = entries["xl/workbook.xml"].decode("utf-8")
    sheet_id = max([int(i) for i in re.findall(r'<sheet[^>]*?sheetId="(\d+)"', wb)] + [0]) + 1
    # Its own namespace declaration: some writers declare the relationships prefix on each sheet, not the root.
    tag = (f'<sheet xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" '
           f'name="{_escape(name)}" sheetId="{sheet_id}" r:id="{rid}"/>')
    wb = wb.replace("</sheets>", tag + "</sheets>", 1)
    entries["xl/workbook.xml"] = wb.encode("utf-8")

    ct = entries["[Content_Types].xml"].decode("utf-8")
    ct = ct.replace("</Types>", f'<Override PartName="/{part}" ContentType="{_SHEET_TYPE}"/></Types>', 1)
    entries["[Content_Types].xml"] = ct.encode("utf-8")
    return name


def _new_cell(ref, value, style):
    s = f' s="{style}"' if style else ""
    if value is None or value == "":
        return f'<c r="{ref}"{s}/>'
    if isinstance(value, bool):
        return f'<c r="{ref}"{s} t="b"><v>{int(value)}</v></c>'
    if isinstance(value, (int, float)):
        value = clean_number(value)
        return f'<c r="{ref}"{s}><v>{repr(float(value)) if isinstance(value, float) else value}</v></c>'
    text = clean_text(str(value))
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


def create_pdf(path, blocks):
    """
    A new PDF document from the same blocks as create_document (headings, paragraphs, bullets, tables): made as a
    Word document and printed through LibreOffice, so it has real text. Found live (2026-10-13): asked for quotations
    as PDFs, a worker printed HTML with a headless browser, which hung for three minutes, and then sent customers
    screenshots wrapped in PDFs.
    """

    blocks = _clean_spec(blocks)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

    p = _path(path)
    if p.suffix.lower() != ".pdf":
        raise OfficeError(f"{p.name}: create_pdf makes .pdf files")
    if p.exists():
        raise OfficeError(f"{p} already exists - pick another name")
    tmp = Path(tempfile.mkdtemp(prefix="decurix-pdf-"))
    try:
        doc = tmp / (p.stem + ".docx")
        create_document(str(doc), blocks)
        pdf = _office_to_pdf(doc, tmp / "out")
        p.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(pdf, p)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    from pypdf import PdfReader
    pages = len(PdfReader(str(p)).pages)
    return f"Created {p} ({pages} page(s), real text - office read shows what it says)."


# Workbooks whose formulas keep their results in the file as Open XML (an .ods is saved by LibreOffice with them).
RESULT_BOOKS = {".xlsx", ".xlsm"}


def formulas_without_results(path):
    """
    The formula cells of a workbook that hold no saved result, as "Sheet!A1" - read from the file's XML, with no
    model and no calculation. A formula a script writes (openpyxl) has none until a spreadsheet program saves the
    file; an empty-text result is stored as one (t="str" with an empty value) and is not missing. Empty for a file
    that is not such a workbook or cannot be read.
    """

    p = Path(path)
    if p.suffix.lower() not in RESULT_BOOKS:
        return []
    try:
        with zipfile.ZipFile(p) as z:
            entries = {name: z.read(name) for name in z.namelist()
                       if name in ("xl/workbook.xml", "xl/_rels/workbook.xml.rels") or name.startswith("xl/worksheets/")}
        sheets = _sheet_files(entries)
    except (OSError, zipfile.BadZipFile, KeyError, AttributeError, UnicodeDecodeError):
        return []
    out = []
    for sheet, name in sheets.items():
        if name not in entries:
            continue
        for m in _CELL_XML.finditer(entries[name].decode("utf-8", "replace")):
            attrs, inner = m.group(1), m.group(2) or ""
            if not re.search(r"<f\b", inner) or re.search(r'<f\b[^>]*\bt="dataTable"', inner):
                continue
            if re.search(r"<v>[^<]+</v>", inner):
                continue
            # An empty result that is text: what Excel and LibreOffice write for a formula that came to "".
            if re.search(r'\bt="str"', attrs) and re.search(r"<v\s*/>|<v>\s*</v>", inner):
                continue
            ref = re.search(r'\br="([A-Z]{1,3}\d+)"', attrs)
            out.append(f"{sheet}!{ref.group(1) if ref else '?'}")
    return out


# A row's label that says it totals what is above it: "Total", "Grand Total", "Sub-total", "Totals".
TOTAL_LABEL = re.compile(r"^\s*(?:grand\s+|sub[\s-]*)?totals?\b", re.I)


def typed_totals(path):
    """
    The cells of a row labelled Total that hold a typed number equal to the sum of the column above them, as
    "Sheet!E14 (2603800 = the sum of E2:E13)" - a total that will not follow its rows when one changes. Read with no
    model; needs the file's formula results (store them first). Empty for anything else.
    Found live (2026-10-19): a GST register asked for "with formulas" had every total of its Total row typed in.
    """

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return []
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
        values = openpyxl.load_workbook(str(path), data_only=True)
    except Exception:
        return []

    def number(v):
        return v if isinstance(v, (int, float)) and not isinstance(v, bool) else None

    out = []
    for ws_f in formulas.worksheets:
        ws_v = values[ws_f.title]
        for row in ws_f.iter_rows(min_row=2):
            if not any(isinstance(c.value, str) and TOTAL_LABEL.match(c.value) for c in row):
                continue
            r = row[0].row
            for c in row:
                if number(c.value) is None:
                    continue  # a label, an empty cell or a formula
                above, rows = [], []
                for rr in range(r - 1, 0, -1):
                    v = ws_v.cell(row=rr, column=c.column).value
                    if v is None or isinstance(v, str):
                        if above:
                            break
                        continue
                    if number(v) is None:
                        break
                    above.append(v)
                    rows.append(rr)
                if len(above) >= 2 and abs(sum(above) - c.value) < 0.005:
                    out.append(f"{ws_f.title}!{c.coordinate} ({c.value:g} = the sum of {c.column_letter}{min(rows)}:"
                               f"{c.column_letter}{max(rows)})")
    return out


# ============================================================
# Coverage: a column filled down a table stops short of the table
#
# Found live (2026-10-19, real Excel): a lookup column and a Line Total column were filled for rows 2 to 24 of a table
# that runs to row 25, and the Total row's =SUM(E2:E25) came to 515127 for a true 579607. The worker's own sum check
# agreed with itself - 515127 IS the sum of what was there - and the report said "verified". Nothing looked at the cells
# that were not there. This reads the saved file, with no model.
# ============================================================

# A cell reference not part of a longer word or a function name (LOG10( is a function).
_CELL_REF = re.compile(r"(?<![A-Za-z0-9_.])(\$?)([A-Z]{1,3})(\$?)(\d+)(?![A-Za-z0-9_(])")
_SUM_RANGE = re.compile(r"^=\s*SUM\(\s*\$?([A-Z]{1,3})\$?(\d+)\s*:\s*\$?([A-Z]{1,3})\$?(\d+)\s*\)\s*$", re.I)
# Sheets bigger than this are left alone: a data dump, not a table somebody filled down.
_COVERAGE_MAX_ROWS, _COVERAGE_MAX_COLUMNS = 5000, 120


def formula_shape(formula, row, column):
    """
    A formula with its references made relative to its own cell, so two cells of one fill-down column - =C2*D2 in row 2,
    =C3*D3 in row 3 - have the same shape ($ marks stay as they are written: they do not move down a column).
    """

    def relative(m):
        column_abs, letters, row_abs, digits = m.groups()
        c = ("C" + letters) if column_abs else f"C[{_col_num(letters.upper()) - column}]"
        r = ("R" + digits) if row_abs else f"R[{int(digits) - row}]"
        return r + c

    return _CELL_REF.sub(relative, formula)


def _is_formula(value):
    return isinstance(value, str) and value.startswith("=") and len(value) > 1


def _filled(value):
    return value is not None and not (isinstance(value, str) and not value.strip())


# A formula result that is an error a spreadsheet shows as one (not #N/A, which a lookup may return on purpose).
FORMULA_ERRORS = ("#REF!", "#VALUE!", "#DIV/0!", "#NAME?", "#NUM!", "#NULL!")


def formula_errors(path):
    """
    The formula cells of a workbook whose stored result is an error (#REF!, #VALUE!, #DIV/0!, #NAME?, #NUM!, #NULL!),
    as one sentence per sheet; [] for anything else. Certain: the file shows the error to anyone who opens it. (#N/A
    is left out: a lookup may say "not found" on purpose.) Needs the formula results stored.
    """

    import io
    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return []
    openpyxl = _openpyxl()
    try:
        data = Path(path).read_bytes()
        formulas = openpyxl.load_workbook(io.BytesIO(data))
        values = openpyxl.load_workbook(io.BytesIO(data), data_only=True)
    except Exception:
        return []
    out = []
    for ws in formulas.worksheets:
        shown = values[ws.title]
        bad = [c.coordinate for row in ws.iter_rows() for c in row if _is_formula(c.value)
               and str(shown[c.coordinate].value or "").strip().upper() in FORMULA_ERRORS]
        if bad:
            out.append(f"{ws.title}!{bad[0]} shows {shown[bad[0]].value}" + (f" ({len(bad)} formula cells show an error)"
                                                                            if len(bad) > 1 else ""))
    return out


def coverage_gaps(path):
    """
    The cells a column filled down a table is missing, as sentences naming the first row (empty for anything else):
    - a column of at least three formulas of one shape (a fill-down) whose neighbouring data columns carry values on
      rows where it has nothing at all - at its end or inside it. A row where another formula of the row came to text
      or an error (a lookup that says NOT FOUND, whose Line Total is meant to stay empty) is explained, and a header
      row (text in every data column) is not a gap;
    - a Total row's =SUM(E2:E24) that stops short of the last data row above it.
    Needs the file's formula results (store them first): they say which rows are explained.
    """

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return []
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
        values = openpyxl.load_workbook(str(path), data_only=True)
    except Exception:
        return []
    out = []
    for ws in formulas.worksheets:
        if ws.max_row > _COVERAGE_MAX_ROWS or ws.max_column > _COVERAGE_MAX_COLUMNS:
            continue
        shown = values[ws.title]
        grid = {(c.row, c.column): c.value for row in ws.iter_rows() for c in row if _filled(c.value)}
        total_rows = {r for (r, k), v in grid.items() if isinstance(v, str) and TOTAL_LABEL.match(v)}
        out.extend(_column_gaps(ws.title, grid, shown, total_rows, ws.max_row, ws.max_column))
        out.extend(_total_gaps(ws.title, grid, total_rows))
    return out


def _column_gaps(title, grid, shown, total_rows, max_row, max_col):
    found = []
    for c in range(1, max_col + 1):
        by_shape = {}
        for r in range(1, max_row + 1):
            v = grid.get((r, c))
            if _is_formula(v) and r not in total_rows:
                by_shape.setdefault(formula_shape(v, r, c), []).append(r)
        if not by_shape:
            continue
        group = max(by_shape.values(), key=len)
        if len(group) < 3:
            continue
        lo, hi = min(group), max(group)
        # The data columns beside it: filled by typed values (not formulas) on at least half of the rows it spans.
        data = [k for k in range(1, max_col + 1) if k != c
                and sum(1 for r in range(lo, hi + 1) if _filled(grid.get((r, k))) and not _is_formula(grid.get((r, k))))
                >= max(2, (hi - lo + 1) // 2)]
        if not data:
            continue

        def has_data(r):
            return any(_filled(grid.get((r, k))) and not _is_formula(grid.get((r, k))) for k in data)

        start, end = lo, hi
        while start > 1 and has_data(start - 1) and (start - 1) not in total_rows:
            start -= 1
        while end < max_row and has_data(end + 1) and (end + 1) not in total_rows:
            end += 1

        def explained(r):
            for k in range(1, max_col + 1):
                if k != c and _is_formula(grid.get((r, k))):
                    result = shown.cell(row=r, column=k).value
                    if isinstance(result, str) and result.strip():
                        return True  # a text result or an error: NOT FOUND, #N/A
            return False

        def header(r):
            return r < lo and all(isinstance(grid.get((r, k)), str) for k in data if _filled(grid.get((r, k))))

        missing = [r for r in range(start, end + 1)
                   if not _filled(grid.get((r, c))) and not explained(r) and not header(r)]
        if missing:
            letter = _col_letter(c)
            more = f" ({len(missing)} rows are missing)" if len(missing) > 1 else ""
            found.append(f"{title}!{letter}{missing[0]} is empty, but {letter}{lo}:{letter}{hi} hold one formula filled "
                         f"down and the table beside it runs to row {end}{more}")
    return found


def _total_gaps(title, grid, total_rows):
    found = []
    for t in sorted(total_rows):
        above = [r for (r, k) in grid if r < t]
        if not above:
            continue
        last = max(above)
        for (r, c), v in sorted(grid.items()):
            m = _SUM_RANGE.match(v) if r == t and isinstance(v, str) else None
            if not m or m.group(1).upper() != m.group(3).upper() or _col_num(m.group(1).upper()) != c:
                continue
            end = int(m.group(4))
            if end < last:
                found.append(f"{title}!{_col_letter(c)}{t} sums {m.group(1).upper()}{m.group(2)}:{m.group(3).upper()}{end}, "
                             f"but the table above it runs to row {last}")
    return found


def constants_among_formulas(path):
    """
    The typed numbers sitting in a column of formulas, as sentences naming the first row (empty for anything else): a
    column with a formula that works from its own row (=E2*9%, a fill-down) beside cells of the same table that hold a
    typed number. Found live (2026-10-19, calc_gst): asked for columns "with formulas", the worker made them in rows 2 and
    3 and typed the right-looking figures into rows 4 to 13 - and its report said so, and passed. A figure typed where a
    formula belongs is right today and stale the day a row changes. Only meaningful when formulas were asked for.
    """

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return []
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
    except Exception:
        return []
    out = []
    for ws in formulas.worksheets:
        if ws.max_row > _COVERAGE_MAX_ROWS or ws.max_column > _COVERAGE_MAX_COLUMNS:
            continue
        grid = {(c.row, c.column): c.value for row in ws.iter_rows() for c in row if _filled(c.value)}
        total_rows = {r for (r, k), v in grid.items() if isinstance(v, str) and TOTAL_LABEL.match(v)}
        for c in range(1, ws.max_column + 1):
            fill = [r for r in range(1, ws.max_row + 1)
                    if r not in total_rows and _is_formula(grid.get((r, c)))
                    and "R[0]" in formula_shape(grid[(r, c)], r, c)]
            if not fill:
                continue

            def beside(r):
                return any(_filled(grid.get((r, k))) for k in range(1, ws.max_column + 1) if k != c)

            lo, hi = min(fill), max(fill)
            start, end = lo, hi
            while start > 1 and beside(start - 1) and (start - 1) not in total_rows:
                start -= 1
            while end < ws.max_row and beside(end + 1) and (end + 1) not in total_rows:
                end += 1
            typed = [r for r in range(start, end + 1)
                     if isinstance(grid.get((r, c)), (int, float)) and not isinstance(grid.get((r, c)), bool)]
            if typed:
                letter = _col_letter(c)
                out.append(f"{ws.title}!{letter}{typed[0]} is the typed number {grid[(typed[0], c)]!r}, but "
                           f"{letter}{lo} is a formula: {len(typed)} cell(s) of column {letter} are typed numbers where "
                           "the formula belongs")
    return out


def split_totals(path):
    """
    A Total whose label stands on a row of its own with the sums on the row below, as sentences (empty for anything
    else): the row labelled Total holds no number and no formula, and the next row holds SUMs. Found live (2026-10-19):
    "Total" was typed at F14 and the four SUMs at F15, so no row was the Total row.
    """

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return []
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
    except Exception:
        return []
    out = []
    for ws in formulas.worksheets:
        if ws.max_row > _COVERAGE_MAX_ROWS or ws.max_column > _COVERAGE_MAX_COLUMNS:
            continue
        grid = {(c.row, c.column): c.value for row in ws.iter_rows() for c in row if _filled(c.value)}
        for (r, c), v in sorted(grid.items()):
            if not (isinstance(v, str) and TOTAL_LABEL.match(v)):
                continue
            if any(k != c and (_is_formula(grid.get((r, k))) or isinstance(grid.get((r, k)), (int, float)))
                   for k in range(1, ws.max_column + 1)):
                continue
            sums = [k for k in range(1, ws.max_column + 1)
                    if _is_formula(grid.get((r + 1, k))) and grid[(r + 1, k)].upper().startswith("=SUM(")]
            if sums:
                out.append(f"{ws.title}!{_col_letter(c)}{r} says {v.strip()!r} but its row holds no figure; the sums are "
                           f"on row {r + 1} ({', '.join(_col_letter(k) + str(r + 1) for k in sums)}) - the label and "
                           "the sums belong on one row")
    return out


def workbook_tail(path, rows=4):
    """
    The last rows of every sheet of a workbook with each cell's formula and result, and the empty cells of those rows
    named - the end of a sheet is where a fill that stopped short and a Total row show, and the text a long sheet is
    read from is cut before it. "" for anything but a workbook that can be read.
    """

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return ""
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
        values = openpyxl.load_workbook(str(path), data_only=True)
    except Exception:
        return ""
    out = []
    for ws in formulas.worksheets:
        if ws.max_row < 2 or ws.max_column > _COVERAGE_MAX_COLUMNS:
            continue
        lines = []
        for r in range(max(2, ws.max_row - rows + 1), ws.max_row + 1):
            cells = []
            for c in range(1, ws.max_column + 1):
                v = ws.cell(row=r, column=c).value
                if not _filled(v):
                    cells.append(f"{_col_letter(c)}{r}: (empty)")
                elif _is_formula(v):
                    cells.append(f"{_col_letter(c)}{r}: {v} -> {values[ws.title].cell(row=r, column=c).value!r}")
                else:
                    cells.append(f"{_col_letter(c)}{r}: {v!r}")
            lines.append("  row " + str(r) + " | " + "; ".join(cells))
        out.append(f"[end of sheet {ws.title!r}, its last {len(lines)} row(s)]\n" + "\n".join(lines))
    return "\n".join(out)[:2500]


def _ref_sample(refs, shown=6):
    """A few of a list of cell references, and how many more there are."""

    return ", ".join(refs[:shown]) + (f" and {len(refs) - shown} more" if len(refs) > shown else "")


def calculate_workbook(path):
    """
    `office calculate BOOK.xlsx`: work out every formula with LibreOffice and store the results in the file, in
    place - formulas, charts and styles stay as they are. For a workbook a script wrote, whose formulas hold none.
    """

    p = _path(path)
    if not p.exists():
        raise OfficeError(f"{p} does not exist")
    if p.suffix.lower() not in RESULT_BOOKS:
        raise OfficeError(f"{p.name}: only an .xlsx or .xlsm workbook stores formula results this way")
    missing = formulas_without_results(p)
    stored = _store_results(p)
    left = formulas_without_results(p)
    if left:
        raise OfficeError(f"{p.name}: {len(left)} formula cell(s) still hold no saved result ({_ref_sample(left)}) - "
                          "LibreOffice could not calculate the file here")
    if not stored:
        return f"{p.name} has no formulas to calculate."
    return (f"Calculated {p.name} and stored every formula's result in it"
            + (f" ({len(missing)} had none)" if missing else "") + "; the formulas are unchanged.")


# LibreOffice keeps a workbook's saved results unless told to work them out again on load (Tools > Options >
# Formula > Recalculation on file load); "always" for Excel and ODF files, set in the conversion's own profile.
_RECALC_PROFILE = """<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<item oor:path="/org.openoffice.Office.Calc/Formula/Load"><prop oor:name="OOXMLRecalcMode" oor:op="fuse"><value>0</value></prop></item>
<item oor:path="/org.openoffice.Office.Calc/Formula/Load"><prop oor:name="ODFRecalcMode" oor:op="fuse"><value>0</value></prop></item>
</oor:items>
"""


def _recalculated_values(p, openpyxl):
    """The workbook's values as LibreOffice calculates them, from a converted copy; None when that cannot be done."""

    soffice = shutil.which("soffice") or shutil.which("libreoffice")
    if not soffice:
        return None
    tmp = Path(tempfile.mkdtemp(prefix="office-recalc-"))
    try:
        user = tmp / "profile" / "user"
        user.mkdir(parents=True)
        (user / "registrymodifications.xcu").write_text(_RECALC_PROFILE, encoding="utf-8")
        src = tmp / ("source" + p.suffix.lower())
        shutil.copyfile(p, src)
        out = tmp / "out"
        out.mkdir()
        subprocess.run([soffice, f"-env:UserInstallation=file://{tmp / 'profile'}", "--headless", "--convert-to",
                        "xlsx:Calc MS Excel 2007 XML", "--outdir", str(out), str(src)],
                       capture_output=True, text=True, timeout=180)
        made = out / "source.xlsx"
        if not made.exists():
            return None
        return openpyxl.load_workbook(str(made), data_only=True)
    except (OSError, subprocess.SubprocessError, ValueError, KeyError) as exc:
        print(f"office: could not calculate {p.name}'s formulas: {exc}", file=sys.stderr)
        return None
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


_CELL_XML = re.compile(r"<c\b([^>]*?)(?:/>|>(.*?)</c>)", re.S)


def _result_xml(attrs, inner, value, openpyxl):
    """One formula cell's XML with `value`, as calculated, as its saved result (None: it came to empty text)."""

    import datetime
    from xml.sax.saxutils import escape

    inner = re.sub(r"<v>.*?</v>|<v\s*/>", "", inner, flags=re.S)
    attrs = re.sub(r'\s+t="[^"]*"', "", attrs)
    if value is None:
        # The calculation came to empty text (=IF(D2="NOT FOUND","",...)): stored the way Excel and LibreOffice
        # store it, so the file says the formula has a result - an empty one - rather than none.
        return f'<c{attrs} t="str">{inner}<v></v></c>'
    if isinstance(value, bool):
        attrs, text = attrs + ' t="b"', "1" if value else "0"
    elif isinstance(value, (int, float)):
        text = repr(value)
    elif isinstance(value, (datetime.datetime, datetime.date, datetime.time)):
        from openpyxl.utils.datetime import to_excel
        text = repr(to_excel(value))
    else:
        value = str(value)
        attrs += ' t="e"' if re.fullmatch(r"#[A-Z0-9/!?]+", value) else ' t="str"'
        text = escape(value)
    return f"<c{attrs}>{inner}<v>{text}</v></c>"


def _store_results(p):
    """
    Save every formula's result in the workbook, calculated by LibreOffice - in place, so charts, styles and
    everything else stay as written. A formula a tool writes has no saved result, and one that depends on a cell
    `update` changed keeps the old one: Excel and LibreOffice work them out on open, but a preview, a phone, a
    script reading the file (and the final check) see blanks or stale totals. Found live (2026-10-14). Returns
    whether it did; nothing changes when LibreOffice is not there or cannot calculate the file.
    """

    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(p))
    except Exception:
        return False
    if not any(isinstance(c.value, str) and c.value.startswith("=") for ws in formulas.worksheets
               for row in ws.iter_rows() for c in row):
        return False
    values = _recalculated_values(p, openpyxl)
    if values is None:
        return False

    with zipfile.ZipFile(p) as z:
        entries = {i.filename: z.read(i.filename) for i in z.infolist()}
        order = [i.filename for i in z.infolist()]
    for sheet, name in _sheet_files(entries).items():
        if sheet not in values.sheetnames or name not in entries:
            continue
        ws = values[sheet]

        def fill(m):
            attrs, inner = m.group(1), m.group(2)
            ref = re.search(r'\br="([A-Z]{1,3}\d+)"', attrs)
            if inner is None or "<f" not in inner or not ref:
                return m.group(0)
            return _result_xml(attrs, inner, ws[ref.group(1)].value, openpyxl)

        entries[name] = _CELL_XML.sub(fill, entries[name].decode("utf-8")).encode("utf-8")

    tmp = p.with_name(p.name + ".tmp")
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as z:
        for name in order:
            z.writestr(name, entries[name])
    os.replace(tmp, p)
    return True


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
# Presentations: creating, and updating in place
# ------------------------------------------------------------------

SLIDE_KEYS = {"title", "subtitle", "text", "bullets", "table", "header", "chart", "image", "notes"}
SLIDE_VISUALS = ("table", "chart", "image")
DECK_CHART_KEYS = {"type", "title", "categories", "series"}
DECK_CHART_TYPES = {"column": "COLUMN_CLUSTERED", "bar": "BAR_CLUSTERED", "line": "LINE_MARKERS", "pie": "PIE"}
DECK_IMAGE_TYPES = {".png", ".jpg", ".jpeg", ".gif", ".bmp", ".tif", ".tiff"}
DECK_EDIT_KEYS = {"slide", "find", "replace", "title", "table", "row", "column", "value", "chart", "categories",
                  "series", "notes", "add_rows", "before"}
SLIDE_SIZE_INCHES = (13.333, 7.5)  # 16:9


def _number(value, where):
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise OfficeError(f"{where}: {value!r} is not a number")
    try:
        return float(str(value).replace(",", "")) if isinstance(value, str) else value
    except ValueError as exc:
        raise OfficeError(f"{where}: {value!r} is not a number") from exc


def _deck_chart_data(spec, where):
    """CategoryChartData from {"categories": [...], "series": [{"name": ..., "values": [...]}]}, checked first."""

    from pptx.chart.data import CategoryChartData

    categories, series = spec.get("categories"), spec.get("series")
    if not isinstance(categories, list) or not categories:
        raise OfficeError(f"{where}: give \"categories\": the labels along the axis, e.g. [\"Jan\", \"Feb\"]")
    if not isinstance(series, list) or not series:
        raise OfficeError(f"{where}: give \"series\": [{{\"name\": \"Revenue\", \"values\": [10, 12]}}]")
    data = CategoryChartData()
    data.categories = [str(c) for c in categories]
    for n, s in enumerate(series, 1):
        if not isinstance(s, dict) or set(s) - {"name", "values"} or not isinstance(s.get("values"), list):
            raise OfficeError(f"{where}: series {n} is {{\"name\": \"...\", \"values\": [...]}}")
        if len(s["values"]) != len(categories):
            raise OfficeError(f"{where}: series {n} has {len(s['values'])} values for {len(categories)} categories")
        data.add_series(str(s.get("name") or f"Series {n}"),
                        [None if v is None else _number(v, f"{where} series {n}") for v in s["values"]])
    return data


def _check_slide(spec, n):
    """Refuse a slide this does not know how to make, before anything is written."""

    if not isinstance(spec, dict):
        raise OfficeError(f"slide {n}: a slide is an object like {{\"title\": \"...\", \"bullets\": [\"...\"]}}")
    unknown = sorted(set(spec) - SLIDE_KEYS)
    if unknown:
        raise OfficeError(f"slide {n} does not take {', '.join(map(repr, unknown))}; a slide takes "
                          f"{', '.join(sorted(SLIDE_KEYS))}")
    visuals = [k for k in SLIDE_VISUALS if k in spec]
    if len(visuals) > 1:
        raise OfficeError(f"slide {n}: one of table, chart or image per slide (it has {', '.join(visuals)}) - "
                          "put the other on a slide of its own")
    if "subtitle" in spec and (visuals or "bullets" in spec or "text" in spec):
        raise OfficeError(f"slide {n}: a subtitle goes on a title slide, which has no text, bullets, table, chart or image")
    if not any(k in spec for k in ("title", "subtitle", "text", "bullets") + SLIDE_VISUALS):
        raise OfficeError(f"slide {n} is empty: give it a title and its content")
    if "bullets" in spec:
        if not isinstance(spec["bullets"], list) or not spec["bullets"]:
            raise OfficeError(f"slide {n}: \"bullets\" is a list of lines (or {{\"text\": ..., \"level\": 1}})")
        for item in spec["bullets"]:
            if isinstance(item, dict) and (set(item) - {"text", "level"} or "text" not in item):
                raise OfficeError(f"slide {n}: a bullet is a line or {{\"text\": \"...\", \"level\": 0-4}}")
    if "table" in spec:
        rows = spec["table"]
        if not isinstance(rows, list) or not rows or not all(isinstance(r, list) and r for r in rows):
            raise OfficeError(f"slide {n}: a table is a list of rows, each a list of cells")
    if "chart" in spec:
        chart = spec["chart"]
        if not isinstance(chart, dict) or set(chart) - DECK_CHART_KEYS:
            raise OfficeError(f"slide {n}: a chart is {{\"type\": \"column|bar|line|pie\", \"title\": \"...\", "
                              "\"categories\": [...], \"series\": [{\"name\": \"...\", \"values\": [...]}]}")
        if str(chart.get("type", "column")).lower() not in DECK_CHART_TYPES:
            raise OfficeError(f"slide {n}: a chart's type is one of {', '.join(DECK_CHART_TYPES)}")
        _deck_chart_data(chart, f"slide {n} chart")
    if "image" in spec:
        image = Path(os.path.expanduser(str(spec["image"])))
        if not image.is_file():
            raise OfficeError(f"slide {n}: the picture {spec['image']} does not exist")
        if image.suffix.lower() not in DECK_IMAGE_TYPES:
            raise OfficeError(f"slide {n}: {image.name} is not a picture a deck can hold ({', '.join(sorted(DECK_IMAGE_TYPES))})")


def _widen_layouts(prs, factor):
    """Stretch the template's placeholders across a wider slide (its layouts are drawn for 4:3)."""

    for owner in [prs.slide_master] + list(prs.slide_layouts):
        for shape in owner.shapes:
            if shape.left is not None and shape.width is not None:
                shape.left, shape.width = int(shape.left * factor), int(shape.width * factor)


def _fill_lines(frame, spec, size=None):
    """A slide's "text" (paragraphs) then its "bullets" into one text frame."""

    from pptx.util import Pt

    lines = [(line, 0, False) for line in str(spec.get("text") or "").split("\n") if line.strip()]
    for item in spec.get("bullets") or []:
        text, level = (item.get("text"), item.get("level", 0)) if isinstance(item, dict) else (item, 0)
        lines.append((str(text), max(0, min(4, int(level or 0))), True))
    frame.word_wrap = True
    first = True
    for text, level, bullet in lines:
        para = frame.paragraphs[0] if first else frame.add_paragraph()
        first = False
        # A text box has no bullet marks of its own; a content placeholder draws them itself.
        para.text = ("\u2022 " + text) if bullet and size else text
        para.level = level
        if size:
            para.font.size = Pt(size - 2 * level)


def create_presentation(path, slides):
    """
    A new PowerPoint deck (.pptx), 16:9, made with python-pptx. `slides` is a
    list, in order, of objects with any of: "title", "subtitle" (a title slide),
    "text" (paragraphs), "bullets" (lines, or {"text", "level"}), one of
    "table" ([[header...], [cell...]], "header": true), "chart" ({"type":
    column|bar|line|pie, "title", "categories": [...], "series": [{"name",
    "values"}]}) or "image" (a picture file), and "notes" (speaker notes).
    Refuses to overwrite a file, a key it does not read, and a chart whose
    series do not match its categories - all before anything is written - and
    reads the deck back before saying it is made. Found live (2026-10-14):
    asked for a review deck, a worker had no tool that made one.
    """

    slides = _clean_spec(slides)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

    from pptx import Presentation
    from pptx.chart.data import CategoryChartData  # noqa: F401 - fail here, not half way, without python-pptx
    from pptx.enum.chart import XL_CHART_TYPE
    from pptx.util import Inches

    p = _path(path)
    if p.suffix.lower() != ".pptx":
        raise OfficeError(f"{p.name}: create_presentation makes .pptx decks")
    if p.exists():
        raise OfficeError(f"{p} already exists - choose another name, or use office update to change it")
    if not isinstance(slides, list) or not slides:
        raise OfficeError('give "slides": a list like [{"title": "Q3 review", "subtitle": "Finance"}, '
                          '{"title": "Highlights", "bullets": ["..."]}]')
    for n, spec in enumerate(slides, 1):
        _check_slide(spec, n)

    prs = Presentation()
    width, height = (Inches(v) for v in SLIDE_SIZE_INCHES)
    _widen_layouts(prs, width / prs.slide_width)
    prs.slide_width, prs.slide_height = width, height
    margin, top = Inches(0.5), Inches(1.5)
    counts = {"table": 0, "chart": 0, "image": 0, "notes": 0}

    for spec in slides:
        visual = next((k for k in SLIDE_VISUALS if k in spec), None)
        body = "text" in spec or "bullets" in spec
        if "subtitle" in spec or not (visual or body):
            slide = prs.slides.add_slide(prs.slide_layouts[0])          # Title Slide
            if "subtitle" in spec:
                slide.placeholders[1].text = str(spec["subtitle"])
            else:
                sub = slide.placeholders[1]
                sub._element.getparent().remove(sub._element)
        elif not visual:
            slide = prs.slides.add_slide(prs.slide_layouts[1])          # Title and Content
            _fill_lines(slide.placeholders[1].text_frame, spec)
        else:
            slide = prs.slides.add_slide(prs.slide_layouts[5])          # Title Only
        if slide.shapes.title is not None:
            if spec.get("title"):
                slide.shapes.title.text = str(spec["title"])
            else:
                slide.shapes.title._element.getparent().remove(slide.shapes.title._element)

        if visual:
            left, box_w, box_h = margin, width - 2 * margin, height - top - margin
            if body:  # words on the left, the table, chart or picture on the right
                words = slide.shapes.add_textbox(margin, top, int(box_w * 0.38), box_h)
                _fill_lines(words.text_frame, spec, size=18)
                left, box_w = margin + int(box_w * 0.42), int(box_w * 0.58)
            if visual == "table":
                rows = spec["table"]
                cols = max(len(r) for r in rows)
                table = slide.shapes.add_table(len(rows), cols, left, top, box_w,
                                               min(box_h, Inches(0.4) * len(rows))).table
                table.first_row = bool(spec.get("header", True))
                for i, row in enumerate(rows):
                    for j in range(cols):
                        value = row[j] if j < len(row) else None
                        table.cell(i, j).text = "" if value is None else str(value)
            elif visual == "chart":
                chart_spec = spec["chart"]
                kind = getattr(XL_CHART_TYPE, DECK_CHART_TYPES[str(chart_spec.get("type", "column")).lower()])
                chart = slide.shapes.add_chart(kind, left, top, box_w, box_h,
                                               _deck_chart_data(chart_spec, "chart")).chart
                chart.has_legend = len(chart_spec["series"]) > 1 or kind == XL_CHART_TYPE.PIE
                if chart_spec.get("title"):
                    chart.has_title = True
                    chart.chart_title.text_frame.text = str(chart_spec["title"])
            else:
                picture = slide.shapes.add_picture(os.path.expanduser(str(spec["image"])), left, top)
                scale = min(box_w / picture.width, box_h / picture.height)
                picture.width, picture.height = int(picture.width * scale), int(picture.height * scale)
                picture.left = int(left + (box_w - picture.width) / 2)
                picture.top = int(top + (box_h - picture.height) / 2)
            counts[visual] += 1
        if spec.get("notes"):
            slide.notes_slide.notes_text_frame.text = str(spec["notes"])
            counts["notes"] += 1

    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_name(p.name + ".tmp")
    prs.save(str(tmp))
    os.replace(tmp, p)
    # What is written must read back, every slide with its title.
    text = _read_pptx(p)
    missing = [str(s["title"]) for s in slides if s.get("title") and str(s["title"]) not in text]
    if text.count("[Slide ") != len(slides) or missing:
        raise OfficeError(f"{p.name} was written but does not read back as made (missing: {', '.join(missing) or 'slides'})")
    made = ", ".join(f"{v} {k if k == 'notes' else k + '(s)'}" for k, v in counts.items() if v)
    return f"Created {p} - {len(slides)} slides" + (f", {made}" if made else "") + ". Run office read on it to check what it says."


def _set_text(frame, value):
    """Put `value` in a text frame or table cell in the formatting of its first run; other paragraphs go."""

    paragraphs = frame.text_frame.paragraphs if hasattr(frame, "text_frame") else frame.paragraphs
    first = paragraphs[0]
    for extra in paragraphs[1:]:
        extra._p.getparent().remove(extra._p)
    runs = first.runs
    if not runs:
        first.add_run().text = str(value)
        return
    runs[0].text = str(value)
    for run in runs[1:]:
        run._r.getparent().remove(run._r)


def _frames(shapes):
    """Every text frame on a slide, with those in groups and table cells."""

    from pptx.shapes.group import GroupShape

    for shape in shapes:
        if isinstance(shape, GroupShape):
            yield from _frames(shape.shapes)
            continue
        if getattr(shape, "has_text_frame", False) and shape.has_text_frame:
            yield shape.text_frame
        if getattr(shape, "has_table", False) and shape.has_table:
            for row in shape.table.rows:
                for cell in row.cells:
                    yield cell.text_frame


def _replace_in_frame(frame, find, replace):
    """Replace `find` in a frame's paragraphs, run by run where it lies inside one run (formatting kept)."""

    count = 0
    for para in frame.paragraphs:
        whole = "".join(r.text for r in para.runs)
        hits = whole.count(find)
        if not hits:
            continue
        inside = sum(r.text.count(find) for r in para.runs)
        if inside == hits:
            for run in para.runs:
                if find in run.text:
                    run.text = run.text.replace(find, replace)
        else:  # it runs across runs: the paragraph takes its first run's formatting
            para.runs[0].text = whole.replace(find, replace)
            for run in para.runs[1:]:
                run._r.getparent().remove(run._r)
        count += hits
    return count


def update_presentation(path, edits, output_path=None):
    """
    Change an existing deck in place, keeping its layout, theme, pictures and
    everything not named. Each edit names a "slide" (from 1) and one change:
    {"find": "Q2", "replace": "Q3"} (every slide when no slide is named),
    {"title": "..."}, {"table": 1, "row": 2, "column": 3, "value": "..."}
    (table and its row/column from 1), {"chart": 1, "categories": [...],
    "series": [{"name", "values"}]} (the chart's data; its look stays), or
    {"notes": "..."}. Every edit is checked before anything is written: a text
    that is not there, a slide, table or chart that does not exist, or a key
    this does not read changes nothing. Writes `output_path` (default: the
    file itself, after keeping the original as <name>.orig.pptx the first time).
    """

    edits = _clean_spec(edits)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

    from pptx import Presentation

    src = _path(path)
    if src.suffix.lower() != ".pptx" or not src.exists():
        raise OfficeError(f"{src} is not an existing .pptx deck")
    dst = _path(output_path) if output_path else src
    if not isinstance(edits, list) or not edits:
        raise OfficeError('give "edits": [{"slide": 2, "find": "old", "replace": "new"}, {"slide": 3, "table": 1, '
                          '"row": 2, "column": 2, "value": "1,250"}, {"slide": 4, "chart": 1, "categories": [...], '
                          '"series": [...]}, {"slide": 1, "title": "..."}, {"slide": 5, "notes": "..."}]')
    try:
        prs = Presentation(str(src))
    except Exception as exc:
        raise OfficeError(f"{src.name}: cannot open the deck ({exc})") from exc
    slides = list(prs.slides)
    done = []

    for n, edit in enumerate(edits, 1):
        if not isinstance(edit, dict) or set(edit) - DECK_EDIT_KEYS:
            raise OfficeError(f"edit {n}: {edit!r} - an edit takes {', '.join(sorted(DECK_EDIT_KEYS))}"[:400])
        actions = [k for k in ("find", "title", "value", "categories", "notes", "add_rows") if k in edit]
        if len(actions) != 1:
            raise OfficeError(f"edit {n}: give exactly one change - find/replace, title, a table cell's value, rows "
                              "added to a table (add_rows), a chart's categories and series, or notes")
        action = actions[0]
        number = edit.get("slide")
        if number is None and action != "find":
            raise OfficeError(f"edit {n}: say which \"slide\" (from 1)")
        if number is not None and (isinstance(number, bool) or not isinstance(number, int) or not 1 <= number <= len(slides)):
            raise OfficeError(f"edit {n}: there is no slide {number!r}; the deck has {len(slides)}")
        slide = slides[number - 1] if number else None

        if action == "find":
            if "replace" not in edit or not str(edit["find"]):
                raise OfficeError(f"edit {n}: find needs a text and \"replace\" its new text")
            hits = sum(_replace_in_frame(frame, str(edit["find"]), str(edit["replace"]))
                       for s in ([slide] if slide else slides) for frame in _frames(s.shapes))
            if not hits:
                where = f"slide {number}" if slide else "the deck"
                raise OfficeError(f"edit {n}: {edit['find']!r} is not in {where}'s text - read the deck for its exact words")
            done.append(f"{'slide ' + str(number) if slide else 'deck'}: {edit['find']!r} -> {edit['replace']!r} x{hits}")
        elif action == "title":
            if slide.shapes.title is None:
                raise OfficeError(f"edit {n}: slide {number} has no title placeholder - use find/replace on its text")
            _set_text(slide.shapes.title, edit["title"])
            done.append(f"slide {number} title")
        elif action == "value":
            tables = [s.table for s in slide.shapes if getattr(s, "has_table", False) and s.has_table]
            which, row, col = edit.get("table", 1), edit.get("row"), edit.get("column")
            if not isinstance(which, int) or not 1 <= which <= len(tables):
                raise OfficeError(f"edit {n}: slide {number} has {len(tables)} table(s), not table {which!r}")
            table = tables[which - 1]
            if not isinstance(row, int) or not isinstance(col, int) or not (1 <= row <= len(table.rows)
                                                                          and 1 <= col <= len(table.columns)):
                raise OfficeError(f"edit {n}: give \"row\" and \"column\" from 1 within the table's "
                                  f"{len(table.rows)} x {len(table.columns)}")
            _set_text(table.cell(row - 1, col - 1).text_frame, "" if edit["value"] is None else edit["value"])
            done.append(f"slide {number} table {which} row {row} column {col}")
        elif action == "add_rows":
            # Rows added at the end of a table, each a copy of its last row (its look kept) holding the given texts.
            # Found live (2026-10-14): a deck's table needed a Total row the copied deck had no room for, and the
            # worker could only report the deck unfinished.
            frames = [s for s in slide.shapes if getattr(s, "has_table", False) and s.has_table]
            which, rows = edit.get("table", 1), edit["add_rows"]
            if not isinstance(which, int) or not 1 <= which <= len(frames):
                raise OfficeError(f"edit {n}: slide {number} has {len(frames)} table(s), not table {which!r}")
            table = frames[which - 1].table
            width = len(table.columns)
            if not isinstance(rows, list) or not rows or not all(isinstance(r, list) and len(r) <= width for r in rows):
                raise OfficeError(f"edit {n}: add_rows is a list of rows, each a list of at most {width} cell texts")
            import copy
            # "before": the row (from 1) the new rows go above - a total row stays last. Found live: rows added
            # after a deck's Total row left the table with two totals. A new row copies the row above where it
            # goes (an ordinary row, not the bold total).
            trs = table._tbl.tr_lst
            before = edit.get("before")
            if before is not None and (isinstance(before, bool) or not isinstance(before, int) or not 2 <= before <= len(trs)):
                raise OfficeError(f"edit {n}: \"before\" is a row number from 2 to {len(trs)} (rows count from 1)")
            model = trs[before - 2] if before else trs[-1]
            anchor = trs[before - 1] if before else None
            for k, values in enumerate(rows):
                tr = copy.deepcopy(model)
                if anchor is not None:
                    anchor.addprevious(tr)
                    index = before - 1 + k
                else:
                    table._tbl.append(tr)
                    index = len(table._tbl.tr_lst) - 1
                new_row = table.rows[index]
                for c in range(width):
                    value = values[c] if c < len(values) else ""
                    _set_text(new_row.cells[c].text_frame, "" if value is None else value)
                frames[which - 1].height += model.h
            done.append(f"slide {number} table {which}: {len(rows)} row(s) added" + (f" above row {before}" if before else ""))
        elif action == "categories":
            charts = [s.chart for s in slide.shapes if getattr(s, "has_chart", False) and s.has_chart]
            which = edit.get("chart", 1)
            if not isinstance(which, int) or not 1 <= which <= len(charts):
                raise OfficeError(f"edit {n}: slide {number} has {len(charts)} chart(s), not chart {which!r}")
            charts[which - 1].replace_data(_deck_chart_data(edit, f"edit {n}"))
            done.append(f"slide {number} chart {which} data")
        else:
            slide.notes_slide.notes_text_frame.text = str(edit["notes"])
            done.append(f"slide {number} notes")

    if dst == src:
        backup = src.with_name(src.stem + ".orig" + src.suffix)
        if not backup.exists() and not _task_made(src):
            shutil.copy2(src, backup)
    dst.parent.mkdir(parents=True, exist_ok=True)
    tmp = dst.with_name(dst.name + ".tmp")
    prs.save(str(tmp))
    os.replace(tmp, dst)
    _read_pptx(dst)  # what is written must read back
    return f"Updated {dst}: {len(done)} change(s) ({'; '.join(done)}). Run office read on it to check what it says."


# ------------------------------------------------------------------
# Command line, for the other harnesses' shell loops: `office read x.pdf`
# ------------------------------------------------------------------


# ============================================================
# Filling a Word document: office update DOC.docx
#
# Found live (2026-10-20, round 5 P4): an offer-letter template with {{FULL_NAME}}, {{CTC_ANNUAL}} and a table of
# {{BASIC}}/{{HRA}} cells was filled by clicking a word processor's Find & Replace, one key at a time - 25 minutes for
# four letters in one run, and stuck in a "Search for formatting" dialog in another. Word keeps a typed placeholder
# in pieces across several runs ("{{FULL" "_NAME}}"), so a plain text search of the XML misses most of them. Each
# paragraph's text is put together across its runs, a match is replaced in the first run it touches (that run's
# formatting is kept) and taken out of the others - in the body, tables, text boxes, headers and footers.
# ============================================================

_W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
# A template's placeholder: {{NAME}}, [[NAME]], <<NAME>> - what is left of one after filling is a fact, not a guess.
PLACEHOLDER = re.compile(r"\{\{\s*[^{}\n]{1,60}?\s*\}\}|\[\[\s*[^\[\]\n]{1,60}?\s*\]\]|<<\s*[A-Za-z_][\w .-]{0,58}\s*>>")
_DOCX_TEXT_PARTS = re.compile(r"^word/(?:document|header\d*|footer\d*|footnotes|endnotes)\.xml$")


def _replace_in_paragraph(paragraph, replace, counts):
    """Replace every key of `replace` in one <w:p>, across its runs. Adds to counts[key]."""

    texts = [t for t in paragraph.iter(_W + "t")]
    if not texts:
        return
    whole = "".join(t.text or "" for t in texts)
    if not any(key in whole for key in replace):
        return
    # Where each w:t starts in the paragraph's text.
    starts, at = [], 0
    for t in texts:
        starts.append(at)
        at += len(t.text or "")
    for key, value in replace.items():
        while True:
            whole = "".join(t.text or "" for t in texts)
            i = whole.find(key)
            if i < 0:
                break
            end = i + len(key)
            starts, at = [], 0
            for t in texts:
                starts.append(at)
                at += len(t.text or "")
            first = True
            for n, t in enumerate(texts):
                s, e = starts[n], starts[n] + len(t.text or "")
                if e <= i or s >= end:
                    continue
                text = t.text or ""
                lo, hi = max(i, s) - s, min(end, e) - s
                if first:
                    t.text = text[:lo] + str(value) + text[hi:]
                    first = False
                else:
                    t.text = text[:lo] + text[hi:]
                if t.text and (t.text[0].isspace() or t.text[-1].isspace()):
                    t.set("{http://www.w3.org/XML/1998/namespace}space", "preserve")
            counts[key] = counts.get(key, 0) + 1


def docx_placeholders(path):
    """The placeholders ({{...}}, [[...]], <<...>>) a .docx still holds anywhere, each once, in reading order."""

    found = []
    with zipfile.ZipFile(path) as z:
        for name in z.namelist():
            if not _DOCX_TEXT_PARTS.match(name):
                continue
            from lxml import etree
            root = etree.fromstring(z.read(name))
            for p in root.iter(_W + "p"):
                text = "".join(t.text or "" for t in p.iter(_W + "t"))
                for m in PLACEHOLDER.findall(text):
                    if m not in found:
                        found.append(m)
    return found


def update_document(path, replace, output_path=None):
    """
    Fill an existing Word document: every key of `replace` (exact text, e.g. "{{FULL_NAME}}") is replaced by its value
    in the body, tables, text boxes, headers, footers and notes - also a key Word split across runs, keeping the
    formatting of the run it starts in. Writes `output_path` (default: the file itself, after keeping the original as
    <name>.orig.docx the first time). Says how many times each key was replaced, a key not found, and every
    placeholder the result still holds.
    """

    replace = _clean_spec(replace)  # (a float's rounding error never written: 7640.220000000001 -> 7640.22)

    from lxml import etree

    p = _path(path)
    if not p.exists():
        raise OfficeError(f"{p} does not exist")
    if p.suffix.lower() != ".docx":
        raise OfficeError(f"{p.name}: office update fills .docx documents (and .xlsx workbooks, .pptx decks)")
    if not isinstance(replace, dict) or not replace:
        raise OfficeError('office update DOC.docx needs {"replace": {"{{KEY}}": "value", ...}}')
    replace = {str(k): ("" if v is None else str(v)) for k, v in replace.items()}
    if any(not k for k in replace):
        raise OfficeError("a key to replace cannot be empty")
    out = _path(output_path) if output_path else p
    if out.suffix.lower() != ".docx":
        raise OfficeError(f"{out.name}: the output must be a .docx")
    try:
        source = zipfile.ZipFile(p)
    except zipfile.BadZipFile:
        raise OfficeError(f"{p.name} is not a Word document (.docx) that can be read") from None
    counts, parts = {}, {}
    with source:
        items = [(info, source.read(info.filename)) for info in source.infolist()]
    for info, data in items:
        if _DOCX_TEXT_PARTS.match(info.filename):
            root = etree.fromstring(data)
            for paragraph in root.iter(_W + "p"):
                _replace_in_paragraph(paragraph, replace, counts)
            parts[info.filename] = etree.tostring(root, xml_declaration=True, encoding="UTF-8", standalone=True)
    if out == p and not p.with_name(p.stem + ".orig.docx").exists() and not _task_made(p):
        shutil.copy2(p, p.with_name(p.stem + ".orig.docx"))
    out.parent.mkdir(parents=True, exist_ok=True)
    tmp = out.with_name(out.name + ".tmp")
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as z:
        for info, data in items:
            z.writestr(info, parts.get(info.filename, data))
    tmp.replace(out)
    left = docx_placeholders(out)
    lines = [f"Wrote {out}."]
    lines.append("Replaced: " + (", ".join(f"{k} x{n}" for k, n in counts.items()) or "nothing"))
    missing = [k for k in replace if k not in counts]
    if missing:
        lines.append("Not found anywhere in the document: " + ", ".join(missing))
    lines.append("Placeholders still in it: " + (", ".join(left) if left else "none"))
    return "\n".join(lines)


USAGE = """office - documents, spreadsheets and email drafts, from the shell

  office read FILE [PAGES]              read a PDF, scan, picture, Word, Excel, PowerPoint or .eml file
  office update BOOK.xlsx SPEC          change cells of an existing workbook, keeping its charts,
                                        formatting and everything else. SPEC (JSON, - for stdin):
                                        {"edits": [{"sheet": "Data", "cell": "B14", "value": 1250.5}],
                                         "replace_refs": [{"from": "Data!$B$2:$B$13", "to": "Data!$B$2:$B$14"}],
                                         "add_sheets": [{"name": "Exceptions", "rows": [[...]], "cells": {...}}],
                                         "output_path": "optional.xlsx"}
                                        a value is a number, text, null (empties the cell) or "=FORMULA";
                                        replace_refs extends what charts, tables and names read.
                                        To add a column (its header in row 1, a value per row), rename one (its
                                        header cell), append rows (cells below the last) or add sheets, use office
                                        update - never edit an Office file's XML with a script: a file that no longer
                                        opens is not done
  office update DOC.docx SPEC           fill a Word document or template: SPEC {"replace": {"{{FULL_NAME}}": "Jane
                                        Doe", ...}, "output_path": "letter_jane.docx"} - in the body, tables,
                                        headers and footers, also where Word split a placeholder; it says what it
                                        replaced and every placeholder still left
  office create BOOK.xlsx SPEC          a new workbook. SPEC: {"sheets": [{"name": "Sheet1", "rows": [[...]],
                                        "cells": {"C5": "=SUM(C2:C4)", "F2": "=B2/AVERAGE(C2:E2)"}, "header": true,
                                        "widths": {"A": 20},
                                        "number_format": {"C": "#,##0.00"}}]} - rows fill from A1, cells set
                                        one address each; a sheet needs one or the other; a row's value may be
                                        "=FORMULA" too - put derived figures (averages, ratios) as formulas so
                                        they keep full precision, and round only with a number format; a sheet's
                                        "charts": [{"type": "column|bar|line|pie", "title": "Revenue",
                                        "data": "B1:B4", "categories": "A2:A4", "anchor": "E2"}] (data starts
                                        at its header cell)
  office create DOC.docx SPEC           a new Word document. SPEC: {"blocks": [{"heading": "Title", "level": 1},
                                        {"paragraph": "text"}, {"bullets": ["a", "b"]},
                                        {"table": [["Header", "..."], ["cell", "..."]]}, {"page_break": true}]}
  office create DOC.pdf SPEC            a new PDF document (an invoice, a quote, a letter): the same SPEC as .docx
  office create DECK.pptx SPEC          a new 16:9 deck. SPEC: {"slides": [{"title": "Q3 review", "subtitle": "Finance"},
                                        {"title": "Highlights", "bullets": ["a", {"text": "detail", "level": 1}],
                                         "notes": "speaker notes"},
                                        {"title": "Figures", "table": [["Region", "Revenue"], ["North", 1200]]},
                                        {"title": "Trend", "chart": {"type": "column|bar|line|pie", "title": "Revenue",
                                         "categories": ["Jul", "Aug"], "series": [{"name": "2026", "values": [10, 12]}]}},
                                        {"title": "Site", "image": "photo.png", "text": "words beside it"}]}
                                        - one of table, chart or image per slide
  office update DECK.pptx SPEC          change an existing deck, keeping its layout and everything else. SPEC:
                                        {"edits": [{"slide": 2, "find": "Q2", "replace": "Q3"}, {"slide": 1, "title": "..."},
                                         {"slide": 3, "table": 1, "row": 2, "column": 2, "value": "1,250"},
                                         {"slide": 3, "table": 1, "add_rows": [["Central", "0"]], "before": 6},
                                         {"slide": 4, "chart": 1, "categories": [...], "series": [...]},
                                         {"slide": 5, "notes": "..."}], "output_path": "optional.pptx"}
                                        find/replace with no slide changes every slide; rows, columns, tables
                                        and charts count from 1
  office calculate BOOK.xlsx            work out every formula and store its result in the file (formulas
                                        kept) - for a workbook a script wrote, whose formulas hold no results
  office combine OUT.pdf IN...          one PDF from PDFs, pictures and office documents, in order
  office email --to ADDR [--cc ADDR] --subject S [--attach FILE]... [--replaces DRAFT_ID] [--body TEXT | body on stdin]
                                        put an email in the Outbox for a person to review and send.
                                        It is never sent from here. A recipient from the records:
                                        --to-party "NAME" [--cc-party "NAME"] --records FILE takes the address
                                        from FILE's row for NAME (never type one); a typed address that is in
                                        no file you were given is refused
  office drafts                         the emails this task drafted (id, to, cc, subject), as JSON - write any
                                        list or log of the drafts from this, never type the addresses again
"""


def _spec(arg):
    """A JSON spec given as `-` (stdin), inline JSON, or a file."""

    import sys

    if arg == "-":
        text = sys.stdin.read()
    elif arg.lstrip().startswith(("{", "[")):
        text = arg
    else:
        text = Path(os.path.expanduser(arg)).read_text(encoding="utf-8")
    # A byte-order mark in front of it is not part of the spec: Windows
    # programs put one on what they pipe and on files they save (found live:
    # PowerShell did, and every spec piped to office was "not valid JSON").
    text = text.lstrip("﻿")
    try:
        spec = json.loads(text)
    except json.JSONDecodeError as exc:
        raise OfficeError(f"the spec is not valid JSON ({exc})") from exc
    if not isinstance(spec, dict):
        raise OfficeError("the spec must be a JSON object")
    return spec


def _only(spec, keys):
    """Refuse a spec key nothing reads: it would be dropped, and the result reported made without it."""

    unknown = sorted(set(spec) - keys)
    if unknown:
        raise OfficeError(f"the spec does not take {', '.join(map(repr, unknown))}; it takes {', '.join(sorted(keys))}")


def _email(argv):
    import argparse
    import sys

    from decurix_agent.tools import outbox

    parser = argparse.ArgumentParser(prog="office email", add_help=False)
    parser.add_argument("--to", action="append", default=[])
    parser.add_argument("--cc", action="append", default=[])
    parser.add_argument("--to-party", action="append", default=[])
    parser.add_argument("--cc-party", action="append", default=[])
    parser.add_argument("--records", default="")
    parser.add_argument("--subject", default="")
    parser.add_argument("--attach", action="append", default=[])
    parser.add_argument("--replaces", default="")
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

    from decurix_agent import payees, task_context
    request = "\n".join([task_context.current_request()] + task_context.current_files())
    # A party's address comes from the records file, never typed (payees.party_address).
    try:
        to = list(args.to) + [payees.party_address(p, args.records or None, request) for p in args.to_party]
        cc = list(args.cc) + [payees.party_address(p, args.records or None, request) for p in args.cc_party]
    except payees.PartyError as exc:
        raise OfficeError(f"not drafted: {exc}") from exc
    # A typed address that is in no input is refused now, with the one on record, not an attempt later at the check.
    typed = [a for a in list(args.to) + list(args.cc)]
    if typed and task_context.current():  # a task's inputs to check against
        try:
            refused = payees.typed_recipients_refused(request, typed, args.subject, body)
        except Exception:  # noqa: BLE001 - a check that cannot be made refuses nothing
            refused = ""
        if refused:
            raise OfficeError(f"not drafted: {refused}")

    try:
        return outbox.draft(to, args.subject, body, cc=cc, attachments=args.attach, replaces=args.replaces or None)
    except outbox.DraftError as exc:
        raise OfficeError(f"not drafted: {exc}") from exc


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE)
        return 0 if argv else 2
    try:
        cmd = argv.pop(0)
        written = None
        if cmd in ("create", "update", "calculate") and argv:
            # A file of the person's shared folder an app of ours still holds is let go first (its copy there is stale),
            # and what the tools saved is kept aside, so an app's later save cannot lose it (tool_made.py; CS).
            from decurix_agent import tool_made
            written = _path(argv[0])
            tool_made.before_write(written)
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
                raise OfficeError("usage: office update BOOK.xlsx|DECK.pptx|DOC.docx SPEC (JSON, or - to read it from stdin)")
            spec = _spec(argv[1])
            if argv[0].lower().endswith(".pptx"):
                _only(spec, {"edits", "output_path"})
                print(update_presentation(argv[0], spec.get("edits"), spec.get("output_path")))
            elif argv[0].lower().endswith(".docx"):
                _only(spec, {"replace", "output_path"})
                print(update_document(argv[0], spec.get("replace"), spec.get("output_path")))
            else:
                _only(spec, {"edits", "replace_refs", "output_path", "add_sheets"})
                print(update_workbook(argv[0], spec.get("edits"), spec.get("replace_refs"), spec.get("output_path"),
                                      spec.get("add_sheets")))
        elif cmd == "create":
            if len(argv) < 2:
                raise OfficeError("usage: office create BOOK.xlsx|DOC.docx|DOC.pdf|DECK.pptx SPEC (JSON, or - to read it from stdin)")
            spec = _spec(argv[1])
            if argv[0].lower().endswith(".pptx"):
                _only(spec, {"slides"})
                print(create_presentation(argv[0], spec.get("slides")))
            elif argv[0].lower().endswith(".docx"):
                print(create_document(argv[0], spec.get("blocks") or []))
            elif argv[0].lower().endswith(".pdf"):
                print(create_pdf(argv[0], spec.get("blocks") or []))
            else:
                print(create_workbook(argv[0], spec.get("sheets") or []))
        elif cmd == "calculate":
            if len(argv) != 1:
                raise OfficeError("usage: office calculate BOOK.xlsx")
            print(calculate_workbook(argv[0]))
        elif cmd == "combine":
            print(combine_pdf(argv[0], argv[1:]))
        elif cmd == "email":
            print(_email(argv))
        elif cmd == "drafts":
            # What this task drafted, as the Outbox has it: a log of the drafts is written from this, never typed.
            from decurix_agent.tools import outbox
            print(json.dumps([{k: d.get(k) for k in ("id", "to", "cc", "subject", "attachments")}
                              for d in outbox.drafted()], indent=1, ensure_ascii=False))
        else:
            print(f"unknown command {cmd!r}\n\n{USAGE}")
            return 2
        if written is not None:
            tool_made.remember(written)
    except (OfficeError, OSError, ValueError, IndexError) as exc:
        print(f"error: {exc}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
