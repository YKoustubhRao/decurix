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
from collections import Counter
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


def _is_picture(head):
    """Whether a file's first bytes are a PNG, JPEG, GIF or WebP picture (what the models are shown)."""
    return (head.startswith(b"\x89PNG\r\n\x1a\n") or head.startswith(b"\xff\xd8\xff")
            or head[:6] in (b"GIF87a", b"GIF89a") or (head[:4] == b"RIFF" and head[8:12] == b"WEBP"))
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
        with open(p, "rb") as f:
            head = f.read(400)  # (only the start says what it is: a large picture is not read whole)
        if not _is_picture(head):
            # Found live (2026-10-22): a ".jpg" that was not a picture went to the model as one and the
            # task stopped. What the file holds is said instead - often a web page a failed download saved.
            shown = " ".join(head.decode("utf-8", errors="replace").split())[:200]
            return {"text": f"{p.name} is named as a picture but is not one (not a PNG, JPEG, GIF or WebP file). It "
                            f"begins: {shown!r} - if it was downloaded, the download did not bring the picture.",
                    "images": []}
        if pages:
            return _zoom_picture(p, pages)
        return {"text": f"{p.name}: an image, shown below. To read a small or faint part closer: office read "
                        f"'{p}' PART - PART is top, bottom, left, right, top-left, top-right, bottom-left, "
                        "bottom-right, center, or x0,y0,x1,y1 in percent of the picture (e.g. 50,0,100,30).",
                "images": [str(p)]}
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
    p = Path(p)
    suffix = p.suffix.lower()
    if suffix not in (".xlsx", ".xlsm", ".xltx", ".xltm", ".docx", ".dotx", ".docm", ".pptx", ".potx", ".pptm"):
        return "unchecked", f"{p.name}: not a kind this check opens"
    raw = p.read_bytes()
    if raw[:8] == _CFB_MAGIC:
        return "protected", f"{p.name} is password-protected or encrypted: it was not checked"
    try:
        with zipfile.ZipFile(io.BytesIO(raw)) as z:
            if any(_STRICT_OOXML in z.read(n)[:4096] for n in z.namelist()
                   if n.endswith((".xml", ".rels")) and n.count("/") <= 1):
                return "unchecked", f"{p.name} is Strict Open XML: it was not checked"
    except zipfile.BadZipFile as exc:
        return "broken", broken_file(p, exc)
    except Exception:  # noqa: BLE001 - the reader below decides
        pass
    try:
        data = io.BytesIO(raw)
        if suffix in (".xlsx", ".xlsm", ".xltx", ".xltm"):
            _openpyxl().load_workbook(data, keep_vba=suffix in (".xlsm", ".xltm")).close()
        else:
            if suffix in (".docx", ".dotx", ".docm"):
                import docx
                reader = docx.Document
            else:
                import pptx
                reader = pptx.Presentation
            try:
                reader(data)
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
        table = table_quality.csv_table(p)
        return table_quality.report(p, *table) if table else ""
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
                table = table_quality.sheet_table(sheet)
                if table:
                    said = table_quality.report(f"{p}#{sheet.title}", *table)
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


MAX_BLOCK_TITLE = 40


def _row_labels(ws):
    """{row: the row's first text, stripped} for the rows of a sheet that have one."""

    labels = {}
    for row in ws.iter_rows(min_row=1, max_row=ws.max_row):
        for c in row:
            if isinstance(c.value, str) and c.value.strip() and not c.value.startswith("="):
                labels[c.row] = " ".join(c.value.split())
                break
    return labels


def sheet_blocks(ws):
    """
    [(first row, last row, title)] when a sheet repeats one layout block after block (a month per block, a client per
    block): the label that recurs most marks each block, and the block's title is the topmost row above it, within the
    gap from the block before, whose label occurs nowhere else. [] for a sheet that does not repeat a layout. Found live
    (2026-10-21): "Closing credit carried forward" stood in six blocks of a workbook read as one list of
    cells, and the worker took August's figure from another month's block. Decided from the sheet's labels alone.
    """

    labels = _row_labels(ws)
    rows_of = {}
    for r, label in labels.items():
        rows_of.setdefault(label.casefold(), []).append(r)
    repeated = {k: v for k, v in rows_of.items() if len(v) >= 2}
    if len(repeated) < 2:
        return []
    anchor = max(repeated.values(), key=lambda rows: (len(rows), -rows[0]))
    # A layout, not a table whose names come again: another label stands at the same distance from the marker in
    # every block ("Date" two rows below the client's name in each month).
    by_row = {r: label.casefold() for r, label in labels.items()}
    shared = None
    for at in anchor:
        here = {(r - at, by_row[r]) for r in by_row if at < r <= at + 6 and by_row[r] != by_row[anchor[0]]}
        shared = here if shared is None else shared & here
    if not shared:
        return []
    # The rows right above each block's marker whose labels occur once: its title rows. Every block has as many; a
    # block that shows more has the notes of the block before glued on (no blank row between), so the fewest counts.
    filled = {c.row for row in ws.iter_rows(min_row=1, max_row=ws.max_row) for c in row if c.value not in (None, "")}
    ups = []  # per block, the title rows above its marker, nearest first (blank rows between are passed over)
    for i, at in enumerate(anchor):
        floor = anchor[i - 1] + 1 if i else 1
        found, r = [], at - 1
        while r >= floor:
            if r not in filled:
                r -= 1
                continue
            if labels.get(r) is None or len(rows_of[labels[r].casefold()]) != 1:
                break
            found.append(r)
            r -= 1
        ups.append(found)
    above = min(len(u) for u in ups)
    starts = []
    for i, at in enumerate(anchor):
        if above:
            start = ups[i][above - 1]
            starts.append((start, labels[start]))
            continue
        # No title above the marker: the block's first line of its own below it names it.
        nxt = anchor[i + 1] if i + 1 < len(anchor) else ws.max_row + 1
        own = next((labels[r] for r in range(at + 1, nxt) if r in labels and len(rows_of[labels[r].casefold()]) == 1), labels[at])
        starts.append((at, own))
    blocks = []
    for i, (start, title) in enumerate(starts):
        end = starts[i + 1][0] - 1 if i + 1 < len(starts) else ws.max_row
        blocks.append((start, end, title[:MAX_BLOCK_TITLE]))
    # A layout is repeated only when its blocks have titles of their own.
    return blocks if len({t.casefold() for _, _, t in blocks}) == len(blocks) else []


def _value_kind(v):
    """What a cell holds, as a layout sees it: a date, a number, text that reads as a date or a number, or text."""

    import datetime
    if isinstance(v, (datetime.date, datetime.datetime)):
        return "date"
    if isinstance(v, bool):
        return "flag"
    if isinstance(v, (int, float)):
        return "number"
    if isinstance(v, str) and not v.startswith("="):
        s = v.strip()
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}(?:[ T][\d:]+)?|\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}", s):
            return "date as text"
        if re.fullmatch(r"-?[\d,]+(?:\.\d+)?", s):
            return "number as text"
        return "text"
    return None


# A cell's kind against its column's: a date written as text, a figure written as text, a code written as a figure.
_MIXED_KINDS = {("date as text", "date"), ("number as text", "number"), ("number", "number as text")}
_KIND_NAMES = {"date": "dates", "number": "numbers", "number as text": "numbers written as text"}


def block_layout_breaks(path, rows=None):
    """
    Where a block of a sheet that repeats one layout (sheet_blocks, a month per block) does not follow it, as sentences:
    a label every other block has that this one lacks (a header row left out), and a cell whose kind - a date, a number,
    text - differs from what the same column holds in every other block (a date typed as text, a code typed as a number).
    `rows`: {sheet: set of row numbers} - only blocks holding one of them are looked at (what the task changed); None
    looks at the last block of each sheet. Found live (2026-10-22): a September block added below August's
    had no header row, its dates were text and its codes numbers; the report said "laid out like the earlier months".
    Decided from the sheet alone; evidence for the judge.
    """

    books = _formula_books(path, with_values=False)
    if books is None:
        return []
    out = []
    for ws in books[0].worksheets:
        blocks = _loose_blocks(ws)
        if len(blocks) < 3:
            continue
        if rows is not None and not rows.get(ws.title):
            continue  # (the task changed nothing on this sheet: its last block is not this task's to answer for)
        labels = _row_labels(ws)
        wanted = rows.get(ws.title) if rows is not None else None
        for i, (a, b, title) in enumerate(blocks):
            if wanted is not None and not any(a <= r <= b for r in wanted):
                continue
            if wanted is None and i != len(blocks) - 1:
                continue
            others = [blk for j, blk in enumerate(blocks) if j != i]
            mine = {labels[r].casefold() for r in range(a, b + 1) if r in labels}
            # The layout's own rows: a label at the same distance from its block's start in every other block (a header
            # row) - not a name that happens to come back in several (a regular customer).
            common = None
            for oa, ob, _ in others:
                theirs = {(r - oa, labels[r].casefold()) for r in range(oa, ob + 1) if r in labels}
                common = theirs if common is None else common & theirs
            missing = sorted({label for _, label in common or ()} - mine)
            if missing:
                out.append(f"{ws.title}: the block {title!r} (rows {a}-{b}) has no row {', '.join(repr(m) for m in missing[:4])}, "
                           f"which each of the other {len(others)} blocks has - it does not follow their layout.")
            out += _period_breaks(ws, a, b, title) + _joined_breaks(ws, a, b, others)
            for column in range(1, ws.max_column + 1):
                # The column's kind in the other blocks: what at least four in five of its dates and figures are.
                seen = [k for oa, ob, _ in others for r in range(oa, ob + 1)
                        if (k := _value_kind(ws.cell(r, column).value)) not in (None, "text", "flag")]
                if len(seen) < 4:
                    continue
                main, n = Counter(seen).most_common(1)[0]
                if n * 5 < len(seen) * 4:
                    continue
                for r in range(a, b + 1):
                    v = ws.cell(r, column).value
                    kind = _value_kind(v)
                    if (kind, main) in _MIXED_KINDS:
                        out.append(f"{ws.title}!{_col_letter(column)}{r} holds {v!r} as {kind}, where the column holds "
                                   f"{_KIND_NAMES[main]} in the other blocks.")
                        break
    return out[:12]


