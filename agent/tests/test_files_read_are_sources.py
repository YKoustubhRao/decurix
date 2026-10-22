import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import long_task, task_context
from decurix_agent.computer import evidence


# ============================================================
# GST suite T9 (2026-10-21): invoice numbers and amounts read from the
# client's workbook (in the person's folder, never named by file in the
# request) were refused as invented when the portal form was saved.
# ============================================================

class TestAFileTheWorkReadIsASource(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        for patcher in (mock.patch.object(evidence, "NOTES_DIR", self.dir / "notes"),
                        mock.patch.object(task_context, "current", return_value="t9")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def book(self):
        import openpyxl
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.append(["Invoice No", "Party", "Taxable Value"])
        ws.append([451, "COASTAL PROVISIONS", 26000])
        ws.append([458, "KARAVALI SUPERMART", 17700])
        path = self.dir / "COMPUTATION_UAT_2627.xlsx"
        wb.save(path)
        return path

    def test_files_read_are_listed_once_and_only_while_they_exist(self):
        path = self.book()
        evidence.note_file_read(str(path))
        evidence.note_file_read(str(path))
        evidence.note_file_read(str(self.dir / "gone.xlsx"))
        self.assertEqual(evidence.files_read(), [str(path)])

    def test_a_value_from_a_file_the_work_read_is_not_unsourced(self):
        request = "File Udupi Agro Traders' September GSTR-1 on the portal from their computation workbook."
        before = long_task.given_sources(request)
        self.assertTrue(before.unsourced("Total invoice value 27,300 for invoice 26000"))
        evidence.note_file_read(str(self.book()))
        self.assertEqual(long_task.given_sources(request).unsourced("Taxable value 17,700"), [])


class TestWhatACommandPrintedIsRecordedWorking(unittest.TestCase):
    """GST suite T7: totals a script summed from the invoices and printed were refused as held by no source."""

    def test_a_printed_total_is_the_works_own_working_not_a_given_source(self):
        notes = Path(tempfile.mkdtemp())
        notes.joinpath("t7.outputs").write_text('{"at": 1, "text": "b2c taxable 67909.30 cgst 6111.84"}\n',
                                                encoding="utf-8")
        with mock.patch.object(evidence, "NOTES_DIR", notes), mock.patch.object(task_context, "current", return_value="t7"):
            request = "Prepare the GSTR-1 from the invoices in the September folder."
            self.assertEqual(long_task.work_sources(request).unsourced("Taxable value 67,909.30"), [])
            self.assertTrue(long_task.given_sources(request).unsourced("Taxable value 67,909.30"))


if __name__ == "__main__":
    unittest.main()
