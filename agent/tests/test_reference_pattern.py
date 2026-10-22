import tempfile
import unittest
from pathlib import Path

from decurix_agent import produced
from decurix_agent.tools import office


# ============================================================
# GST suite T4 (2026-10-21): a month's block copied from the one above took
# its opening balance from the row above last month's closing balance.
# ============================================================

def book(rows):
    import openpyxl
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Computation"
    for r in rows:
        ws.append(r)
    path = Path(tempfile.mkdtemp()) / "blocks.xlsx"
    wb.save(path)
    return path


def blocks(n, slip_last=False):
    """n monthly blocks of 5 rows: In, Paid, Opening (= the block above's Closing), Total, Closing."""
    rows = []
    for k in range(n):
        top = len(rows) + 1
        rows.append([f"MONTH {k + 1}"])
        rows.append(["In", 100])
        rows.append(["Paid", 40])
        if k == 0:
            rows.append(["Opening (last month's closing)", 10])
        else:
            closing_above = top - 1
            rows.append(["Opening (last month's closing)", f"=B{closing_above - (1 if slip_last and k == n - 1 else 0)}"])
        rows.append(["Total", f"=B{top + 1}+B{top + 3}"])
        rows.append(["Closing", f"=B{top + 4}-B{top + 2}"])
    return rows


class TestABlockThatReadsAnotherRowThanItsSiblings(unittest.TestCase):

    def test_a_reference_a_row_off_is_named(self):
        found = office.reference_pattern_breaks(book(blocks(5, slip_last=True)))
        self.assertEqual(len(found), 1, found)
        self.assertIn("row \"opening (last month's closing)\"", found[0])
        self.assertIn("reads the row \"total\"", found[0])
        self.assertIn("reads the row \"closing\"", found[0])

    def test_blocks_that_follow_one_pattern_say_nothing(self):
        self.assertEqual(office.reference_pattern_breaks(book(blocks(5))), [])

    def test_two_blocks_are_not_a_pattern(self):
        self.assertEqual(office.reference_pattern_breaks(book(blocks(2, slip_last=True))), [])

    def test_it_is_evidence_for_the_judge(self):
        path = book(blocks(4, slip_last=True))
        facts = produced.file_facts("", [str(path)])
        self.assertTrue(any("a reference may be a row off" in n for n in facts.notes()), facts.notes())


if __name__ == "__main__":
    unittest.main()