_MONTHS = {name: n for n, names in enumerate(
    (("january", "jan"), ("february", "feb"), ("march", "mar"), ("april", "apr"), ("may",), ("june", "jun"),
     ("july", "jul"), ("august", "aug"), ("september", "sep", "sept"), ("october", "oct"), ("november", "nov"),
     ("december", "dec")), start=1) for name in names}
# Two values in one cell: "4410/7208", "12%/5%" - each side a code or a figure (it holds a digit). Words joined by a
# slash ("Source / Use", "Debit/Credit") are a label, not two entries.
_JOINED = re.compile(r"\s*([\w.%-]*\d[\w.%-]*)\s*/\s*([\w.%-]*\d[\w.%-]*)\s*")


def _cell_date(v):
    """A cell's date - a date, or text written year first (2026-09-02) - else None. A day-or-month-first text date is
    left alone: which is which is not the cell's to say."""

    import datetime
    if isinstance(v, datetime.datetime):
        return v.date()
    if isinstance(v, datetime.date):
        return v
    if isinstance(v, str):
        m = re.fullmatch(r"\s*(\d{4})-(\d{2})-(\d{2})(?:[ T][\d:]+)?\s*", v)
        if m:
            try:
                return datetime.date(int(m.group(1)), int(m.group(2)), int(m.group(3)))
            except ValueError:
                return None
    return None


def _period_breaks(ws, a, b, title):
    """
    A row of a block whose title names a month (a month per block) dated in another month. Found live (2026-10-24): a
    row of August was copied into September's new block and counted in its total; nothing said so.
    """

    def month_of(text):
        found = {_MONTHS[w] for w in re.findall(r"[a-z]+", (text or "").lower()) if w in _MONTHS}
        return found.pop() if len(found) == 1 else None

    # The block's period: its title, else the nearest label above it, else one near its top, that names one month.
    labels = _row_labels(ws)
    month = month_of(title)
    for r in [*range(a - 1, max(0, a - 4), -1), *range(a, min(b, a + 3) + 1)]:
        if month is None and r in labels:
            month = month_of(labels[r])
    if month is None:
        return []
    for r in range(a, b + 1):
        for column in range(1, min(ws.max_column, 40) + 1):
            d = _cell_date(ws.cell(r, column).value)
            if d is not None and d.month != month:
                name = next(n for n, m in _MONTHS.items() if m == month and len(n) > 3 or (m == month == 5)).title()
                return [f"{ws.title} row {r} is dated {d:%Y-%m-%d}, inside the block of {name} (rows {a}-{b}) - a row of "
                        "another period inside this one."]
    return []


def _joined_breaks(ws, a, b, others):
    """
    A cell holding two values joined ("4410/7208") in a column that holds one value per row in every other block - a
    document with two parts recorded as one row. Found live (2026-10-24): two bills at two tax rates each went in as
    one row each, two codes joined in the code cell, rate blank, where every earlier row held one code and one rate.
    """

    out = []
    for column in range(1, min(ws.max_column, 40) + 1):
        per_block = [cells for oa, ob, _ in others
                     if (cells := {(r - oa, str(v)) for r in range(oa, ob + 1)
                                   if (v := ws.cell(r, column).value) not in (None, "")})]
        # A value at the same place in every block that fills the column is the layout's own (a header such as "Code/Ref").
        layout = {v for _, v in set.intersection(*per_block)} if len(per_block) >= 2 else set()
        theirs = [v for cells in per_block for _, v in cells if v not in layout]
        if len(theirs) < 3 or any("/" in v and _value_kind(v) == "text" for v in theirs):
            continue
        for r in range(a, b + 1):
            v = ws.cell(r, column).value
            if isinstance(v, str) and v not in layout and _value_kind(v) == "text" and _JOINED.fullmatch(v):
                out.append(f"{ws.title}!{_col_letter(column)}{r} holds {v!r} - two values in one cell, where the column "
                           "holds one per row in the other blocks: a document with two parts goes in as one row per part.")
                break
    return out


def _loose_blocks(ws):
    """sheet_blocks without its demand that every block share the marker's layout - so a block that breaks it is still
    a block, to be told it does."""

    labels = _row_labels(ws)
    rows_of = {}
    for r, label in labels.items():
        rows_of.setdefault(label.casefold(), []).append(r)
    repeated = {k: v for k, v in rows_of.items() if len(v) >= 3}
    if not repeated:
        return []
    anchor = max(repeated.values(), key=lambda rows: (len(rows), -rows[0]))
    starts = []
    for i, at in enumerate(anchor):
        floor = anchor[i - 1] + 1 if i else 1
        start = at
        r = at - 1
        while r >= floor and labels.get(r) is not None and len(rows_of[labels[r].casefold()]) == 1:
            start = r
            r -= 1
        starts.append((start, labels[start]))
    return [(s, (starts[i + 1][0] - 1 if i + 1 < len(starts) else ws.max_row), t[:MAX_BLOCK_TITLE])
            for i, (s, t) in enumerate(starts)]


def read_workbook(path, sheet=None, cells=None, max_rows=DEFAULT_ROWS, limit=MAX_TEXT_CHARS):
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

        try:
            blocks = sheet_blocks(ws_f)
        except Exception:  # noqa: BLE001 - the cells are still worth reading when the layout cannot be told
            blocks = []
        if blocks:
            # First, so a reply cut short still says where each block is; each row below carries its block's title.
            out.append("Blocks (this sheet repeats one layout; each row below is tagged with its block): "
                       + "; ".join(f"rows {a}-{b} {t!r}" for a, b, t in blocks)
                       + f". Read one block with its range, e.g. 'A{blocks[-1][0]}:{_col_letter(ws_f.max_column)}{blocks[-1][1]}'.")

        def block_of(r):
            for a, b, t in blocks:
                if a <= r <= b:
                    return t
            return None

        if cells:
            rows = ws_f[cells]
            if not isinstance(rows, tuple) or (rows and not isinstance(rows[0], tuple)):
                rows = (rows if isinstance(rows, tuple) else (rows,),)
        elif blocks and ws_f.max_row > max_rows:
            # A sheet that repeats a block per period grows at its end: its newest blocks are what a task goes on from,
            # shown whole. Found live (2026-10-24): the first 60 rows of a computation sheet ended inside July, the
            # August block's closing figures were never shown, and the worker reported July's as August's.
            first = blocks[-1][0]
            for a, _, _ in reversed(blocks):
                if ws_f.max_row - a + 1 > max_rows:
                    break
                first = a
            rows = ws_f.iter_rows(min_row=first, max_row=ws_f.max_row)
            earlier = [(a, b, t) for a, b, t in blocks if b < first]
            if earlier:
                out.append(f"(the newest blocks are shown whole, rows {first}-{ws_f.max_row}; earlier blocks not shown: "
                           + ", ".join(repr(t) for _, _, t in earlier)
                           + f" - read one with its range, e.g. 'A{earlier[-1][0]}:{_col_letter(ws_f.max_column)}{earlier[-1][1]}')")
        else:
            last = min(ws_f.max_row, max_rows)
            rows = ws_f.iter_rows(min_row=1, max_row=last)
            if ws_f.max_row > last:
                out.append(f"(first {last} rows shown; ask for cells like 'A{last + 1}:H{min(ws_f.max_row, last + 100)}' for more)")

        for row in rows:
            shown = []
            tag = block_of(row[0].row) if blocks and row else None
            if tag:
                shown.append(f"[{tag}]")
            for c in row:
                if c.value is None:
                    continue
                v = c.value
                if isinstance(v, str) and v.startswith("="):
                    cached = ws_v[c.coordinate].value
                    shown.append(f"{c.coordinate}={v} -> {_cell_shown(cached)}")
                else:
                    shown.append(f"{c.coordinate}={_cell_shown(v)}")
            if len(shown) > (1 if tag else 0):
                out.append("  ".join(shown))

    out.extend(_workbook_objects(p))
    return _clip("\n".join(out), limit)


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

    from decurix_agent.tools import table_quality

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
            runs = ", ".join(table_quality._runs(set(rows)))
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


def _cell_shown(value):
    """A cell's value as office read writes it: text and numbers as Python writes them ('Total', 1240.0), a date as a
    date (2026-07-04, with its time when it has one). Found live (2026-10-21): dates came out as
    datetime.datetime(2026, 7, 4, 0, 0), which the check that a typed date was read did not know as a date - the
    invoice dates typed into a portal were refused as guesses."""

    import datetime as _dt

    if isinstance(value, _dt.datetime):
        return value.date().isoformat() if value.time() == _dt.time(0, 0) else value.isoformat(sep=" ")
    if isinstance(value, (_dt.date, _dt.time)):
        return value.isoformat()
    return repr(value)


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
            cell = _cell_address(ref)
            if isinstance(v, dict):
                v = _given_value(v, f"cell {cell} has no value: give a value, \"=FORMULA\" or {{\"formula\": ...}}")
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


