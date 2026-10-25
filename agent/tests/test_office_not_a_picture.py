"""A file named as a picture that is not one is said to be so, never shown to the model as a picture."""
import tempfile
import unittest
from pathlib import Path

from decurix_agent.tools import office


class AFileNamedAsAPicture(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_a_web_page_saved_as_jpg_is_named_for_what_it_is(self):
        p = self.dir / "IMG_20261001_0932.jpg"
        p.write_text("<!DOCTYPE html><html><body>Not found</body></html>", encoding="utf-8")
        r = office.read_document(str(p))
        self.assertEqual(r["images"], [])
        self.assertIn("is named as a picture but is not one", r["text"])
        self.assertIn("<!DOCTYPE html>", r["text"])

    def test_a_real_picture_is_shown(self):
        for name, head in (("a.jpg", b"\xff\xd8\xff\xe0\x00\x10JFIF"), ("b.png", b"\x89PNG\r\n\x1a\nIHDR"),
                           ("c.gif", b"GIF89a"), ("d.webp", b"RIFF\x00\x00\x00\x00WEBPVP8 ")):
            p = self.dir / name
            p.write_bytes(head + b"\x00" * 20)
            # (Compared resolved: a short temp path, C:\Users\ADMINI~1, is read back long.)
            shown = [Path(i).resolve() for i in office.read_document(str(p))["images"]]
            self.assertEqual(shown, [p.resolve()], name)


if __name__ == "__main__":
    unittest.main()
