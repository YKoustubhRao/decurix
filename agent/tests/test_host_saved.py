import unittest
from unittest import mock

from decurix_agent import produced

EXCEL = ("Use Excel (the Excel app on your desktop) to open sales_q3.csv from the \"Decurix demo\" folder in my "
         "Documents (C:\\Users\\Administrator\\Documents\\Decurix demo). Sort the rows by Amount from largest to "
         "smallest. Save it in that same folder as an Excel workbook named sales_q3_report.xlsx.")
FOLDER = "C:\\Users\\Administrator\\Documents\\Decurix demo"


def disk(files, folders=(FOLDER,), modified="2030-01-01T00:00:00Z", kind=None):
    """A stand-in for the control plane's answer about the person's disk."""

    asked = []

    def ask(path):
        asked.append(path)
        if path in folders:
            return {"exists": True, "folder": True}
        if path in files:
            return {"exists": True, "folder": False, "modified": modified, "kind": kind}
        return {"exists": False}

    ask.asked = asked
    return ask


class MissingOnHost(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.object(produced, "since", return_value=1_800_000_000.0)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_saved_workbook_passes(self):
        ask = disk({FOLDER + "\\sales_q3_report.xlsx"})
        self.assertEqual(produced.missing_on_host(EXCEL, ask=ask), "")
        self.assertIn(FOLDER, ask.asked)

    def test_a_workbook_saved_with_another_type_is_missing(self):
        # Excel kept the CSV type: sales_q3_report.xlsx.csv is there, sales_q3_report.xlsx is not.
        why = produced.missing_on_host(EXCEL, ask=disk({FOLDER + "\\sales_q3_report.xlsx.csv"}))
        self.assertIn("sales_q3_report.xlsx is not in", why)
        self.assertNotIn("sales_q3.csv", why)

    def test_a_file_left_from_before_the_task_was_not_saved_now(self):
        why = produced.missing_on_host(EXCEL, ask=disk({FOLDER + "\\sales_q3_report.xlsx"},
                                                       modified="2020-01-01T00:00:00Z"))
        self.assertIn("before this task began", why)

    def test_a_csv_saved_under_a_workbooks_name_is_not_the_workbook(self):
        why = produced.missing_on_host(EXCEL, ask=disk({FOLDER + "\\sales_q3_report.xlsx"}, kind="text"))
        self.assertIn("not a real xlsx file", why)
        self.assertEqual(produced.missing_on_host(EXCEL, ask=disk({FOLDER + "\\sales_q3_report.xlsx"}, kind="zip")), "")

    def test_a_folder_that_is_not_there_decides_nothing(self):
        self.assertEqual(produced.missing_on_host(EXCEL, ask=disk(set(), folders=())), "")

    def test_a_control_plane_that_cannot_be_asked_decides_nothing(self):
        self.assertEqual(produced.missing_on_host(EXCEL, ask=lambda path: None), "")

    def test_a_request_with_no_host_path_asks_nothing(self):
        def ask(path):
            raise AssertionError("asked about " + path)

        self.assertEqual(produced.missing_on_host("Save it as report.xlsx in my work folder.", ask=ask), "")

    def test_a_whole_path_is_looked_for_where_it_says(self):
        request = "Write the letter in Word and save it as C:\\Users\\me\\Desktop\\reply.docx."
        self.assertEqual(produced.missing_on_host(request, ask=disk({"C:\\Users\\me\\Desktop\\reply.docx"})), "")
        self.assertIn("reply.docx", produced.missing_on_host(request, ask=disk(set())))

    def test_word_wording(self):
        request = ("Use Word to write a reply. Save it as a Word document named reply_55821.docx in the \"Decurix "
                   "demo\" folder in my Documents (C:\\Users\\Administrator\\Documents\\Decurix demo).")
        self.assertEqual(produced.missing_on_host(request, ask=disk({FOLDER + "\\reply_55821.docx"})), "")
        self.assertIn("reply_55821.docx", produced.missing_on_host(request, ask=disk(set())))


if __name__ == "__main__":
    unittest.main()


class CopyFromHost(unittest.TestCase):
    def test_a_saved_host_file_is_copied_for_the_check(self):
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as home, \
                mock.patch.object(produced.config, "DECURIX_DIR", Path(home)), \
                mock.patch.object(produced, "since", return_value=1_800_000_000.0):
            good = produced.saved_on_host(EXCEL, ask=disk({FOLDER + "\\sales_q3_report.xlsx"}, kind="zip"))
            self.assertEqual(good, [FOLDER + "\\sales_q3_report.xlsx"])
            copies = produced.copy_from_host(good, fetch=lambda path: b"PK\x03\x04data")
            self.assertEqual([c.name for c in copies], ["sales_q3_report.xlsx"])
            self.assertEqual(copies[0].read_bytes(), b"PK\x03\x04data")

    def test_a_missing_file_is_not_copied(self):
        self.assertEqual(produced.saved_on_host(EXCEL, ask=disk(set())), [])
