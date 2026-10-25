import tempfile
import unittest
import zipfile
from pathlib import Path

import openpyxl
from openpyxl.chart import BarChart, Reference
from openpyxl.formatting.rule import CellIsRule
from openpyxl.workbook.defined_name import DefinedName

from decurix_agent.tools import office


# ============================================================
# Found live (2026-10-22): with no way to put rows into a workbook, the
# worker used openpyxl's insert_rows, which moves cells but not references;
# the month above the new rows read the rows the new month took. office
# update's insert_rows moves every reference with the rows, as an app does.
# ============================================================

class TestInsertRows(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        wb = openpyxl.Workbook()
        sales = wb.active
        sales.title = "SALES"
        sales.append(["Invoice", "Taxable"])
        for n, v in enumerate((100, 200, 300), 197):
            sales.append([n, v])
        sales["A5"], sales["B5"] = "TOTAL", "=SUM(B2:B4)"           # the total row new rows go above
        sales["C2"] = '=IF(B2>0,"B5 is text",B5)'                   # text in quotes is never a reference
        sales["D1"] = "=SUM(5:5)"                                    # a whole-row range
        sales.merge_cells("A7:B7")
        sales.conditional_formatting.add("B2:B5", CellIsRule(operator="greaterThan", formula=["B$5"], fill=None))
        chart = BarChart()
        chart.add_data(Reference(sales, min_col=2, min_row=1, max_row=4), titles_from_data=True)
        sales.add_chart(chart, "E2")
        comp = wb.create_sheet("COMPUTATION")
        comp["A1"], comp["B1"] = "Sales total", "=SALES!B5"
        comp["B2"] = "=SUM(SALES!B2:B4)+'SALES'!$B$5"
        comp["B3"] = "=B1*2"                                         # its own sheet: untouched
        wb.defined_names["SalesTotal"] = DefinedName("SalesTotal", attr_text="SALES!$B$5")
        self.book = self.dir / "book.xlsx"
        wb.save(self.book)
        self.out = self.dir / "after.xlsx"

    def insert(self, **spec):
        said = office.update_workbook(self.book, output_path=self.out,
                                      insert_rows=[dict({"sheet": "SALES", "before": 5, "count": 2}, **spec)])
        return said, openpyxl.load_workbook(self.out)

    def test_rows_go_in_and_every_reference_below_moves_with_them(self):
        said, wb = self.insert()
        self.assertIn("put 2 empty row(s) into SALES above row 5", said)
        sales, comp = wb["SALES"], wb["COMPUTATION"]
        self.assertEqual((sales["A7"].value, sales["B7"].value), ("TOTAL", "=SUM(B2:B4)"))
        self.assertIsNone(sales["A5"].value)
        self.assertEqual(sales["C2"].value, '=IF(B2>0,"B5 is text",B7)')
        self.assertEqual(sales["D1"].value, "=SUM(7:7)")
        self.assertEqual(comp["B1"].value, "=SALES!B7")
        self.assertEqual(comp["B2"].value, "=SUM(SALES!B2:B4)+'SALES'!$B$7")
        self.assertEqual(comp["B3"].value, "=B1*2")
        self.assertEqual(wb.defined_names["SalesTotal"].attr_text, "SALES!$B$7")
        self.assertEqual([str(r) for r in sales.merged_cells.ranges], ["A9:B9"])
        self.assertEqual([str(cf.sqref) for cf in sales.conditional_formatting], ["B2:B7"])
        with zipfile.ZipFile(self.out) as z:
            sheet = z.read("xl/worksheets/sheet1.xml").decode()
            chart = z.read("xl/charts/chart1.xml").decode()
        self.assertIn("B$7", sheet)                                  # the conditional format's own formula
        self.assertIn("'SALES'!$B$2:$B$4", chart)                    # above the new rows: unchanged
        self.assertEqual(office.moved_formula_breaks(self.book, self.out), [])

    def test_edits_address_the_rows_as_they_are_after_it(self):
        office.update_workbook(self.book, output_path=self.out,
                               insert_rows=[{"sheet": "SALES", "before": 5, "count": 1}],
                               edits=[{"sheet": "SALES", "cell": "A5", "value": 200},
                                      {"sheet": "SALES", "cell": "B5", "value": 400},
                                      {"sheet": "SALES", "cell": "B6", "value": "=SUM(B2:B5)"}])
        sales = openpyxl.load_workbook(self.out)["SALES"]
        self.assertEqual([sales.cell(5, c).value for c in (1, 2)], [200, 400])
        self.assertEqual((sales["A6"].value, sales["B6"].value), ("TOTAL", "=SUM(B2:B5)"))

    def test_shared_formulas_are_written_out_before_rows_go_in_among_them(self):
        xml = ('<sheetData><row r="2"><c r="C2"><f t="shared" ref="C2:C4" si="0">B2*2</f><v>2</v></c></row>'
               '<row r="3"><c r="C3"><f t="shared" si="0"/><v>4</v></c></row>'
               '<row r="4"><c r="C4"><f t="shared" si="0"/><v>6</v></c></row></sheetData>')
        expanded = office._expand_shared(xml)
        self.assertNotIn("shared", expanded)
        self.assertIn('<c r="C3"><f>B3*2</f>', expanded)
        self.assertIn('<c r="C4"><f>B4*2</f>', expanded)
        shifted, moved = office._shift_formulas(expanded, "SALES", True, 3, 5)
        self.assertEqual((moved, office._move_rows(shifted, 3, 5).count('r="C8"')), (2, 1))
        self.assertIn("<f>B9*2</f>", office._move_rows(shifted, 3, 5))

    def test_what_cannot_be_done_is_refused_and_nothing_is_written(self):
        for spec, words in (({"sheet": "Nope"}, "no sheet"), ({"before": 0}, "row number"), ({"count": 0}, "count"),
                            ({"after": 3}, "insert_rows is")):
            with self.assertRaises(office.OfficeError) as caught:
                office.update_workbook(self.book, output_path=self.out,
                                       insert_rows=[dict({"sheet": "SALES", "before": 5, "count": 2}, **spec)])
            self.assertIn(words, str(caught.exception))
            self.assertFalse(self.out.exists())

    def test_an_array_formula_is_not_split(self):
        with zipfile.ZipFile(self.book) as z:
            entries = {n: z.read(n) for n in z.namelist()}
        part = "xl/worksheets/sheet1.xml"
        entries[part] = entries[part].replace(b'<c r="C2"', b'<c r="E3"><f t="array" ref="E3:E6">B2:B5*2</f></c><c r="C2"', 1)
        with self.assertRaises(office.OfficeError) as caught:
            office._insert_rows(entries, office._sheet_files(entries), {"sheet": "SALES", "before": 5, "count": 1})
        self.assertIn("array formula over rows 3-6", str(caught.exception))

    def test_the_command_takes_it(self):
        self.assertIn("insert_rows", office.USAGE)


if __name__ == "__main__":
    unittest.main()
