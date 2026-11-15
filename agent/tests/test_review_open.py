"""
Points an independent review left open are no checked done (verify.review_left_open, verify.claim_for_judge, final_gate).
Found live (2026-11-15, a GSTR-1 run): soul carried four open review points with a long report, the judge's prompt cut
the report before them, and the run went out "checked" on a return that was saved and never filed.
"""

import re
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import verify

KNOWLEDGE_GO = Path(__file__).resolve().parents[1] / "thebot" / "internal" / "skills" / "knowledge.go"

REPORT = ("The return is saved.\n\nJUDGEMENTS:\n- B2B rows from the export | backed by: the export file"
          "\n\nStill open after an independent review of the work - please check:"
          "\n- GSTIN 29AAAAA0000A1Z5 is claimed as 'Valid' but no shown output confirms this status."
          "\n- The report says filed; the screen shows saved."
          "\n- An independent review of this work could not be made, so nothing beyond the worker's own checks backs it.")


class TheMarkIsSouls(unittest.TestCase):
    def test_both_lines_are_the_ones_soul_writes(self):
        go = KNOWLEDGE_GO.read_text(encoding="utf-8")
        self.assertIn('"\\n\\n' + verify.REVIEW_OPEN_MARK + '"', go)
        self.assertRegex(go, re.escape('const unreviewed = "' + verify.REVIEW_NOT_MADE))


class ReviewLeftOpen(unittest.TestCase):
    def test_the_points_are_read_and_a_review_not_made_is_none_of_them(self):
        self.assertEqual(verify.review_left_open(REPORT),
                         ["GSTIN 29AAAAA0000A1Z5 is claimed as 'Valid' but no shown output confirms this status.",
                          "The report says filed; the screen shows saved."])

    def test_a_report_without_the_block_has_none(self):
        self.assertEqual(verify.review_left_open("Saved bill HS-0318."), [])
        self.assertEqual(verify.review_left_open(None), [])

    def test_only_a_review_not_made_is_no_open_point(self):
        only = ("Done.\n\n" + verify.REVIEW_OPEN_MARK + "\n- " + verify.REVIEW_NOT_MADE + ", so nothing beyond the "
                "worker's own checks backs it.")
        self.assertEqual(verify.review_left_open(only), [])


class ClaimForJudge(unittest.TestCase):
    def test_a_long_report_keeps_its_open_points(self):
        long = "row\n" * 3000 + REPORT
        cut = verify.claim_for_judge(long)
        self.assertLessEqual(len(cut), verify.CLAIM_CHARS + 20)
        self.assertIn("The report says filed; the screen shows saved.", cut)
        self.assertTrue(cut.startswith("row\n"))

    def test_a_short_report_is_whole_and_a_long_one_without_points_is_cut(self):
        self.assertEqual(verify.claim_for_judge(REPORT), REPORT)
        self.assertEqual(len(verify.claim_for_judge("x" * 9000)), verify.CLAIM_CHARS)
        self.assertEqual(verify.claim_for_judge(""), "(nothing)")
        self.assertEqual(verify.claim_for_judge("  "), "")


class TheGateSaysNotChecked(unittest.TestCase):
    def test_judged_done_with_points_open_goes_out_once_not_checked(self):
        from decurix_agent import final_gate, skill_task
        judged = []
        with mock.patch.object(skill_task.guidance, "get",
                               lambda: {"mode": "engine", "run": None, "task": "File the return", "attempt": 1}), \
                mock.patch.object(final_gate, "_did_screen_work", lambda: True), \
                mock.patch.object(final_gate, "_saved_unread", lambda: []), \
                mock.patch.object(skill_task.produced, "files", lambda *a, **k: []), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.guidance, "refused", lambda: None), \
                mock.patch.object(skill_task.guidance, "end", lambda: None), \
                mock.patch.object(skill_task.source_guard, "changed_sources", lambda *a: []), \
                mock.patch.object(skill_task.source_guard, "tidy_own_backups", lambda *a: None), \
                mock.patch.object(skill_task.source_guard, "resaved", lambda *a: []), \
                mock.patch.object(skill_task.verify, "judge", lambda *a, **k: judged.append(1) or {
                    "verdict": verify.DONE, "confidence": 0.95, "seen": "Saved.", "why": "saved"}), \
                mock.patch.object(skill_task, "write_final_reply") as sent:
            self.assertTrue(skill_task._gate(REPORT, "final", "thebot"))
        self.assertEqual(len(judged), 1)
        self.assertTrue(sent.call_args.kwargs.get("gated"), sent.call_args)
        self.assertFalse(sent.call_args.kwargs["checked"])
        self.assertIn("NOT checked: an independent review of this work left 2 point(s) open", sent.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
