"""
Finding CI (2026-10-20, round 5 P6 on pc3): stage 1 was refused twice for "Action states 1,490.00 with no working" (and
35,960, 6,900, 745, 7,600) - every one stated in the customer's own email ("Please refund the 10 mugs, that is Rs
1,490"). The "no working" finding never looked at the messages, and the other check read the .eml raw (quoted-printable
soft breaks). A row is tied to its message by its file name, then sender + subject; an amount that message states is
read, not computed, and needs no working; a row tied to no message is not decided.
"""

import shutil
import tempfile
import unittest
from pathlib import Path

import openpyxl

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import stage_facts

INBOX = Path(r"C:\Users\Administrator\decurix-tests\wide\process\data\P6\inbox")
LIVE = fixture_paths.fixture("p6_tracker_live.xlsx")  # the run's tracker (pc3, read only)


@unittest.skipUnless(INBOX.is_dir(), "round 5 P6's inbox is not on this machine")
class TheLiveInbox(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.messages = sorted(str(p) for p in INBOX.glob("*.eml"))

    def tracker(self, e03_amount, working=True):
        """The live Tracker sheet; E03 at `e03_amount`; a Working sheet holding only E03's working (as attempt 1)."""

        path = self.dir / "support_triage_tracker.xlsx"
        shutil.copy(fixture_paths.need(LIVE.name), path)
        book = openpyxl.load_workbook(path)
        for row in book["Tracker"].iter_rows(min_row=2):
            if row[0].value == "1016_E03.eml":
                row[7].value = f"Approve refund Rs {e03_amount:,.2f} per policy (up to Rs 5,000)."
        del book["Working"]
        if working:
            sheet = book.create_sheet("Working")
            sheet.append(["File", "Reference", "Quantity", "Unit Price", "Unit Price Source", "Result"])
            sheet.append(["1016_E03.eml", "SO-5103", 60, 35, "Portal_Orders SO-5103 LAN-S", 2100])
        book.save(path)
        return [str(path)]

    def test_amounts_the_customers_state_need_no_working(self):
        found = stage_facts.working_problems(self.tracker(2100), messages=self.messages)
        self.assertEqual(found, [])

    def test_a_derived_amount_with_no_working_is_still_refused(self):
        # E03's email gives no amount: Rs 1,800 with no working for it (only another item has one) is refused.
        made = self.tracker(1800)
        book = openpyxl.load_workbook(made[0])
        book["Working"].delete_rows(2)
        book["Working"].append(["1015_E01.eml", "SO-5101", 10, 149, "1015_E01.eml", 1490])
        book.save(made[0])
        found = stage_facts.working_problems(made, messages=self.messages)
        self.assertEqual(len(found), 1, found)
        self.assertIn("SO-5103", found[0])
        self.assertIn("1,800.00 with no working", found[0])

    def test_with_no_working_at_all(self):
        found = stage_facts.unworked_amounts(self.tracker(1800, working=False), self.messages)
        self.assertEqual(len(found), 1, found)
        self.assertIn("SO-5103", found[0])

    def test_quoted_printable_is_read(self):
        read = stage_facts.read_messages([str(INBOX / "1015_E01.eml")])["1015_e01.eml"]
        self.assertIn(1490.0, read["amounts"])
        self.assertEqual(read["sender"], "rohan@cafebloom.example.in")


class Ties(unittest.TestCase):
    def test_by_sender_and_subject_and_not_at_all(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / "m1.eml"
        path.write_bytes(b"From: A <a@x.example>\nSubject: Refund for mugs\nContent-Type: text/plain; charset=utf-8\n"
                         b"Content-Transfer-Encoding: quoted-printable\n\nPlease refund =E2=82=B9 1,49=\n0 for the mugs.\n")
        messages = stage_facts.read_messages([str(path)])
        row = ["a@x.example", "Refund for mugs", "Approve refund Rs 1,490.00"]
        self.assertTrue(stage_facts.stated_by_message(1490, row, messages))
        self.assertFalse(stage_facts.stated_by_message(1800, row, messages))
        self.assertIsNone(stage_facts.stated_by_message(1490, ["someone@else.example", "Other"], messages))


if __name__ == "__main__":
    unittest.main()
