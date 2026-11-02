"""
The `office` shell command as TheBot uses it: spreadsheet changes that keep
the workbook's charts, and email drafts that go to the Outbox, never out.
"""

import io
import json
import os
import tempfile
import unittest
import zipfile
from contextlib import redirect_stdout
from unittest import mock

from decurix_agent.tools import office, outbox

WORKBOOK = {
    "[Content_Types].xml": '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
    "xl/workbook.xml": ('<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" '
                        'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">'
                        '<sheets><sheet name="Data" sheetId="1" r:id="rId1"/></sheets></workbook>'),
    "xl/_rels/workbook.xml.rels": ('<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                                   '<Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/></Relationships>'),
    "xl/worksheets/sheet1.xml": ('<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
                                 '<dimension ref="A1:B3"/><sheetData>'
                                 '<row r="2"><c r="A2" t="inlineStr"><is><t>Jan</t></is></c><c r="B2"><v>100</v></c></row>'
                                 '<row r="3"><c r="A3" t="inlineStr"><is><t>Feb</t></is></c><c r="B3"><v>120</v></c></row>'
                                 '</sheetData></worksheet>'),
    "xl/charts/chart1.xml": '<?xml version="1.0"?><c:chartSpace xmlns:c="x"><c:f>Data!$B$2:$B$3</c:f></c:chartSpace>',
}


def make_workbook(path):
    with zipfile.ZipFile(path, "w") as z:
        for name, text in WORKBOOK.items():
            z.writestr(name, text)


def run(argv, stdin=None):
    out = io.StringIO()
    with redirect_stdout(out), mock.patch("sys.stdin", io.StringIO(stdin or "")):
        code = office.main(argv)
    return code, out.getvalue()


class UpdatingASpreadsheetKeepsItsChart(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.book = os.path.join(self.dir.name, "mis.xlsx")
        make_workbook(self.book)

    def tearDown(self):
        self.dir.cleanup()

    def read(self, name):
        with zipfile.ZipFile(self.book) as z:
            return z.read(name).decode()

    def test_a_spec_on_stdin_adds_a_month_and_the_chart_takes_it_in(self):
        spec = {"edits": [{"sheet": "Data", "cell": "A4", "value": "Mar"}, {"sheet": "Data", "cell": "B4", "value": 135}],
                "replace_refs": [{"from": "Data!$B$2:$B$3", "to": "Data!$B$2:$B$4"}]}
        code, text = run(["update", self.book, "-"], stdin=json.dumps(spec))

        self.assertEqual(code, 0, text)
        sheet = self.read("xl/worksheets/sheet1.xml")
        self.assertIn('r="B4"', sheet)
        self.assertIn("135", sheet)
        self.assertIn("Data!$B$2:$B$4", self.read("xl/charts/chart1.xml"))
        self.assertTrue(os.path.exists(os.path.join(self.dir.name, "mis.orig.xlsx")), "the original was not kept")

    def test_a_spec_can_be_given_inline(self):
        code, text = run(["update", self.book, '{"edits": [{"sheet": "Data", "cell": "B2", "value": 99}]}'])
        self.assertEqual(code, 0, text)
        self.assertIn("<v>99</v>", self.read("xl/worksheets/sheet1.xml"))

    def test_a_byte_order_mark_in_front_of_the_spec_is_not_part_of_it(self):
        # What Windows PowerShell puts in front of anything it pipes: found
        # live, every piped spec was refused as "not valid JSON".
        code, text = run(["update", self.book, "-"], stdin='﻿{"edits": [{"sheet": "Data", "cell": "B2", "value": 77}]}')
        self.assertEqual(code, 0, text)
        self.assertIn("<v>77</v>", self.read("xl/worksheets/sheet1.xml"))

    def test_a_bad_spec_says_so_and_changes_nothing(self):
        before = self.read("xl/worksheets/sheet1.xml")
        code, text = run(["update", self.book, "-"], stdin="edits: B2=99")
        self.assertEqual(code, 1)
        self.assertIn("not valid JSON", text)
        self.assertEqual(before, self.read("xl/worksheets/sheet1.xml"))


    def test_a_formula_edit_is_written_not_erased(self):
        # Live: {"cell": "D2", "formula": ...} was read as "empty the cell".
        code, text = run(["update", self.book, '{"edits": [{"sheet": "Data", "cell": "D2", "formula": "=SUM(B2:B4)"}]}'])
        self.assertEqual(code, 0, text)
        self.assertIn("<f>SUM(B2:B4)</f>", self.read("xl/worksheets/sheet1.xml"))

    def test_an_edit_with_no_value_is_refused_not_taken_as_empty(self):
        before = self.read("xl/worksheets/sheet1.xml")
        code, text = run(["update", self.book, '{"edits": [{"sheet": "Data", "cell": "B2"}]}'])
        self.assertEqual(code, 1)
        self.assertIn("has no value", text)
        self.assertEqual(before, self.read("xl/worksheets/sheet1.xml"))

    def test_a_range_matches_whether_or_not_the_sheet_name_is_quoted(self):
        # The file says Data!$B$2:$B$3; a model wrote 'Data'!... (live, the
        # other way round, and nothing was rewritten on its first try).
        spec = {"replace_refs": [{"from": "'Data'!$B$2:$B$3", "to": "'Data'!$B$2:$B$4"}]}
        code, text = run(["update", self.book, json.dumps(spec)])
        self.assertEqual(code, 0, text)
        self.assertIn("Data!$B$2:$B$4", self.read("xl/charts/chart1.xml"))
        self.assertIn("1 range reference", text)


class AnEmailIsDraftedNeverSent(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.statement = os.path.join(self.dir.name, "statement.pdf")
        with open(self.statement, "wb") as f:
            f.write(b"%PDF-1.4 statement")
        self.posted = []

        def urlopen(request, timeout=None):
            self.posted.append((request.full_url, json.loads(request.data)))
            return io.BytesIO(b'{"id": 7}')

        patcher = mock.patch("decurix_agent.tools.outbox.urllib.request.urlopen", side_effect=urlopen)
        patcher.start()
        self.addCleanup(patcher.stop)
        # The temporary folder is the work folder: only a file of the work is attached (privacy.file_may_leave).
        from pathlib import Path
        work = mock.patch.object(outbox.config, "WORK_DIR", Path(self.dir.name))
        work.start()
        self.addCleanup(work.stop)

    def tearDown(self):
        self.dir.cleanup()

    def test_a_file_from_a_hidden_folder_or_outside_the_work_is_never_attached(self):
        # Review 2026-11-02: any path could be attached, ~/.ssh or ~/.claude/.credentials.json among them.
        hidden = os.path.join(self.dir.name, ".ssh")
        os.mkdir(hidden)
        with open(os.path.join(hidden, "id_rsa"), "w", encoding="utf-8") as f:
            f.write("key")
        outside = tempfile.TemporaryDirectory()
        self.addCleanup(outside.cleanup)
        elsewhere = os.path.join(outside.name, "credentials.json")
        with open(elsewhere, "w", encoding="utf-8") as f:
            f.write("{}")
        for path in (os.path.join(hidden, "id_rsa"), elsewhere, os.path.join(self.dir.name, "..", os.path.basename(outside.name),
                                                                             "credentials.json")):
            code, text = run(["email", "--to", "a@b.example", "--subject", "s", "--attach", path], stdin="body")
            self.assertEqual(code, 1, text)
            self.assertIn("is not attached", text)
        self.assertEqual(self.posted, [])

    def test_the_subject_and_body_go_through_the_secret_withholding(self):
        code, text = run(["email", "--to", "ap@acme.example", "--subject", "Portal password: Lift$2026",
                          "--attach", self.statement],
                         stdin="Dear Acme,\n\nGSTIN 29ABCDE1234F1Z5, Rs 1,25,000.00 due.\npassword: Lift$2026\n")
        self.assertEqual(code, 0, text)
        _, payload = self.posted[0]
        self.assertNotIn("Lift$2026", json.dumps(payload))
        self.assertIn("29ABCDE1234F1Z5, Rs 1,25,000.00 due.", payload["body"], "ordinary figures stay as they are")

    def test_it_goes_to_the_outbox_with_its_attachment(self):
        code, text = run(["email", "--to", "ap@acme.example", "--cc", "me@us.example", "--subject", "Overdue statement",
                          "--attach", self.statement], stdin="Dear Acme,\n\nPlease find your statement attached.\n")

        self.assertEqual(code, 0, text)
        self.assertIn("has NOT been sent", text)
        url, payload = self.posted[0]
        self.assertTrue(url.endswith("/api/outbox"))
        self.assertEqual(payload["to"], ["ap@acme.example"])
        self.assertEqual(payload["cc"], ["me@us.example"])
        self.assertEqual(payload["subject"], "Overdue statement")
        self.assertIn("Please find your statement attached.", payload["body"])
        self.assertEqual([a["name"] for a in payload["attachments"]], ["statement.pdf"])

    def test_nothing_is_drafted_without_a_recipient_a_body_or_the_attachment(self):
        for argv, stdin, why in (
            (["email", "--subject", "s"], "body", "at least one recipient"),
            (["email", "--to", "a@b.example", "--subject", "s"], "", "no body"),
            (["email", "--to", "a@b.example", "--subject", "s", "--attach", os.path.join(self.dir.name, "nope.pdf")],
             "body", "does not exist"),
        ):
            code, text = run(argv, stdin=stdin)
            self.assertEqual(code, 1, text)
            self.assertIn(why, text)
        self.assertEqual(self.posted, [])

    def test_the_older_harness_drafts_through_the_same_code(self):
        with mock.patch.object(outbox, "draft", return_value="Drafted (id 9)") as draft:
            from decurix_agent import azure_harness
            self.assertEqual(azure_harness._draft_email({"to": ["a@b.c"], "subject": "s", "body": "b"}), "Drafted (id 9)")
        draft.assert_called_once()


if __name__ == "__main__":
    unittest.main()


class CreatingAWorkbook(unittest.TestCase):
    """office create never says "Created" over data it did not write."""

    def setUp(self):
        try:
            import openpyxl  # noqa: F401
        except ImportError:
            self.skipTest("openpyxl is only in the desktop images")
        self.dir = tempfile.mkdtemp()
        self.path = os.path.join(self.dir, "invoices.xlsx")

    def values(self):
        import openpyxl
        ws = openpyxl.load_workbook(self.path).active
        return {c.coordinate: c.value for row in ws.iter_rows() for c in row if c.value is not None}

    # The spec the engine sent live: cells by address, a formula as an object.
    def test_cells_by_address_are_written(self):
        spec = {"sheets": [{"name": "Invoices", "cells": {
            "A1": "Invoice", "B1": "Customer", "C1": "Amount",
            "A2": "INV-101", "B2": "Acme", "C2": 1200,
            "A3": "INV-102", "B3": "Globex", "C3": 850,
            "B4": "Total", "C4": {"formula": "SUM(C2:C3)"}}}]}
        code, out = run(["create", self.path, "-"], stdin=json.dumps(spec))
        self.assertEqual(code, 0, out)
        v = self.values()
        self.assertEqual(v["B2"], "Acme")
        self.assertEqual(v["C3"], 850)
        self.assertEqual(v["C4"], "=SUM(C2:C3)")
        self.assertIn("4 rows x 3 columns, 11 cells filled", out)

    def test_rows_and_cells_together(self):
        office.create_workbook(self.path, [{"name": "S", "rows": [["a", "b"], [1, 2]], "cells": {"C2": "=A2+B2"}}])
        self.assertEqual(self.values()["C2"], "=A2+B2")

    def test_a_sheet_with_no_data_is_refused_and_nothing_is_written(self):
        code, out = run(["create", self.path, "-"], stdin=json.dumps({"sheets": [{"name": "Invoices"}]}))
        self.assertEqual(code, 1)
        self.assertIn("has no data", out)
        self.assertFalse(os.path.exists(self.path))

    def test_a_key_it_does_not_read_is_refused(self):
        with self.assertRaises(office.OfficeError) as e:
            office.create_workbook(self.path, [{"name": "S", "data": [["a"]]}])
        self.assertIn("'data'", str(e.exception))
        self.assertFalse(os.path.exists(self.path))
