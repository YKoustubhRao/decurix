"""A part of a picture is read closer: cropped and enlarged (found live 2026-10-27: a photo's date was unreadable)."""
import os
import tempfile
import unittest

from PIL import Image

from decurix_agent.tools import office


class Zoom(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.photo = os.path.join(self.tmp.name, "IMG-0001.jpg")
        im = Image.new("RGB", (800, 600), "white")
        for x in range(400, 800):
            for y in range(0, 300):
                if (x + y) % 7 == 0:
                    im.putpixel((x, y), (0, 0, 0))
        im.save(self.photo, "JPEG")

    def test_the_whole_picture_says_how_to_look_closer(self):
        r = office.read_document(self.photo)
        self.assertEqual([os.path.basename(i) for i in r["images"]], ["IMG-0001.jpg"])
        self.assertIn("PART is top, bottom", r["text"])

    def test_a_named_part_is_cropped_and_enlarged(self):
        r = office.read_document(self.photo, pages="top-right")
        self.assertIn("part top-right", r["text"])
        with Image.open(r["images"][0]) as im:
            self.assertGreaterEqual(max(im.size), 1600)
            self.assertAlmostEqual(im.size[0] / im.size[1], (800 * 0.55) / (600 * 0.55), places=1)

    def test_a_box_in_percent_and_a_bad_part(self):
        r = office.read_document(self.photo, pages="50,0,100,30")
        self.assertIn("x 50-100%, y 0-30%", r["text"])
        with self.assertRaises(office.OfficeError):
            office.read_document(self.photo, pages="upper-left-ish")
        with self.assertRaises(office.OfficeError):
            office.read_document(self.photo, pages="60,0,40,30")


if __name__ == "__main__":
    unittest.main()