def _cell_address(ref):
    cell = str(ref or "").upper().replace("$", "")
    if not re.fullmatch(r"[A-Z]{1,3}[1-9]\d*", cell):
        raise OfficeError(f"{ref!r} is not a cell address like B14")
    return cell


def _given_value(spec, missing):
    """A cell's "value", or its "formula" with its "="; OfficeError(missing) when it has neither."""

    if "value" in spec:
        return spec["value"]
    if "formula" in spec:
        value = str(spec["formula"] or "")
        return "=" + value if value and not value.startswith("=") else value
    raise OfficeError(missing)


# A whole number with this many digits or more is written as text: Excel shows it as 1.23457E+11 from 12 digits and
# keeps only 15 of them, so a bank account or Aadhaar number would be changed. (No amount in whole rupees runs to 12
# digits; a 9-11 digit figure stays a number, or its SUM would silently leave it out.)
_LONG_DIGITS = 12


def _cell_value(v):
    """A value as written into a cell: a text that is a plain number becomes the number, unless it is an identifier
    written in digits - one with a leading zero or too long for a number to keep as typed - which stays text."""

    if isinstance(v, float):
        return clean_number(v)
    if isinstance(v, str):
        s = v.strip()
        m = re.fullmatch(r"-?(\d+)(\.\d+)?", s)
        if m and not (len(m.group(1)) > 1 and m.group(1).startswith("0")) \
                and len(m.group(1)) < _LONG_DIGITS and len(m.group(1) + (m.group(2) or "")) <= 16:
            return clean_number(float(s)) if "." in s else int(s)
        return clean_text(v)
    return v


# ------------------------------------------------------------------
# Workbooks: updating in place (charts survive)
# ------------------------------------------------------------------

def copy_rows_touches(copy_rows, sheet, cell):
    """Whether a copy_rows of this update writes the cell: its text is then the copy's, not the sheet's (an edit may
    blank what the copy brought)."""
    _, row = _split(cell)
    for spec in copy_rows or []:
        if (spec.get("sheet") or "") != sheet:
            continue
        try:
            a, b = (int(x) for x in str(spec.get("rows", "")).split(":"))
            to = int(spec.get("to"))
        except (TypeError, ValueError):
            continue
        if to <= int(row) <= to + (b - a):
            return True
    return False


_HELD = {}


def _held_value(path, sheet, cell):
    """What a saved cell holds (None when empty), read with openpyxl from the file as saved; the workbook is read once
    per path and kept for the update."""
    wb = _held_workbook(path)
    if wb is None:
        return None
    try:
        if sheet not in wb.sheetnames:
            return None
        v = wb[sheet][cell].value
    except Exception:
        return None
    return None if v in (None, "") else v


def _date_cell(path, sheet, cell):
    """Whether a saved cell is formatted as a date (its own format, or the format of the filled cell above it when it
    is new): a column of dates takes dates."""
    wb = _held_workbook(path)
    if wb is None or sheet not in wb.sheetnames:
        return False
    try:
        ws = wb[sheet]
        c = ws[cell]
        if c.value not in (None, "") or c.has_style:
            return bool(c.is_date) or _date_format(c.number_format)
        col, row = _split(cell)
        for r in range(int(row) - 1, max(int(row) - 6, 0), -1):
            above = ws[f"{col}{r}"]
            if above.value not in (None, ""):
                return bool(above.is_date) or _date_format(above.number_format)
    except Exception:
        return False
    return False


def _date_format(fmt):
    f = str(fmt or "").lower()
    return "general" not in f and any(ch in f for ch in "dy") and "m" in f


def _date_serial(d):
    import datetime
    return (d - datetime.date(1899, 12, 30)).days


def _held_workbook(path):
    key = (str(path), os.path.getmtime(str(path)) if os.path.exists(str(path)) else 0)
    wb = _HELD.get(key)
    if wb is None:
        try:
            import openpyxl
            wb = openpyxl.load_workbook(str(path), data_only=False)
        except Exception:
            return None
        _HELD.clear()
        _HELD[key] = wb
    return wb


def _short_value(v):
    t = str(v)
    return repr(t[:30] + ("..." if len(t) > 30 else "")) if isinstance(v, str) else t[:30]


def update_workbook(path, edits=None, replace_refs=None, output_path=None, add_sheets=None, insert_rows=None,
                    copy_rows=None, clear_text=False):
    """
    Change cells of an existing workbook without touching anything else.

    `edits`: [{"sheet": "Data", "cell": "B14", "value": 1250.5}] - a number,
    text, true/false, null (empties the cell) or a formula ("=SUM(B2:B14)").
    `replace_refs`: [{"from": "Data!$B$2:$B$13", "to": "Data!$B$2:$B$14"}] -
    rewritten wherever charts, tables and defined names refer to them, so a
    chart takes in a month that was appended.
    `insert_rows`: [{"sheet": "Data", "before": 54, "count": 12}] - empty rows
    put in above row 54, every reference below them moved with them (as a
    spreadsheet app's Insert rows); done before the edits, which address the
    rows as they are after it.
    `copy_rows`: [{"sheet": "Calc", "rows": "69:84", "to": 86, "replace": {"AUGUST": "SEPTEMBER"}}] - a block of
    rows copied to start at row `to`, as a spreadsheet's copy and paste (_copy_rows): the next period's block made
    from the last one, then filled with edits in the same update.
    `clear_text`: an edit that empties a cell holding text (a label, a heading, a name) is refused unless this is
    true - found live (2026-10-26): a block copied like the earlier ones had its label row blanked along with the
    figures, and the sheet lost the row that names its columns.

    Writes `output_path` (default: the file itself, after keeping the
    original as <name>.orig.xlsx the first time). Excel and LibreOffice
    recalculate every formula when the file is next opened.
    """

    src = _path(path)
    if not src.exists():
        raise OfficeError(f"{src} does not exist")
    dst = _path(output_path) if output_path else src

    entries, order = _zip_parts(src)

    # New sheets first, so the edits below can fill them too. Found live (2026-10-14): asked to add an Exceptions
    # and a By Account sheet to an existing model, a worker had no way to and made a second workbook instead.
    added = []
    for sheet in add_sheets or []:
        added.append(_add_sheet(entries, order, sheet))

    sheet_files = _sheet_files(entries)
    changed = []

    inserted = [_insert_rows(entries, sheet_files, spec) for spec in insert_rows or []]

    if copy_rows and insert_rows:
        raise OfficeError("insert_rows and copy_rows go in separate updates (the copy reads the rows as saved) - "
                          "nothing was written")
    copied = []
    for spec in copy_rows or []:
        words, refs, outside, cleared = _copy_rows(src, entries, sheet_files, spec)
        changed += refs
        copied.append(words + (("; formulas that read another sheet moved with the copy - check each reads the new "
                                "period's rows there: " + ", ".join(outside[:8])) if outside else "")
                      + (("; typed figures left empty to fill: " + ", ".join(cleared[:12])
                          + (" ..." if len(cleared) > 12 else "")) if cleared else ""))

    # The guards below read each cell as the edits will find it. After rows were put in, that is not the file as saved:
    # found in review (2026-11-02), an edit emptying the TOTAL label that insert_rows had moved down read the saved
    # file's (empty) cell at that address, and the label went without a word. So they read the workbook as it is now.
    probe = src
    if inserted and edits:
        handle, name = tempfile.mkstemp(suffix=src.suffix or ".xlsx", prefix="office-probe-")
        os.close(handle)
        probe = Path(name)
        _write_zip(probe, order, entries)
    try:
        by_sheet, labels, replaced = _guard_edits(probe, edits, sheet_files, copy_rows, clear_text)
    finally:
        if probe != src:
            _HELD.clear()
            try:
                probe.unlink()
            except OSError:
                pass

    for sheet, cell_edits in by_sheet.items():
        name = sheet_files[sheet]
        xml = entries[name].decode("utf-8")
        for cell, value in cell_edits:
            xml = _set_cell(xml, cell, value)
            changed.append(f"{sheet}!{cell}")
        entries[name] = _fix_dimension(xml).encode("utf-8")

    return _finish_update(src, dst, entries, order, replace_refs, added, inserted, copied, changed, replaced)


