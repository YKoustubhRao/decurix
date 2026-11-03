"""The structured checks judge each row on its own: wrong work worded or laid
out like right work must not pass."""
import sys
import unittest
from pathlib import Path

sys.argv = sys.argv[:1]  # e2e reads a desktop id from its arguments
sys.path.insert(0, str(Path(__file__).resolve().parent))
import e2e  # noqa: E402


class PerRowTests(unittest.TestCase):
    def test_prices_swapped_between_rows_do_not_pass(self):
        swapped = {"Review": [["Code", "Old", "New"], ["BRG-6204", 142, 179], ["BRG-6205", 168.5, 151]]}
        right = {"Review": [["Code", "Old", "New"], ["BRG-6204", 142, 151], ["BRG-6205", 168.5, 179]]}
        def ok(sheets, code, old, new):
            return any(e2e.row_has_number(r, old) and e2e.row_has_number(r, new) for r in e2e.rows_for(sheets, code))
        self.assertFalse(ok(swapped, "BRG-6204", 142, 151))
        self.assertTrue(ok(right, "BRG-6204", 142, 151))
        self.assertTrue(ok({"S": [["BRG-6204 (Mehta)", "142.00", "151.00"]]}, "BRG-6204", 142, 151))

    def test_a_code_inside_another_code_is_not_that_item(self):
        self.assertEqual(e2e.rows_for({"S": [["BRG-62045", 1]]}, "BRG-6204"), [])
        self.assertEqual(len(e2e.rows_for({"S": [["Item BRG-6204", 1]]}, "BRG-6204")), 1)

    def test_a_flag_is_a_note_not_the_item_copied(self):
        words = ("BRG-6008", "Ball bearing 6008", "Mehta Bearings", "Nos")
        self.assertFalse(e2e.row_has_note(["BRG-6008", "Ball bearing 6008", 205, "=C2*1"], words))
        self.assertTrue(e2e.row_has_note(["BRG-6008", "Ball bearing 6008", 205, "Not on the new list - confirm"], words))
        # A date, a percent or a price in rupees is not a note.
        self.assertFalse(e2e.row_has_note(["BRG-6008", "01-Oct-2026", "2026-10-01 00:00:00", "6.3%", "Rs 205"], words))

    def test_chart_series_end_rows(self):
        xml = "<c:val><c:numRef><c:f>Summary!$B$2:$B$6</c:f></c:numRef></c:val><c:val><c:numRef><c:f>Summary!$C$2:$C$5</c:f></c:numRef></c:val>"
        self.assertEqual(e2e.chart_series_end_rows(xml), [6, 5])


if __name__ == "__main__":
    unittest.main()
