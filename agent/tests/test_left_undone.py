"""A report that lists what it left undone is not a finished task, decided with no model."""

import unittest

from decurix_agent import verify


REPORT = """Completed and verified on screen as far as this desktop allowed.

Completed:
- Verified the data is sorted by Amount from largest to smallest

Left undone:
- I could not use Excel (Host) itself because no Excel window became available
- I could not verify the exact host path C:\\Users\\Administrator\\Documents\\Decurix demo

How I verified:
- On-screen spreadsheet showed the rows
"""


class LeftUndone(unittest.TestCase):
    def test_a_list_under_the_heading_is_read(self):
        found = verify.left_undone(REPORT)
        self.assertIn("could not use Excel (Host)", found)
        self.assertIn("host path", found)
        self.assertNotIn("On-screen spreadsheet", found)

    def test_on_the_same_line_and_in_bold(self):
        self.assertIn("the chart", verify.left_undone("Done.\n**Not done:** the chart, no data for it."))

    def test_nothing_left_is_nothing(self):
        for text in ("Left undone: none.", "Left undone:\n- Nothing.\n\nVerified: yes", "Left undone: None needed",
                     "Left undone: N/A", "All done, verified.", ""):
            self.assertEqual(verify.left_undone(text), "", text)


if __name__ == "__main__":
    unittest.main()
