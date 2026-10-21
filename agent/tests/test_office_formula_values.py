"""
A formula a tool wrote has no saved result until a spreadsheet program opens the file. Read back, every total
showed "-> None" and the final check could not see what a correct model came to (found live, 2026-10-14).
"""

import io
import os
import re
import shutil
import tempfile
import unittest
import zipfile
from unittest import mock

try:
    import openpyxl
except ImportError:  # pragma: no cover
    openpyxl = None

from decurix_agent.tools import office


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class FormulaValues(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, ignore_errors=True)
        self.path = os.path.join(self.dir, "model.xlsx")
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "Data"
        ws.append(["Dept", "Budget", "Actual", "Var"])
        ws.append(["Eng", 100, 128.5, "=C2-B2"])
        ws.append(["Sales", 80, 70, "=C3-B3"])
        ws["D4"] = "=SUM(D2:D3)"
        wb.save(self.path)

    def test_formulas_with_no_saved_result_are_noticed(self):
        self.assertEqual(office.formulas_without_results(self.path), ["Data!D2", "Data!D3", "Data!D4"])

    def test_the_calculated_values_are_shown_and_said_to_be_calculated(self):
        calculated = openpyxl.Workbook()
        calculated.active.title = "Data"
        calculated["Data"]["D2"], calculated["Data"]["D3"], calculated["Data"]["D4"] = 28.5, -10, 18.5
        with mock.patch.object(office, "_recalculated_values", lambda p, o: calculated):
            text = office.read_workbook(self.path)
        self.assertIn("D4==SUM(D2:D3) -> 18.5", text)
        self.assertIn("calculated now by LibreOffice and are NOT in the file", text)
        # Said as a fault of the file, with the command that mends it - not only shown as calculated.
        self.assertIn("Formulas without stored results: 3 formula cell(s)", text)
        self.assertIn("office calculate", text)

    def test_without_libreoffice_it_reads_as_before(self):
        with mock.patch.object(office.shutil, "which", lambda name: None):
            text = office.read_workbook(self.path)
        self.assertIn("D4==SUM(D2:D3) -> None", text)
        self.assertNotIn("calculated now by LibreOffice", text)
        self.assertIn("Formulas without stored results", text)

    def test_results_are_stored_in_the_file_in_place(self):
        calculated = openpyxl.Workbook()
        calculated.active.title = "Data"
        for ref, v in (("D2", 28.5), ("D3", -10), ("D4", 18.5)):
            calculated["Data"][ref] = v
        with mock.patch.object(office, "_recalculated_values", lambda p, o: calculated):
            self.assertTrue(office._store_results(office.Path(self.path)))
        saved = openpyxl.load_workbook(self.path, data_only=True)["Data"]
        self.assertEqual([saved["D2"].value, saved["D3"].value, saved["D4"].value], [28.5, -10, 18.5])
        self.assertEqual(openpyxl.load_workbook(self.path)["Data"]["D4"].value, "=SUM(D2:D3)")

    def test_a_result_is_written_as_its_kind(self):
        self.assertEqual(office._result_xml(' r="A1" t="n"', "<f>1</f><v>0</v>", "OVER", openpyxl),
                         '<c r="A1" t="str"><f>1</f><v>OVER</v></c>')
        self.assertEqual(office._result_xml(' r="A1"', "<f>1/0</f>", "#DIV/0!", openpyxl), '<c r="A1" t="e"><f>1/0</f><v>#DIV/0!</v></c>')
        # A calculation that came to empty text is stored as an empty text result, not left with none.
        self.assertEqual(office._result_xml(' r="A1"', "<f>1</f><v>3</v>", None, openpyxl),
                         '<c r="A1" t="str"><f>1</f><v></v></c>')
        self.assertEqual(office._result_xml(' r="A1"', "<f>A&lt;B</f>", 2.5, openpyxl), '<c r="A1"><f>A&lt;B</f><v>2.5</v></c>')

    def calculated(self):
        book = openpyxl.Workbook()
        book.active.title = "Data"
        for ref, v in (("D2", 28.5), ("D3", -10), ("D4", 18.5)):
            book["Data"][ref] = v
        return book

    def test_an_empty_text_result_is_a_result_and_a_stored_number_is_one(self):
        # What Excel and LibreOffice write for =IF(...,"",...) that came to "": a result, an empty one.
        for xml, missing in (('<c r="E2" t="str"><f>IF(D2="X","",1)</f><v></v></c>', []),
                             ('<c r="E2"><f>IF(D2="X","",1)</f><v>1</v></c>', []),
                             ('<c r="E2"><f>SUM(A1:A2)</f><v></v></c>', ["Data!E2"]),
                             ('<c r="E2"><f>SUM(A1:A2)</f><v/></c>', ["Data!E2"]),
                             ('<c r="E2"><f>SUM(A1:A2)</f></c>', ["Data!E2"]),
                             ('<c r="E2"><f t="shared" si="0"/></c>', ["Data!E2"]),
                             ('<c r="E2" t="s"><v>3</v></c>', [])):
            with self.subTest(xml=xml):
                path = os.path.join(self.dir, "one.xlsx")
                with zipfile.ZipFile(self.path) as src, zipfile.ZipFile(path, "w") as dst:
                    for name in src.namelist():
                        data = src.read(name)
                        if name == "xl/worksheets/sheet1.xml":
                            data = re.sub(rb"<sheetData>.*</sheetData>|<sheetData/>",
                                          b"<sheetData><row r=\"2\">" + xml.encode() + b"</row></sheetData>", data,
                                          flags=re.S)
                        dst.writestr(name, data)
                self.assertEqual(office.formulas_without_results(path), missing)

    def test_not_a_workbook_or_not_readable_is_nothing_missing(self):
        notes = os.path.join(self.dir, "notes.txt")
        with open(notes, "w") as f:
            f.write("=SUM(A1)")
        broken = os.path.join(self.dir, "broken.xlsx")
        with open(broken, "w") as f:
            f.write("not a zip")
        self.assertEqual(office.formulas_without_results(notes), [])
        self.assertEqual(office.formulas_without_results(broken), [])

    def test_calculate_stores_every_result_and_keeps_the_formulas(self):
        with mock.patch.object(office, "_recalculated_values", lambda p, o: self.calculated()):
            said = office.calculate_workbook(self.path)
        self.assertIn("stored every formula's result", said)
        self.assertIn("3 had none", said)
        self.assertEqual(office.formulas_without_results(self.path), [])
        saved = openpyxl.load_workbook(self.path, data_only=True)["Data"]
        self.assertEqual(saved["D4"].value, 18.5)
        self.assertEqual(openpyxl.load_workbook(self.path)["Data"]["D4"].value, "=SUM(D2:D3)")

    def test_calculate_says_so_when_it_cannot(self):
        with mock.patch.object(office.shutil, "which", lambda name: None):
            with self.assertRaises(office.OfficeError) as said:
                office.calculate_workbook(self.path)
        self.assertIn("3 formula cell(s) still hold no saved result", str(said.exception))
        with mock.patch.object(office.shutil, "which", lambda name: None),                 mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(office.main(["calculate", self.path]), 1)
        self.assertIn("still hold no saved result", out.getvalue())

    def test_the_command_is_offered(self):
        with mock.patch.object(office, "_recalculated_values", lambda p, o: self.calculated()),                 mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(office.main(["calculate", self.path]), 0)
        self.assertIn("Calculated model.xlsx", out.getvalue())
        self.assertIn("office calculate BOOK.xlsx", office.USAGE)

    @unittest.skipUnless(shutil.which("soffice") or shutil.which("libreoffice"), "LibreOffice is not installed")
    def test_libreoffice_stores_an_empty_text_result_as_one(self):
        ws = openpyxl.load_workbook(self.path)
        ws["Data"]["E2"] = '=IF(C2>100,"",1)'
        ws.save(self.path)
        office.calculate_workbook(self.path)
        self.assertEqual(office.formulas_without_results(self.path), [])

    @unittest.skipUnless(shutil.which("soffice") or shutil.which("libreoffice"), "LibreOffice is not installed")
    def test_libreoffice_works_them_out(self):
        self.assertIn("D4==SUM(D2:D3) -> 18.5", office.read_workbook(self.path))


if __name__ == "__main__":
    unittest.main()
