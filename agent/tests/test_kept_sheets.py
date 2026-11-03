"""
CE follow-up (2026-10-20): a workbook built across stages (one analysis a stage) keeps each completed stage's sheets.
A later stage that drops or changes one is not done, naming the sheet and the stage that made it - unless its
instruction or a change the person made asks for that sheet to change. Snapshots live in the run's own folder.
"""

import json
import shutil
import unittest
from pathlib import Path
from unittest import mock

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo
from temp_case import TempDirCase

from decurix_agent import long_task
from decurix_agent.tools import office

RUN = "run-sheets"


class Runner:
    def __init__(self, root):
        self.root = Path(root)


class KeptSheets(TempDirCase):
    def setUp(self):
        super().setUp()
        self.book = self.dir / "work" / "month_end_close_Oct2026.xlsx"
        self.book.parent.mkdir(parents=True)
        self.data = {"amendments": []}
        self.steps = [{"id": "s1-bank", "title": "Bank Rec", "instruction": "Create the workbook with the Bank Rec sheet."},
                      {"id": "s2-purchases", "title": "Purchase Register",
                       "instruction": "Add the Purchase Register sheet to the workbook; read the Bank Rec sheet for "
                                      "the payments."}]
        self.state = {"run_id": RUN, "spec": {"steps": self.steps},
                      "steps": {"s1-bank": {"status": "completed"}, "s2-purchases": {"status": "running"}}}
        patches = [mock.patch.object(long_task, "_runner", lambda: Runner(self.dir / "runs")),
                   mock.patch.object(long_task, "ledger", lambda run_id: self.data)]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        # Stage 1 makes the workbook with its Bank Rec sheet and completes.
        book = openpyxl.Workbook()
        sheet = book.active
        sheet.title = "Bank Rec"
        for row in (["Balance per books", 878193.78], ["Cheques not presented", 129400.0],
                    ["Balance per bank", "=B1-B2"]):
            sheet.append(row)
        book.save(self.book)
        long_task._snapshot_sheets(RUN, "s1-bank", [str(self.book)])

    def check(self):
        return long_task.lost_sheets(RUN, self.state, self.steps[1], [str(self.book)])

    def test_the_snapshot_is_in_the_runs_folder(self):
        self.assertTrue((self.dir / "runs" / RUN / "sheets" / "s1-bank.json").is_file())

    def test_a_rewrite_that_drops_bank_rec_is_refused(self):
        book = openpyxl.Workbook()
        book.active.title = "Purchase Register"
        book.active.append(["Bill", "Taxable"])
        book.save(self.book)
        found = self.check()
        self.assertEqual(len(found), 1)
        self.assertIn("The sheet \"Bank Rec\" that the stage \"Bank Rec\" made in month_end_close_Oct2026.xlsx is gone",
                      found[0])

    def test_adding_a_sheet_with_office_update_passes(self):
        office.update_workbook(str(self.book), add_sheets=[{"name": "Purchase Register", "rows": [["Bill", "Taxable"],
                                                                                                 ["NS-0452", 41800]]}])
        self.assertEqual(self.check(), [])

    def test_an_unasked_change_to_bank_rec_is_refused(self):
        office.update_workbook(str(self.book), edits=[{"sheet": "Bank Rec", "cell": "B2", "value": 1}])
        found = self.check()
        self.assertEqual(len(found), 1)
        self.assertIn("\"Bank Rec\"", found[0])
        self.assertIn("was changed (B2)", found[0])

    def test_an_amendment_that_changes_bank_rec_allows_it(self):
        office.update_workbook(str(self.book), edits=[{"sheet": "Bank Rec", "cell": "B2", "value": 129990.0}])
        self.data["amendments"] = [{"n": 1, "text": "The bank charges were 590 more - put them in the Bank Rec."}]
        self.assertEqual(self.check(), [])

    def test_an_instruction_that_asks_to_change_it_allows_it(self):
        office.update_workbook(str(self.book), edits=[{"sheet": "Bank Rec", "cell": "B2", "value": 129990.0}])
        self.steps[1]["instruction"] += " Update the Bank Rec sheet with the bills paid by cheque."
        self.assertEqual(self.check(), [])

    def test_an_accepted_change_is_the_sheet_for_the_next_stage(self):
        office.update_workbook(str(self.book), edits=[{"sheet": "Bank Rec", "cell": "B2", "value": 129990.0}])
        self.data["amendments"] = [{"n": 1, "text": "Fix the Bank Rec."}]
        long_task._snapshot_sheets(RUN, "s2-purchases", [str(self.book)])
        self.data["amendments"] = []
        self.state["steps"]["s2-purchases"]["status"] = "completed"
        third = {"id": "s3-gst", "title": "GSTR-3B", "instruction": "Add the GSTR-3B sheet."}
        self.steps.append(third)
        self.state["steps"]["s3-gst"] = {"status": "running"}
        self.assertEqual(long_task.lost_sheets(RUN, self.state, third, [str(self.book)]), [])


