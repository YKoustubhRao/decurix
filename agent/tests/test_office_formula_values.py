"""
A formula a tool wrote has no saved result until a spreadsheet program opens the file. Read back, every total
showed "-> None" and the final check could not see what a correct model came to (found live, 2026-10-14).
"""

import os
import shutil
import tempfile
import unittest
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
        formulas = openpyxl.load_workbook(self.path)
        values = openpyxl.load_workbook(self.path, data_only=True)
        self.assertTrue(office._formulas_without_values(formulas, values))

    def test_the_calculated_values_are_shown_and_said_to_be_calculated(self):
        calculated = openpyxl.Workbook()
        calculated.active.title = "Data"
        calculated["Data"]["D2"], calculated["Data"]["D3"], calculated["Data"]["D4"] = 28.5, -10, 18.5
        with mock.patch.object(office, "_recalculated_values", lambda p, o: calculated):
            text = office.read_workbook(self.path)
        self.assertIn("D4==SUM(D2:D3) -> 18.5", text)
        self.assertIn("calculated by LibreOffice", text)

    def test_without_libreoffice_it_reads_as_before(self):
        with mock.patch.object(office.shutil, "which", lambda name: None):
            text = office.read_workbook(self.path)
        self.assertIn("D4==SUM(D2:D3) -> None", text)
        self.assertNotIn("calculated by LibreOffice", text)

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
        self.assertEqual(office._result_xml(' r="A1"', "<f>1</f><v>3</v>", None, openpyxl), '<c r="A1"><f>1</f></c>')
        self.assertEqual(office._result_xml(' r="A1"', "<f>A&lt;B</f>", 2.5, openpyxl), '<c r="A1"><f>A&lt;B</f><v>2.5</v></c>')

    @unittest.skipUnless(shutil.which("soffice") or shutil.which("libreoffice"), "LibreOffice is not installed")
    def test_libreoffice_works_them_out(self):
        self.assertIn("D4==SUM(D2:D3) -> 18.5", office.read_workbook(self.path))


if __name__ == "__main__":
    unittest.main()
