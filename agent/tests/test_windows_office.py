"""
The document tools on a native Windows worker (tools/winoffice.py, and the
places office.py and computer/ocr.py hand over to it), tested on any system:
Windows' own OCR, PDF renderer and Office are stood in for by what winrt.ps1
would answer. What only Windows can show was run there, live.
"""

import io
import json
import os
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

from decurix_agent.computer import ocr
from decurix_agent.tools import office, winoffice


def answered(payload, code=0, stderr=b""):
    """subprocess.run's result for a winrt.ps1 that printed this answer."""

    def run(argv, **kwargs):
        run.calls.append(argv)
        out = payload if isinstance(payload, bytes) else (json.dumps(payload) + "\n").encode("utf-8")
        return subprocess.CompletedProcess(argv, code, out, stderr)

    run.calls = []
    return run


class AskingWindows(unittest.TestCase):
    def test_the_answer_is_the_scripts_json_line(self):
        run = answered({"ok": True, "lines": [{"text": "Invoice INV-7", "box": [10, 20, 90, 14]}]})
        with patch.object(winoffice.subprocess, "run", side_effect=run):
            self.assertEqual(winoffice.ocr_lines("scan.png"), [{"text": "Invoice INV-7", "box": [10, 20, 90, 14]}])
        argv = run.calls[0]
        self.assertEqual(argv[-2:], ["ocr", "scan.png"])
        self.assertIn(str(winoffice.SCRIPT), argv)
        self.assertTrue(winoffice.SCRIPT.is_file(), "winrt.ps1 ships beside winoffice.py")

    def test_what_windows_could_not_do_is_said_in_its_words(self):
        with patch.object(winoffice.subprocess, "run", side_effect=answered({"ok": False, "error": "the file is not a PDF"}, code=1)):
            with self.assertRaises(winoffice.Unavailable) as ctx:
                winoffice._run("pdfpages", "x.pdf", 1, 1, "out", 900)
        self.assertEqual(str(ctx.exception), "the file is not a PDF")

    def test_no_answer_at_all_is_an_error_not_an_empty_result(self):
        with patch.object(winoffice.subprocess, "run", side_effect=answered(b"", code=1, stderr=b"powershell: script blocked\n")):
            with self.assertRaises(winoffice.Unavailable) as ctx:
                winoffice._run("ocr", "x.png")
        self.assertIn("script blocked", str(ctx.exception))

    def test_reading_a_picture_never_fails_a_task(self):
        # OCR is a way to read more, not a step that can stop the work.
        with patch.object(winoffice.subprocess, "run", side_effect=OSError("no powershell")):
            self.assertEqual(winoffice.ocr_lines("x.png"), [])
            self.assertEqual(winoffice.ocr("x.png"), "")

    def test_office_missing_is_said_plainly(self):
        error = 'Retrieving the COM class factory for component with CLSID {000209FF-0000-0000-C000-000000000046} failed due to the following error: 80040154 Class not registered'
        with tempfile.TemporaryDirectory() as folder, \
                patch.object(winoffice.subprocess, "run", side_effect=answered({"ok": False, "error": error}, code=1)):
            with self.assertRaises(winoffice.Unavailable) as ctx:
                winoffice.office_to_pdf(Path(folder) / "letter.docx", Path(folder) / "out")
        self.assertIn("neither Microsoft Office nor LibreOffice is installed", str(ctx.exception))
        self.assertNotIn("CLSID", str(ctx.exception))


