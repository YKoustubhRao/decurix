import tempfile
import unittest
from pathlib import Path

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

from decurix_agent.tools import office


# ============================================================
# Found live (2026-10-21): "Closing credit carried forward" stood in six
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

    def test_a_long_sheet_of_blocks_shows_its_newest_blocks_whole(self):
        # Found live (2026-10-24): the first 60 rows ended inside July; August's closing figure was never shown and
        # the worker reported July's as August's.
        def build(ws):
            r = 1
            for name, closing in (("APRIL", 400), ("MAY", 500), ("JUNE", 600), ("JULY", 700), ("AUGUST", 800)):
                r = month(ws, r, name, closing) + 1
        path, _ = self.book(build)
        text = office.read_workbook(path, max_rows=14)
        self.assertIn("[AUGUST 2026]  A28='Closing credit carried forward'  B28=800", text)
        self.assertIn("[JULY 2026]", text)
        self.assertNotIn("[APRIL 2026]  A", text)
        self.assertIn("earlier blocks not shown: 'APRIL 2026', 'MAY 2026', 'JUNE 2026'", text)

    def test_a_plain_table_is_not_a_layout_of_blocks(self):
        def build(ws):
            ws.append(["Invoice", "Party", "Amount"])
            for n, party in enumerate(("Asha", "Ravi", "Asha", "Meera", "Ravi"), 1):
                ws.append([n, party, n * 100])
        path, ws = self.book(build)
        self.assertEqual(office.sheet_blocks(ws), [])
        self.assertNotIn("Blocks", office.read_workbook(path))


