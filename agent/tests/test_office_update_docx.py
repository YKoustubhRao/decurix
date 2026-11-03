"""
Finding BP1 (2026-10-20, round 5 P4): offer letters were filled from a Word template by clicking a word processor's
Find & Replace - 25 minutes for four letters, and once stuck in a formatting dialog. `office update DOC.docx` fills a
template in one command: in the body, tables, headers and footers, also where Word split a placeholder across runs,
keeping the formatting; and what is still left is a code-decided fact.
"""

from needs import needs
import os
import tempfile
import unittest
from pathlib import Path

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import produced
from decurix_agent.tools import office
import fixture_paths  # noqa: E402 - what may not be on this machine is counted

TEMPLATE = Path(r"C:\Users\Administrator\Documents\Decurix demo\process\P4\Offer_letter_template.docx")


def make_template(path):
    import docx
    document = docx.Document()
    p = document.add_paragraph("Dear ")
    bold = p.add_run("{{FULL")
    bold.bold = True
    p.add_run("_NAME}}")  # Word keeps a typed placeholder in pieces like this
    p.add_run(", welcome aboard as {{DESIGNATION}}.")
    table = document.add_table(rows=2, cols=2)
    table.cell(0, 0).text, table.cell(0, 1).text = "Basic", "{{BASIC}}"
    table.cell(1, 0).text, table.cell(1, 1).text = "HRA", "Rs {{HRA}} a month"
    document.sections[0].header.paragraphs[0].text = "Offer for {{FULL_NAME}}"
    document.save(path)


@needs("python-docx")
class FillingATemplate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.template = self.dir / "Offer_letter_template.docx"
        make_template(self.template)

    def test_split_runs_tables_and_headers_are_filled_and_formatting_kept(self):
        out = self.dir / "letters" / "offer_ananya.docx"
        said = office.update_document(str(self.template), {"{{FULL_NAME}}": "Ananya Deshpande", "{{DESIGNATION}}":
                                                           "Analyst", "{{BASIC}}": "45,000", "{{HRA}}": "18,000"},
                                      str(out))
        self.assertIn("{{FULL_NAME}} x2", said)  # the body (split across runs) and the header
        self.assertIn("Placeholders still in it: none", said)
        import docx
        document = docx.Document(str(out))
        first = document.paragraphs[0]
        self.assertEqual(first.text, "Dear Ananya Deshpande, welcome aboard as Analyst.")
        bold = [r for r in first.runs if r.text][1]
        self.assertTrue(bold.bold)
        self.assertEqual(bold.text, "Ananya Deshpande")  # in the run it started in, with its formatting
        self.assertEqual(document.tables[0].cell(1, 1).text, "Rs 18,000 a month")
        self.assertEqual(document.sections[0].header.paragraphs[0].text, "Offer for Ananya Deshpande")
        self.assertTrue(self.template.exists())  # the template itself is not touched when an output is named
        self.assertEqual(office.docx_placeholders(str(self.template))[:1], ["{{FULL_NAME}}"])

    def test_what_is_not_found_and_what_is_left_are_said(self):
        said = office.update_document(str(self.template), {"{{FULL_NAME}}": "A", "{{PAN}}": "X"},
                                      str(self.dir / "out.docx"))
        self.assertIn("Not found anywhere in the document: {{PAN}}", said)
        self.assertIn("Placeholders still in it: {{DESIGNATION}}, {{BASIC}}, {{HRA}}", said)
        # The template the letter was filled from held the placeholders: left unfilled, they block (E-11).
        facts = produced.document_findings(f"Fill {self.template.name} and write the offer letters.",
                                           [str(self.dir / "out.docx")], root=str(self.dir))
        self.assertTrue(any("still holds the template's placeholder(s) {{DESIGNATION}}" in f for f in facts), facts)
        # A request that gives no source document: a placeholder left is evidence for the judge, never a block.
        self.assertEqual(produced.document_findings("Write the offer letters.", [str(self.dir / "out.docx")],
                                                    root=str(self.dir / "elsewhere")), [])
        self.assertTrue(produced.template_placeholder_notes("Write the offer letters.", [str(self.dir / "out.docx")],
                                                            root=str(self.dir / "elsewhere")))

    def test_in_place_keeps_the_original(self):
        office.update_document(str(self.template), {"{{HRA}}": "1"})
        self.assertTrue((self.dir / "Offer_letter_template.orig.docx").exists())

    def test_bad_input_is_refused_and_nothing_written(self):
        for args in (({"{{A}}": "b"}, str(self.dir / "x.pdf")), ({}, None), ("not a dict", None)):
            with self.assertRaises(office.OfficeError):
                office.update_document(str(self.template), *args)
        bad = self.dir / "bad.docx"
        bad.write_text("not a zip", encoding="utf-8")
        with self.assertRaises(office.OfficeError):
            office.update_document(str(bad), {"{{A}}": "b"})
        with self.assertRaises(office.OfficeError):
            office.update_document(str(self.dir / "missing.docx"), {"{{A}}": "b"})

    def test_the_command_line(self):
        out = self.dir / "cli.docx"
        spec = self.dir / "spec.json"
        spec.write_text('{"replace": {"{{FULL_NAME}}": "Ananya"}, "output_path": "%s"}' % str(out).replace("\\", "/"),
                        encoding="utf-8")
        self.assertEqual(office.main(["update", str(self.template), str(spec)]), 0)
        self.assertTrue(out.exists())


@fixture_paths.skip_unless(TEMPLATE.exists(), "the P4 template is not on this machine")
class TheLiveTemplate(unittest.TestCase):
    def test_every_placeholder_of_the_offer_letter_is_filled(self):
        keys = office.docx_placeholders(str(TEMPLATE))
        self.assertIn("{{CTC_ANNUAL}}", keys)
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "offer.docx")
            said = office.update_document(str(TEMPLATE), {k: "v" for k in keys}, out)
            self.assertIn("Placeholders still in it: none", said)
            self.assertNotIn("{{", office.read_document(out)["text"])


if __name__ == "__main__":
    unittest.main()
