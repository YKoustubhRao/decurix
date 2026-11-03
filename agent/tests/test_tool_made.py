"""
CS (round 5 P1, 2026-10-20): the memo the tools made in the person's shared folder was saved over from Word -
a stale copy Word still held, then a blank one - and read back with 0 headings. What the tools save there is kept
aside; an app of ours holding the file lets it go before the tools write it and before an app is opened; a document
an app's save emptied is found, put back, and the stage is told; a save into a read-only or blank document is refused.

    PYTHONPATH="agent;agent/tests" python -m unittest test_tool_made
"""

from needs import needs
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, tool_made
from decurix_agent.computer import core
from decurix_agent.computer.errors import ComputerError
from decurix_agent.tools import office

MEMO = [{"heading": "Exceptions Memo - October 2026", "level": 1}] + [
    {"paragraph": f"Item {n}: duplicate bill BCP-7781 counted once, rate problem on SP-2291 held out of ITC, "
                  f"bank item {n} not yet in our books for Rs {n},250.00."} for n in range(1, 8)]
HOST = r"C:\Users\Person\Documents\Shared\Exceptions_memo_Oct2026.docx"


@needs("python-docx")
class WhatTheToolsMadeIsKept(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.doc = self.root / "PC" / "Exceptions_memo_Oct2026.docx"
        self.doc.parent.mkdir()
        self.released = []
        for patch in (mock.patch.object(config, "DECURIX_DIR", self.root / "decurix"),
                      mock.patch.object(tool_made, "_host", lambda local: HOST if "PC" in str(local) else ""),
                      mock.patch.object(tool_made, "release", lambda hosts: self.released.extend(hosts) or [])):
            patch.start()
            self.addCleanup(patch.stop)

    def test_an_emptied_document_is_found_and_put_back(self):
        office.create_document(str(self.doc), MEMO)
        tool_made.remember(self.doc)
        self.assertEqual(tool_made.emptied([self.doc]), [])
        # The app saves a blank document over it.
        office.create_document(str(self.doc.with_name("blank.docx")), [{"paragraph": "Exceptions Memo"}])
        self.doc.write_bytes(self.doc.with_name("blank.docx").read_bytes())
        lost = tool_made.emptied([self.doc])
        self.assertEqual(len(lost), 1)
        self.assertLess(lost[0][2], lost[0][1] / 2)
        found = long_task.app_emptied([str(self.doc)])
        self.assertEqual(len(found), 1)
        self.assertIn("The tools' version is put back", found[0])
        self.assertIn("never retype it", found[0])
        self.assertIn(HOST, self.released)  # an app of ours holding it let it go first
        self.assertIn("SP-2291", office.read_document(str(self.doc))["text"])
        self.assertEqual(tool_made.emptied([self.doc]), [])
        self.assertTrue(long_task.is_blocking("app_emptied"))

    def test_an_edit_in_the_app_that_keeps_the_content_is_not_a_finding(self):
        office.create_document(str(self.doc), MEMO)
        tool_made.remember(self.doc)
        from docx import Document
        doc = Document(str(self.doc))  # a figure corrected in the app
        doc.paragraphs[1].runs[0].text = doc.paragraphs[1].runs[0].text.replace("1,250.00", "1,350.00")
        doc.save(str(self.doc))
        self.assertEqual(tool_made.emptied([self.doc]), [])

    def test_the_office_command_releases_before_it_writes_and_keeps_what_it_saved(self):
        office.create_document(str(self.doc), MEMO[:2] + [{"paragraph": "Prepared by {{NAME}}"}])  # there already
        spec = json.dumps({"replace": {"{{NAME}}": "Accounts"}})
        self.assertEqual(office.main(["update", str(self.doc), spec]), 0)
        self.assertEqual(self.released, [HOST])
        office.create_document(str(self.doc.with_name("full.docx")), MEMO + [{"paragraph": "Prepared by {{NAME}}"}])
        self.doc.write_bytes(self.doc.with_name("full.docx").read_bytes())
        self.assertEqual(office.main(["update", str(self.doc), spec]), 0)
        self.assertEqual([str(Path(p)) for p in tool_made.remembered()], [str(self.doc.resolve())])

    def test_before_an_app_of_the_persons_computer_opens_the_tools_files_are_let_go(self):
        office.create_document(str(self.doc), MEMO)
        tool_made.remember(self.doc)
        tool_made.held_by_our_apps()
        self.assertEqual(self.released, [HOST])

    def test_a_file_outside_the_shared_folder_is_not_kept(self):
        local = self.root / "work" / "memo.docx"
        local.parent.mkdir()
        office.create_document(str(local), MEMO)
        tool_made.remember(local)
        self.assertEqual(tool_made.remembered(), [])


class ASaveIntoAReadOnlyOrBlankDocument(unittest.TestCase):

    def page(self, *names):
        return {"page_url": "http://host.docker.internal:8080/host-gui/view?id=x", "active_title": "Word - Chromium",
                "elements": [{"name": n} for n in names]}

    def test_is_refused(self):
        for names in (("Exceptions_memo_Oct2026 - Read-Only",), ("Document1",), ("Document2 - Word",)):
            with self.assertRaises(ComputerError, msg=names):
                core._refuse_unsafe_save("key", None, ["ctrl+s"], self.page(*names))
        with self.assertRaises(ComputerError):
            core._refuse_unsafe_save("click", {"name": "Save"}, None, self.page("Book1"))

    def test_a_save_of_the_file_itself_goes_ahead(self):
        for names in (("Exceptions_memo_Oct2026 - Compatibility Mode", "Page 1 content"), ("Exceptions_memo_Oct2026",)):
            core._refuse_unsafe_save("key", None, ["ctrl+s"], self.page(*names))
        # Not a save, or not an app of the person's computer: nothing to refuse.
        core._refuse_unsafe_save("key", None, ["ctrl+c"], self.page("Document1"))
        core._refuse_unsafe_save("key", None, ["ctrl+s"], {"page_url": "file:///x", "elements": [{"name": "Document1"}]})


if __name__ == "__main__":
    unittest.main()
