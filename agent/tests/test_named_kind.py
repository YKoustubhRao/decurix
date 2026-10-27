"""A file named as a PDF or a picture that holds something else is a fault the code decides (2026-10-24)."""
import tempfile
import unittest
from pathlib import Path

from decurix_agent import produced


class NamedKind(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def write(self, name, data):
        p = self.dir / name
        p.write_bytes(data)
        return str(p)

    def test_a_web_page_saved_as_a_pdf_is_a_fault(self):
        junk = self.write("197_invoice.pdf", b"<!doctype html><html><body>blocked</body></html>")
        real = self.write("198_invoice.pdf", b"%PDF-1.4\n%...")
        faults = produced.named_kind_faults([junk, real])
        self.assertEqual(len(faults), 1)
        self.assertIn("197_invoice.pdf is named as a PDF but holds a web page", faults[0])

    def test_pictures_and_empty_files(self):
        png = self.write("a.png", b"\x89PNG\r\n\x1a\nxxxx")
        jpg = self.write("b.jpg", b"\xff\xd8\xff\xe0xxxx")
        fake = self.write("c.jpg", b"GIF89a....")
        empty = self.write("d.pdf", b"")
        faults = produced.named_kind_faults([png, jpg, fake, empty])
        self.assertEqual(len(faults), 2, faults)
        self.assertTrue(any("c.jpg is named as a JPEG picture but holds" in f for f in faults))
        self.assertTrue(any("d.pdf is named as a PDF but holds nothing" in f for f in faults))

    def test_a_pdf_with_bytes_before_its_header_is_a_pdf(self):
        ok = self.write("e.pdf", b"\r\n\r\n%PDF-1.7 ...")
        self.assertEqual(produced.named_kind_faults([ok]), [])


if __name__ == "__main__":
    unittest.main()


class AWrongKindFileIsGotAgainNotCorrected(unittest.TestCase):
    """Found live (2026-10-24): told "the saved file is not finished, correct it in the file itself", the worker
    re-saved a sign-in page named .pdf three times and never clicked the attachment."""

    def test_the_remedy_fits_the_fault(self):
        from decurix_agent import produced
        faults = ["197.pdf is named as a PDF but holds a web page - not a PDF: get it again and check what it holds",
                  "memo.docx: a placeholder {{NAME}} is left in it"]
        self.assertEqual(produced.wrong_kind(faults), faults[:1])
        self.assertIn("get the document again where it is offered", produced.WRONG_KIND_REMEDY)
        self.assertNotIn("Correct it in the file itself", produced.WRONG_KIND_REMEDY)