def _guard_edits(src, edits, sheet_files, copy_rows, clear_text):
    """The edits by sheet [(cell, value)], after the guards that read `src` - the workbook as the edits will find it:
    a date cell takes a date, text is not emptied unless clear_text, a filled cell written over is said. Returns
    (by_sheet, labels, replaced); raises OfficeError when an edit is refused."""

    by_sheet = {}
    labels, replaced = [], []
    for e in edits or []:
        sheet = e.get("sheet") or next(iter(sheet_files))
        if sheet not in sheet_files:
            raise OfficeError(f"no sheet {sheet!r}; the sheets are {list(sheet_files)}")
        cell = _cell_address(e.get("cell"))
        if "value" in e and _date_cell(src, sheet, cell):
            # A date cell takes a date: text written year first becomes the cell's own date (never "date as text"), and
            # a bare number is refused - found live (2026-10-26): serials worked out in the head gave 2025 dates, and
            # text dates in a column of dates drew four questions.
            v = e.get("value")
            if isinstance(v, str) and _cell_date(v) is not None:
                e = dict(e, value=_date_serial(_cell_date(v)))
            elif isinstance(v, (int, float)) and not isinstance(v, bool) and not copy_rows_touches(copy_rows, sheet, cell):
                raise OfficeError(f"not updated - {sheet}!{cell} is a date cell: give the date as text, year first "
                                  f"(2026-09-26), not a number ({v}) - nothing was written")
        if "value" in e and not copy_rows_touches(copy_rows, sheet, cell):
            held = _held_value(src, sheet, cell)
            if held is not None and e.get("value") in (None, ""):
                if not clear_text and isinstance(held, str) and not held.startswith("=") and re.search(r"[A-Za-z]", held):
                    labels.append(f"{sheet}!{cell} {held.strip()[:40]!r}")
            elif held is not None and str(held).strip() != str(e.get("value")).strip():
                # Written over: said in the update's words, so what was in the cell (another record's figure, an
                # earlier value) is known to be gone. Found live (2026-10-26): a bill's row was written over by the
                # next bill's and the first bill left the file unnoticed.
                replaced.append(f"{sheet}!{cell} {_short_value(held)} -> {_short_value(e.get('value'))}")
        # "formula" is how a model naturally names a formula's edit; taken as
        # the value (with its "="). An edit with neither key is refused, never
        # read as "empty the cell": live, {"cell": "D2", "formula": "=SUM(..)"}
        # wiped a total and the engine reported it updated.
        value = _given_value(e, f"the edit of {sheet}!{cell} has no value: give \"value\" (a number, text, \"=FORMULA\", "
                                "or null to empty the cell)")
        by_sheet.setdefault(sheet, []).append((cell, value))
    if labels:
        raise OfficeError("not updated - these cells hold text (labels, headings, names), not figures, and the update "
                          "would empty them: " + ", ".join(labels[:10]) + (" ..." if len(labels) > 10 else "")
                          + ". A block made like the earlier ones keeps their labels; empty only the figures. To clear "
                          "text on purpose, put \"clear_text\": true in the update.")
    return by_sheet, labels, replaced


def _finish_update(src, dst, entries, order, replace_refs, added, inserted, copied, changed, replaced):
    """The rest of update_workbook once the cells are set: ranges rewritten, recalculation on open, the file written
    and its results stored. Returns the update's sentence."""

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
        _keep_original(src)

    dst.parent.mkdir(parents=True, exist_ok=True)
    _write_zip(dst, order, entries)
    stored = _store_results(dst)

    msg = (f"Updated {dst}: " + (f"added sheet(s) {', '.join(added)}; " if added else "")
           + "".join(f"{words}; " for words in inserted) + "".join(f"{words}; " for words in copied)
           + f"{len(changed)} cell(s)")
    if changed:
        msg += " (" + ", ".join(changed[:20]) + (" ..." if len(changed) > 20 else "") + ")"
    if replaced:
        msg += (f"; replaced {len(replaced)} filled cell(s): " + ", ".join(replaced[:20])
                + (" ..." if len(replaced) > 20 else "")
                + " - what those cells held is gone from the file: if it was another record's, put it back in its own row")
    if replace_refs:
        msg += f"; {swaps} range reference(s) rewritten"
        if not swaps:
            msg += " - none matched: read the workbook to see the exact ranges its charts use"
    msg += (". Formula results are recalculated and saved in it." if stored else ". Formulas recalculate when the file is opened.")
    return msg + _layout_note(dst, changed)


MAX_COPIED_ROWS = 200


def _cell_style(row_xml, ref):
    """The format index of a cell in its row's XML, or None."""

    found = re.search(_CELL_RE.format(ref=ref), row_xml, re.S) if row_xml else None
    own = re.search(r'\bs="(\d+)"', found.group(0)) if found else None
    return own.group(1) if own else None
# A fact a label of one period holds: a date, a long number, a reference of letters and digits (copy_rows).
_PERIOD_FACT = re.compile(r"\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}|\d{4}-\d{2}-\d{2}|\d{5,}|\b(?=\w*\d)(?=\w*[A-Za-z])\w{8,}\b")


def _copy_rows(src, entries, sheet_files, spec):
    """
    One copy_rows entry: {"sheet": "Calc", "rows": "69:84", "to": 86, "replace": {"AUGUST": "SEPTEMBER"},
    "keep_figures": false}. The rows are copied to start at row `to` as a spreadsheet's copy and paste does: each
    formula's relative references move with it (a block's "previous closing" then reads the block it was copied from),
    labels take the replacements, each cell keeps its format, and figures typed as values (not formulas) are left empty
    to fill - unless keep_figures. Nothing is written over a cell that holds something. Found live (2026-10-23/24): a
    new month's block typed by hand into a register of monthly blocks came out with another month's row in it, and its
    labels and totals pointing at the wrong rows. Merged cells are not copied. Returns (sentence, changed cells,
    formulas that read another sheet, cells left to fill).
    """

    import datetime
    from openpyxl.formula.translate import Translator

    sheet = spec.get("sheet") or next(iter(sheet_files))
    if sheet not in sheet_files:
        raise OfficeError(f"copy_rows: no sheet {sheet!r}; the sheets are {list(sheet_files)} - nothing was written")
    m = re.fullmatch(r"\s*(\d+)\s*[:-]\s*(\d+)\s*", str(spec.get("rows") or ""))
    to = spec.get("to")
    if not m or not isinstance(to, int) or isinstance(to, bool):
        raise OfficeError('each of copy_rows is {"sheet": "Calc", "rows": "69:84", "to": 86, "replace": {"OLD": "NEW"}}'
                          " - nothing was written")
    a, b = int(m.group(1)), int(m.group(2))
    end = to + (b - a)
    if a < 1 or a > b or b - a + 1 > MAX_COPIED_ROWS or to < 1 or not (end < a or to > b):
        raise OfficeError(f"copy_rows: rows {a}-{b} to row {to} - give the first and last row of the block (at most "
                          f"{MAX_COPIED_ROWS}) and a row the copy does not overlap it from - nothing was written")
    replace = spec.get("replace") or {}
    if not isinstance(replace, dict):
        raise OfficeError('copy_rows: "replace" is {"OLD": "NEW"} - nothing was written')
    keep = bool(spec.get("keep_figures"))
    offset = to - a

    wb = _openpyxl().load_workbook(str(src), data_only=False)
    ws = wb[sheet]
    taken = [c.coordinate for r in range(to, end + 1) for c in ws[r] if c.value not in (None, "")]
    if taken:
        raise OfficeError(f"copy_rows would write over {', '.join(taken[:6])} - nothing was written: copy the block to "
                          "empty rows (below the last block)")
    # A block copied short of its end: the row after the range still belongs to it (a total, a closing line - more
    # than one cell filled, or a figure or formula in it). Found live (2026-10-26): rows 29-33 copied where the block
    # ran to 35, and the new block had no total; the next run copied 29-35 and passed.
    if not spec.get("partial_block") and b + 1 <= ws.max_row:
        after = [c for c in ws[b + 1] if c.value not in (None, "")]
        figures = [c for c in after if not isinstance(c.value, str) or c.value.startswith("=")]
        if len(after) >= 2 or figures:
            shown = ", ".join(f"{c.coordinate}={_short_value(c.value)}" for c in after[:4])
            raise OfficeError(f"copy_rows: the block seems to go on past row {b} - row {b + 1} holds {shown} - nothing "
                              f"was written: copy the whole block (its total rows too), or say \"partial_block\": true "
                              "if that row is not part of it")

    name = sheet_files[sheet]
    xml = entries[name].decode("utf-8")
    changed, outside, cleared = [], [], []
    for r in range(a, b + 1):
        row_m = re.search(_ROW_RE.format(n=r), xml, re.S)
        row_xml = row_m.group(0) if row_m else ""
        # A dated row is an entry of its period (a bill's line: its date, party, number), not the layout: none of what
        # was typed in it is copied - its formulas are.
        entry = not keep and any(isinstance(c.value, (datetime.date, datetime.datetime)) or
                                 (isinstance(c.value, str) and _cell_date(c.value) is not None) for c in ws[r])
        for c in ws[r]:
            if c.value is None:
                continue
            dest = f"{c.column_letter}{r + offset}"
            if entry and not (isinstance(c.value, str) and c.value.startswith("=")):
                cleared.append(dest)
                xml = _set_cell(xml, dest, None, style=_cell_style(row_xml, c.coordinate))
                changed.append(f"{sheet}!{dest}")
                continue
            found = re.search(_CELL_RE.format(ref=c.coordinate), row_xml, re.S) if row_xml else None
            own = re.search(r'\bs="(\d+)"', found.group(0)) if found else None
            value = c.value
            if isinstance(value, str) and value.startswith("="):
                value = Translator(value, origin=c.coordinate).translate_formula(dest)
                if "!" in value:
                    outside.append(f"{dest} {value}")
            elif isinstance(value, str) and not keep and _PERIOD_FACT.search(value):
                # A label that carries a date or a reference ("filed on 11/09/2026. Ref AB29082600...") is a fact of
                # the period it was written for: copied with the period's name replaced it would state the old date
                # and reference as the new period's. Left to fill, like a typed figure.
                cleared.append(dest)
                value = None
            elif isinstance(value, str):
                for old, new in replace.items():
                    value = value.replace(str(old), str(new))
            elif isinstance(value, bool):
                pass
            elif isinstance(value, (int, float)):
                if not keep:
                    cleared.append(dest)
                    value = None
            elif isinstance(value, (datetime.date, datetime.time)):
                cleared.append(dest)  # (a date is the period's own: typed, never copied)
                value = None
            else:
                value = str(value)
            xml = _set_cell(xml, dest, value, style=own.group(1) if own else None)
            changed.append(f"{sheet}!{dest}")
    entries[name] = _fix_dimension(xml).encode("utf-8")
    return (f"copied rows {a}-{b} of {sheet} to rows {to}-{end} (formulas moved {offset:+d} rows, as a copy and "
            "paste)", changed, outside, cleared)


