import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import produced, skill_task, verify


# ============================================================
# GST suite T1 (2026-10-21): invoice 198 was saved as a second download of
# 197, and the final check asked "I cannot confirm this is done" nine times.
# ============================================================

class TestFilesThatAreTheSameFile(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_two_names_with_the_same_bytes_are_named(self):
        a, b = self.dir / "197_invoice.pdf", self.dir / "198_invoice.pdf"
        a.write_bytes(b"%PDF-197")
        b.write_bytes(b"%PDF-197")
        found = produced.identical_files([a, b])
        self.assertEqual(len(found), 1)
        self.assertIn("197_invoice.pdf and 198_invoice.pdf are byte for byte the same file", found[0])

    def test_different_files_and_empty_files_are_not(self):
        a, b, c, d = (self.dir / n for n in ("a.pdf", "b.pdf", "c.txt", "d.txt"))
        a.write_bytes(b"one")
        b.write_bytes(b"two")
        c.write_bytes(b"")
        d.write_bytes(b"")
        self.assertEqual(produced.identical_files([a, b, c, d, self.dir / "missing.pdf"]), [])

    def test_it_is_evidence_for_the_judge_not_a_verdict(self):
        facts = produced.FileFacts(duplicates=["x.pdf and y.pdf are byte for byte the same file"])
        self.assertIn("x.pdf and y.pdf are byte for byte the same file", facts.notes())


class TestTheCheckStopsAskingTheSameQuestion(unittest.TestCase):

    def setUp(self):
        skill_task._confirm_rounds.clear()

    def test_rounds_are_counted_per_task(self):
        with mock.patch.object(skill_task.task_context, "current", return_value="t1"):
            self.assertEqual([skill_task._confirm_round() for _ in range(3)], [1, 2, 3])
        with mock.patch.object(skill_task.task_context, "current", return_value="t2"):
            self.assertEqual(skill_task._confirm_round(), 1)

    def test_the_last_report_says_not_confirmed_and_asks_nothing(self):
        text = skill_task._given_up_text("Saved both invoices.", {"verdict": verify.UNSURE, "seen": "the chat page",
                                                                  "why": "no folder view"}, True)
        self.assertIn("NOT confirmed", text)
        self.assertIn("Saved both invoices.", text)
        self.assertNotIn("Reply `done`", text)



class TestWhatOfficeReadIsEvidence(unittest.TestCase):
    """GST suite T3: the check saw the portal on the screen and nothing of the workbook the worker read."""

    def test_a_file_read_is_in_the_observed_evidence_as_a_file_read(self):
        from decurix_agent.computer import evidence
        from decurix_agent.tools import office
        notes = Path(tempfile.mkdtemp())
        with mock.patch.object(evidence, "NOTES_DIR", notes), \
                mock.patch.object(evidence.task_context, "current", return_value="t-read"):
            office._note_read("/home/headless/PC/book.xlsx", "COMPUTATION!D82 closing credit 42,933.50")
            seen = evidence.observed("t-read")
        self.assertIn("read from the file /home/headless/PC/book.xlsx: COMPUTATION!D82 closing credit 42,933.50", seen)
        self.assertFalse(any(s.startswith("read off the screen: read from the file") for s in seen))

class TestTheCheckCanLookInWhatTheReportRead(unittest.TestCase):
    """GST suite T3: a figure read off the wrong row of the workbook the report names could not be looked up."""

    def test_a_file_the_report_names_with_spaces_in_its_path_is_found(self):
        folder = Path(tempfile.mkdtemp()) / "GST Returns" / "KAVYA CONSTRUCTIONS"
        folder.mkdir(parents=True)
        book = folder / "COMPUTATION_KC_2627.xlsx"
        book.write_bytes(b"x")
        report = f"August closing credit in the workbook:\n- File: `{book.as_posix()}`\n- CGST: 115,528.12"
        self.assertEqual(produced.named_sources(report), [str(Path(book.as_posix()))])

    def test_what_the_task_made_is_not_listed_again_and_a_missing_file_is_not_listed(self):
        made = Path(tempfile.mkdtemp()) / "out.xlsx"
        made.write_bytes(b"x")
        report = f"Saved `{made.as_posix()}` and read `/nowhere/in.xlsx`."
        self.assertEqual(produced.named_sources(report, made=[str(Path(made.as_posix()))]), [])


if __name__ == "__main__":
    unittest.main()
