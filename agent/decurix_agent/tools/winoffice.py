"""
office.py's helpers on a native Windows worker.

On a Linux desktop office.py leans on programs the desktop image installs:
poppler (pdftotext, pdftoppm), tesseract and LibreOffice. A native worker runs
on a person's own computer, where none of them is and nothing may be installed.
So the same jobs are done with what is there:

- a PDF's text and page count: pypdf, pure Python, shipped with the worker's
  Python (as openpyxl is, for workbooks);
- a PDF's pages as pictures, and reading text off a picture (OCR): Windows'
  own PDF renderer and OCR engine, in every Windows 10 and 11, reached through
  winrt.ps1 (Windows PowerShell can call them with no SDK);
- an Office document as a PDF: LibreOffice when the person has it, else
  Microsoft Office itself, started unseen; with neither there is nothing on the
  computer that can lay one out, and the error says so.

Everything here raises Unavailable with words a person can act on; office.py
turns it into its own OfficeError.
"""

import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

CREATE_NO_WINDOW = 0x08000000

SCRIPT = Path(__file__).with_name("winrt.ps1")

# pdftoppm's 110 dpi, which office.py's page pictures are made at on Linux.
PAGE_DPI = 110
OCR_PAGE_DPI = 200


class Unavailable(Exception):
    pass


def _powershell():
    root = os.environ.get("SystemRoot", r"C:\Windows")
    exe = Path(root) / "System32" / "WindowsPowerShell" / "v1.0" / "powershell.exe"
    return str(exe) if exe.is_file() else "powershell.exe"


def _run(op, *args, timeout=180):
    """One operation of winrt.ps1; its JSON answer, or Unavailable with its reason."""

    try:
        done = subprocess.run(
            [_powershell(), "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
             "-File", str(SCRIPT), op, *[str(a) for a in args]],
            capture_output=True, timeout=timeout, creationflags=CREATE_NO_WINDOW if os.name == "nt" else 0,
        )
    except subprocess.TimeoutExpired:
        raise Unavailable(f"it took longer than {timeout} seconds")
    except OSError as exc:
        raise Unavailable(f"Windows PowerShell could not be run: {exc}")

    answer = None
    for line in reversed(done.stdout.decode("utf-8", errors="replace").splitlines()):
        line = line.strip()
        if line.startswith("{"):
            try:
                answer = json.loads(line)
                break
            except ValueError:
                continue

    if answer is None:
        detail = done.stderr.decode("utf-8", errors="replace").strip().splitlines()
        raise Unavailable(detail[0][:200] if detail else "Windows gave no answer")
    if not answer.get("ok"):
        raise Unavailable(str(answer.get("error") or "Windows could not do it")[:300])

    return answer


def _pypdf():
    try:
        import pypdf
    except ImportError as exc:
        raise Unavailable("pypdf is not installed with this worker's Python") from exc
    return pypdf


# ------------------------------------------------------------------
# PDFs in
# ------------------------------------------------------------------

def pdf_page_count(path):
    pypdf = _pypdf()
    try:
        return len(pypdf.PdfReader(str(path)).pages)
    except Exception as exc:
        raise Unavailable(f"{Path(path).name} could not be opened as a PDF: {str(exc)[:200]}")


def pdf_text(path, first, last):
    """The text of pages first..last (from 1), laid out as on the page where pypdf can."""

    pypdf = _pypdf()
    try:
        reader = pypdf.PdfReader(str(path))
        if reader.is_encrypted:
            reader.decrypt("")
        parts = []
        for index in range(first - 1, min(last, len(reader.pages))):
            page = reader.pages[index]
            try:
                parts.append(page.extract_text(extraction_mode="layout") or "")
            except Exception:
                # The layout mode is the newer one; a page it cannot follow is still text.
                parts.append(page.extract_text() or "")
    except Unavailable:
        raise
    except Exception as exc:
        raise Unavailable(f"{Path(path).name} could not be read as a PDF: {str(exc)[:200]}")

    return "\f".join(parts)