class TestABlockThatDoesNotFollowTheLayout(unittest.TestCase):
    """Found live (2026-10-22): a September block without the header row, its dates as text and its codes numbers."""

    def sales(self, ws, top, name, day, rows, header=True, as_text=False):
        ws.cell(top, 1, name)
        ws.cell(top + 1, 1, "KAVYA CONSTRUCTIONS")
        r = top + 2
        if header:
            for c, h in enumerate(("Date", "Party", "HSN"), 1):
                ws.cell(r, c, h)
            r += 1
        import datetime
        for party in rows:
            ws.cell(r, 1, f"2026-{day}" if as_text else datetime.datetime(2026, int(day[:2]), int(day[3:])))
            ws.cell(r, 2, party)
            ws.cell(r, 3, 9954 if as_text else "9954")
            r += 1
        ws.cell(r, 2, "TOTAL")
        return r + 2

    def test_the_new_block_is_told_where_it_breaks_the_layout(self):
        wb = openpyxl.Workbook()
        ws = wb.active
        r = 1
        for month, day, parties in (("JUNE", "06-04", ["ASHA", "CASH SALE"]), ("JULY", "07-04", ["RAVI", "MEERA", "CASH SALE"]),
                                    ("AUGUST", "08-04", ["CASH SALE"])):
            r = self.sales(ws, r, month, day, parties)
        last = r
        self.sales(ws, r, "SEPTEMBER", "09-21", ["ANIL", "CASH SALE"], header=False, as_text=True)
        path = Path(tempfile.mkdtemp()) / "book.xlsx"
        wb.save(path)
        found = office.block_layout_breaks(path, rows={"Sheet": {last + 2}})
        self.assertTrue(any("has no row 'date'" in f for f in found), found)
        self.assertTrue(any("as date as text, where the column holds dates" in f for f in found), found)
        self.assertTrue(any("as number, where the column holds numbers written as text" in f for f in found), found)
        self.assertFalse(any("cash sale" in f for f in found), found)
        self.assertEqual(office.block_layout_breaks(path, rows={"Sheet": {2}}), [])   # an earlier block that follows it

    def test_a_row_of_another_period_and_two_values_in_one_cell_are_told(self):
        # Found live (2026-10-24): August's row copied into September's new block, and two bills at two rates each
        # entered as one row ("1905/1006") where every earlier row held one code.
        import datetime
        wb = openpyxl.Workbook()
        ws = wb.active
        r = 1
        for month, day, parties in (("JUNE", "06-04", ["ASHA", "CASH SALE"]), ("JULY", "07-04", ["RAVI", "MEERA"]),
                                    ("AUGUST", "08-04", ["CASH SALE", "RAVI"])):
            r = self.sales(ws, r, month, day, parties)
        last = r
        codes = iter(["1006", "2001", "0801", "1905", "0910", "1006"])
        for row in range(1, last):                                     # the earlier entries' own codes, one per row
            if ws.cell(row, 3).value == "9954":
                ws.cell(row, 3, next(codes))
        self.sales(ws, r, "SEPTEMBER", "09-21", ["ANIL", "PRIYA"])
        ws.cell(last + 3, 1, datetime.datetime(2026, 8, 4))           # a row of August inside September
        ws.cell(last + 4, 3, "1905/1006")                              # two codes in one cell
        path = Path(tempfile.mkdtemp()) / "book.xlsx"
        wb.save(path)
        found = office.block_layout_breaks(path, rows={"Sheet": {last + 3}})
        self.assertTrue(any("is dated 2026-08-04, inside the block of September" in f for f in found), found)
        self.assertTrue(any("holds '1905/1006' - two values in one cell" in f for f in found), found)
        # The header "HSN" every block shares, and the blocks that follow the layout, raise nothing.
        self.assertFalse(any("'HSN'" in f for f in found), found)
        self.assertEqual(office.block_layout_breaks(path, rows={"Sheet": {2}}), [])

    def test_a_sheet_the_task_did_not_change_is_not_looked_at(self):
        # Independent review (2026-10-22): `rows` named only another sheet, and this one's last block was checked
        # anyway - its old layout break was put to the task that never touched it.
        wb = openpyxl.Workbook()
        ws = wb.active
        r = 1
        for month, day, parties in (("JUNE", "06-04", ["ASHA"]), ("JULY", "07-04", ["RAVI"]), ("AUGUST", "08-04", ["MEERA"])):
            r = self.sales(ws, r, month, day, parties)
        self.sales(ws, r, "SEPTEMBER", "09-21", ["ANIL"], header=False, as_text=True)
        wb.create_sheet("Notes").append(["changed by the task"])
        path = Path(tempfile.mkdtemp()) / "book.xlsx"
        wb.save(path)
        self.assertTrue(office.block_layout_breaks(path))                               # None: the last block is checked
        self.assertEqual(office.block_layout_breaks(path, rows={"Notes": {1}}), [])     # another sheet changed only
        self.assertEqual(office.block_layout_breaks(path, rows={}), [])                 # nothing changed

    def test_the_worker_is_told_as_it_writes_a_block_that_breaks_the_layout(self):
        # Found live (2026-10-23): the new month's block went in with no header row; the check saw it only after.
        wb = openpyxl.Workbook()
        ws = wb.active
        r = 1
        for month, day, parties in (("JUNE", "06-04", ["ASHA"]), ("JULY", "07-04", ["RAVI"]), ("AUGUST", "08-04", ["MEERA"])):
            r = self.sales(ws, r, month, day, parties)
        top = r
        path = Path(tempfile.mkdtemp()) / "book.xlsx"
        wb.save(path)
        edits = [{"sheet": "Sheet", "cell": f"A{top}", "value": "SEPTEMBER"},
                 {"sheet": "Sheet", "cell": f"A{top + 1}", "value": "KAVYA CONSTRUCTIONS"},
                 {"sheet": "Sheet", "cell": f"A{top + 2}", "value": "2026-09-21"},
                 {"sheet": "Sheet", "cell": f"B{top + 2}", "value": "ANIL"},
                 {"sheet": "Sheet", "cell": f"B{top + 3}", "value": "TOTAL"}]
        msg = office.update_workbook(str(path), edits=edits)
        self.assertIn("Compared with the sheet's other blocks", msg)
        self.assertIn("has no row 'date'", msg)
        # A write that keeps the layout says nothing more.
        msg = office.update_workbook(str(path), edits=[{"sheet": "Sheet", "cell": "B4", "value": "ASHA K"}])
        self.assertNotIn("Compared with", msg)


if __name__ == "__main__":
    unittest.main()
