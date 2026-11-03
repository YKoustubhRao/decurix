"""
Which line of a source states its balance, and what sign a balance carries, are the book's conventions: code reads them
for the judge and never fails work on them by itself (review 2026-11-03, E-13). A cash book's "15,000.00 Dr" is a
positive debit balance; a bank export listed newest first ends on its oldest line. Each difference still reaches the
judge as the exact fact (review round 2): with the opposite sign, or another line of the column.
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
    def test_a_dr_balance_given_as_a_positive_figure_is_evidence_of_the_sign_never_a_block(self):
        cash = self.dir / "cash_book.csv"
        cash.write_text("Date,Particulars,Debit,Credit,Balance\n01-09-2026,Opening,,,10000.00 Dr\n"
                        "15-09-2026,Sales,5000,,15000.00 Dr\n", encoding="utf-8")
        made = result(self.dir / "rec.xlsx", "Balance as per cash book", 15000)
        known = [{"name": "cash_book.csv", "path": str(cash), "about": "cash book"}]
        found = sources.mismatches("reconcile", [made], found_inputs=known)
        self.assertEqual(len(found), 1, found)
        self.assertIn("with the opposite sign", found[0])
        self.assertEqual(produced.FileFacts(source_mismatch=found).blocking(), [])

    def test_a_newest_first_export_is_evidence_of_which_line_never_a_block(self):
        bank = self.dir / "bank.csv"
        bank.write_text("Date,Narration,Withdrawal,Deposit,Balance\n30-09-2026,Transfer in,,2000,52000\n"
                        "01-09-2026,Opening,,,50000\n15-09-2026,Charge,,1000,51000\n", encoding="utf-8")
        made = result(self.dir / "rec.xlsx", "Closing balance per bank statement", 52000)
        known = [{"name": "bank.csv", "path": str(bank), "about": "bank statement"}]
        found = sources.mismatches("reconcile", [made], found_inputs=known)
        self.assertEqual(len(found), 1, found)
        self.assertIn("equals row 2's balance", found[0])
        self.assertEqual(produced.FileFacts(source_mismatch=found).blocking(), [])

    def test_an_overdraft_given_with_the_wrong_sign_and_the_opening_given_as_closing_reach_the_judge(self):
        bank = self.dir / "bank.csv"
        bank.write_text("Date,Narration,Withdrawal,Deposit,Balance\n01-09-2026,Opening,,,50000\n"
                        "15-09-2026,Charge,1000,,49000\n20-09-2026,Rent,60000,,-11000\n", encoding="utf-8")
        known = [{"name": "bank.csv", "path": str(bank), "about": "bank statement"}]
        flipped = sources.mismatches("reconcile", [result(self.dir / "a.xlsx", "Closing balance per bank statement",
                                                          11000)], found_inputs=known)
        self.assertTrue(flipped and "with the opposite sign" in flipped[0], flipped)
        opening = sources.mismatches("reconcile", [result(self.dir / "b.xlsx", "Closing balance per bank statement",
                                                          50000)], found_inputs=known)
        self.assertTrue(opening and "equals row 2's balance" in opening[0], opening)
        exact = sources.mismatches("reconcile", [result(self.dir / "c.xlsx", "Closing balance per bank statement",
                                                        -11000)], found_inputs=known)
        self.assertEqual(exact, [])
        facts = produced.FileFacts(source_mismatch=flipped + opening)
        self.assertEqual(facts.blocking(), [])
        self.assertEqual(facts.notes(), flipped + opening)

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
