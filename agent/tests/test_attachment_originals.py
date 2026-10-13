import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import attachments, produced


class WhatThePersonSentIsKept(unittest.TestCase):
    def test_a_file_changed_in_place_is_read_as_it_arrived(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            inbox = root / "work" / "inbox" / "up-1"
            inbox.mkdir(parents=True)
            counter = inbox / "last_invoice_number.txt"
            counter.write_text("INV-2026-0087\n")
            request = "Bill September.\n\n" + attachments.block([str(counter)])
            with mock.patch("decurix_agent.config.DECURIX_DIR", root / ".decurix"):
                attachments.keep_originals(request, "t1")
                self.assertEqual(attachments.original(str(counter), "t1"), str(counter))  # unchanged: the file itself
                counter.write_text("INV-2026-0090\n")
                kept = attachments.original(str(counter), "t1")
                self.assertNotEqual(kept, str(counter))
                self.assertEqual(Path(kept).read_text(), "INV-2026-0087\n")
                self.assertEqual(attachments.original(str(counter), "t2"), str(counter))  # another task kept nothing


class OnlyAFilePlacedInTheWorkFolderCanBeMisplaced(unittest.TestCase):
    def test_an_attachment_the_request_says_to_update_is_not_misplaced(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "inbox" / "up-1").mkdir(parents=True)
            (root / "inbox" / "up-1" / "last_invoice_number.txt").write_text("INV-2026-0090")
            request = ("Number invoices on from last_invoice_number.txt and update that file when done. Each invoice is a PDF "
                       "named <NO>.pdf in my work folder.")
            with mock.patch.object(produced, "since", return_value=0):
                self.assertEqual(produced.misplaced(request, root=root), "")
                request2 = "Sort it and save it as last_invoice_number.txt in my work folder."
                self.assertIn("not in the work folder", produced.misplaced(request2, root=root))


if __name__ == "__main__":
    unittest.main()
