"""
`office update` never leaves a stale result or rewrites part of a reference (review 2026-11-03, F-06 and F-07).

F-06: when LibreOffice cannot recalculate after an update, the old saved results of the formulas are dropped, so
`office read` and the checks see "no saved result", not the old total as the value.
F-07: a range reference is rewritten only as a whole reference, never inside a longer one.
"""

import os
import re
import shutil
import tempfile
import unittest
import zipfile
from unittest import mock

from needs import needs

from decurix_agent.tools import office


def _set_cached(path, cell, value):
    """Give a formula cell a saved result, as a spreadsheet program would have."""

    with zipfile.ZipFile(path) as z:
        parts = {n: z.read(n) for n in z.namelist()}
    name = "xl/worksheets/sheet1.xml"
    xml = parts[name].decode("utf-8")
    xml = re.sub(r'(<c r="%s"[^>]*>)(<f>[^<]*</f>)(<v\s*/>|<v>[^<]*</v>)?' % cell,
                 lambda m: m.group(1) + m.group(2) + f"<v>{value}</v>", xml)
    parts[name] = xml.encode("utf-8")
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        for n, data in parts.items():
            z.writestr(n, data)


@needs("openpyxl")
class UpdateResults(unittest.TestCase):
    def setUp(self):
        import openpyxl
        from openpyxl.workbook.defined_name import DefinedName

        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, ignore_errors=True)
        self.path = os.path.join(self.dir, "model.xlsx")
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "Data"
        ws.append(["Item", "Amount"])
        for i, v in enumerate([10, 20, 30], 2):
            ws[f"A{i}"], ws[f"B{i}"] = f"x{i}", v
        ws["A5"], ws["B5"] = "Total", "=SUM(B2:B4)"
        wb.defined_names["long_range"] = DefinedName("long_range", attr_text="Data!$B$2:$B$130")
        wb.defined_names["short_range"] = DefinedName("short_range", attr_text="Data!$B$2:$B$13")
        wb.save(self.path)
        _set_cached(self.path, "B5", 60)
        self.assertEqual(office.formulas_without_results(self.path), [])

    def test_a_result_that_could_not_be_recalculated_is_not_left_stale(self):
        with mock.patch.object(office, "_recalculated_values", return_value=None):
            said = office.update_workbook(self.path, edits=[{"sheet": "Data", "cell": "B2", "value": 1000}])
        self.assertEqual(office.formulas_without_results(self.path), ["Data!B5"])
        self.assertIn("could not be recalculated", said)
        shown = office.read_workbook(self.path)
        self.assertNotRegex(shown, r"SUM\(B2:B4\)\D*60\b")
        self.assertIn("=SUM(B2:B4)", shown)

    def test_a_result_that_was_recalculated_is_kept(self):
        def recalculated(path, openpyxl):
            wb = openpyxl.load_workbook(str(path))
            wb["Data"]["B5"] = 1050
            return wb

        with mock.patch.object(office, "_recalculated_values", side_effect=recalculated):
            said = office.update_workbook(self.path, edits=[{"sheet": "Data", "cell": "B2", "value": 1000}])
        self.assertIn("recalculated and saved", said)
        self.assertEqual(office.formulas_without_results(self.path), [])

    def test_only_the_whole_reference_is_rewritten(self):
        with mock.patch.object(office, "_recalculated_values", return_value=None):
            said = office.update_workbook(self.path, replace_refs=[{"from": "Data!$B$2:$B$13",
                                                                     "to": "Data!$B$2:$B$14"}])
        with zipfile.ZipFile(self.path) as z:
            names = dict(re.findall(r'<definedName name="(\w+)"[^>]*>([^<]*)</definedName>',
                                    z.read("xl/workbook.xml").decode("utf-8")))
        self.assertEqual(names["short_range"], "Data!$B$2:$B$14")
        self.assertEqual(names["long_range"], "Data!$B$2:$B$130")
        self.assertIn("1 range reference(s) rewritten", said)


if __name__ == "__main__":
    unittest.main()
