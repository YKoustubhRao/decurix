"""Native OpenDocument evidence is read from saved body content, not claims."""
import tempfile
import unittest
import zipfile
from pathlib import Path

from decurix_agent.workflow_runner import WorkflowRunner


class OpenDocumentEvidence(unittest.TestCase):
    def run_document(self, fmt, mime, text):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        def produce(stage, context):
            path = Path(context["work_dir"]) / ("out." + fmt)
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("mimetype", "application/vnd.oasis.opendocument." + mime)
                archive.writestr("content.xml", text)
        runner = WorkflowRunner(temporary.name, produce)
        runner.create({"id": "native-artifact", "steps": [{"id": "save", "instruction": "Save requested output",
                       "checks": [{"path": "out." + fmt, "format": fmt, "contains": ["verified rows"]}]}]}, {}, "run")
        return runner.advance("run")

    def content(self, body, value):
        return ('<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" '
                'xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body>'
                f'<office:{body}><text:p>{value}</text:p></office:{body}></office:body></office:document-content>')

    def test_native_saved_document_formats_accept_checked_visible_body(self):
        for fmt, body in [("ods", "spreadsheet"), ("odt", "text"), ("odp", "presentation")]:
            with self.subTest(fmt=fmt):
                self.assertEqual(self.run_document(fmt, body, self.content(body, "verified rows"))["status"], "completed")

    def test_wrong_mime_or_comment_marker_does_not_verify_saved_content(self):
        self.assertEqual(self.run_document("ods", "text", self.content("spreadsheet", "verified rows"))["status"], "failed")
        self.assertEqual(self.run_document("odt", "text", self.content("text", "wrong content<!-- verified rows -->"))["status"], "failed")


if __name__ == "__main__":
    unittest.main()
