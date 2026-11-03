"""
What `office update` filled is noted by the tool itself, so a placeholder its source held and the result still holds is
a mechanical fact whatever the request's words, a template filled in place included (review round 2, 2026-11-03).
A source that cannot be read is a check not made, never a pass.
"""

import unittest
from unittest import mock

from needs import needs
from temp_case import TempDirCase

from decurix_agent import config, produced


def letter(path, *lines):
    import docx
    d = docx.Document()
    for line in lines:
        d.add_paragraph(line)
    d.save(path)
    return str(path)


@needs("python-docx", "lxml")
class FilledFrom(TempDirCase):
    def setUp(self):
        super().setUp()
        for patch in (mock.patch.object(config, "DECURIX_DIR", self.dir / "own"),
                      mock.patch.object(config, "WORK_DIR", self.dir / "work")):
            patch.start()
            self.addCleanup(patch.stop)
        (self.dir / "work").mkdir()

    def test_a_template_filled_in_place_with_one_left_still_blocks(self):
        from decurix_agent.tools import office
        path = letter(self.dir / "work" / "notice.docx", "Dear {{NAME}},", "Due: {{DATE}}.")
        office.update_document(path, {"{{NAME}}": "Alex Doe"})
        found = produced.document_findings("Write the notice.", [path])
        self.assertEqual(len(found), 1, found)
        self.assertIn("{{DATE}}", found[0])

    def test_a_document_the_tool_did_not_fill_keeps_its_placeholders_as_evidence(self):
        path = letter(self.dir / "work" / "guide.docx", "Write <<Customer Name>> in the subject.")
        self.assertEqual(produced.document_findings("Write a guide.", [path]), [])
        self.assertTrue(produced.template_placeholder_notes("Write a guide.", [path]))

    def test_a_filled_source_that_cannot_be_read_is_not_made(self):
        source = self.dir / "work" / "form.docx"
        source.write_bytes(b"not a document")
        made = letter(self.dir / "work" / "out.docx", "Dear {{NAME}},")
        failed = []
        found = produced.document_findings("Fill form.docx and save it as out.docx.", [made], failed=failed)
        self.assertEqual(found, [])
        self.assertEqual(len(failed), 1, failed)
        self.assertIn("form.docx", failed[0])


if __name__ == "__main__":
    unittest.main()