class CellsByMeaning(unittest.TestCase):
    """An app saving the workbook again changes how cells are stored, not what they hold."""

    def same(self, a, b, day_first=True):
        return long_task.same_cell(a, b, day_first)

    def test_the_same_thing_stored_another_way(self):
        cases = [
            ([480000, None], ["480000", None]), ([480000, None], ["4,80,000", None]), ([480000, None], [480000.0, None]),
            (["2026-10-31", None], [46326, None]),                       # a date and its serial number
            (["2026-10-31 00:00:00", None], ["31/10/2026", None]),       # ... and its text in the request's convention
            (["=SUM($C$2:C4)-'Bank Rec'!C5", None], ["=sum(c2:c4) - Bank Rec!C5", None]),   # re-serialised formula
            (["=C2+C3", None], [863851.27, None]),                       # no computed value kept: not a visible change
            (["=C2+C3", 863851.27], [863851.27, None]),                  # a formula and its computed value
            ([True, None], ["=TRUE()", True]), ([True, None], ["TRUE", None]), ([False, None], ["false", None]),
            (["as at 31 Oct ", None], ["as at 31 Oct", None]),           # trailing whitespace
            (["", None], ["  ", None]),                                  # empty and ""
        ]
        for a, b in cases:
            self.assertTrue(self.same(a, b), (a, b))
            self.assertTrue(self.same(b, a), (b, a))

    def test_real_changes(self):
        cases = [
            ([480000, None], [480500, None]),
            (["2026-10-31", None], ["30/10/2026", None]),
            (["=C2+C3", 863851.27], ["=C2+C4", 900000.0]),               # a different formula result
            (["=C2+C3", 863851.27], [863000.0, None]),
            ([True, None], [False, None]),
            (["book only", None], ["bank only", None]),
            ([43260, None], None),                                       # a row removed
            (None, ["Extra item", None]),                                # a row added
        ]
        for a, b in cases:
            self.assertFalse(self.same(a, b), (a, b))

    def test_a_workbook_libreoffice_saved_again_is_unchanged(self):
        # fixtures/roundtrip: before.xlsx written by openpyxl, after_libreoffice.xlsx = soffice --headless --convert-to
        # xlsx of it (run in the desktop image, 2026-10-20). LibreOffice adds computed values and turns TRUE into =TRUE().
        here = fixture_paths.need("roundtrip")
        before = long_task._sheet_cells(here / "before.xlsx")
        after = long_task._sheet_cells(here / "after_libreoffice.xlsx")
        self.assertNotEqual(before, after)  # stored differently ...
        for sheet, cells in before.items():
            for key in set(cells) | set(after[sheet]):
                self.assertTrue(long_task.same_cell(cells.get(key), after[sheet].get(key)), (sheet, key))  # ... same


class AdditionsAreNoChange(TempDirCase):
    """CO (round 5 P2, 2026-10-20): stage 3 added column N to stage 1's "Working" sheet in dispatch_plan.xlsx and
    was refused for "changed (N1, N10, N11 ...)". Adding cells is allowed; changing or clearing a value, removing a row
    or reordering rows is not."""

    LIVE = fixture_paths.fixture("p2_dispatch")

    def setUp(self):
        self.LIVE = fixture_paths.need(self.LIVE.name)
        super().setUp()
        self.book = self.dir / "dispatch_plan.xlsx"
        shutil.copy(self.LIVE / "dispatch_plan.xlsx", self.book)
        kept = json.loads((self.LIVE / "s1-extract-orders.json").read_text(encoding="utf-8"))
        kept = {str(self.book.resolve()): v for k, v in kept.items() if k.endswith("dispatch_plan.xlsx")}
        sheets = self.dir / "runs" / RUN / "sheets"
        sheets.mkdir(parents=True)
        (sheets / "s1-extract.json").write_text(json.dumps(kept), encoding="utf-8")
        self.steps = [{"id": "s1-extract", "title": "Extract every order line and pricing basis into dispatch_plan.xlsx",
                       "instruction": "Extract every order line."},
                      {"id": "s3-allocate", "title": "Allocate stock and complete dispatch_plan.xlsx",
                       "instruction": "Allocate stock and complete dispatch_plan.xlsx."}]
        self.state = {"run_id": RUN, "spec": {"steps": self.steps},
                      "steps": {"s1-extract": {"status": "completed"}, "s3-allocate": {"status": "running"}}}
        for p in (mock.patch.object(long_task, "_runner", lambda: Runner(self.dir / "runs")),
                  mock.patch.object(long_task, "ledger", lambda run_id: {"amendments": []})):
            p.start()
            self.addCleanup(p.stop)

    def check(self):
        return long_task.lost_sheets(RUN, self.state, self.steps[1], [str(self.book)])

    def edit(self, change):
        book = openpyxl.load_workbook(self.book)
        change(book["Working"])
        book.save(self.book)

    def test_the_live_file_as_it_is_passes(self):
        self.assertEqual(self.check(), [])

    def test_a_new_column_and_rows_appended_pass(self):
        def add(sheet):
            column = sheet.max_column + 1
            for row in range(1, sheet.max_row + 1):
                sheet.cell(row=row, column=column, value="Allocated" if row == 1 else row)
            sheet.append(["new line"] + [None] * 3 + [1])
        self.edit(add)
        self.assertEqual(self.check(), [])

    def test_a_changed_value_is_refused(self):
        def change(sheet):
            cell = next(c for c in sheet[2] if isinstance(c.value, (int, float)) and not isinstance(c.value, bool))
            cell.value = cell.value + 1
        self.edit(change)
        found = self.check()
        self.assertEqual(len(found), 1, found)
        self.assertIn("\"Working\"", found[0])

    def test_a_deleted_row_is_refused(self):
        self.edit(lambda sheet: sheet.delete_rows(2))
        found = self.check()
        self.assertEqual(len(found), 1, found)
        self.assertIn("was changed", found[0])


