"""
A workbook's formatting as `office read` reports it, and the code-decided fact that a highlight asked for was not
applied at all. Found live (2026-10-20, round 5, real Excel): asked for "a red fill on every overdue invoice row" and
filters on the header row, the worker could not tell from office read whether either was in the saved file, and the
file had no fill and no conditional format.

    PYTHONPATH="agent;agent/tests" python -m unittest test_workbook_formatting
"""

import unittest

from needs import require

require("openpyxl")  # every test here reads or writes workbooks
import openpyxl
from openpyxl.formatting.rule import CellIsRule, ColorScaleRule, FormulaRule
from openpyxl.styles import Font, PatternFill

from temp_case import TempDirCase

from decurix_agent import produced
from decurix_agent.tools import office

ASK = "Add Days Overdue, a red fill on every overdue invoice row, filters on the header row, and freeze the header."


def _book(path, fills=(), rules=(), autofilter=None, header_fill=False):
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Invoices"
    ws.append(["Invoice", "Amount", "Days Overdue"])
    for n in range(2, 12):
        ws.append([f"INV-{n}", 100.0 * n, (n * 4) if n % 3 else 0])
        ws.cell(n, 2).number_format = "#,##0.00"
    if header_fill:
        for cell in ws[1]:
            cell.fill = PatternFill(start_color="FFD9D9D9", end_color="FFD9D9D9", fill_type="solid")
    for row, colour in fills:
        for cell in ws[row]:
            cell.fill = PatternFill(start_color=colour, end_color=colour, fill_type="solid")
    for target, rule in rules:
        ws.conditional_formatting.add(target, rule)
    if autofilter:
        ws.auto_filter.ref = autofilter
    wb.save(path)
    return path


class Reading(TempDirCase):

    def read(self, path):
        return office.read_workbook(path)

    def test_static_fills_are_named_and_grouped_into_runs(self):
        path = _book(self.dir / "a.xlsx", fills=[(2, "FFFF0000"), (5, "FFFF0000"), (9, "FFFF0000"), (10, "FFFF0000"),
                                                  (11, "FFFF0000"), (3, "FFFFFF00")])
        text = self.read(path)
        self.assertIn("red FF0000 fill on rows 2, 5, 9-11", text)
        self.assertIn("yellow FFFF00 fill on rows 3", text)
        self.assertIn("Number formats: B #,##0.00", text)

    def test_conditional_formats_say_their_rule_and_what_they_apply(self):
        path = _book(self.dir / "b.xlsx", rules=[
            ("A2:C11", FormulaRule(formula=["$C2>0"], fill=PatternFill(bgColor="FFFF0000", fill_type="solid"))),
            ("C2:C11", CellIsRule(operator="greaterThan", formula=["30"], font=Font(color="FF9C0006", bold=True))),
            ("B2:B11", ColorScaleRule(start_type="min", start_color="FFF8696B", end_type="max", end_color="FF63BE7B"))])
        text = self.read(path)
        self.assertIn("A2:C11: expression =$C2>0 -> fill red FF0000", text)
        self.assertIn("C2:C11: cellIs greaterThan =30 -> font dark red 9C0006, bold", text)
        self.assertIn("B2:B11: colorScale", text)

    def test_the_filter_range_is_reported(self):
        self.assertIn("Filter: on A1:C11", self.read(_book(self.dir / "c.xlsx", autofilter="A1:C11")))

    def test_a_plain_workbook_says_none_of_it(self):
        text = self.read(_book(self.dir / "d.xlsx"))
        for line in ("Filter: none", "Conditional formatting: none", "Fills: none"):
            self.assertIn(line, text)


class HighlightNotApplied(TempDirCase):

    def test_nothing_applied_is_a_finding(self):
        # A header fill is not a highlight of the rows asked for.
        path = _book(self.dir / "plain.xlsx", autofilter="A1:C11", header_fill=True)
        found = produced.missing_highlight([str(path)], ASK)
        self.assertEqual(found, ["plain.xlsx: no row has a fill and no conditional format exists"])

    def test_a_fill_or_a_rule_is_enough_for_code_the_rest_is_for_the_check(self):
        filled = _book(self.dir / "filled.xlsx", fills=[(4, "FFFF0000")])
        ruled = _book(self.dir / "ruled.xlsx", rules=[
            ("A2:C11", FormulaRule(formula=["$C2>0"], fill=PatternFill(bgColor="FFFFC7CE", fill_type="solid")))])
        self.assertEqual(produced.missing_highlight([str(filled)], ASK), [])
        self.assertEqual(produced.missing_highlight([str(ruled)], ASK), [])

    def test_only_a_request_that_asks_for_it(self):
        path = _book(self.dir / "plain.xlsx")
        for request in ("Add a Days Overdue column and a By Customer sheet.",
                        "List the green tea orders and the red wine invoices.",
                        "Fill in the missing amounts."):
            self.assertEqual(produced.missing_highlight([str(path)], request), [], request)
        for request in ("Highlight the overdue rows.", "Use conditional formatting on the late invoices.",
                        "Give overdue rows a red fill.", "Shade the overdue rows.", "fill them in yellow"):
            self.assertTrue(produced.missing_highlight([str(path)], request), request)

    def test_files_that_are_not_workbooks_decide_nothing(self):
        note = self.dir / "note.txt"
        note.write_text("x", encoding="utf-8")
        self.assertEqual(produced.missing_highlight([str(note)], ASK), [])


import os  # noqa: E402
import sys  # noqa: E402
from unittest import mock  # noqa: E402

sys.path.insert(0, os.path.dirname(__file__))
from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)
from decurix_agent import skill_task  # noqa: E402


class HighlightAtTheGate(FileWorkIsChecked):
    """At the final check: no fill and no rule at all is not done, decided before any model is asked."""

    def gate(self, request, report, found):
        skill_task.state.current_task = request
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []),                 mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""),                 mock.patch.object(skill_task.produced, "typed_totals", lambda paths, request="": []),                 mock.patch.object(skill_task.produced, "missing_highlight", lambda paths, request="": list(found)):
            return skill_task._gate(report, "final", "thebot")

    def test_a_highlight_that_is_nowhere_in_the_file_is_evidence_for_the_judge_and_the_person(self):
        # Harness audit (2026-10-21): whether the request asked for a fill is a reading of it - evidence, not a verdict.
        from test_apprentice import verdict
        from decurix_agent import verify
        self.verdicts = [verdict(verify.DONE, seen="the file is there")]
        self.assertTrue(self.gate(ASK + " Save it as receivables_review.xlsx in my work folder.",
                                  "Saved receivables_review.xlsx.",
                                  ["receivables_review.xlsx: no row has a fill and no conditional format exists"]))
        self.assertIn("no row has a fill and no conditional format exists", self.judged[0]["produced"])
        self.assertIn("Please check", self.finals[-1][0])
        self.assertIn("no row has a fill and no conditional format exists", self.finals[-1][0])


if __name__ == "__main__":
    unittest.main()