class APictureAsAPdfPage(unittest.TestCase):
    JPEG = b"\xff\xd8\xff\xe0" + b"jpeg-bytes" * 20 + b"\xff\xd9"

    def test_the_page_holds_the_jpeg_as_it_is_at_the_pictures_size(self):
        pdf = winoffice._jpeg_pdf(self.JPEG, 1500, 750, dpi=150)

        self.assertTrue(pdf.startswith(b"%PDF-1.4"))
        self.assertIn(self.JPEG, pdf)
        self.assertIn(b"/Filter /DCTDecode", pdf)
        self.assertIn(b"/Width 1500 /Height 750", pdf)
        # 1500 px at 150 dpi is 10 inches: 720 points.
        self.assertIn(b"/MediaBox [0 0 720.00 360.00]", pdf)

        # Every object is where the cross-reference table says it is.
        xref = int(pdf.rsplit(b"startxref\n", 1)[1].split(b"\n", 1)[0])
        table = pdf[xref:].split(b"\n")
        self.assertEqual(table[0], b"xref")
        for number, entry in enumerate(table[3:8], 1):
            offset = int(entry[:10])
            self.assertTrue(pdf[offset:].startswith(f"{number} 0 obj".encode()), f"object {number}")

    def test_pypdf_reads_it_back(self):
        try:
            import pypdf
        except ImportError:
            self.skipTest("pypdf is not installed here")
        reader = pypdf.PdfReader(io.BytesIO(winoffice._jpeg_pdf(self.JPEG, 800, 600, dpi=150)))
        self.assertEqual(len(reader.pages), 1)
        self.assertAlmostEqual(float(reader.pages[0].mediabox.width), 384.0, places=1)

    def test_a_picture_is_made_a_jpeg_first(self):
        def run(argv, **kwargs):
            Path(argv[-1]).write_bytes(self.JPEG)
            return subprocess.CompletedProcess(argv, 0, b'{"ok": true, "width": 800, "height": 600}\n', b"")

        with tempfile.TemporaryDirectory() as folder, patch.object(winoffice.subprocess, "run", side_effect=run):
            out = Path(folder) / "page.pdf"
            winoffice.image_to_pdf(Path(folder) / "scan.png", out)
            self.assertIn(self.JPEG, out.read_bytes())