def pdf_pages_as_png(path, first, last):
    """Pages first..last as PNG files in a new folder, at the size pdftoppm -r 110 makes them."""

    # A page that is going to be read by OCR (a brain that cannot see: DECURIX_OFFICE_IMAGES=ocr) is made larger.
    # Measured on a page holding a screenshot: at 110 dpi Windows read "Character Map" as "enaraaer Map" and lost
    # half the lines; at the size the picture was made, it read them all.
    dpi = OCR_PAGE_DPI if os.environ.get("DECURIX_OFFICE_IMAGES") == "ocr" else PAGE_DPI

    width = 910  # A4 upright at 110 dpi, when the page will not say
    try:
        box = _pypdf().PdfReader(str(path)).pages[first - 1].mediabox
        width = max(200, min(4000, int(float(box.width) / 72 * dpi)))
    except Exception:
        pass

    out_dir = Path(tempfile.mkdtemp(prefix="decurix-pages-"))
    try:
        answer = _run("pdfpages", path, first, last, out_dir, width)
    except Unavailable:
        return []

    return [str(f) for f in answer.get("files") or []]


# ------------------------------------------------------------------
# Reading text off a picture
# ------------------------------------------------------------------

def ocr_lines(path):
    """[{"text", "box": [x, y, w, h]}] for each line Windows reads off a picture; [] when it cannot."""

    try:
        return _run("ocr", path, timeout=120).get("lines") or []
    except Unavailable:
        return []


def ocr(path):
    return "\n".join(line["text"] for line in ocr_lines(path))


# ------------------------------------------------------------------
# PDFs out
# ------------------------------------------------------------------

def image_to_pdf(src, dst, dpi=150):
    """
    One picture as a one-page PDF. A PDF holds a JPEG as it is, so the picture
    is made one (GDI+ opens whatever Windows can show) and wrapped - no imaging
    library needed.
    """

    with tempfile.TemporaryDirectory(prefix="decurix-jpeg-") as folder:
        jpeg = Path(folder) / "page.jpg"
        answer = _run("tojpeg", src, jpeg)
        Path(dst).write_bytes(_jpeg_pdf(jpeg.read_bytes(), int(answer["width"]), int(answer["height"]), dpi))


def _jpeg_pdf(jpeg, width, height, dpi):
    page_w, page_h = width * 72.0 / dpi, height * 72.0 / dpi
    content = f"q {page_w:.2f} 0 0 {page_h:.2f} 0 0 cm /Im0 Do Q".encode("ascii")

    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        (f"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 {page_w:.2f} {page_h:.2f}] "
         "/Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>").encode("ascii"),
        (f"<< /Type /XObject /Subtype /Image /Width {width} /Height {height} /ColorSpace /DeviceRGB "
         f"/BitsPerComponent 8 /Filter /DCTDecode /Length {len(jpeg)} >>\nstream\n").encode("ascii") + jpeg + b"\nendstream",
        f"<< /Length {len(content)} >>\nstream\n".encode("ascii") + content + b"\nendstream",
    ]

    out = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = []
    for number, body in enumerate(objects, 1):
        offsets.append(len(out))
        out += f"{number} 0 obj\n".encode("ascii") + body + b"\nendobj\n"

    xref = len(out)
    out += f"xref\n0 {len(objects) + 1}\n".encode("ascii") + b"0000000000 65535 f \n"
    for offset in offsets:
        out += f"{offset:010d} 00000 n \n".encode("ascii")
    out += f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode("ascii")

    return bytes(out)


def libreoffice():
    """LibreOffice's program when the person has it, else None."""

    found = shutil.which("soffice") or shutil.which("libreoffice")
    if found:
        return found

    for root in (os.environ.get("ProgramFiles"), os.environ.get("ProgramFiles(x86)")):
        if root:
            exe = Path(root) / "LibreOffice" / "program" / "soffice.exe"
            if exe.is_file():
                return str(exe)

    return None


def office_to_pdf(src, out_dir):
    """
    A Word, Excel or PowerPoint document as a PDF, through Microsoft Office
    (office.py tries LibreOffice first, on both systems). Returns the PDF's path.
    """

    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    pdf = out_dir / (Path(src).stem + ".pdf")

    try:
        _run("topdf", src, pdf, timeout=240)
    except Unavailable as exc:
        reason = str(exc)
        if "80040154" in reason or "class not registered" in reason.lower() or "Retrieving the COM class factory" in reason:
            raise Unavailable(
                f"{Path(src).name}: neither Microsoft Office nor LibreOffice is installed on this computer, "
                "so it cannot be made a PDF"
            )
        raise Unavailable(f"{Path(src).name}: Microsoft Office could not convert it ({reason})")

    return pdf
