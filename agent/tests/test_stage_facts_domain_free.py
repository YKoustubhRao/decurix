"""
The stage facts read shapes, never a business's words (review 2026-11-03, E-28): which amounts need a working, which
column is a percentage factor, and what a "work left for later" status looks like are told without naming a kind of
business (refunds, a tax's names, a policy). And a formula is worked out only when it is plain arithmetic (E-39).
"""

import unittest

from needs import needs
from temp_case import TempDirCase

from decurix_agent import stage_facts


class Shapes(TempDirCase):
    def table(self, header, rows, name="work.csv"):
        path = self.dir / name
        lines = [",".join(header)] + [",".join(str(c) for c in row) for row in rows]
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        return [str(path)]

    def book(self, header, rows, name="work.xlsx"):
        import openpyxl
        wb = openpyxl.Workbook()
        wb.active.append(header)
        for row in rows:
            wb.active.append(row)
        wb.save(self.dir / name)
        return [str(self.dir / name)]

    def test_an_amount_a_status_states_needs_a_working_whatever_it_is_called(self):
        message = self.dir / "msg_0101.txt"
        message.write_text("Hello, about AB-0101: please sort out the 60 units.", encoding="utf-8")
        made = self.table(["Ref", "Message", "Action"], [["AB-0101", "msg_0101.txt", "Settle Rs 1800.00 now"]])
        found = stage_facts.unworked_amounts(made, [str(message)])
        self.assertEqual(len(found), 1, found)
        self.assertIn("AB-0101", found[0])

    @needs("openpyxl")
    def test_a_percentage_column_is_a_factor_whatever_it_is_a_percentage_of(self):
        right = self.book(["Order", "Qty", "Unit price", "Levy %", "Result"], [["AB-0101", 10, 100, 10, 1100]])
        self.assertEqual(stage_facts.working_problems(right), [])
        wrong = self.book(["Order", "Qty", "Unit price", "Levy %", "Result"], [["AB-0101", 10, 100, 10, 1250]],
                          name="wrong.xlsx")
        self.assertEqual(len(stage_facts.working_problems(wrong)), 1)

    @needs("openpyxl")
    def test_a_discount_percentage_is_still_a_discount(self):
        made = self.book(["Order", "Qty", "Unit price", "Discount %", "Amount"], [["AB-0101", 60, 185, 5, 10545]])
        self.assertEqual(stage_facts.working_problems(made), [])

    def test_work_left_for_later_is_told_by_its_shape_not_by_a_policy_word(self):
        made = self.table(["Ref", "Action"], [["AB-0101", "to be calculated"], ["AB-0102", "handled per the policy"]])
        found = stage_facts.deferred_actions(made)
        self.assertEqual(len(found), 1, found)
        self.assertIn("AB-0101", found[0])


class PlainArithmeticOnly(unittest.TestCase):
    def test_a_comma_outside_round_is_not_worked_out(self):
        self.assertFalse(stage_facts._commas_only_in_round("(1,)*3"))
        self.assertTrue(stage_facts._commas_only_in_round("_round((2+3)*4,2)"))
        self.assertIsNone(stage_facts.evaluate("=(1,)*3", 2, []))
        self.assertIsNone(stage_facts.evaluate("=B2,C2", 2, [None, 1, 2]))

    def test_round_still_is(self):
        self.assertEqual(stage_facts.evaluate("=ROUND(B2*C2,2)", 2, [None, 2.5, 3.333]), 8.33)
        self.assertEqual(stage_facts.evaluate("=ROUND((B2+C2),0)", 2, [None, 1.4, 1.4]), 3.0)


if __name__ == "__main__":
    unittest.main()
