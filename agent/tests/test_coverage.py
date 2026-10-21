"""
A column filled down a table must reach the end of the table, and a Total must cover it. Found live (2026-10-19, real
Excel): Unit Price and Line Total were filled for rows 2 to 24 of a table that runs to row 25; the Total row's
=SUM(E2:E25) showed 515127 for a true 579607, and the report said "verified" - the worker's own sum agreed with what
was there. Decided in code from the saved file, never by the judge.
"""

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from temp_case import TempDirCase  # noqa: E402

try:
    import openpyxl
except ImportError:  # pragma: no cover
    openpyxl = None

from decurix_agent import produced, skill_task, verify
from decurix_agent.tools import office

QTY = 51
PRICE = 640
NOT_FOUND = (14, 15)


def workbook(path, filled=range(2, 26), total="=SUM(E2:E25)", total_row=26, blank_line_total=(), skip_header=False):
    """
    The lookup workbook of the live case: 24 order rows (2 to 25) with A order, B SKU, C qty; D and E formulas on the rows
    in `filled`. Returns (path, the workbook LibreOffice would calculate from it).
    """

    wb, calc = openpyxl.Workbook(), openpyxl.Workbook()
    ws, cs = wb.active, calc.active
    ws.title = cs.title = "Orders"
    for sheet in (ws, cs):
        sheet.append(["Order", "SKU", "Qty"] + ([] if skip_header else ["Unit Price", "Line Total"]))
        for r in range(2, 26):
            sheet.append([f"ORD-{3300 + r}", f"SKU-{100 + r}", QTY])
    for r in filled:
        ws[f"D{r}"] = f'=IFERROR(VLOOKUP(B{r},Prices!$A$2:$C$13,3,FALSE),"NOT FOUND")'
        if r not in blank_line_total:
            ws[f"E{r}"] = f'=IF(ISTEXT(D{r}),"",C{r}*D{r})'
        missing = r in NOT_FOUND
        cs[f"D{r}"] = "NOT FOUND" if missing else PRICE
        cs[f"E{r}"] = "" if missing else QTY * PRICE
    if total:
        ws.cell(row=total_row, column=1, value="Total")
        ws.cell(row=total_row, column=5, value=total)
    wb.save(path)
    return str(path), calc


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class Coverage(TempDirCase):
    def gaps(self, **kw):
        path, calc = workbook(self.dir / "orders.xlsx", **kw)
        with mock.patch.object(office, "_recalculated_values", lambda p, o: calc):
            self.assertTrue(office._store_results(Path(path)))
        return office.coverage_gaps(path), path

    def test_a_complete_table_has_no_gap(self):
        self.assertEqual(self.gaps()[0], [])

    def test_the_live_case_a_last_row_never_filled_is_named(self):
        found, _ = self.gaps(filled=range(2, 25))
        self.assertEqual(len(found), 2)
        self.assertIn("Orders!D25 is empty, but D2:D24 hold one formula filled down and the table beside it runs to "
                      "row 25", found[0])
        self.assertIn("Orders!E25 is empty, but E2:E24", found[1])

    def test_a_hole_inside_names_its_first_row_and_counts_the_rest(self):
        found, _ = self.gaps(filled=[r for r in range(2, 26) if r not in (10, 11)])
        self.assertIn("Orders!D10 is empty", found[0])
        self.assertIn("(2 rows are missing)", found[0])

    def test_a_line_total_left_empty_where_the_price_says_not_found_is_explained(self):
        # Rows 14 and 15 say NOT FOUND in D; their Line Total is blank on purpose (the request asks for it).
        found, _ = self.gaps(blank_line_total=NOT_FOUND)
        self.assertEqual(found, [])

    def test_a_row_with_nothing_explaining_it_is_not_excused(self):
        found, _ = self.gaps(filled=[r for r in range(2, 26) if r != 14])
        self.assertTrue(any("D14 is empty" in f for f in found), found)

    def test_a_missing_header_is_not_a_gap_and_a_few_formulas_or_typed_values_are_ignored(self):
        self.assertEqual(self.gaps(skip_header=True)[0], [])
        self.assertEqual(self.gaps(filled=[2, 3])[0], [])  # two formulas are not a fill-down
        path, calc = workbook(self.dir / "typed.xlsx", filled=[])
        ws = openpyxl.load_workbook(path)
        ws.active["D2"], ws.active["D3"], ws.active["D5"] = 640, 1180, 210  # typed figures, with holes
        ws.save(path)
        self.assertEqual(office.coverage_gaps(path), [])

    def test_a_total_that_stops_short_of_the_table_above_it(self):
        found, _ = self.gaps(total="=SUM(E2:E24)")
        self.assertEqual(found, ["Orders!E26 sums E2:E24, but the table above it runs to row 25"])
        self.assertEqual(self.gaps(total="=SUM(E2:E25)")[0], [])

    def test_a_total_over_a_column_with_a_blank_last_row_is_flagged_through_the_column(self):
        # The live case exactly: the Total's range is right, the column under it is not.
        found, _ = self.gaps(filled=range(2, 25), total="=SUM(E2:E25)")
        self.assertTrue(any(f.startswith("Orders!E25 is empty") for f in found))

    def test_the_ends_of_a_sheet_are_shown_cell_by_cell_with_empty_ones_named(self):
        _, path = self.gaps(filled=range(2, 25))[0], str(self.dir / "orders.xlsx")
        tail = office.workbook_tail(path)
        self.assertIn("[end of sheet 'Orders', its last 4 row(s)]", tail)
        self.assertIn("D25: (empty)", tail)
        self.assertIn("E24: =IF(ISTEXT(D24),\"\",C24*D24) -> 32640", tail)
        self.assertIn("E26: =SUM(E2:E25) -> ", tail)
        self.assertEqual(office.workbook_tail(str(self.dir / "notes.txt")), "")

    def test_the_shape_of_a_formula_is_relative_to_its_cell(self):
        self.assertEqual(office.formula_shape("=C2*D2", 2, 5), office.formula_shape("=C9*D9", 9, 5))
        self.assertNotEqual(office.formula_shape("=C2*D2", 2, 5), office.formula_shape("=C9*D8", 9, 5))
        self.assertEqual(office.formula_shape("=B2*$G$1", 2, 5), office.formula_shape("=B7*$G$1", 7, 5))
        self.assertIn("LOG10(", office.formula_shape("=LOG10(B2)", 2, 3))


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class CoverageAtEveryRoute(TempDirCase):
    def setUp(self):
        super().setUp()
        self.path, self.calc = workbook(self.dir / "orders.xlsx", filled=range(2, 25))
        patch = mock.patch.object(office, "_recalculated_values", lambda p, o: self.calc)
        patch.start()
        self.addCleanup(patch.stop)

    def test_file_facts_carries_the_gaps_and_a_stage_is_sent_back_for_them(self):
        from decurix_agent import long_task

        facts = produced.file_facts("Add Unit Price and Line Total with formulas.", [self.path])
        # Evidence, not a verdict (independent review, 2026-10-21): an intentional blank looks the same.
        self.assertEqual(facts.gaps, [])
        self.assertEqual(len(facts.gap_notes), 2)
        self.assertIn("orders.xlsx: Orders!D25 is empty", facts.gap_notes[0])
        self.assertIn("orders.xlsx: Orders!D25 is empty", " ".join(facts.notes()))

    def test_the_judge_is_shown_the_end_of_the_workbook_even_when_the_sheet_is_long(self):
        text = produced.text([self.path])
        self.assertIn("D25: (empty)", text)
        self.assertIn("E26: =SUM(E2:E25)", text)


