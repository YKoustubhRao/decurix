import tempfile
import unittest
from pathlib import Path
from unittest import mock

import openpyxl

from decurix_agent import long_task, produced, source_guard
from decurix_agent.computer import evidence


# ============================================================
# GST suite T9 (2026-10-22): the portal showed the return's ARN as
# AA2909267663712A; the workbook was given AA2909267663796A, and the report
# read the workbook back and called it verified.
# ============================================================

SHOWN = ["action: click button VERIFY (done) -> page is now http://x/gstr1?msg=GSTR1+filed+successfully.+ARN+AA2909267663712A",
         "action: gui observe -> text 'ARN: AA2909261540520B'"]


class TestAReferenceCopiedWrong(unittest.TestCase):

    def book(self, lines):
        wb = openpyxl.Workbook()
        for line in lines:
            wb.active.append([line])
        path = Path(tempfile.mkdtemp()) / "comp.xlsx"
        wb.save(path)
        return str(path)

    def check(self, path):
        with mock.patch.object(source_guard, "before_copy", return_value=None), \
                mock.patch.object(long_task, "_page_notes", return_value=[]), \
                mock.patch.object(evidence, "observed", return_value=SHOWN):
            return produced.miscopied_codes([path], "File the September returns and note both ARNs.")

    def test_a_code_a_few_characters_off_what_the_screen_showed_is_found(self):
        found = self.check(self.book(["GSTR-1 filed. ARN AA2909267663796A", "GSTR-3B filed. ARN AA2909261540520B"]))
        self.assertEqual(len(found), 1)
        self.assertIn("AA2909267663796A", found[0])
        self.assertIn("AA2909267663712A is what was shown, 2 character(s) apart", found[0])

    def test_codes_as_shown_and_codes_unlike_any_shown_are_left_alone(self):
        self.assertEqual(self.check(self.book(["ARN AA2909267663712A", "ARN AA2909261540520B"])), [])
        self.assertEqual(self.check(self.book(["Order ZX4471KQ2026PL"])), [])   # nothing like it was shown: not a typo
        self.assertEqual(self.check(self.book(["INV-0453"])), [])                # short codes are left out


if __name__ == "__main__":
    unittest.main()