def _layout_note(path, changed):
    """
    What the blocks this write changed do differently from the sheet's other blocks (block_layout_breaks), told to
    the worker as it writes - facts to check, never a refusal. Found live (2026-10-23): a new period's
    block went in with no header row; the layout check saw it only as evidence for the final judge, after the work.
    """

    rows = {}
    for ref in changed:
        m = re.match(r"^'?(.+?)'?!\$?[A-Za-z]{1,3}\$?(\d+)$", str(ref))
        if m:
            rows.setdefault(m.group(1), set()).add(int(m.group(2)))
    if not rows:
        return ""
    try:
        breaks = block_layout_breaks(path, rows=rows)
    except Exception as exc:  # noqa: BLE001 - a note, never a reason the write fails
        print(f"office: could not compare the changed blocks with the others: {exc}", file=sys.stderr)
        return ""
    if not breaks:
        return ""
    return ("\nCompared with the sheet's other blocks (check it, and change it if the layout should match):\n- "
            + "\n- ".join(breaks[:12]))


def _keep_original(src, suffix=None):
    """Keep `src` as it was, beside it as <name>.orig<ext>, the first time it is changed in place - unless the task
    made it."""

    backup = src.with_name(src.stem + ".orig" + (suffix or src.suffix))
    if not backup.exists() and not _task_made(src):
        from decurix_agent import shared
        shared.copy_file(src, backup)


def _zip_parts(p):
    with zipfile.ZipFile(p) as z:
        return {i.filename: z.read(i.filename) for i in z.infolist()}, [i.filename for i in z.infolist()]


def _write_zip(p, order, entries):
    tmp = p.with_name(p.name + ".tmp")
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as z:
        for name in order:
            z.writestr(name, entries[name])
    os.replace(tmp, p)


def _other_sheet_quoting(ref):
    """Data!$B$2:$B$3 <-> 'Data'!$B$2:$B$3 (the sheet name quoted or not)."""

    sheet, bang, rest = ref.partition("!")
    if not bang:
        return ref
    if sheet.startswith("'") and sheet.endswith("'") and len(sheet) > 1:
        return sheet[1:-1] + "!" + rest
    return f"'{sheet}'!" + rest


# ------------------------------------------------------------------
# Workbooks: putting rows in, references following them (as a spreadsheet app's "Insert rows")
#
# Found live (2026-10-22): with no way to put rows into a workbook, the worker used openpyxl's
# insert_rows, which moves cells but not the references that point at them - the month above the new rows went on
# reading rows the new month had taken, and its closing credit came out wrong. Here every reference to a row at or below
# the insertion moves with it: formulas on every sheet, conditional formats and validations, merged cells, defined
# names, chart ranges, tables, pivot sources, comments and pictures anchored there.
# ------------------------------------------------------------------

MAX_SHEET_ROWS = 1048576
MAX_INSERTED_ROWS = 10000

# A cell or range reference of a formula, or a whole-row range ("5:10"), with the sheet it names, if any.
_REF_TOKEN = re.compile(
    r"(?<![\w.$'!:])"
    r"(?P<sheet>(?:'(?:[^']|'')+'|[A-Za-z_À-￿][\w.À-￿]*)!)?"
    r"(?:(?P<c1>\$?[A-Z]{1,3})(?P<r1>\$?\d{1,7})(?::(?P<c2>\$?[A-Z]{1,3})(?P<r2>\$?\d{1,7}))?"
    r"|(?P<w1>\$?\d{1,7}):(?P<w2>\$?\d{1,7}))"
    r"(?![\w(!])")
# Attributes that hold references to the sheet they are on.
_REF_ATTR = re.compile(r'(\s(?:ref|sqref|topLeftCell|activeCell)=")([^"]*)(")')
# A formula element of a sheet, a conditional format or a validation: <f ...>...</f>, <formula>, <formula1/2>.
_FORMULA_EL = re.compile(r"(<(f|formula[12]?)\b[^>]*>)(.*?)(</\2>)", re.S)


def _unquote_sheet(name):
    return name[1:-1].replace("''", "'") if len(name) > 1 and name.startswith("'") and name.endswith("'") else name


def _shift_refs(text, sheet, own, before, count):
    """(text, how many references moved): each reference in `text` to a row of `sheet` at or below `before` moved down
    `count` rows. `own`: `text` is on that sheet, so a reference with no sheet name is to it. Text in quotes is left."""

    moved = 0

    def row(value):
        nonlocal moved
        n = int(value.lstrip("$"))
        if n >= before:
            n += count
            moved += 1
        return ("$" if value.startswith("$") else "") + str(n)

    def fix(m):
        named = m.group("sheet")
        target = _unquote_sheet(named[:-1]) if named else None
        if not ((target is None and own) or (target is not None and target.casefold() == sheet.casefold())):
            return m.group(0)
        if m.group("w1"):
            return (named or "") + row(m.group("w1")) + ":" + row(m.group("w2"))
        out = (named or "") + m.group("c1") + row(m.group("r1"))
        if m.group("c2"):
            out += ":" + m.group("c2") + row(m.group("r2"))
        return out

    parts = re.split(r'("(?:[^"]|"")*")', text)
    shifted = "".join(part if i % 2 else _REF_TOKEN.sub(fix, part) for i, part in enumerate(parts))
    return shifted, moved


def _shift_formulas(xml, sheet, own, before, count):
    """(xml, references moved) for every formula element of a part's XML (escaped as XML text)."""

    total = 0

    def fix(m):
        nonlocal total
        text, moved = _shift_refs(_unescape(m.group(3)), sheet, own, before, count)
        total += moved
        return m.group(1) + _escape(text) + m.group(4)

    return _FORMULA_EL.sub(fix, xml), total


def _shift_ref_attrs(xml, sheet, before, count):
    """(xml, references moved) for the ref/sqref/topLeftCell/activeCell attributes of a part on `sheet`."""

    total = 0

    def fix(m):
        nonlocal total
        text, moved = _shift_refs(m.group(2), sheet, True, before, count)
        total += moved
        return m.group(1) + text + m.group(3)

    return _REF_ATTR.sub(fix, xml), total


def _expand_shared(xml):
    """A sheet's shared formulas written out cell by cell (each follower's own formula, as the app works it out from the
    first): a row put in among them would otherwise change what every follower reads."""

    if 't="shared"' not in xml:
        return xml
    from openpyxl.formula.translate import Translator
    cell_re = re.compile(r'<c\b[^>]*?\br="([A-Z]+\d+)"[^>]*?(?:/>|>.*?</c>)', re.S)
    f_re = re.compile(r'<f\b([^>]*)(?:/>|>(.*?)</f>)', re.S)
    masters = {}
    for cell in cell_re.finditer(xml):
        f = f_re.search(cell.group(0))
        if f and 't="shared"' in f.group(1) and f.group(2):
            si = re.search(r'\bsi="(\d+)"', f.group(1))
            if si:
                masters[si.group(1)] = (cell.group(1), _unescape(f.group(2)))

    def fix(cell):
        body = cell.group(0)
        f = f_re.search(body)
        if not f or 't="shared"' not in f.group(1):
            return body
        si = re.search(r'\bsi="(\d+)"', f.group(1))
        if not si or si.group(1) not in masters:
            raise OfficeError(f"{cell.group(1)} holds a shared formula whose first cell cannot be found - nothing was written")
        origin, text = masters[si.group(1)]
        own = text if origin == cell.group(1) else Translator("=" + text, origin=origin).translate_formula(cell.group(1))[1:]
        return body[:f.start()] + f"<f>{_escape(own)}</f>" + body[f.end():]

    return cell_re.sub(fix, xml)


def _move_rows(xml, before, count):
    """A sheet's rows at or below `before` (and their cells) numbered `count` lower."""

    start, end = xml.find("<sheetData"), xml.find("</sheetData>")
    if start < 0 or end < 0:
        return xml

    def row(m):
        n = int(m.group(2))
        return m.group(1) + str(n + count if n >= before else n) + m.group(3)

    def cell(m):
        n = int(m.group(3))
        return m.group(1) + m.group(2) + str(n + count if n >= before else n) + m.group(4)

    body = xml[start:end]
    body = re.sub(r'(<row\b[^>]*?\br=")(\d+)(")', row, body)
    body = re.sub(r'(<c\b[^>]*?\br=")([A-Z]+)(\d+)(")', cell, body)
    return xml[:start] + body + xml[end:]


def _sheet_related(entries, part):
    """The parts a sheet's relationships point at (tables, comments, drawings, legacy drawings), by kind."""

    import posixpath
    rels = posixpath.join(posixpath.dirname(part), "_rels", posixpath.basename(part) + ".rels")
    out = {}
    if rels not in entries:
        return out
    for rel in re.findall(r"<Relationship\b[^>]*/>", entries[rels].decode("utf-8")):
        target = re.search(r'\bTarget="([^"]+)"', rel)
        kind = re.search(r'\bType="[^"]*/([^"/]+)"', rel)
        if not target or not kind or 'TargetMode="External"' in rel:
            continue
        path = posixpath.normpath(posixpath.join(posixpath.dirname(part), target.group(1)))
        if path in entries:
            out.setdefault(kind.group(1), []).append(path)
    return out


