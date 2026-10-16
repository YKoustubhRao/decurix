"""
A sheet added to an existing workbook, a row added to a deck's table - found live (2026-10-14): a worker asked to add
two sheets to a model made a second workbook, and one asked for a Total row reported the deck unfinished.
"""

import os
import shutil
import tempfile
import unittest
import zipfile
from unittest import mock

import openpyxl

from decurix_agent.tools import office

try:
    import pptx
except ImportError:  # pragma: no cover
    pptx = None


class AddingSheets(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, ignore_errors=True)
        self.path = os.path.join(self.dir, "model.xlsx")
        office.create_workbook(self.path, [{"name": "Data", "rows": [["Dept", "Spend"], ["Ops", 100], ["IT", 250]],
                                            "charts": [{"type": "bar", "title": "Spend", "data": "B1:B3",
                                                        "categories": "A2:A3", "anchor": "D2"}]}])
        patch = mock.patch.object(office, "_store_results", lambda p: False)
        patch.start()
        self.addCleanup(patch.stop)

    def test_new_sheets_are_added_and_the_rest_is_kept(self):
        office.update_workbook(self.path, add_sheets=[
            {"name": "Exceptions", "rows": [["Dept", "Over by"], ["IT", 50]]},
            {"name": "By Account", "cells": {"A1": "Account", "B2": {"formula": "SUM(Data!B2:B3)"}}}])
        wb = openpyxl.load_workbook(self.path)
        self.assertEqual(wb.sheetnames, ["Data", "Exceptions", "By Account"])
        self.assertEqual([[c.value for c in r] for r in wb["Exceptions"].iter_rows()], [["Dept", "Over by"], ["IT", 50]])
        self.assertEqual(wb["By Account"]["B2"].value, "=SUM(Data!B2:B3)")
        self.assertEqual(wb["Data"]["B3"].value, 250)
        with zipfile.ZipFile(self.path) as z:
            self.assertIn("xl/charts/chart1.xml", z.namelist())

    def test_edits_can_fill_a_sheet_added_in_the_same_update(self):
        office.update_workbook(self.path, edits=[{"sheet": "Notes", "cell": "B1", "value": "checked"}],
                               add_sheets=[{"name": "Notes", "rows": [["Status"]]}])
        self.assertEqual(openpyxl.load_workbook(self.path)["Notes"]["B1"].value, "checked")

    def test_a_sheet_that_exists_or_a_spec_it_does_not_read_is_refused_and_nothing_written(self):
        before = open(self.path, "rb").read()
        for bad in ([{"name": "Data", "rows": [["x"]]}], [{"name": "New", "table": [["x"]]}], [{"name": "Empty"}],
                    [{"name": "a/b", "rows": [["x"]]}]):
            with self.assertRaises(office.OfficeError):
                office.update_workbook(self.path, add_sheets=bad)
        self.assertEqual(open(self.path, "rb").read(), before)


@unittest.skipIf(pptx is None, "python-pptx is not installed")
class AddingTableRows(unittest.TestCase):
    def test_rows_are_added_at_the_end_of_a_table(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, ignore_errors=True)
        path = os.path.join(d, "deck.pptx")
        office.create_presentation(path, [{"title": "By region", "table": [["Region", "Revenue"], ["North", "10"]]}])
        office.update_presentation(path, [{"slide": 1, "table": 1, "add_rows": [["South", "20"], ["Total", "30"]]}])
        text = office.read_document(path)["text"]
        self.assertIn("| South | 20 |", text)
        self.assertIn("| Total | 30 |", text)

    def test_rows_can_go_above_a_total_row(self):
        # Found live (2026-10-14): a region added after the Total row left the table with two totals.
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, ignore_errors=True)
        path = os.path.join(d, "deck.pptx")
        office.create_presentation(path, [{"title": "T", "table": [["Region", "Revenue"], ["North", "10"], ["Total", "10"]]}])
        office.update_presentation(path, [{"slide": 1, "table": 1, "add_rows": [["Central", "0"]], "before": 3}])
        text = office.read_document(path)["text"]
        self.assertLess(text.index("| Central | 0 |"), text.index("| Total | 10 |"))
        with self.assertRaises(office.OfficeError):
            office.update_presentation(path, [{"slide": 1, "table": 1, "add_rows": [["x", "1"]], "before": 9}])

    def test_a_row_wider_than_the_table_is_refused(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, ignore_errors=True)
        path = os.path.join(d, "deck.pptx")
        office.create_presentation(path, [{"title": "T", "table": [["A", "B"], ["1", "2"]]}])
        with self.assertRaises(office.OfficeError):
            office.update_presentation(path, [{"slide": 1, "table": 1, "add_rows": [["1", "2", "3"]]}])


if __name__ == "__main__":
    unittest.main()
