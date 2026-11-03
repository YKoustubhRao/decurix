"""
Facts of a saved file are decided in code, before any model, so a report the file contradicts is never done. Found
live (2026-10-19): a picture "resized to 50%" was saved at its old size (the report itself said 1200 x 600); four
letters "each on its own page" were joined by "\\f" typed as text; a request for error_summary.txt was answered by a
built program, and the report went out unchecked.
"""

from needs import needs
import io
import os
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from temp_case import TempDirCase  # noqa: E402

try:
    from PIL import Image
except ImportError:  # pragma: no cover
    Image = None

from decurix_agent import produced, skill_task, verify
from decurix_agent.computer import cli
from decurix_agent.computer.errors import ComputerError
from decurix_agent.tools import office

W = 'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"'


def docx(path, body):
    """A minimal Word document whose body is `body` (WordprocessingML)."""

    with zipfile.ZipFile(path, "w") as z:
        # (A package Word opens: content types and the relationship to the document part - office read refuses a
        # broken one, series 14 P3.)
        z.writestr("[Content_Types].xml", '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/'
                   'package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-'
                   'package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override '
                   'PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.'
                   'wordprocessingml.document.main+xml"/></Types>')
        z.writestr("_rels/.rels", '<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/'
                   '2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/'
                   '2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>')
        z.writestr("word/document.xml", f'<?xml version="1.0"?><w:document {W}><w:body>{body}</w:body></w:document>')
    return str(path)


def para(text, extra=""):
    return f"<w:p>{extra}<w:r><w:t>{text}</w:t></w:r></w:p>"


PAGE = '<w:p><w:r><w:br w:type="page"/></w:r></w:p>'


class DocumentStructure(TempDirCase):
    def test_page_breaks_headings_and_tables_are_read_in_order(self):
        path = docx(self.dir / "r.docx", para("Reminders", '<w:pPr><w:pStyle w:val="Heading1"/></w:pPr>')
                    + para("Blue Lotus Hotels") + PAGE + para("Kaveri Electricals")
                    + "<w:tbl><w:tr><w:tc>" + para("INV-1") + "</w:tc><w:tc>" + para("100") + "</w:tc></w:tr></w:tbl>"
                    + para("Pinnacle", '<w:pPr><w:pageBreakBefore/></w:pPr>'))
        s = office.docx_structure(path)
        self.assertEqual(s["page_breaks"], 2)
        self.assertEqual(s["tables"], 1)
        self.assertEqual(s["headings"], ["Reminders"])
        text = office.read_document(path)["text"]
        self.assertIn("r.docx: 2 page break(s), 1 table(s), 1 heading(s)", text)
        self.assertIn("Blue Lotus Hotels\n[page break]\nKaveri Electricals\n[Table 1]\nINV-1 | 100\n[page break]", text)

    def test_a_backslash_f_typed_as_text_is_named(self):
        path = docx(self.dir / "r.docx", para("Please pay.\\fKaveri Electricals"))
        text = office.read_document(path)["text"]
        self.assertIn("0 page break(s)", text)
        self.assertIn('"\\f" typed as characters', text)
        found = produced.document_findings("Write one letter per customer, each letter on its own page.", [path])
        self.assertEqual(len(found), 1)  # hygiene: decided without reading the request
        self.assertIn('holds the characters "\\f" typed as text', found[0])
        breaks = produced.page_break_findings("Write one letter per customer, each letter on its own page.", [path])
        self.assertIn("has no page break", breaks[0])  # a reading of the request: evidence

    def test_a_document_with_its_breaks_or_not_asked_for_them_is_fine(self):
        good = docx(self.dir / "good.docx", para("A") + PAGE + para("B"))
        flat = docx(self.dir / "flat.docx", para("A") + para("B"))
        self.assertEqual(produced.document_findings("Each letter on its own page.", [good]), [])
        self.assertEqual(produced.document_findings("Write a summary in a document.", [flat]), [])


@unittest.skipIf(Image is None, "Pillow is not installed")
class PictureSizes(TempDirCase):
    def setUp(self):
        super().setUp()
        self.source = self.picture("logo.png", 1200, 600)
        self.request = ("Open logo.png from the folder in Paint, resize it to 50% keeping its proportions, and save it "
                        "in the same folder as logo_small.png.")

    def picture(self, name, w, h):
        path = self.dir / name
        Image.new("RGB", (w, h), "white").save(path)
        return str(path)

    def test_the_old_size_is_not_resized(self):
        out = self.picture("logo_small.png", 1200, 600)
        found = produced.image_findings(self.request, [out], [self.source])
        self.assertEqual(len(found), 1)
        self.assertIn("the same size as the picture it was made from; it was not resized", found[0])

    def test_the_size_asked_passes_and_another_does_not(self):
        self.assertEqual(produced.image_findings(self.request, [self.picture("logo_small.png", 600, 300)],
                                                 [self.source]), [])
        self.assertIn("50% of 1200 x 600 is 600 x 300",
                      produced.image_findings(self.request, [self.picture("logo_small.png", 640, 300)],
                                              [self.source])[0])
        asked = "Resize photo.jpg to 800 x 600 pixels and save it as photo_web.jpg."
        self.assertIn("asks for 800 x 600",
                      produced.image_findings(asked, [self.picture("photo_web.jpg", 800, 533)], [])[0])

    def test_the_source_is_found_on_this_desktop_or_fetched_from_the_persons_computer(self):
        local = produced.source_images(self.request, [], root=self.dir)
        self.assertEqual([Path(p).name for p in local], ["logo.png"])
        host = ("Open logo.png from the \"Decurix demo\" folder (C:\\Users\\Administrator\\Documents\\Decurix demo), "
                "resize it to 50% and save it in the same folder as logo_small.png.")
        asked = []
        with tempfile.TemporaryDirectory() as empty, \
                mock.patch.object(produced.config, "DECURIX_DIR", Path(empty)):
            copies = produced.source_images(host, [], root=empty,
                                            fetch=lambda p: asked.append(p) or Path(self.source).read_bytes())
        self.assertEqual(asked, ["C:\\Users\\Administrator\\Documents\\Decurix demo\\logo.png"])
        self.assertEqual(len(copies), 1)


