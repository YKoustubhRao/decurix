"""
Finding BK (2026-10-20, round 5 P5): the August sales export repeats the week 8-14 August (35 rows); summed, every
August and quarter figure was inflated (quarter 3,63,68,806 against a true 3,40,40,974). office read now states the
repeat, and a total that counts it with no word of it is not done. The live CSVs are read where the test data keeps
them (skipped when they are not on this machine).
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, task_context
from decurix_agent.tools import office, table_quality
import fixture_paths  # noqa: E402 - what may not be on this machine is counted

P5 = Path(r"C:\Users\Administrator\decurix-tests\wide\process\data\P5")


class Isolated(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for patch in (mock.patch.object(config, "DECURIX_DIR", self.root), mock.patch.object(task_context, "current",
                                                                                               return_value="t0123456789abcdef")):
            patch.start()
            self.addCleanup(patch.stop)


class FactsOfOneTable(Isolated):
    def test_exact_repeats_a_renumbered_copy_and_nothing_on_a_clean_table(self):
        header = ["Date", "Invoice No", "Customer", "Amount"]
        rows = [["01-08-2026", "INV-1", "A", "100"], ["02-08-2026", "INV-2", "B", "200"],
                ["03-08-2026", "INV-3", "C", "300"], ["01-08-2026", "INV-1", "A", "100"],
                ["02-08-2026", "INV-2", "B", "200"], ["03-08-2026", "INV-9", "C", "300"]]
        facts = table_quality.table_facts(header, rows)
        self.assertIn("2 row(s) are exact copies of earlier rows: rows 5-6, dated 2026-08-01 to 2026-08-02 - the same "
                      "rows, in the same order, as rows 2-3", facts["lines"][0])
        self.assertIn("1 row(s) repeat an earlier row in everything but Invoice No: rows 7", facts["lines"][1])
        self.assertEqual(facts["totals"]["Amount"], (1200, 900))
        clean = table_quality.table_facts(header, rows[:3])
        self.assertEqual(clean["lines"], [])


@fixture_paths.skip_unless(P5.is_dir(), "the P5 test data is not on this machine")
class TheLiveP5Exports(Isolated):
    def read_all(self):
        return {f: office.read_document(str(P5 / f))["text"] for f in
                ("sales_export_Jul2026.csv", "sales_export_Aug2026.csv", "sales_export_Sep2026.csv")}

    def test_office_read_states_the_repeated_week(self):
        texts = self.read_all()
        aug = texts["sales_export_Aug2026.csv"].splitlines()[0]
        self.assertTrue(aug.startswith("Data check"), aug)
        self.assertIn("35 row(s) are exact copies of earlier rows: rows 175-209, dated 2026-08-08 to 2026-08-14", aug)
        for clean in ("sales_export_Jul2026.csv", "sales_export_Sep2026.csv"):
            self.assertFalse(texts[clean].startswith("Data check"), clean)

    def test_a_quarter_total_that_counts_the_repeat_is_not_done(self):
        self.read_all()
        inflated = table_quality.inflated_totals("Q2 revenue | 3,63,68,806\nWest Aug | 12,00,437", "Built the MIS pack.")
        self.assertTrue(any("3,63,68,806 is the Net Amount (Rs) total including the rows sales_export_Aug2026.csv "
                            "repeats" in f and "without them it is 34,040,974" in f for f in inflated), inflated)
        self.assertEqual(table_quality.inflated_totals("Q2 revenue | 3,40,40,974", "Built the MIS pack."), [])
        # Said in the report or not, the figure is found: no word decides it (harness audit, 2026-10-21).
        self.assertTrue(table_quality.inflated_totals("Q2 revenue | 3,63,68,806",
                                                      "August repeats 35 rows (8-14 Aug); I kept them as the file has them."))
        # Its own check: a loud advisory - it fails no stage, and the person sees it under "Needs you".
        problems = long_task.repeated_rows([], "Built the MIS pack. Q2 revenue 3,63,68,806.")
        self.assertTrue(any("include rows the input repeats" in p for p in problems), problems)
        self.assertFalse(long_task.is_blocking("repeated_rows"))

    def test_the_live_series_10_report_and_figures_are_caught(self):
        # Round 5 P5 (2026-10-21): "Removed the prior `Expenses status` note sheet" switched the check off; the
        # outputs showed the quarter as 3.64 crore in the deck and West/North August in the commentary.
        self.read_all()
        report = "Removed the prior `Expenses status` note sheet. Added `Expenses by branch` with quarter totals."
        found = table_quality.inflated_totals("Q2 revenue Rs 3.64 crore", report)
        self.assertTrue(any("3.64 crore is the Net Amount (Rs) total" in f for f in found), found)
        found = table_quality.inflated_totals("West revenue in August was 5,600,437 against a target of 4,400,000.", report)
        self.assertTrue(any("5,600,437 is the Net Amount (Rs) for Region=West total" in f for f in found), found)
        # A rounded figure the right total also rounds to tells nothing: no finding.
        self.assertEqual(table_quality.inflated_totals("Q2 revenue about Rs 3 crore", report), [])
        # The right August West (without the repeated week) passes.
        self.assertEqual(table_quality.inflated_totals("West revenue in August was 4,522,080.", report), [])
        # No word switches it off: "kept" or "excluded", the inflated figure is found (series 11: "with the repeated
        # Aug rows excluded" - the workbook still held them).
        self.assertTrue(table_quality.inflated_totals("Q2 revenue Rs 3.64 crore", "Kept the duplicated week as given."))
        said = "Recalculated actual revenue by region and month with the repeated Aug rows excluded."
        self.assertTrue(table_quality.inflated_totals("Q2 revenue Rs 3.64 crore", said))

    def test_the_inputs_are_read_here_whatever_the_worker_read_them_with(self):
        # Series 11: no office read record - the check reads the request's input tables itself.
        with mock.patch.object(table_quality, "_known", lambda: {}):
            files = [str(P5 / f) for f in ("sales_export_Jul2026.csv", "sales_export_Aug2026.csv",
                                           "sales_export_Sep2026.csv")]
            self.assertEqual(table_quality.inflated_totals("Q2 revenue Rs 3.64 crore", ""), [])
            found = table_quality.inflated_totals("North 10,009,817 | Q2 revenue Rs 3.64 crore", "", files)
            self.assertTrue(any("10,009,817 is the Net Amount (Rs) for Region=North" in f for f in found), found)
            self.assertEqual(table_quality.inflated_totals("Q2 revenue 34,040,974", "", files), [])


if __name__ == "__main__":
    unittest.main()
