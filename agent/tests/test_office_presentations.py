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


if __name__ == "__main__":
    unittest.main()