class RequestedFiles(unittest.TestCase):
    def test_a_named_file_nowhere_to_be_found_is_missing(self):
        request = ("Look through app.log and write a summary, saved in my work folder as error_summary.txt: one line "
                   "per service.")
        self.assertEqual(produced.requested_names(request), ["error_summary.txt"])
        with tempfile.TemporaryDirectory() as root:
            (Path(root) / "app.log").write_text("x")
            self.assertIn("error_summary.txt was not made", produced.missing_requested(request, [], root=root))
            (Path(root) / "error_summary.txt").write_text("api: 3")
            self.assertEqual(produced.missing_requested(request, [], root=root), "")

    def test_an_input_named_in_the_same_sentence_is_not_missing(self):
        with tempfile.TemporaryDirectory() as root:
            (Path(root) / "inbox").mkdir()
            (Path(root) / "inbox" / "invoice.pdf").write_text("x")
            self.assertEqual(produced.missing_requested("Write an email to a@b.com with invoice.pdf attached.", [],
                                                        root=root), "")

    def test_a_request_for_the_persons_computer_is_checked_there_not_here(self):
        self.assertEqual(produced.missing_requested(
            "Save it in C:\\Users\\me\\Documents as error_summary.txt.", [], root=tempfile.gettempdir()), "")


from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)
from test_apprentice import verdict  # noqa: E402


class FactsAtTheGate(FileWorkIsChecked):
    def gate(self, report, **patches):
        defaults = {"store_results": lambda paths: [], "typed_totals": lambda paths, request="": [],
                    "missing_requested": lambda request, made, root=None, only=None: "",
                    "named_not_written": lambda request, made, root=None: [],
                    "image_findings": lambda request, made, sources: [], "source_images": lambda request, made: [],
                    "document_findings": lambda request, made, root=None: [],
                    "page_break_findings": lambda request, made: []}
        defaults.update(patches)
        with mock.patch.multiple(skill_task.produced, **defaults):
            return skill_task._gate(report, "final", "thebot")

    def test_a_file_the_requests_words_name_and_nothing_wrote_is_evidence_the_judge_weighs(self):
        # Review 2026-11-03 (E-10): read from the words alone, "Do not create report.pdf" asked for report.pdf and was
        # refused at certainty; such a name is evidence for the judge, which decides.
        self.verdicts = [verdict(verify.NOT_DONE, seen="no error_summary.txt was written")]
        said = ["The request's words name error_summary.txt, and no file of that name was written during this task"]
        self.assertTrue(self.gate("Built and tested: decurix-error-summary.",
                                  named_not_written=lambda request, made, root=None: said))
        self.assertIn("error_summary.txt", self.judged[0]["produced"])
        self.assertEqual(self.finals, [])

    @needs("openpyxl")
    def test_a_picture_at_its_old_size_is_evidence_for_the_judge(self):
        # Harness audit (2026-10-21): the size asked is a reading of the request - the judge weighs it.
        self.verdicts = [verdict(verify.NOT_DONE, seen="logo_small.png is 1200 x 600, not 50%")]
        self.gate("Resized it to 50%.", image_findings=lambda r, m, s: ["logo_small.png is 1200 x 600"],
                  page_break_findings=lambda request, made: [])
        self.assertIn("logo_small.png is 1200 x 600", self.judged[0]["produced"])

    def test_a_file_on_the_persons_computer_that_cannot_be_read_is_asked_not_passed(self):
        self.verdicts = []
        with mock.patch.object(skill_task.produced, "missing_on_host", lambda request: ""), \
                mock.patch.object(skill_task.produced, "saved_on_host",
                                  lambda request: ["C:\\Users\\me\\Documents\\logo_small.png"]), \
                mock.patch.object(skill_task.produced, "copy_from_host", lambda paths: []):
            self.assertTrue(self.gate("Saved logo_small.png."))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("could not read C:\\Users\\me\\Documents\\logo_small.png", self.asked[-1][0])

    def test_a_task_that_wrote_nothing_but_was_asked_for_a_file_is_checked(self):
        self.made = []
        self.verdicts = []
        skill_task.state.current_task = "Write a summary, saved in my work folder as error_summary.txt."
        self.verdicts = [verdict(verify.NOT_DONE, seen="no error_summary.txt was written")]
        said = ["The request's words name error_summary.txt, and no file of that name was written during this task"]
        self.assertTrue(self.gate("Built and tested a program.",
                                  named_not_written=lambda request, made, root=None: said))
        self.assertIn("error_summary.txt", self.judged[0]["produced"])
        self.assertEqual(self.finals, [])


