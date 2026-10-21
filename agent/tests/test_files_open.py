"""
Series 14 P3 on pc3 (2026-10-21): all three workbooks the run handed in were corrupt - the worker's own python script
rewrote their XML (Decision / Reason columns as shared-string cells) and never registered xl/sharedStrings.xml in the
workbook's relationships, so openpyxl ("IndexError: list index out of range") and Excel could not open them. The stages
were reported checked; the stage that broke them handed in another file. Hygiene, certain, blocking: every Office file
the work hands in opens with a strict reader - the stage's own and every file the run hands in - and office read says a
broken file is broken.
"""

import json
import re
import tempfile
import unittest
import zipfile
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import produced
from decurix_agent.tools import office

LIVE_SPEC = Path(r"C:\Users\Administrator\decurix-tests\wide\process\out\P3\20261021-102912\worker\P3_payment_run"
                 r"\outputs\ap_register_spec.json")


def break_shared_strings(path):
    """The live breakage: a cell turned into a shared-string cell, a sharedStrings part nothing registers."""

    with zipfile.ZipFile(path) as z:
        parts = {n: z.read(n) for n in z.namelist()}
    sheet = parts["xl/worksheets/sheet1.xml"].decode("utf-8")
    sheet = re.sub(r'<c r="A1"([^>]*?) t="inlineStr"><is><t[^>]*>[^<]*</t></is></c>',
                   r'<c r="A1"\1 t="s"><v>3</v></c>', sheet, count=1)
    parts["xl/worksheets/sheet1.xml"] = sheet.encode("utf-8")
    parts["xl/sharedStrings.xml"] = (b'<?xml version="1.0" encoding="utf-8"?><sst xmlns="http://schemas.openxmlformats'
                                     b'.org/spreadsheetml/2006/main" count="1" uniqueCount="1"><si><t>Decision</t></si>'
                                     b'</sst>')
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        for name, data in parts.items():
            z.writestr(name, data)


class OfficeFilesOpen(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)  # (other file checks of a broken file may leave it open on Windows)
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)

    def book(self, name="register.xlsx"):
        path = self.dir / name
        office.create_workbook(str(path), [{"name": "Register", "rows": [["Vendor", "Amount"], ["Apex", 1200]]}])
        return path

    def test_a_workbook_with_an_unregistered_shared_string_table_cannot_be_opened(self):
        good = self.book()
        self.assertEqual(produced.unopenable([str(good)]), [])
        bad = self.book("broken.xlsx")
        break_shared_strings(bad)
        found = produced.unopenable([str(bad), str(good)])
        self.assertEqual(len(found), 1)
        self.assertIn("broken.xlsx cannot be opened", found[0])
        # Blocking hygiene in every route's file facts.
        self.assertTrue(any("broken.xlsx cannot be opened" in f for f in produced.file_facts("", [str(bad)]).file_faults))
        # office read says so, never reads it as if it were whole.
        with self.assertRaises(office.OfficeError) as said:
            office.read_document(str(bad))
        self.assertIn("cannot be opened", str(said.exception))

    def test_a_broken_document_and_deck_are_found_too(self):
        for name in ("letter.docx", "deck.pptx"):
            path = self.dir / name
            path.write_bytes(b"PK\x03\x04 not a document")
            self.assertTrue(produced.unopenable([str(path)]), name)

    def test_office_update_adds_a_column_rows_and_a_sheet_and_the_file_opens(self):
        book = self.book()
        office.update_workbook(str(book), edits=[{"sheet": "Register", "cell": "C1", "value": "Decision"},
                                                 {"sheet": "Register", "cell": "C2", "value": "Held"},
                                                 {"sheet": "Register", "cell": "A3", "value": "Total"},
                                                 {"sheet": "Register", "cell": "B3", "value": "=SUM(B2:B2)"}],
                               add_sheets=[{"name": "Notes", "rows": [["Approved as proposed."]]}])
        self.assertEqual(produced.unopenable([str(book)]), [])
        self.assertIn("never edit an Office file's XML", office.USAGE)

    @unittest.skipUnless(LIVE_SPEC.is_file(), f"the live spec is not here: {LIVE_SPEC}")
    def test_the_live_spec_round_trips_to_a_workbook_that_opens(self):
        spec = json.loads(LIVE_SPEC.read_text(encoding="utf-8"))
        out = self.dir / "AP_register.xlsx"
        office.create_workbook(str(out), spec["sheets"])
        self.assertEqual(produced.unopenable([str(out)]), [])
        import openpyxl
        self.assertEqual(openpyxl.load_workbook(out).sheetnames, [s["name"] for s in spec["sheets"]])


class TheRunsFiles(unittest.TestCase):
    """long_task.files_open: an earlier stage's file a later stage broke fails the later stage (blocking)."""

    def test_an_earlier_stages_workbook_a_later_stage_broke_is_found(self):
        from unittest import mock
        from decurix_agent import long_task
        self.assertTrue(long_task.is_blocking("files_open"))
        with tempfile.TemporaryDirectory(ignore_cleanup_errors=True) as tmp:
            work = Path(tmp)
            book = work / "AP_register.xlsx"
            office.create_workbook(str(book), [{"name": "Register", "rows": [["Vendor", "Amount"], ["Apex", 1200]]}])
            run_state = {"run_id": "r", "spec": {"steps": [{"id": "s1"}, {"id": "s2"}]},
                         "steps": {"s1": {"status": "completed", "evidence": [{"path": "AP_register.xlsx"}]},
                                   "s2": {"status": "running", "evidence": []}}}
            runner = mock.Mock()
            runner.context.return_value = {"work_dir": str(work)}
            with mock.patch.object(long_task, "_runner", lambda: runner), \
                    mock.patch.object(long_task, "_steps", lambda state: state["spec"]["steps"]):
                self.assertEqual(long_task.files_open(run_state, [str(work / "s5.csv")]), [])
                break_shared_strings(book)
                found = long_task.files_open(run_state, [str(work / "s5.csv")])
        self.assertEqual(len(found), 1)
        self.assertIn("A file this run hands in no longer opens: AP_register.xlsx cannot be opened", found[0])


if __name__ == "__main__":
    unittest.main()