class OfficeOnWindows(unittest.TestCase):
    """office.py's Windows branches: the same answers, from other programs."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.pdf = Path(self.dir.name) / "po.pdf"
        self.pdf.write_bytes(b"%PDF-1.4 stand-in")
        patcher = patch.object(office, "WINDOWS", True)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.dir.cleanup)

    def test_a_pdf_with_text_is_read_as_text(self):
        with patch.object(winoffice, "pdf_page_count", return_value=2), \
                patch.object(winoffice, "pdf_text", return_value="Purchase order GF-5522\nCustomer: Gemini Furniture\n"
                             "Corner Desk Right Sit   qty 3   unit price 147.00\fTerms: 30 days from the invoice date") as text, \
                patch.object(winoffice, "pdf_pages_as_png") as pictures:
            got = office.read_document(self.pdf, pages="1-2")
        text.assert_called_once_with(self.pdf.resolve(), 1, 2)
        pictures.assert_not_called()
        self.assertIn("PDF, 2 page(s); pages 1-2.", got["text"])
        self.assertIn("Gemini Furniture", got["text"])
        self.assertEqual(got["images"], [])

    def test_a_scan_is_shown_as_pictures(self):
        with patch.object(winoffice, "pdf_page_count", return_value=1), \
                patch.object(winoffice, "pdf_text", return_value="  \n"), \
                patch.object(winoffice, "pdf_pages_as_png", return_value=["C:\\t\\page-001.png"]):
            got = office.read_document(self.pdf)
        self.assertIn("it is a scan", got["text"])
        self.assertEqual(got["images"], ["C:\\t\\page-001.png"])

    def test_a_file_that_is_not_a_pdf_says_so(self):
        with patch.object(winoffice, "pdf_page_count", side_effect=winoffice.Unavailable("po.pdf could not be opened as a PDF: EOF marker not found")):
            with self.assertRaises(office.OfficeError) as ctx:
                office.read_document(self.pdf)
        self.assertIn("could not be opened as a PDF", str(ctx.exception))

    def test_a_scan_is_read_by_windows_for_a_brain_that_cannot_see(self):
        picture = Path(self.dir.name) / "invoice.png"
        picture.write_bytes(b"\x89PNG stand-in")
        out = io.StringIO()
        with patch.dict(os.environ, {"DECURIX_OFFICE_IMAGES": "ocr"}), \
                patch.object(winoffice, "ocr", return_value="Invoice INV-7 total 1,200.00") as read, \
                redirect_stdout(out):
            self.assertEqual(office.main(["read", str(picture)]), 0)
        read.assert_called_once()
        self.assertIn("INV-7", out.getvalue())

    def test_libreoffice_is_used_when_the_person_has_it(self):
        def run(argv, **kwargs):
            out_dir = Path(argv[argv.index("--outdir") + 1])
            (out_dir / "letter.pdf").write_bytes(b"%PDF")
            return subprocess.CompletedProcess(argv, 0, "", "")

        src = Path(self.dir.name) / "letter.docx"
        src.write_bytes(b"docx")
        with patch.object(winoffice, "libreoffice", return_value="C:\\LibreOffice\\soffice.exe"), \
                patch.object(winoffice, "office_to_pdf") as through_office, \
                patch.object(office.subprocess, "run", side_effect=run) as ran:
            pdf = office._office_to_pdf(src, Path(self.dir.name) / "out")
        through_office.assert_not_called()
        self.assertEqual(pdf.name, "letter.pdf")
        self.assertEqual(ran.call_args[0][0][0], "C:\\LibreOffice\\soffice.exe")

    def test_without_it_microsoft_office_makes_the_pdf(self):
        src = Path(self.dir.name) / "letter.docx"
        src.write_bytes(b"docx")
        with patch.object(winoffice, "libreoffice", return_value=None), \
                patch.object(winoffice, "office_to_pdf", return_value=Path("out/letter.pdf")) as through_office:
            self.assertEqual(office._office_to_pdf(src, Path("out")), Path("out/letter.pdf"))
        through_office.assert_called_once()

    def test_with_neither_the_person_is_told(self):
        src = Path(self.dir.name) / "letter.docx"
        src.write_bytes(b"docx")
        with patch.object(winoffice, "libreoffice", return_value=None), \
                patch.object(winoffice, "office_to_pdf", side_effect=winoffice.Unavailable("letter.docx: neither Microsoft Office nor LibreOffice is installed on this computer, so it cannot be made a PDF")):
            with self.assertRaises(office.OfficeError) as ctx:
                office._office_to_pdf(src, Path("out"))
        self.assertIn("neither Microsoft Office nor LibreOffice", str(ctx.exception))


class ReadingAScreenOnWindows(unittest.TestCase):
    """computer/ocr.py, the replay ladder's eyes, with Windows' OCR in tesseract's place."""

    def test_lines_come_back_top_to_bottom_with_their_boxes(self):
        found = [
            {"text": "Best songs of Jimi Hendrix", "box": [352, 544, 300, 18]},
            {"text": "Search results", "box": [40, 120, 160, 20]},
            {"text": "•  12", "box": [10, 300, 20, 10]},   # nothing with a word in it
        ]
        ocr._cache.clear()
        with patch.object(ocr.os, "name", "nt"), patch.object(winoffice, "ocr_lines", return_value=found):
            self.assertTrue(ocr.available())
            lines = ocr.read(4, 2, bytes(24))
        self.assertEqual([l["text"] for l in lines], ["Search results", "Best songs of Jimi Hendrix"])
        self.assertEqual(lines[1]["box"], [352, 544, 300, 18])
        self.assertEqual(ocr.line_at(lines, (400, 550))["text"], "Best songs of Jimi Hendrix")

    def test_a_failure_reads_as_nothing(self):
        ocr._cache.clear()
        with patch.object(ocr.os, "name", "nt"), patch.object(winoffice, "ocr_lines", side_effect=RuntimeError("boom")):
            self.assertEqual(ocr.read(4, 2, bytes([1]) * 24), [])


if __name__ == "__main__":
    unittest.main()