class OneSetOfChecksForEveryRoute(TempDirCase):
    """The gate, a planned stage and a proven tool all read produced.file_facts, so a fix to a check applies to all."""

    def test_a_stage_is_checked_for_the_files_its_own_instruction_names(self):
        from decurix_agent import long_task

        instruction = "Write the summary and save it as error_summary.txt in the work folder."
        whole = instruction + " Then make the deck deck.pptx and the register register.xlsx."
        # A planned stage's own outputs are structure: a missing one blocks.
        found = long_task.code_problems([], "done", [], whole, instruction=instruction, root=str(self.dir),
                                        outputs=["error_summary.txt"])
        self.assertEqual(len(found), 1)
        self.assertIn("error_summary.txt was not made", found[0])
        # A name read only from the words is evidence for the judge (review 2026-11-03, E-10), never a block.
        notes = []
        self.assertEqual(long_task.code_problems([], "done", [], whole, instruction=instruction, root=str(self.dir),
                                                 notes=notes), [])
        self.assertIn("error_summary.txt", " ".join(notes))
        (self.dir / "error_summary.txt").write_text("api: 3")
        self.assertEqual(long_task.code_problems([str(self.dir / "error_summary.txt")], "done", [], whole,
                                                 instruction=instruction, root=str(self.dir),
                                                 outputs=["error_summary.txt"]), [])

    @unittest.skipIf(Image is None, "Pillow is not installed")
    def test_a_stages_picture_and_document_are_measured_like_the_gates(self):
        from decurix_agent import long_task

        Image.new("RGB", (1200, 600)).save(self.dir / "logo.png")
        Image.new("RGB", (1200, 600)).save(self.dir / "logo_small.png")
        letters = docx(self.dir / "r.docx", para("One\\fTwo"))
        instruction = "Resize logo.png to 50% and save it as logo_small.png. Each letter in r.docx on its own page."
        notes = []
        found = " ".join(long_task.code_problems([str(self.dir / "logo_small.png"), letters], "done", [], instruction,
                                                 instruction=instruction, root=str(self.dir), notes=notes))
        self.assertIn("it was not resized", " ".join(notes))  # evidence
        self.assertIn("typed as text", found)  # hygiene: blocking

    def test_a_stage_asked_for_formulas_is_sent_back_for_a_typed_total_and_a_proven_tool_is_too(self):
        from decurix_agent import long_task, tool_task

        typed = ["gst.xlsx: the Total row holds typed numbers, not formulas - Register!E14"]
        with mock.patch.object(produced, "store_results", lambda paths: []), \
                mock.patch.object(produced, "typed_totals", lambda paths, request="": list(typed)):
            facts = produced.file_facts("Add the columns with formulas and a Total row.", [])
            self.assertEqual(facts.typed_totals, typed)
            notes = []
            self.assertEqual(long_task.code_problems([], "done", [], "Add the columns with formulas and a Total row.",
                                                     root=str(self.dir), notes=notes), [])
            self.assertIn("typed numbers", " ".join(notes))  # evidence for the judge, never a verdict
            noted = produced.file_facts("Add the columns and a Total row.", [])
            self.assertEqual((noted.typed_totals, noted.totals_note), ([], typed))
        self.assertTrue(hasattr(tool_task, "produced"))


class TypedEscapes(unittest.TestCase):
    """The typing-time refusal is narrow (a lone escape token); run-on prose is the gate's, see test_typed_paths."""

    def test_a_lone_backslash_sequence_is_refused_with_how_to_press_it(self):
        with mock.patch.object(cli.core, "act") as act, mock.patch("sys.stdout", new_callable=io.StringIO):
            with self.assertRaises(ComputerError) as caught:
                cli.run(["type", "e12", "Please pay. \\f Kaveri Electricals"])
        act.assert_not_called()
        self.assertIn("gui key ctrl+Return", str(caught.exception))

    def test_a_windows_path_keeps_its_backslashes(self):
        self.assertEqual(cli._unsupported_escape("C:\\Users\\me\\files\\report.txt"), "")
        self.assertEqual(cli._unsupported_escape("a\\tb\\nc"), "")
        self.assertEqual(cli._unsupported_escape("letter one \\f letter two"), "\\f")


class TheJudgeReadsTheReportsOwnEvidence(unittest.TestCase):
    def test_its_rules_make_a_contradiction_not_done(self):
        self.assertIn("contradicts what the request asks", verify.JUDGE_SYSTEM)


if __name__ == "__main__":
    unittest.main()
