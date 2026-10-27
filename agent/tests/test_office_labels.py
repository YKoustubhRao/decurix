"""An update that would empty a cell holding text (a label, a heading) is refused unless asked for in so many words.
Found live (2026-10-26): a block copied like the earlier ones had its label row blanked with the figures."""
import os
import tempfile
import unittest

import openpyxl

from decurix_agent.tools import office


class ClearingLabels(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "calc.xlsx")
        office.create_workbook(self.path, [{"name": "Calc", "rows": [
            ["AUGUST 2026", None, None, None], ["Head", "Rate A", "Rate B", "Rate C"], ["Sales", 100, 50, 50], ["Total", "=B3", "=C3", "=D3"]]}])

    def test_blanking_a_label_is_refused_and_nothing_written(self):
        with self.assertRaises(office.OfficeError) as cm:
            office.update_workbook(self.path, edits=[{"sheet": "Calc", "cell": "B2", "value": None},
                                                     {"sheet": "Calc", "cell": "B3", "value": None}])
        self.assertIn("Calc!B2 'Rate A'", str(cm.exception))
        self.assertIn("clear_text", str(cm.exception))
        ws = openpyxl.load_workbook(self.path)["Calc"]
        self.assertEqual(ws["B2"].value, "Rate A")
        self.assertEqual(ws["B3"].value, 100)

    def test_blanking_figures_and_formulas_goes_through(self):
        office.update_workbook(self.path, edits=[{"sheet": "Calc", "cell": "B3", "value": None},
                                                 {"sheet": "Calc", "cell": "B4", "value": None}])
        ws = openpyxl.load_workbook(self.path)["Calc"]
        self.assertIsNone(ws["B3"].value)
        self.assertIsNone(ws["B4"].value)

    def test_clear_text_allows_it(self):
        office.update_workbook(self.path, edits=[{"sheet": "Calc", "cell": "B2", "value": None}], clear_text=True)
        self.assertIsNone(openpyxl.load_workbook(self.path)["Calc"]["B2"].value)

    def test_a_copied_block_can_have_its_copied_figures_emptied(self):
        # The copy brings the earlier block's cells; blanking what the copy put is the usual fill, not a label lost.
        office.update_workbook(self.path, copy_rows=[{"sheet": "Calc", "rows": "1:4", "to": 6,
                                                      "replace": {"AUGUST": "SEPTEMBER"}}],
                               edits=[{"sheet": "Calc", "cell": "B8", "value": None}])
        ws = openpyxl.load_workbook(self.path)["Calc"]
        self.assertEqual(ws["A6"].value, "SEPTEMBER 2026")
        self.assertEqual(ws["B7"].value, "Rate A")
        self.assertIsNone(ws["B8"].value)


if __name__ == "__main__":
    unittest.main()


class WritingOverAndShortBlocks(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "reg.xlsx")
        office.create_workbook(self.path, [{"name": "REG", "rows": [
            ["AUGUST 2026", None, None], ["Date", "Party", "No."], ["2026-08-02", "Alpha", 451], ["2026-08-09", "Beta", 452],
            ["TOTAL", None, "=COUNT(C3:C4)"], [None, None, None]]}])

    def test_writing_over_a_filled_cell_is_said_in_the_update(self):
        msg = office.update_workbook(self.path, edits=[{"sheet": "REG", "cell": "C4", "value": 453},
                                                       {"sheet": "REG", "cell": "B4", "value": "Gamma"},
                                                       {"sheet": "REG", "cell": "A6", "value": "x"}])
        self.assertIn("replaced 2 filled cell(s): REG!C4 452 -> 453, REG!B4 'Beta' -> 'Gamma'", msg)
        self.assertIn("gone from the file", msg)

    def test_a_block_copied_short_of_its_total_is_refused(self):
        with self.assertRaises(office.OfficeError) as cm:
            office.update_workbook(self.path, copy_rows=[{"sheet": "REG", "rows": "1:4", "to": 8}])
        self.assertIn("go on past row 4", str(cm.exception))
        self.assertIn("TOTAL", str(cm.exception))
        office.update_workbook(self.path, copy_rows=[{"sheet": "REG", "rows": "1:5", "to": 8, "replace": {"AUGUST": "SEPTEMBER"}}])
        ws = openpyxl.load_workbook(self.path)["REG"]
        self.assertEqual(ws["A12"].value, "TOTAL")
        self.assertEqual(ws["A8"].value, "SEPTEMBER 2026")


class DateCells(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "dates.xlsx")
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "REG"
        import datetime
        ws["A1"] = "Date"
        ws["A2"] = datetime.date(2026, 8, 2)
        ws["A2"].number_format = "dd-mm-yyyy"
        ws["A3"] = datetime.date(2026, 8, 9)
        ws["A3"].number_format = "dd-mm-yyyy"
        wb.save(self.path)

    def test_a_text_date_into_a_date_cell_becomes_a_date(self):
        office.update_workbook(self.path, edits=[{"sheet": "REG", "cell": "A3", "value": "2026-09-26"},
                                                 {"sheet": "REG", "cell": "A4", "value": "2026-09-27"}])
        ws = openpyxl.load_workbook(self.path)["REG"]
        import datetime
        self.assertEqual(ws["A3"].value, datetime.datetime(2026, 9, 26))  # a date cell: the date itself
        self.assertNotIsInstance(ws["A4"].value, str)  # a new cell under the dates: never "date as text"

    def test_a_number_into_a_date_cell_is_refused(self):
        with self.assertRaises(office.OfficeError) as cm:
            office.update_workbook(self.path, edits=[{"sheet": "REG", "cell": "A3", "value": 45996}])
        self.assertIn("date cell", str(cm.exception))