from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)


class TheGateSendsBackAShortFill(FileWorkIsChecked):
    def test_a_column_that_stops_short_is_evidence_for_the_judge(self):
        self.verdicts = [{"verdict": verify.NOT_DONE, "confidence": 0.9, "seen": "D25 is empty", "why": ""}]
        gaps = ["orders.xlsx: Orders!D25 is empty, but D2:D24 hold one formula filled down and the table beside it runs "
                "to row 25"]
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""), \
                mock.patch.object(skill_task.produced, "coverage_gaps", lambda paths: list(gaps)):
            skill_task._gate("Done: E26 = 515127, verified by reading the sheet.", "final", "thebot")
        self.assertIn("Orders!D25 is empty", self.judged[0]["produced"])

    def test_a_formula_that_shows_an_error_is_not_done_without_a_model(self):
        self.verdicts = []
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""), \
                mock.patch.object(skill_task.produced, "formula_errors",
                                  lambda paths: ["orders.xlsx: Orders!E5 shows #REF!"]):
            self.assertTrue(skill_task._gate("Done.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertIn("Orders!E5 shows #REF!", self.asked[-1][0])

    def test_a_complete_table_goes_on_to_the_judge(self):
        self.verdicts = [{"verdict": verify.DONE, "confidence": 0.95, "seen": "all 24 rows", "why": ""}]
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""), \
                mock.patch.object(skill_task.produced, "coverage_gaps", lambda paths: []):
            self.assertTrue(skill_task._gate("Done.", "final", "thebot"))
        self.assertEqual(len(self.judged), 1)


if __name__ == "__main__":
    unittest.main()


def gst_register(path, typed_from=4, split_total=False, label_column=1):
    """
    The calc_gst workbook: rows 2 to 13 of a register, CGST and Invoice Total columns (F, I) as formulas, typed numbers from
    row `typed_from` on (and 14 to 99 meaning none); the Total row in 14 - or, split, its label in 14 and its sums in 15.
    """

    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Register"
    ws.append(["Invoice", "Date", "Customer", "State", "Taxable Value", "CGST", "SGST", "IGST", "Invoice Total"])
    for r in range(2, 14):
        ws.append([f"MH/26/{700 + r}", None, f"Customer {r}", "Maharashtra", 100000, None, None, None, None])
        if r < typed_from:
            ws[f"F{r}"] = f'=IF(LOWER(TRIM(D{r}))="maharashtra",ROUND(E{r}*9%,2),0)'
            ws[f"I{r}"] = f"=ROUND(E{r}+F{r}+G{r}+H{r},2)"
        else:
            ws[f"F{r}"], ws[f"I{r}"] = 9000, 118000
        ws[f"G{r}"], ws[f"H{r}"] = 9000, 0
    total_row = 14
    ws.cell(row=total_row, column=label_column, value="Total")
    sums_row = total_row + 1 if split_total else total_row
    for col in "FGHI":
        ws[f"{col}{sums_row}"] = f"=SUM({col}2:{col}13)"
    wb.save(path)
    return str(path)


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class TypedNumbersAmongFormulas(TempDirCase):
    def test_the_live_case_rows_typed_in_under_a_formula_are_named(self):
        found = office.constants_among_formulas(gst_register(self.dir / "g.xlsx", typed_from=4))
        self.assertEqual(len(found), 2)  # F and I carry formulas in rows 2-3; G and H are typed throughout
        self.assertIn("Register!F4 is the typed number 9000, but F2 is a formula: 10 cell(s) of column F are typed "
                      "numbers where the formula belongs", found[0])
        self.assertIn("Register!I4 is the typed number 118000", found[1])

    def test_a_column_that_is_formulas_throughout_or_typed_throughout_is_fine(self):
        self.assertEqual(office.constants_among_formulas(gst_register(self.dir / "all.xlsx", typed_from=99)), [])
        self.assertEqual(office.constants_among_formulas(gst_register(self.dir / "none.xlsx", typed_from=2)), [])

    def test_a_sum_over_other_rows_is_not_a_formula_filled_down(self):
        # A subtotal inside a table is a formula over other rows; a typed figure beside it is not "a hole in a fill".
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.append(["Item", "Qty", "Note"])
        for r in range(2, 8):
            ws.append([f"Item {r}", r, 5])
        ws["C5"] = "=SUM(B2:B4)"
        wb.save(self.dir / "sub.xlsx")
        self.assertEqual(office.constants_among_formulas(str(self.dir / "sub.xlsx")), [])


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class ASplitTotal(TempDirCase):
    def test_the_live_case_a_label_on_one_row_and_its_sums_on_the_next(self):
        found = office.split_totals(gst_register(self.dir / "g.xlsx", split_total=True, label_column=6))
        self.assertEqual(len(found), 1)
        self.assertIn("Register!F14 says 'Total' but its row holds no figure; the sums are on row 15 (F15, G15, H15, "
                      "I15)", found[0])

    def test_a_total_row_with_its_sums_beside_the_label_is_fine_wherever_the_label_is(self):
        self.assertEqual(office.split_totals(gst_register(self.dir / "a.xlsx")), [])
        self.assertEqual(office.split_totals(gst_register(self.dir / "f.xlsx", label_column=6)), [])

    def test_a_heading_with_nothing_summed_below_it_is_not_a_split_total(self):
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.append(["Item", "Qty"])
        ws.append(["a", 1])
        ws["A3"] = "Totals by region follow"
        ws["A4"] = "North"
        ws["B4"] = 10
        wb.save(self.dir / "h.xlsx")
        self.assertEqual(office.split_totals(str(self.dir / "h.xlsx")), [])


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class WhatTheRequestAskedGatesThem(TempDirCase):
    def setUp(self):
        super().setUp()
        self.path = gst_register(self.dir / "g.xlsx", typed_from=4, split_total=True, label_column=6)

    def test_formulas_asked_names_the_typed_rows_and_a_total_row_asked_names_the_split(self):
        asked = ("Add the columns CGST, SGST, IGST and Invoice Total with formulas. Add a Total row at the bottom.")
        found = produced.layout_faults([self.path], asked)
        self.assertEqual(len(found), 3)  # F and I (typed from row 4), and the split Total; G and H are typed throughout
        self.assertTrue(found[0].startswith("g.xlsx: Register!F4 is the typed number"))
        self.assertIn("the sums are on row 15", found[-1])

    def test_a_request_that_asked_for_neither_is_not_judged_on_them(self):
        self.assertEqual(produced.layout_faults([self.path], "Add the columns and save it."), [])
        self.assertEqual(len(produced.layout_faults([self.path], "Add the columns with formulas.")), 2)
        self.assertEqual(len(produced.layout_faults([self.path], "Add a Total row at the bottom.")), 1)

    def test_what_asks_for_a_total_row(self):
        for asked in ("Add a Total row at the bottom.", "Under the data put a row labelled Total with the sum",
                      "a total line", "put the totals below the data"):
            self.assertTrue(produced.total_asked(asked), asked)
        for plain in ("What is the total revenue?", "Add the columns and save it."):
            self.assertFalse(produced.total_asked(plain), plain)

    def test_file_facts_and_a_stage_carry_it(self):
        from decurix_agent import long_task

        with mock.patch.object(produced, "store_results", lambda paths: []):
            facts = produced.file_facts("Add columns with formulas and a Total row.", [self.path])
            notes = []
            long_task.code_problems([self.path], "done", [], "Add columns with formulas and a Total row.",
                                    root=os.path.dirname(self.path), notes=notes)
        self.assertEqual(len(facts.layout), 3)
        self.assertIn("is the typed number", " ".join(notes))  # evidence, never a verdict


class TheGateSendsBackALayoutFault(FileWorkIsChecked):
    def test_typed_rows_are_evidence_for_the_judge(self):
        from test_apprentice import verdict
        from decurix_agent import verify
        self.verdicts = [verdict(verify.NOT_DONE, seen="rows 4-13 are typed")]
        layout = ["g.xlsx: Register!F4 is the typed number 9000, but F2 is a formula: 10 cell(s) of column F are typed "
                  "numbers where the formula belongs"]
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""), \
                mock.patch.object(skill_task.produced, "layout_faults", lambda paths, request="": list(layout)):
            skill_task._gate("Done. Note: rows 4-13 were entered as fixed numbers rather than formulas.",
                             "final", "thebot")
        self.assertIn("Register!F4 is the typed number", self.judged[0]["produced"])
