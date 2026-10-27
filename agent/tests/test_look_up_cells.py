"""A judge that asks to see a cell or range is shown those rows (2026-10-23)."""
import unittest
from unittest import mock

from decurix_agent import produced
from decurix_agent.tools import office

LISTING = "\n".join([
    "=== Sheet COMPUTATION (rows 1-90)",
    "69 ['AUGUST 2026', None, None, None, None]",
    "79 ['Total credit', None, 0.0, 50000.0, 50000.0]",
    "80 ['Closing credit carried forward', None, 0.0, 42933.5, 42933.5]",
    "81 ['', None, None, None, None]",
    "[SEPTEMBER]  A62=2026-09-21  K62==IF(O62=\"Karnataka\",1,0) -> 1",
    "790 ['far below', None]",
])


class ACellOrRangeIsLookedUp(unittest.TestCase):

    def look(self, *terms):
        with mock.patch.object(office, "read_document", lambda p: {"text": LISTING, "images": []}):
            return produced.lines_with(["/x/COMPUTATION.xlsx"], terms)

    def test_a_range_finds_its_rows(self):
        out = self.look("A79:E81")
        self.assertIn("Closing credit carried forward", out)
        self.assertIn("Total credit", out)
        self.assertNotIn("far below", out)  # 790 is not row 79
        self.assertNotIn("AUGUST 2026", out.split("[...]")[-1])  # row 69 is outside the range

    def test_a_cell_in_the_cell_listing_and_on_a_named_sheet(self):
        self.assertIn("K62==IF", self.look("COMPUTATION!K62"))

    def test_words_are_still_looked_up(self):
        self.assertIn("Closing credit", self.look("closing credit carried forward"))


if __name__ == "__main__":
    unittest.main()
