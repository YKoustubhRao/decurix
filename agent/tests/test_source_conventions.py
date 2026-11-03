"""
Which line of a source states its balance, and what sign a balance carries, are the book's conventions: code reads them
for the judge and never fails work on them by itself (review 2026-11-03, E-13). A cash book's "15,000.00 Dr" is a
positive debit balance; a bank export listed newest first ends on its oldest line.
"""

import unittest

from needs import require

require("openpyxl")
import openpyxl

from temp_case import TempDirCase

from decurix_agent import produced, sources


def result(path, label, value):
    wb = openpyxl.Workbook()
    ws = wb.active
    ws["A1"], ws["B1"] = label, value
    wb.save(path)
    return str(path)


class BookConventionsAreNotMismatches(TempDirCase):
    def test_a_dr_balance_given_as_a_positive_figure_is_not_a_mismatch(self):
        cash = self.dir / "cash_book.csv"
        cash.write_text("Date,Particulars,Debit,Credit,Balance\n01-09-2026,Opening,,,10000.00 Dr\n"
                        "15-09-2026,Sales,5000,,15000.00 Dr\n", encoding="utf-8")
        made = result(self.dir / "rec.xlsx", "Balance as per cash book", 15000)
        known = [{"name": "cash_book.csv", "path": str(cash), "about": "cash book"}]
        self.assertEqual(sources.mismatches("reconcile", [made], found_inputs=known), [])

    def test_a_newest_first_export_is_not_read_as_ending_on_its_oldest_line(self):
        bank = self.dir / "bank.csv"
        bank.write_text("Date,Narration,Withdrawal,Deposit,Balance\n30-09-2026,Transfer in,,2000,52000\n"
                        "01-09-2026,Opening,,,50000\n15-09-2026,Charge,,1000,51000\n", encoding="utf-8")
        made = result(self.dir / "rec.xlsx", "Closing balance per bank statement", 52000)
        known = [{"name": "bank.csv", "path": str(bank), "about": "bank statement"}]
        self.assertEqual(sources.mismatches("reconcile", [made], found_inputs=known), [])

    def test_a_figure_the_source_holds_nowhere_is_still_found_as_evidence(self):
        bank = self.dir / "bank.csv"
        bank.write_text("Date,Narration,Withdrawal,Deposit,Balance\n01-09-2026,Opening,,,50000\n"
                        "15-09-2026,Charge,1000,,49000\n", encoding="utf-8")
        made = result(self.dir / "rec.xlsx", "Closing balance per bank statement", 61234)
        known = [{"name": "bank.csv", "path": str(bank), "about": "bank statement"}]
        found = sources.mismatches("reconcile", [made], found_inputs=known)
        self.assertEqual(len(found), 1)
        facts = produced.FileFacts(source_mismatch=found)
        self.assertEqual(facts.blocking(), [])
        self.assertEqual(facts.notes(), found)


if __name__ == "__main__":
    unittest.main()
