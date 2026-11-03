"""
A file name read from the request's words is evidence for the judge, never a block (review 2026-11-03, E-10); a
placeholder left in a made document blocks only when a document the task filled held it (E-11). Each check here used
to refuse correct work at certainty.
"""

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from needs import needs
from temp_case import TempDirCase

from decurix_agent import config, produced


class NamesReadFromWords(TempDirCase):
    def setUp(self):
        super().setUp()
        patch = mock.patch.object(config, "WORK_DIR", self.dir)
        patch.start()
        self.addCleanup(patch.stop)

    def test_correct_work_whose_request_names_other_files_is_not_refused(self):
        summary = self.dir / "summary.txt"
        summary.write_text("Figures: 1, 2, 3", encoding="utf-8")
        for request, made in (("Reply to the email. Do not create report.pdf - the figures go in the body.", []),
                              ("Write a two-line note to the client about invoice_0423.pdf, which is on the portal.", []),
                              ("Make a summary.txt of the figures in the client's statement.pdf on the portal.",
                               [str(summary)])):
            with self.subTest(request=request):
                facts = produced.file_facts(request, made, root=str(self.dir))
                self.assertEqual(facts.missing_files, "")
                self.assertEqual(facts.blocking(), [])

    def test_the_name_still_reaches_the_judge_as_evidence(self):
        facts = produced.file_facts("Write the summary and save it as error_summary.txt.", [], root=str(self.dir))
        self.assertEqual(facts.blocking(), [])
        self.assertTrue(any("error_summary.txt" in n for n in facts.notes()))

    def test_a_planned_stages_own_output_still_blocks(self):
        facts = produced.file_facts("Save it as error_summary.txt.", [], root=str(self.dir),
                                    named_in="Write the summary and save it as error_summary.txt.",
                                    own_outputs=["error_summary.txt"])
        self.assertIn("error_summary.txt was not made", facts.missing_files)


@needs("python-docx", "lxml")
class Placeholders(TempDirCase):
    def doc(self, name, *lines):
        import docx
        d = docx.Document()
        for line in lines:
            d.add_paragraph(line)
        d.save(self.dir / name)
        return str(self.dir / name)

    def test_a_requested_template_and_a_guide_keep_their_placeholders(self):
        template = self.doc("merge_template.docx", "Dear {{Name}},", "Your balance is {{Amount}}.")
        guide = self.doc("guide.docx", "Use <<Customer Name>> in the subject line.")
        with mock.patch.object(config, "WORK_DIR", self.dir):
            self.assertEqual(produced.document_findings("Make a mail-merge template merge_template.docx with {{Name}} "
                                                        "and {{Amount}} fields.", [template]), [])
            self.assertEqual(produced.document_findings("Write a how-to guide.", [guide]), [])
            notes = produced.template_placeholder_notes("Write a how-to guide.", [guide])
        self.assertTrue(notes and "<<Customer Name>>" in notes[0])

    def test_a_template_filled_with_its_placeholder_left_still_blocks(self):
        source = self.doc("offer_template.docx", "Dear {{FULL_NAME}},", "Start date: {{START}}.")
        made = self.doc("offer_alpha.docx", "Dear Alex Doe,", "Start date: {{START}}.")
        with mock.patch.object(config, "WORK_DIR", self.dir):
            found = produced.document_findings("Fill offer_template.docx for Alex Doe and save it as offer_alpha.docx.",
                                               [made])
        self.assertEqual(len(found), 1)
        self.assertIn("{{START}}", found[0])
        self.assertNotIn("{{FULL_NAME}}", found[0])
        self.assertTrue(Path(source).is_file())


if __name__ == "__main__":
    unittest.main()