def _insert_rows(entries, sheet_files, spec):
    """Put `count` empty rows into a sheet before row `before`; returns what was done, as words."""

    if not isinstance(spec, dict) or set(spec) - {"sheet", "before", "count"}:
        raise OfficeError('each of insert_rows is {"sheet": "Data", "before": 54, "count": 12} - nothing was written')
    sheet = spec.get("sheet") or next(iter(sheet_files))
    if sheet not in sheet_files:
        raise OfficeError(f"no sheet {sheet!r}; the sheets are {list(sheet_files)} - nothing was written")
    try:
        before, count = int(spec.get("before")), int(spec["count"] if "count" in spec else 1)
    except (TypeError, ValueError):
        raise OfficeError("insert_rows needs \"before\" (the row number the new rows go above) and \"count\" - nothing was written")
    if before < 1 or not 1 <= count <= MAX_INSERTED_ROWS:
        raise OfficeError(f"insert_rows: before must be a row number and count 1 to {MAX_INSERTED_ROWS} - nothing was written")

    part = sheet_files[sheet]
    xml = _expand_shared(entries[part].decode("utf-8"))
    for f in re.finditer(r'<f\b[^>]*\bt="array"[^>]*\bref="[A-Z]+(\d+)(?::[A-Z]+(\d+))?"', xml):
        first, last = int(f.group(1)), int(f.group(2) or f.group(1))
        if first < before <= last:
            raise OfficeError(f"{sheet} has an array formula over rows {first}-{last}; rows cannot go in the middle of it "
                              "- nothing was written")
    last_row = max([int(n) for n in re.findall(r'<row\b[^>]*?\br="(\d+)"', xml)] + [0])
    if last_row >= before and last_row + count > MAX_SHEET_ROWS:
        raise OfficeError(f"{sheet} would run past row {MAX_SHEET_ROWS} - nothing was written")

    moved = 0
    xml, n = _shift_formulas(xml, sheet, True, before, count)
    moved += n
    xml, n = _shift_ref_attrs(xml, sheet, before, count)
    moved += n
    xml = re.sub(r'(<brk\b[^>]*?\bid=")(\d+)(")',
                 lambda m: m.group(1) + str(int(m.group(2)) + count if int(m.group(2)) >= before else int(m.group(2))) + m.group(3),
                 xml) if "<rowBreaks" in xml else xml
    entries[part] = _move_rows(xml, before, count).encode("utf-8")

    for other, other_part in sheet_files.items():
        if other_part == part:
            continue
        text = entries[other_part].decode("utf-8")
        if "!" not in text:
            continue
        text = _expand_shared(text)
        text, n = _shift_formulas(text, sheet, False, before, count)
        if n:
            moved += n
            entries[other_part] = text.encode("utf-8")

    workbook = entries["xl/workbook.xml"].decode("utf-8")

    def name(m):
        nonlocal moved
        text, n = _shift_refs(_unescape(m.group(2)), sheet, False, before, count)
        moved += n
        return m.group(1) + _escape(text) + m.group(3)
    entries["xl/workbook.xml"] = re.sub(r"(<definedName\b[^>]*>)(.*?)(</definedName>)", name, workbook, flags=re.S).encode("utf-8")

    for path in list(entries):
        if re.match(r"xl/charts/chart\w*\.xml$", path):
            text = entries[path].decode("utf-8")

            def chart_ref(m):
                nonlocal moved
                shifted, n = _shift_refs(_unescape(m.group(2)), sheet, False, before, count)
                moved += n
                return m.group(1) + _escape(shifted) + m.group(3)
            entries[path] = re.sub(r"(<(?:\w+:)?f>)(.*?)(</(?:\w+:)?f>)", chart_ref, text, flags=re.S).encode("utf-8")
        elif re.match(r"xl/pivotCache/pivotCacheDefinition\d+\.xml$", path):
            text = entries[path].decode("utf-8")

            def source(m):
                nonlocal moved
                tag = m.group(0)
                where = re.search(r'\bsheet="([^"]*)"', tag)
                if not where or _unescape(where.group(1)).casefold() != sheet.casefold():
                    return tag
                shifted, n = _shift_ref_attrs(tag, sheet, before, count)
                moved += n
                return shifted
            entries[path] = re.sub(r"<worksheetSource\b[^>]*/>", source, text).encode("utf-8")

    related = _sheet_related(entries, part)
    for path in related.get("table", []) + related.get("comments", []):
        text, n = _shift_ref_attrs(entries[path].decode("utf-8"), sheet, before, count)
        moved += n
        entries[path] = text.encode("utf-8")
    for path in related.get("drawing", []):
        # Anchors count rows from 0: row index before-1 is the first row that moves.
        text = re.sub(r"(<xdr:row>)(\d+)(</xdr:row>)",
                      lambda m: m.group(1) + str(int(m.group(2)) + count if int(m.group(2)) >= before - 1 else int(m.group(2)))
                      + m.group(3), entries[path].decode("utf-8"))
        entries[path] = text.encode("utf-8")
    for path in related.get("vmlDrawing", []):
        text = entries[path].decode("utf-8", "replace")
        text = re.sub(r"(<x:Row>)(\d+)(</x:Row>)",
                      lambda m: m.group(1) + str(int(m.group(2)) + count if int(m.group(2)) >= before - 1 else int(m.group(2)))
                      + m.group(3), text)

        def anchor(m):
            values = [v.strip() for v in m.group(2).split(",")]
            if len(values) == 8:
                for i in (2, 6):
                    if values[i].isdigit() and int(values[i]) >= before - 1:
                        values[i] = str(int(values[i]) + count)
            return m.group(1) + ", ".join(values) + m.group(3)
        entries[path] = re.sub(r"(<x:Anchor>)(.*?)(</x:Anchor>)", anchor, text, flags=re.S).encode("utf-8")

    return f"put {count} empty row(s) into {sheet} above row {before} ({moved} reference(s) moved with them)"


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


def _set_cell(xml, ref, value, style=None):
    """Write one cell into a sheet's XML; style: the format index to give it (copy_rows), else the cell's own or the
    one above it."""
    col, row = _split(ref)

    if "<sheetData/>" in xml:
        xml = xml.replace("<sheetData/>", "<sheetData></sheetData>")

    m = re.search(_ROW_RE.format(n=row), xml, re.S)
    if not m:
        new_row = f'<row r="{row}">{_new_cell(ref, value, style)}</row>'
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
        own = re.search(r'\bs="(\d+)"', old)
        row_xml = row_xml.replace(old, _new_cell(ref, value, style if style is not None else
                                                 (own.group(1) if own else None)), 1)
    else:
        # Keep the cells of a row in column order; a new one takes the style
        # of the cell above it when there is one.
        cells = list(re.finditer(r'<c\b[^>]*?\br="([A-Z]+)\d+"[^>]*?(?:/>|>.*?</c>)', row_xml, re.S))
        new = _new_cell(ref, value, style if style is not None else _style_above(xml, col, row))
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
    onboarding file assembled from its id card scan, registration certificate, ...
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


def _formula_books(path, with_values=True):
    """A workbook's formulas and, `with_values`, its stored results (else None), loaded with openpyxl; None for a file
    that is not such a workbook or cannot be read."""

    if Path(path).suffix.lower() not in RESULT_BOOKS:
        return None
    openpyxl = _openpyxl()
    try:
        formulas = openpyxl.load_workbook(str(path))
        return formulas, (openpyxl.load_workbook(str(path), data_only=True) if with_values else None)
    except Exception:
        return None


# A row's label that says it totals what is above it: "Total", "Grand Total", "Sub-total", "Totals".
TOTAL_LABEL = re.compile(r"^\s*(?:grand\s+|sub[\s-]*)?totals?\b", re.I)


def typed_totals(path):
    """
    The cells of a row labelled Total that hold a typed number equal to the sum of the column above them, as
    "Sheet!E14 (2603800 = the sum of E2:E13)" - a total that will not follow its rows when one changes. Read with no
    model; needs the file's formula results (store them first). Empty for anything else.
    Found live (2026-10-19): a tax register asked for "with formulas" had every total of its Total row typed in.
    """

    books = _formula_books(path)
    if books is None:
        return []
    formulas, values = books

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

    books = _formula_books(path)
    if books is None:
        return []
    formulas, values = books
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

    books = _formula_books(path)
    if books is None:
        return []
    formulas, values = books
    out = []
    for ws, grid in _table_grids(formulas):
        shown = values[ws.title]
        total_rows = _total_rows(grid)
        out.extend(_column_gaps(ws.title, grid, shown, total_rows, ws.max_row, ws.max_column))
        out.extend(_total_gaps(ws.title, grid, total_rows))
    return out


# A single cell a formula reads: an optional sheet ('My sheet'! or Sheet1!), a column and a row - not one end of a range
# (A1:B9), not a function's name (LOG10(), and not inside a quoted string (removed first).
_FORMULA_REF = re.compile(r"(?<![\w:$.])(?:(?:'((?:[^']|'')+)'|([A-Za-z_][\w.]*))!)?\$?([A-Z]{1,3})\$?(\d{1,7})(?![\w(:!])")
_FORMULA_STRING = re.compile(r'"(?:[^"]|"")*"')
PATTERN_MIN_BLOCKS = 3


