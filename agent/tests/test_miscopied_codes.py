import tempfile
import unittest
from pathlib import Path
from unittest import mock

import openpyxl

from decurix_agent import long_task, produced, source_guard
from decurix_agent.computer import evidence


# ============================================================
# Found live (2026-10-22): the portal showed the return's ARN as
# AA2909267663712A; the workbook was given AA2909267663796A, and the report
# read the workbook back and called it verified.
# Independent review (2026-10-22): "a few characters apart" is a judgement -
# order numbers in a run and one company's GSTINs of two states are near twins
# too - so it is evidence for the judge, never a block.
# ============================================================

SHOWN = ["action: click button VERIFY (done) -> page is now http://x/gstr1?msg=GSTR1+filed+successfully.+ARN+AA2909267663712A",
         "action: gui observe -> text 'ARN: AA2909261540520B'"]
REQUEST = "File the September returns and note both ARNs."


class TestAReferenceCopiedWrong(unittest.TestCase):

    def book(self, lines):
        wb = openpyxl.Workbook()
        for line in lines:
            wb.active.append([line])
        path = Path(tempfile.mkdtemp()) / "comp.xlsx"
        wb.save(path)
        return str(path)

    def patches(self, shown):
        return (mock.patch.object(source_guard, "before_copy", return_value=None),
                mock.patch.object(long_task, "_page_notes", return_value=[]),
                mock.patch.object(evidence, "observed", return_value=shown),
                mock.patch.object(evidence, "_task_events", return_value=[]))

    def check(self, path, shown=SHOWN):
        a, b, c, d = self.patches(shown)
        with a, b, c, d:
            return produced.miscopied_codes([path], REQUEST)

    def facts(self, path, shown):
        a, b, c, d = self.patches(shown)
        with a, b, c, d:
            return produced.file_facts(REQUEST, [path])

    def test_a_code_a_few_characters_off_what_the_screen_showed_is_evidence(self):
        found = self.check(self.book(["GSTR-1 filed. ARN AA2909267663796A", "GSTR-3B filed. ARN AA2909261540520B"]))
        self.assertEqual(found, ["AA2909267663796A in comp.xlsx differs in 2 character(s) from AA2909267663712A shown "
                                 "on the screen; check which is right."])

    def test_it_reaches_the_judge_as_a_note_and_never_blocks(self):
        facts = self.facts(self.book(["GSTR-1 filed. ARN AA2909267663796A"]), SHOWN)
        self.assertEqual(facts.source_mismatch, [])
        self.assertTrue(any("AA2909267663796A" in n and "check which is right" in n for n in facts.notes()))

    def test_sequential_references_read_from_an_input_are_not_blocked(self):
        # SO2026000121..125 read from an input; SO2026000124 was on the screen.
        book = self.book([f"Order SO2026000{n}" for n in (121, 122, 123, 124, 125)])
        facts = self.facts(book, ["action: gui observe -> text 'Order SO2026000124 saved'"])
        self.assertEqual(facts.source_mismatch, [])
        self.assertFalse(any("copied wrong" in n or "Correct it" in n for n in facts.notes()))

    def test_a_sibling_id_is_not_blocked(self):
        # One company's GSTINs of two states: 27... (Maharashtra) in the file, 29... (Karnataka) on the screen.
        book = self.book(["Maharashtra GSTIN 27AABCU9603R1ZN"])
        facts = self.facts(book, ["action: gui observe -> text 'GSTIN 29AABCU9603R1ZM'"])
        self.assertEqual(facts.source_mismatch, [])
        notes = [n for n in facts.notes() if "27AABCU9603R1ZN" in n]
        self.assertEqual(len(notes), 1)
        self.assertIn("check which is right", notes[0])
        self.assertNotIn("copied wrong", notes[0])

    def test_codes_as_shown_and_codes_unlike_any_shown_are_left_alone(self):
        self.assertEqual(self.check(self.book(["ARN AA2909267663712A", "ARN AA2909261540520B"])), [])
        self.assertEqual(self.check(self.book(["Order ZX4471KQ2026PL"])), [])   # nothing like it was shown: not a typo
        self.assertEqual(self.check(self.book(["INV-0453"])), [])                # short codes are left out


if __name__ == "__main__":
    unittest.main()
