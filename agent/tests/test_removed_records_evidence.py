"""
A record the person removed: code finds only the mechanical fact (a row of its own, an arithmetic match) and the judge
reads the words (review 2026-11-03, E-07, E-08). No word list decides that a row "shows it taken out".
"""

import unittest

from temp_case import TempDirCase

from decurix_agent import stage_facts

REMOVED = [{"n": 1, "text": "Leave bill AB-0101 out of everything.", "removed": ["AB-0101"]}]


class TheRemovedRecordsRow(TempDirCase):
    def table(self, text):
        path = self.dir / "register.csv"
        path.write_text(text, encoding="utf-8")
        return [str(path)]

    def test_a_row_that_only_refers_to_it_is_not_its_row(self):
        made = self.table("Ref,Amount,Note\nCN-12,5000,credit note against AB-0101 - kept\n")
        self.assertEqual(stage_facts.removed_still_present(REMOVED, made), [])

    def test_its_own_row_is_quoted_whatever_words_it_uses_for_the_judge_to_read(self):
        for note in ("taken out at your request; not in the register or the return", "Cancelled", "as billed"):
            with self.subTest(note=note):
                made = self.table(f"Ref,Amount,Note\nAB-0101,66700,\"{note}\"\n")
                found = stage_facts.removed_still_present(REMOVED, made)
                self.assertEqual(len(found), 1)
                self.assertIn(note, found[0])
                self.assertIn("check that the row shows it taken out", found[0])

    def test_a_row_of_its_own_with_no_figures_is_not_reported(self):
        made = self.table("Ref,Amount,Note\nAB-0101,,taken out\n")
        self.assertEqual(stage_facts.removed_still_present(REMOVED, made), [])


class AnArithmeticMatchIsWordedAsOne(TempDirCase):
    def test_a_total_equal_to_a_sum_plus_a_removed_figure_is_a_possibility_not_a_fact(self):
        bill = self.dir / "AB-0101.txt"
        bill.write_text("Tax invoice AB-0101\nFreight 500.00\nTaxable 9,000.00\n", encoding="utf-8")
        made = self.dir / "summary.csv"
        made.write_text("Ref,Amount,Payable\nAB-0201,1000,\nAB-0202,2000,\nTotal payable incl. courier,,3500\n",
                        encoding="utf-8")
        found = stage_facts.removed_residue(REMOVED, [str(made)], [str(bill)])
        self.assertTrue(found, found)
        self.assertIn("may still include it (an arithmetic match only", found[0])


if __name__ == "__main__":
    unittest.main()