class AfterARecordWasRemoved(TempDirCase):
    """CP2 (round 5 P1, 2026-10-20): the person cancelled bill NS-0457 mid-run; the Purchase Register stage, re-run,
    was refused for the totals still holding NS-0457 AND for changing "Purchase Working" and "GSTR-3B" - reported as the
    AR Ageing stage's sheets. A sheet is the stage's whose completion first held it; after a change to a record, the
    figures built on it may change everywhere and its rows may go. fixtures/p1_cancelled: the live workbook, the run's
    sheet snapshots and its stages (statuses, completion times, the change)."""

    LIVE = fixture_paths.fixture("p1_cancelled")

    def setUp(self):
        self.LIVE = fixture_paths.need(self.LIVE.name)
        super().setUp()
        self.run =json.loads((self.LIVE / "run.json").read_text(encoding="utf-8"))
        self.book = self.dir / "month_end_close_Oct2026.xlsx"
        shutil.copy(self.LIVE / "month_end_close_Oct2026.xlsx", self.book)
        sheets = self.dir / "runs" / RUN / "sheets"
        sheets.mkdir(parents=True)
        for f in (self.LIVE / "sheets").glob("*.json"):
            kept = json.loads(f.read_text(encoding="utf-8"))
            (sheets / f.name).write_text(json.dumps({str(self.book.resolve()): v for v in kept.values()}),
                                         encoding="utf-8")
        self.state = {"run_id": RUN, "spec": {"steps": self.run["steps"]}, "steps": self.run["status"]}
        self.data = {"amendments": self.run["amendments"], "request": ""}
        for p in (mock.patch.object(long_task, "_runner", lambda: Runner(self.dir / "runs")),
                  mock.patch.object(long_task, "ledger", lambda run_id: self.data)):
            p.start()
            self.addCleanup(p.stop)

    def step(self, step_id):
        return next(s for s in self.run["steps"] if s["id"] == step_id)

    def test_the_owner_is_the_stage_whose_completion_first_held_the_sheet(self):
        owners = long_task._sheet_owners(RUN, self.state)
        names = {name: owner for (_, name), owner in owners.items()}
        self.assertEqual(names["Purchase Working"], "s2-purchase-register")
        self.assertEqual(names["GSTR-3B"], "s3-gstr-3b")
        self.assertEqual(names["Bank Rec"], "s1-bank-rec")

    def test_both_checks_pass_the_live_rerun(self):
        made = [str(self.book)]
        self.assertEqual(long_task.lost_sheets(RUN, self.state, self.step("s2-purchase-register"), made), [])
        self.assertEqual(long_task._removed_residue(RUN, made, ""), [])

    def test_figures_may_change_but_another_records_row_may_not_go(self):
        book = openpyxl.load_workbook(self.book)
        sheet = book["Bank Rec"]
        numeric = next(c for row in sheet.iter_rows(min_row=2) for c in row
                       if isinstance(c.value, (int, float)) and not isinstance(c.value, bool))
        numeric.value = numeric.value + 100  # a figure the change may explain
        book.save(self.book)
        self.assertEqual(long_task.lost_sheets(RUN, self.state, self.step("s2-purchase-register"), [str(self.book)]), [])
        book = openpyxl.load_workbook(self.book)
        book["Bank Rec"].delete_rows(3)  # a row naming no cancelled record
        book.save(self.book)
        found = long_task.lost_sheets(RUN, self.state, self.step("s2-purchase-register"), [str(self.book)])
        self.assertEqual(len(found), 1, found)
        self.assertIn("\"Bank Rec\" that the stage \"" + self.step("s1-bank-rec")["title"], found[0])


if __name__ == "__main__":
    unittest.main()
