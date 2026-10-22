import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent.computer import core


# ============================================================
# A click that downloads says what it downloaded (GST suite T1, 2026-10-21):
# the result only said the link was focused, and a second click on the same
# attachment went unnoticed until its copy was saved under another name.
# ============================================================

class TestWhatAClickDownloaded(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        patcher = mock.patch.object(core, "_downloads_dir", return_value=self.dir)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_new_file_is_named_with_its_size(self):
        before = core._downloads_snapshot()
        (self.dir / "198_invoice.pdf").write_bytes(b"%PDF-198")
        note = core._downloads_note(before, wait=0)
        self.assertIn("Downloaded:", note)
        self.assertIn("198_invoice.pdf (8 bytes)", note)

    def test_the_same_attachment_downloaded_again_is_called_that(self):
        (self.dir / "197_invoice.pdf").write_bytes(b"%PDF-197")
        before = core._downloads_snapshot()
        (self.dir / "197_invoice (1).pdf").write_bytes(b"%PDF-197")
        note = core._downloads_note(before, wait=0)
        self.assertIn("197_invoice (1).pdf", note)
        self.assertIn("byte for byte the same file", note)

    def test_a_different_file_with_the_same_name_is_not_called_a_repeat(self):
        (self.dir / "report.pdf").write_bytes(b"%PDF-old")
        before = core._downloads_snapshot()
        (self.dir / "report (1).pdf").write_bytes(b"%PDF-new!")
        note = core._downloads_note(before, wait=0)
        self.assertIn("its content differs", note)

    def test_nothing_downloaded_says_nothing(self):
        (self.dir / "old.pdf").write_bytes(b"x")
        before = core._downloads_snapshot()
        self.assertEqual(core._downloads_note(before, wait=0), "")

    def test_a_download_still_running_is_reported_as_such(self):
        before = core._downloads_snapshot()
        (self.dir / "big.zip.crdownload").write_bytes(b"x")
        self.assertIn("Still downloading", core._downloads_note(before, wait=0))

    def test_no_download_folder_is_no_note(self):
        with mock.patch.object(core, "_downloads_dir", return_value=self.dir / "missing"):
            self.assertEqual(core._downloads_note(core._downloads_snapshot(), wait=0), "")


if __name__ == "__main__":
    unittest.main()
