"""
A figure the result presents as a source's own ("Closing balance per bank statement") against what the source states.
Found live (2026-10-20, round 5, real Excel): a reconciliation arrived at "Closing balance per bank statement" -14,140.00;
the bank statement's own last balance was 398,360.00; the report said -14,140.00 and passed.

    PYTHONPATH="agent;agent/tests" python -m unittest test_sources
"""

import shutil
import unittest
from decimal import Decimal
from pathlib import Path
from unittest import mock

import openpyxl

from temp_case import TempDirCase

from decurix_agent import config, produced, sources
import fixture_paths  # noqa: E402 - what may not be on this machine is counted

# The round's bank statement, as the person gave it (decurix-tests/wide/apps, bank_oct.csv).
BANK_CSV = """Date,Description,Debit,Credit,Balance
2026-10-02,NEFT SHARMA TRADERS,,84000,496500.0
2026-10-04,RENT OCT,65000,,431500.0
2026-10-07,KAVERI ELECTRICALS CHQ 1182,23600,,407900.0
2026-10-09,NEFT BLUE LOTUS,,152500,560400.0
2026-10-14,SALARIES OCT,210000,,350400.0
2026-10-18,GST PAYMENT,41250,,309150.0
2026-10-21,NEFT PINNACLE INFRA,,97300,406450.0
2026-10-25,ELECTRICITY,8740,,397710.0
2026-10-31,BANK CHARGES,590,,397120.0
2026-10-31,INTEREST CREDIT,,1240,398360.0
"""

REQUEST = ("Use Excel (the Excel app on your desktop) to do the October bank reconciliation from bank_oct.csv (the bank "
           "statement) and cashbook_oct.xlsx (our cash book), both in C:\\Users\\Administrator\\Documents\\Decurix demo. "
           "Make bank_rec_oct.xlsx in that folder with a sheet Exceptions listing every item that is on only one side "
           "(which side, date, description, amount), and a sheet Reconciliation that starts from the closing balance "
           "per cash book, adds and subtracts the exceptions, and arrives at the closing balance per bank statement.")

REAL_RESULT = Path(r"C:\Users\Administrator\Documents\Decurix demo\bank_rec_oct.xlsx")


def cashbook(path):
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Cash Book"
    ws.append(["Date", "Particulars", "Receipt", "Payment"])
    ws.append(["2026-10-02", "NEFT Sharma Traders", 84000, None])
    ws.append(["2026-10-04", "Rent Oct", None, 65000])
    wb.save(path)
    return path


def reconciliation(path, bank_close, book_close=-41290):
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Reconciliation"
    ws.append(["Particulars", "Amount"])
    ws.append(["Closing balance per cash book", book_close])
    ws.append(["Add: Cheque issued but not yet presented", 45000])
    ws.append(["Less: Deposit in transit", -18500])
    ws.append(["Closing balance per bank statement", bank_close])
    wb.save(path)
    return path


class Fixture(TempDirCase):

    def setUp(self):
        super().setUp()
        self.bank = self.dir / "bank_oct.csv"
        self.bank.write_text(BANK_CSV, encoding="utf-8")
        self.book = cashbook(self.dir / "cashbook_oct.xlsx")
        self.inputs = [{"name": "bank_oct.csv", "path": str(self.bank), "about": "bank oct bank statement"},
                       {"name": "cashbook_oct.xlsx", "path": str(self.book), "about": "cashbook oct cash book"}]


class WhatASourceStates(Fixture):

    def test_a_balance_columns_last_line_is_its_closing_balance(self):
        facts = sources.stated(self.bank)
        self.assertEqual(facts["closing"][0], Decimal("398360.0"))
        self.assertIn("Balance column, row 11", facts["closing"][1])
        self.assertNotIn("opening", facts)  # the first line is not an opening balance

    def test_a_cash_book_with_no_balance_states_nothing(self):
        self.assertEqual(sources.stated(self.book), {"totals": []})

    def test_labelled_rows_state_their_figure(self):
        path = self.dir / "ledger.csv"
        path.write_text("Particulars,Amount\nOpening balance,\"1,000.00\"\nSales,500\nClosing balance,1500\n"
                        "Total,\"(250.00)\"\n", encoding="utf-8")
        facts = sources.stated(path)
        self.assertEqual(facts["opening"][0], Decimal("1000.00"))
        self.assertEqual(facts["closing"][0], Decimal("1500"))
        self.assertEqual(facts["totals"][0][0], Decimal("-250.00"))


