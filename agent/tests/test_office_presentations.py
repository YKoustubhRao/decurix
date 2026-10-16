"""PowerPoint content inspection independent of optional python-pptx."""
import tempfile
import unittest
import zipfile
from pathlib import Path

from decurix_agent.tools import office


class PresentationReading(unittest.TestCase):
    def test_slide_order_comes_from_relationships_and_entities_are_decoded(self):
        with tempfile.TemporaryDirectory() as tmp:
            deck = Path(tmp) / "review.pptx"
            with zipfile.ZipFile(deck, "w") as z:
                z.writestr("ppt/presentation.xml", '<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId r:id="second"/><p:sldId r:id="first"/></p:sldIdLst></p:presentation>')
                z.writestr("ppt/_rels/presentation.xml.rels", '<Relationships><Relationship Id="first" Target="slides/slide1.xml"/><Relationship Id="second" Target="slides/slide2.xml"/></Relationships>')
                for n, text in [(1, "Next steps"), (2, "Revenue &amp; costs")]:
                    z.writestr(f"ppt/slides/slide{n}.xml", f'<slide xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:p><a:r><a:t>{text}</a:t></a:r></a:p></slide>')
            result = office.read_document(deck)
            self.assertIn("[Slide 1]\nRevenue & costs", result["text"])
            self.assertIn("[Slide 2]\nNext steps", result["text"])
            self.assertEqual(result["images"], [])

    def test_invalid_package_has_actionable_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            deck = Path(tmp) / "broken.pptx"
            deck.write_text("not a zip", encoding="utf-8")
            with self.assertRaisesRegex(office.OfficeError, "cannot read PowerPoint"):
                office.read_document(deck)


try:
    import pptx  # noqa: F401
    HAVE_PPTX = True
except ImportError:
    HAVE_PPTX = False


def _png(path):
    import struct
    import zlib

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)
    raw = b"".join(b"\x00" + b"\xff\x00\x00" * 4 for _ in range(2))
    Path(path).write_bytes(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 4, 2, 8, 2, 0, 0, 0))
                           + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


SLIDES = [
    {"title": "Q3 review", "subtitle": "Finance team"},
    {"title": "Highlights", "bullets": ["Revenue up 12%", {"text": "North led", "level": 1}],
     "notes": "Mention the new customers"},
    {"title": "By region", "table": [["Region", "Revenue"], ["North", 1200], ["South", 950]]},
    {"title": "Trend", "chart": {"type": "column", "title": "Revenue by month", "categories": ["Jul", "Aug", "Sep"],
                                 "series": [{"name": "2026", "values": [100, 120, 135]}]}},
    {"title": "The site", "image": "PICTURE", "text": "Opened in August"},
]