def _row_label(grid, row):
    """The first text of a row (its label: "Closing credit carried forward", "TOTAL"), lower case, or ""."""

    for column in range(1, 5):
        value = grid.get((row, column))
        if isinstance(value, str) and value.strip() and not value.startswith("="):
            return " ".join(value.lower().split())[:80]
    return ""


def reference_pattern_breaks(path):
    """
    Rows of a sheet laid out as repeated blocks (a month per block) whose formula reads a differently labelled row than
    the same row of the other blocks does, as sentences. Found live (2026-10-21): a September block copied
    from August's took its "Opening credit balance (previous month's closing)" from August's "Output tax paid from credit"
    - one row off - while the five earlier blocks took it from "Closing credit carried forward"; the figures looked
    plausible and the check passed them. Decided from labels and references only (no subject knowledge); evidence for
    the judge, never a verdict: a block can be meant to work differently. Only single-cell references are compared (a
    range's coverage is coverage_gaps'), and only rows that appear in at least PATTERN_MIN_BLOCKS blocks.
    """

    books = _formula_books(path, with_values=False)
    if books is None:
        return []
    grids = {ws.title: grid for ws, grid in _table_grids(books[0])}
    out = []
    for sheet, grid in grids.items():
        groups = {}
        for (row, column), value in grid.items():
            if not (isinstance(value, str) and value.startswith("=")):
                continue
            label = _row_label(grid, row)
            if not label:
                continue
            reads = []
            for m in _FORMULA_REF.finditer(_FORMULA_STRING.sub('""', value)):
                other = (m.group(1) or m.group(2) or "").replace("''", "'") or sheet
                target_row = int(m.group(4))
                if other == sheet and target_row == row:
                    reads.append((other, m.group(3), "(its own row)"))
                    continue
                target = grids.get(other)
                reads.append((other, m.group(3), _row_label(target, target_row) if target is not None else "?"))
            if reads:
                groups.setdefault((column, label), []).append((row, value, tuple(reads)))
        for (column, label), cells in groups.items():
            if len(cells) < PATTERN_MIN_BLOCKS:
                continue
            counts = {}
            for _, _, reads in cells:
                counts[reads] = counts.get(reads, 0) + 1
            usual, seen = max(counts.items(), key=lambda kv: kv[1])
            if seen < 2 or seen * 5 < len(cells) * 3:  # no pattern at least 60% of the blocks follow
                continue
            for row, value, reads in cells:
                if reads == usual:
                    continue
                differs = [(got, want) for got, want in zip(reads, usual) if got != want] or [(reads[0], usual[0])]
                got, want = differs[0]
                cell = f"{sheet}!{_col_letter(column)}{row}"
                where = f" of {got[0]}" if got[0] != sheet else ""
                out.append(f"{cell} (row \"{label}\") is {value}: it reads the row \"{got[2]}\"{where}, but the same row "
                           f"in {seen} other blocks reads the row \"{want[2]}\" - if this block was copied from another, "
                           "a reference may be a row off.")
    return out[:12]


# Any cell reference of a formula, range ends included: an optional sheet, the column, "$" before the row, the row.
_ANY_REF = re.compile(r"(?<![\w.$])(?:('(?:[^']|'')+'|[A-Za-z_][\w.]*)!)?\$?[A-Z]{1,3}(\$?)(\d{1,7})(?![\w(!])")


def _row_refs(formula):
    """[(sheet or None, row, row is absolute)] for every cell reference of a formula; the end of a range "Sheet!A1:B2"
    belongs to the start's sheet. Text in quotes is not a reference."""

    text = _FORMULA_STRING.sub('""', formula)
    out, last_sheet, last_end = [], None, -1
    for m in _ANY_REF.finditer(text):
        sheet = m.group(1)
        if sheet:
            sheet = sheet.strip("'").replace("''", "'")
        elif m.start() == last_end + 1 and text[last_end:m.start()] == ":":
            sheet = last_sheet
        out.append((sheet, int(m.group(3)), bool(m.group(2))))
        last_sheet, last_end = sheet, m.end()
    return out


def moved_formula_breaks(before, after):
    """
    Formulas of a workbook (`after`) that changed cells this task and are, word for word, a formula that sat elsewhere
    in the same column before it (`before`, the copy kept when the task began), and that still read rows of the stretch
    they moved with - as sentences. Rows put in (or taken out) by a tool that moves cells but not the references that
    point at them leave exactly this: found live (2026-10-22), openpyxl's insert_rows put a September
    block above August's, and August's block, now 17 rows lower, went on reading the rows September had taken, its
    opening and closing credit with them. A real insert, and a copy-and-paste, move such a reference; a formula that
    only reads rows above the change rightly keeps its words, and an absolute row ($72) is left out (a copy keeps it).
    Decided from the files alone; evidence for the judge.
    """

    old_books, new_books = _formula_books(before, with_values=False), _formula_books(after, with_values=False)
    if old_books is None or new_books is None:
        return []
    old_wb, new_wb = old_books[0], new_books[0]
    out = []
    olds, moved = {}, {}  # per sheet: its formulas before; where rows moved ({sheet: (first moved row, shift)})
    for ws in new_wb.worksheets:
        if ws.title not in old_wb.sheetnames:
            continue
        old = olds[ws.title] = {(c.row, c.column): c.value for row in old_wb[ws.title].iter_rows() for c in row
                                if isinstance(c.value, str) and c.value.startswith("=")}
        rows_of = {}
        for (r, column), formula in old.items():
            rows_of.setdefault((column, formula), []).append(r)
        by_shift = {}
        for row in ws.iter_rows():
            for c in row:
                formula = c.value
                if not (isinstance(formula, str) and formula.startswith("=")) or old.get((c.row, c.column)) == formula:
                    continue
                was = rows_of.get((c.column, formula)) or []
                if len(was) == 1 and was[0] != c.row:
                    by_shift.setdefault(c.row - was[0], []).append((c.row, c.column, was[0], formula))
        for shift, cells in sorted(by_shift.items()):
            start = min(was for _, _, was, _ in cells)  # the moved stretch begins at or above its first formula
            if ws.title not in moved or start < moved[ws.title][0]:
                moved[ws.title] = (start, shift)
            stale = [cell for cell in cells
                     if any(sheet in (None, ws.title) and not absolute and r >= start for sheet, r, absolute in _row_refs(cell[3]))]
            if not stale:
                continue
            r, column, was, formula = stale[0]
            letter = _col_letter(column)
            out.append(f"{ws.title}: {len(stale)} formula(s) are {abs(shift)} rows {'lower' if shift > 0 else 'higher'} than "
                       "before this task and written word for word as they were there, so they still read the rows they "
                       f"read there - e.g. {letter}{r} {formula[:80]} was at {letter}{was}. Rows were added or removed "
                       "without moving the references that point at them: the figures of that part of the sheet now come "
                       "from the wrong rows.")
    # A formula the task did not touch (same cell, same words, above any stretch of its own sheet that moved) that reads
    # rows of a sheet at or below where its rows moved: found live (a second run), 12 rows put into SALES
    # at row 54 left COMPUTATION's untouched "=SALES!I54" - August's sales total - reading a September row.
    for ws in new_wb.worksheets:
        old = olds.get(ws.title)
        if old is None:
            continue
        own = moved.get(ws.title, (None, 0))[0]
        stale = []
        for row in ws.iter_rows():
            for c in row:
                formula = c.value
                if (not isinstance(formula, str) or old.get((c.row, c.column)) != formula
                        or (own is not None and c.row >= own)):
                    continue
                for sheet, r, _ in _row_refs(formula):
                    target = sheet or ws.title
                    if target in moved and r >= moved[target][0]:
                        stale.append((c.row, c.column, formula, target))
                        break
        if stale:
            r, column, formula, target = stale[0]
            start, shift = moved[target]
            out.append(f"{ws.title}: {len(stale)} formula(s) this task did not change read rows of {target} from row "
                       f"{start} down, where its rows moved {abs(shift)} {'down' if shift > 0 else 'up'} - e.g. "
                       f"{_col_letter(column)}{r} {formula[:80]} now reads what moved into that row. Rows were added "
                       "or removed without moving the references that point at them.")
    return out[:12]


def _table_grids(formulas):
    """(sheet, {(row, column): value of each filled cell}) of a workbook's sheets small enough to be a table."""

    for ws in formulas.worksheets:
        if ws.max_row > _COVERAGE_MAX_ROWS or ws.max_column > _COVERAGE_MAX_COLUMNS:
            continue
        yield ws, {(c.row, c.column): c.value for row in ws.iter_rows() for c in row if _filled(c.value)}


def _total_rows(grid):
    return {r for (r, k), v in grid.items() if isinstance(v, str) and TOTAL_LABEL.match(v)}