class AFigurePerASource(Fixture):

    def test_a_closing_balance_the_statement_contradicts_is_found(self):
        result = reconciliation(self.dir / "bank_rec_oct.xlsx", bank_close=-14140)
        found = sources.mismatches(REQUEST, [str(result)], found_inputs=self.inputs)
        self.assertEqual(found, ['bank_rec_oct.xlsx: Reconciliation!B5 "Closing balance per bank statement" is '
                                 '-14,140.00, but bank_oct.csv itself states 398,360.00 (the last line of its Balance '
                                 'column, row 11)'])

    def test_the_right_balance_passes_and_a_source_that_states_nothing_decides_nothing(self):
        result = reconciliation(self.dir / "bank_rec_oct.xlsx", bank_close=398360, book_close=-41290)
        # The cash book line is wrong too (no opening balance), but the cash book states no balance: not decided here.
        self.assertEqual(sources.mismatches(REQUEST, [str(result)], found_inputs=self.inputs), [])

    def test_a_label_that_matches_no_single_input_decides_nothing(self):
        result = reconciliation(self.dir / "r.xlsx", bank_close=-14140)
        both = [dict(i, about="bank statement cash book") for i in self.inputs]
        self.assertEqual(sources.mismatches(REQUEST, [str(result)], found_inputs=both), [])
        self.assertEqual(sources.mismatches(REQUEST, [str(result)], found_inputs=[]), [])

    def test_inputs_are_found_by_name_with_the_words_the_request_puts_beside_them(self):
        work = self.dir / "work"
        work.mkdir()
        shutil.copy(self.bank, work / "bank_oct.csv")
        shutil.copy(self.book, work / "cashbook_oct.xlsx")
        with mock.patch.object(config, "WORK_DIR", work), mock.patch.object(produced, "copy_from_host", lambda *a, **k: []):
            found = {i["name"]: i["about"] for i in sources.inputs(REQUEST, made=[str(work / "bank_rec_oct.xlsx")])}
        self.assertEqual(found, {"bank_oct.csv": "bank oct bank statement", "cashbook_oct.xlsx": "cashbook oct cash book"})

    def test_the_judge_is_shown_what_the_inputs_state_for_balance_work(self):
        with mock.patch.object(sources, "inputs", lambda request, made=(): self.inputs):
            text = sources.evidence(REQUEST)
            self.assertIn("bank_oct.csv: closing balance 398,360.00", text)
            self.assertEqual(sources.evidence("Make a price list workbook from these files."), "")

    @fixture_paths.skip_unless(REAL_RESULT.is_file(), "the round's saved reconciliation is not on this machine")
    def test_the_rounds_real_reconciliation(self):
        # The file is the latest run's: the one that ended at -14,140.00 is found, a later correct one passes.
        copy = self.dir / "bank_rec_oct.xlsx"
        shutil.copy(REAL_RESULT, copy)  # read-only: a copy is read
        bank_line = [line for line in sources._per_source_lines(copy) if line[3] == ["bank", "statement"]]
        self.assertEqual(len(bank_line), 1)
        found = sources.mismatches(REQUEST, [str(copy)], found_inputs=self.inputs)
        if bank_line[0][4] == Decimal("398360"):
            self.assertEqual(found, [])
        else:
            self.assertEqual(len(found), 1)
            self.assertIn(f'"Closing balance per bank statement" is {bank_line[0][4]:,.2f}, but bank_oct.csv itself '
                          'states 398,360.00', found[0])


class AtTheGate(Fixture):

    def test_file_facts_carry_the_mismatch(self):
        result = reconciliation(self.dir / "bank_rec_oct.xlsx", bank_close=-14140)
        with mock.patch.object(sources, "inputs", lambda request, made=(): self.inputs):
            facts = produced.file_facts(REQUEST, [str(result)])
        self.assertEqual(len(facts.source_mismatch), 1)
        self.assertIn("398,360.00", facts.source_mismatch[0])


import os  # noqa: E402
import sys  # noqa: E402

sys.path.insert(0, os.path.dirname(__file__))
from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)
from decurix_agent import skill_task  # noqa: E402


class AtTheFinalCheck(FileWorkIsChecked):
    """At the final check a figure the source contradicts is not done, decided before any model is asked."""

    def test_a_contradicted_source_figure_is_sent_back_without_a_model(self):
        self.verdicts = []
        found = ['bank_rec_oct.xlsx: Reconciliation!B7 "Closing balance per bank statement" is -14,140.00, but '
                 'bank_oct.csv itself states 398,360.00']
        skill_task.state.current_task = REQUEST
        with mock.patch.object(skill_task.produced, "file_facts",
                               lambda *a, **k: produced.FileFacts(source_mismatch=found)),                 mock.patch.object(skill_task.produced, "missing_highlight", lambda paths, request="": []):
            self.assertTrue(skill_task._gate("Saved bank_rec_oct.xlsx, ending at -14,140.00.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("bank_oct.csv itself states 398,360.00", self.asked[-1][0])


if __name__ == "__main__":
    unittest.main()
