import tempfile
import unittest
from pathlib import Path

import openpyxl

from decurix_agent.tools import office


# ============================================================
# GST suite T3 (2026-10-21): "Closing credit carried forward" stood in six
# blocks of a workbook read as one list of cells, and the worker took the
# figure from another month's block. office read now maps a sheet that
# repeats one layout and tags every row with its block.
# ============================================================

def month(ws, top, name, closing, notes=True, blank_after_title=False):
    ws.cell(top, 1, f"{name} 2026")
    r = top + (2 if blank_after_title else 1)
    ws.cell(r, 1, "Particulars")
    ws.cell(r + 1, 1, "Opening credit")
    ws.cell(r + 1, 2, 100)
    ws.cell(r + 2, 1, "Closing credit carried forward")
    ws.cell(r + 2, 2, closing)
    if notes:
        ws.cell(r + 3, 1, f"Return for {name} filed")
        return r + 4
    return r + 3


class TestSheetBlocks(unittest.TestCase):

    def book(self, build):
        wb = openpyxl.Workbook()
        build(wb.active)
        path = Path(tempfile.mkdtemp()) / "comp.xlsx"
        wb.save(path)
        return path, openpyxl.load_workbook(path).active

    def test_a_repeated_layout_is_mapped_by_its_titles(self):
        def build(ws):
            r = 1
            for name, closing in (("JULY", 700), ("AUGUST", 800), ("SEPTEMBER", 900)):
                r = month(ws, r, name, closing) + 1     # a blank row between blocks
        path, ws = self.book(build)
        self.assertEqual(office.sheet_blocks(ws), [(1, 6, "JULY 2026"), (7, 12, "AUGUST 2026"), (13, 17, "SEPTEMBER 2026")])
        text = office.read_workbook(path)
        self.assertIn("Blocks (this sheet repeats one layout", text)
        self.assertIn("[AUGUST 2026]  A10='Closing credit carried forward'  B10=800", text)

    def test_the_notes_of_the_block_before_are_not_its_title(self):
        def build(ws):
            r = 1
            for name, closing in (("JULY", 700), ("AUGUST", 800), ("SEPTEMBER", 900)):
                r = month(ws, r, name, closing) + (1 if name == "JULY" else 0)   # AUGUST's notes run into SEPTEMBER
        _, ws = self.book(build)
        self.assertEqual([t for _, _, t in office.sheet_blocks(ws)], ["JULY 2026", "AUGUST 2026", "SEPTEMBER 2026"])

    def test_a_title_above_a_blank_row_is_still_the_title(self):
        def build(ws):
            r = 1
            for name, closing in (("JULY", 700), ("AUGUST", 800)):
                r = month(ws, r, name, closing, notes=False, blank_after_title=True) + 2
        _, ws = self.book(build)
        self.assertEqual([t for _, _, t in office.sheet_blocks(ws)], ["JULY 2026", "AUGUST 2026"])

    def test_a_plain_table_is_not_a_layout_of_blocks(self):
        def build(ws):
            ws.append(["Invoice", "Party", "Amount"])
            for n, party in enumerate(("Asha", "Ravi", "Asha", "Meera", "Ravi"), 1):
                ws.append([n, party, n * 100])
        path, ws = self.book(build)
        self.assertEqual(office.sheet_blocks(ws), [])
        self.assertNotIn("Blocks", office.read_workbook(path))


if __name__ == "__main__":
    unittest.main()