def _extent(lo, hi, last_row, beside, total_rows):
    """lo..hi widened over the rows around it that `beside` says belong to the table (never over a Total row)."""

    while lo > 1 and beside(lo - 1) and (lo - 1) not in total_rows:
        lo -= 1
    while hi < last_row and beside(hi + 1) and (hi + 1) not in total_rows:
        hi += 1
    return lo, hi


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

        start, end = _extent(lo, hi, max_row, has_data, total_rows)

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

    books = _formula_books(path, with_values=False)
    if books is None:
        return []
    formulas, _ = books
    out = []
    for ws, grid in _table_grids(formulas):
        total_rows = _total_rows(grid)
        for c in range(1, ws.max_column + 1):
            fill = [r for r in range(1, ws.max_row + 1)
                    if r not in total_rows and _is_formula(grid.get((r, c)))
                    and "R[0]" in formula_shape(grid[(r, c)], r, c)]
            if not fill:
                continue

            def beside(r):
                return any(_filled(grid.get((r, k))) for k in range(1, ws.max_column + 1) if k != c)

            lo, hi = min(fill), max(fill)
            start, end = _extent(lo, hi, ws.max_row, beside, total_rows)
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

    books = _formula_books(path, with_values=False)
    if books is None:
        return []
    formulas, _ = books
    out = []
    for ws, grid in _table_grids(formulas):
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

    books = _formula_books(path)
    if books is None:
        return ""
    formulas, values = books
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


def _result_xml(attrs, inner, value):
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

    entries, order = _zip_parts(p)
    for sheet, name in _sheet_files(entries).items():
        if sheet not in values.sheetnames or name not in entries:
            continue
        ws = values[sheet]

        def fill(m):
            attrs, inner = m.group(1), m.group(2)
            ref = re.search(r'\br="([A-Z]{1,3}\d+)"', attrs)
            if inner is None or "<f" not in inner or not ref:
                return m.group(0)
            return _result_xml(attrs, inner, ws[ref.group(1)].value)

        entries[name] = _CELL_XML.sub(fill, entries[name].decode("utf-8")).encode("utf-8")

    _write_zip(p, order, entries)
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


def _ocr_file(path):
    """Text tesseract reads off one picture, "" when it cannot run."""

    try:
        done = subprocess.run(["tesseract", str(path), "stdout", "--psm", "3"],
                              capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError):
        return ""
    text = (done.stdout or "").strip()
    return text[:MAX_TEXT_CHARS]


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


def _pick(found, which, kind, n, number):
    if not isinstance(which, int) or not 1 <= which <= len(found):
        raise OfficeError(f"edit {n}: slide {number} has {len(found)} {kind}(s), not {kind} {which!r}")
    return found[which - 1]


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
            which, row, col = edit.get("table", 1), edit.get("row"), edit.get("column")
            table = _pick([s.table for s in slide.shapes if getattr(s, "has_table", False) and s.has_table], which,
                          "table", n, number)
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
            which, rows = edit.get("table", 1), edit["add_rows"]
            frame = _pick([s for s in slide.shapes if getattr(s, "has_table", False) and s.has_table], which, "table",
                          n, number)
            table = frame.table
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
                frame.height += model.h
            done.append(f"slide {number} table {which}: {len(rows)} row(s) added" + (f" above row {before}" if before else ""))
        elif action == "categories":
            which = edit.get("chart", 1)
            _pick([s.chart for s in slide.shapes if getattr(s, "has_chart", False) and s.has_chart], which, "chart", n,
                  number).replace_data(_deck_chart_data(edit, f"edit {n}"))
            done.append(f"slide {number} chart {which} data")
        else:
            slide.notes_slide.notes_text_frame.text = str(edit["notes"])
            done.append(f"slide {number} notes")

    if dst == src:
        _keep_original(src)
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
    for key, value in replace.items():
        if not key:
            continue
        # Left to right, never into what was just put in: a value that holds its key ("Q3" -> "Q3 FY26") is
        # replaced once, not again and again until memory runs out.
        at_from = 0
        while True:
            whole = "".join(t.text or "" for t in texts)
            i = whole.find(key, at_from)
            if i < 0:
                break
            at_from = i + len(str(value))
            end = i + len(key)
            # Where each w:t starts in the paragraph's text.
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
    if out == p:
        _keep_original(p, ".docx")
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
                                         "insert_rows": [{"sheet": "Data", "before": 54, "count": 12}],
                                         "copy_rows": [{"sheet": "Calc", "rows": "69:84", "to": 86,
                                                        "replace": {"AUGUST": "SEPTEMBER"}}],
                                         "output_path": "optional.xlsx"}
                                        a value is a number, text, null (empties the cell) or "=FORMULA";
                                        replace_refs extends what charts, tables and names read;
                                        insert_rows puts empty rows in above a row, every formula, name, chart
                                        and format that reads the rows below moving with them (a script's
                                        insert_rows does not: formulas below then read the wrong rows) - done
                                        first, so edits address the rows as they are after it.
                                        copy_rows makes the next period's block of a register from the last
                                        one, as a copy and paste: formulas move with it (its opening then reads
                                        the block above), labels take the replacements, formats come along, and
                                        typed figures are left empty - fill them with edits in the same update.
                                        It names every copied formula that reads another sheet: check those.
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
    typed = list(args.to) + list(args.cc)
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


READ_NOTE_CHARS = 1200


def _note_read(path, text):
    """
    What `office read` read, kept with the task's evidence for the final check beside what was read off the screen.
    Found live (2026-10-21): asked whether a portal's credit ledger agreed with a workbook's closing
    credit, the check saw the portal on the screen and nothing of the workbook the worker had read, and asked the person
    three times. Never raises.
    """

    try:
        from decurix_agent.computer import evidence
        text = (text or "").strip()
        if text:
            cut = text[:READ_NOTE_CHARS] + (" ..." if len(text) > READ_NOTE_CHARS else "")
            evidence.note(f"{evidence.FILE_READ_LEAD}{path}: {cut}")
            evidence.note_file_read(str(_path(path)))
    except Exception:  # noqa: BLE001 - evidence that cannot be kept decides nothing
        pass


def main(argv=None):
    """The `office` command. Its output is what an engine reads as the command's result, so the agent's own log lines
    (the helpers it calls log as they go) go to a file meanwhile, as the gui command's do. Found live
    (2026-10-21): "tool_made: could not release ..." was the first line of a failed `office update`'s output, ahead of
    the real error; the engine took the person's workbook for locked and worked in a copy instead."""

    from decurix_agent import logging_utils

    before = logging_utils._log_path
    logging_utils.log_to_file(logging_utils.log_file("decurix-office.log"))
    try:
        return _main(argv)
    finally:
        logging_utils._log_path = before


# The parts of a picture a reader can ask for by name, as (x0, y0, x1, y1) in percent.
_PICTURE_PARTS = {
    "top": (0, 0, 100, 50), "bottom": (0, 50, 100, 100), "left": (0, 0, 50, 100), "right": (50, 0, 100, 100),
    "top-left": (0, 0, 55, 55), "top-right": (45, 0, 100, 55), "bottom-left": (0, 45, 55, 100),
    "bottom-right": (45, 45, 100, 100), "center": (20, 20, 80, 80), "middle": (20, 20, 80, 80),
}


def _zoom_picture(p, part):
    """
    A part of a picture, cropped and enlarged to be read closer: {"text", "images"} like read_document. Found live
    (2026-10-27): a photographed bill's date could not be read at the size the whole photo is shown; the worker had no
    way to look closer and asked the person, who did not have the bill either.
    """

    spec = str(part).strip().lower()
    box = _PICTURE_PARTS.get(spec)
    if box is None:
        try:
            box = tuple(float(v) for v in spec.replace("%", "").split(","))
        except ValueError:
            box = ()
        if len(box) != 4 or not (0 <= box[0] < box[2] <= 100 and 0 <= box[1] < box[3] <= 100):
            raise OfficeError(f"a part of a picture is one of {', '.join(_PICTURE_PARTS)} or x0,y0,x1,y1 in percent "
                              f"(0-100, x0 < x1, y0 < y1) - not {part!r}")
    try:
        from PIL import Image
    except ImportError as exc:  # pragma: no cover - the desktop image has Pillow
        raise OfficeError(f"cannot crop a picture here: {exc}")
    with Image.open(p) as im:
        im = im.convert("RGB")
        w, h = im.size
        x0, y0, x1, y1 = (int(round(box[0] * w / 100)), int(round(box[1] * h / 100)),
                          int(round(box[2] * w / 100)), int(round(box[3] * h / 100)))
        piece = im.crop((x0, y0, max(x1, x0 + 1), max(y1, y0 + 1)))
        # Enlarged to at least 1600 pixels on its long side (at most 4x): what the model is shown is scaled down to
        # its own limit, and a small part sent small stays unreadable.
        scale = min(4.0, max(1.0, 1600 / max(piece.size)))
        if scale > 1:
            piece = piece.resize((int(piece.size[0] * scale), int(piece.size[1] * scale)), Image.LANCZOS)
        out_dir = Path(tempfile.gettempdir()) / "decurix-zoom"
        out_dir.mkdir(parents=True, exist_ok=True)
        tag = spec.replace(",", "_").replace(".", "p")
        out = out_dir / f"{p.stem}-{tag}.png"
        piece.save(out, "PNG")
    return {"text": f"{p.name}, part {part} (x {box[0]:g}-{box[2]:g}%, y {box[1]:g}-{box[3]:g}% of the picture), "
                    f"enlarged {scale:.1f}x: shown below.", "images": [str(out)]}


def _main(argv=None):
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
            _note_read(argv[0], r["text"])
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
                _only(spec, {"edits", "replace_refs", "output_path", "add_sheets", "insert_rows", "copy_rows", "clear_text"})
                print(update_workbook(argv[0], spec.get("edits"), spec.get("replace_refs"), spec.get("output_path"),
                                      spec.get("add_sheets"), spec.get("insert_rows"), spec.get("copy_rows"),
                                      bool(spec.get("clear_text"))))
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