@unittest.skipUnless(HAVE_PPTX, "python-pptx is not installed")
class PresentationMaking(unittest.TestCase):
    # Found live (2026-10-14): asked for a review deck, a worker had no tool that made a .pptx.

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        _png(self.root / "site.png")
        self.slides = [dict(s, image=str(self.root / "site.png")) if s.get("image") else s for s in SLIDES]

    def make(self, name="review.pptx"):
        deck = self.root / name
        said = office.create_presentation(str(deck), self.slides)
        return deck, said

    def test_a_deck_is_made_and_reads_back_slide_by_slide(self):
        deck, said = self.make()
        self.assertIn("5 slides", said)
        self.assertIn("1 table(s), 1 chart(s), 1 image(s), 1 notes", said)
        text = office.read_document(deck)["text"]
        self.assertIn("[Slide 1]\nQ3 review\nFinance team", text)
        self.assertIn("Speaker notes: Mention the new customers", text)
        self.assertIn("| North | 1200 |", text)
        self.assertIn('Chart "Revenue by month" (barChart):', text)
        self.assertIn("categories: Jul, Aug, Sep", text)
        self.assertIn("2026: 100, 120, 135", text)
        self.assertIn("[picture", text)
        from pptx import Presentation
        prs = Presentation(str(deck))
        self.assertAlmostEqual(prs.slide_width / prs.slide_height, 16 / 9, places=2)

    def test_what_it_cannot_make_is_refused_before_anything_is_written(self):
        deck = self.root / "bad.pptx"
        for slides, why in (
                ([{"title": "x", "bulets": ["typo"]}], "does not take 'bulets'"),
                ([{"title": "x", "table": [["a"]], "chart": {"categories": ["a"], "series": [{"values": [1]}]}}],
                 "one of table, chart or image"),
                ([{"title": "x", "chart": {"categories": ["a", "b"], "series": [{"name": "s", "values": [1]}]}}],
                 "1 values for 2 categories"),
                ([{"title": "x", "chart": {"categories": ["a"], "series": [{"name": "s", "values": ["lots"]}]}}],
                 "is not a number"),
                ([{"title": "x", "image": str(self.root / "missing.png")}], "does not exist"),
                ([{}], "is empty"),
                ([], "slides")):
            with self.subTest(why=why):
                with self.assertRaisesRegex(office.OfficeError, why):
                    office.create_presentation(str(deck), slides)
                self.assertFalse(deck.exists())
        made, _ = self.make()
        with self.assertRaisesRegex(office.OfficeError, "already exists"):
            office.create_presentation(str(made), self.slides)

    def test_an_update_changes_only_what_it_names(self):
        deck, _ = self.make()
        from pptx import Presentation
        before = Presentation(str(deck))
        layout = [[(s.shape_type, s.left, s.top, s.width, s.height) for s in slide.shapes] for slide in before.slides]
        said = office.update_presentation(str(deck), [
            {"find": "Q3", "replace": "Q4"},
            {"slide": 3, "table": 1, "row": 2, "column": 2, "value": "1,350"},
            {"slide": 4, "chart": 1, "categories": ["Oct", "Nov", "Dec"], "series": [{"name": "2026", "values": [140, 150, 160]}]},
            {"slide": 2, "notes": "Thank the team"},
            {"slide": 2, "title": "What went well"}])
        self.assertIn("5 change(s)", said)
        text = office.read_document(deck)["text"]
        self.assertIn("Q4 review", text)
        self.assertNotIn("Q3", text)
        self.assertIn("| North | 1,350 |", text)
        self.assertIn("2026: 140, 150, 160", text)
        self.assertIn("Speaker notes: Thank the team", text)
        self.assertIn("What went well", text)
        after = Presentation(str(deck))
        self.assertEqual(layout, [[(s.shape_type, s.left, s.top, s.width, s.height) for s in slide.shapes]
                                  for slide in after.slides], "the layout is kept")
        self.assertTrue((self.root / "review.orig.pptx").exists())

    def test_a_bad_edit_changes_nothing(self):
        deck, _ = self.make()
        original = deck.read_bytes()
        for edits, why in (([{"find": "Q3", "replace": "Q4"}, {"slide": 2, "find": "nowhere", "replace": "x"}], "is not in slide 2"),
                           ([{"slide": 9, "title": "x"}], "no slide 9"),
                           ([{"slide": 3, "table": 1, "row": 9, "column": 1, "value": "x"}], "within the table"),
                           ([{"slide": 2, "chart": 1, "categories": ["a"], "series": [{"values": [1]}]}], "0 chart"),
                           ([{"slide": 2, "colour": "red"}], "an edit takes"),
                           ([{"slide": 2, "title": "a", "notes": "b"}], "exactly one change")):
            with self.subTest(why=why):
                with self.assertRaisesRegex(office.OfficeError, why):
                    office.update_presentation(str(deck), edits)
                self.assertEqual(deck.read_bytes(), original)

    def test_the_command_line_makes_and_updates_a_deck_and_refuses_an_unknown_key(self):
        import contextlib
        import io
        import json
        deck = self.root / "cli.pptx"
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(office.main(["create", str(deck), json.dumps({"slides": [{"title": "One", "bullets": ["a"]}]})]), 0)
            self.assertEqual(office.main(["update", str(deck), json.dumps({"edits": [{"slide": 1, "find": "a", "replace": "b"}]})]), 0)
            self.assertEqual(office.main(["create", str(self.root / "x.pptx"), json.dumps({"slide": []})]), 1)
        self.assertIn("does not take 'slide'", out.getvalue())
        self.assertFalse((self.root / "x.pptx").exists())


if __name__ == "__main__":
    unittest.main()
