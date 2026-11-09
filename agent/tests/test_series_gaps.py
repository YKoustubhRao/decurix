"""
A numbered series the run's documents carry that skips numbers is a fact the final check is given (skill_task.
_series_gaps). Found live (2026-11-09): bills 1058, 1059 and 1061 were entered and confirmed done, though the taught
rules said to ask the person for a bill missing from the series.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import final_gate, skill_task
from decurix_agent.computer import records


def run_with(numbers, label="Voucher No."):
    skill = {"params": [{"name": "voucher_number", "label": label, "list": True, "varies": True},
                        {"name": "amount", "label": "Amount", "list": True, "varies": True}]}
    return {"skill": skill, "values": {"voucher_number": numbers, "amount": "100; 1058; 2000"}}


class SeriesGaps(unittest.TestCase):
    def test_a_hole_in_the_documents_numbers_is_named(self):
        facts = skill_task._series_gaps(run_with("1058; 1059; 1061"))
        self.assertEqual(len(facts), 1)
        self.assertIn("skip 1060", facts[0])
        self.assertIn("CHECKED IN CODE", facts[0])

    def test_a_prefix_and_leading_zeros_are_kept(self):
        self.assertIn("skip INV-007", skill_task._series_gaps(run_with("INV-006; INV-008; INV-009"))[0])

    def test_no_fact_for_a_whole_series_two_series_far_apart_numbers_or_a_field_that_is_no_number(self):
        for numbers in ("1058; 1059; 1060", "A-1; B-3", "1058; 2200", "1058", "07; 9; 10"):
            self.assertEqual(skill_task._series_gaps(run_with(numbers)), [], numbers)
        self.assertEqual(skill_task._series_gaps(run_with("1058; 1059; 1061", label="Amount")), [])



def said(text=""):
    return mock.patch.multiple(records, _answers_text=lambda task_id=None: text,
                               _worker_words_text=lambda task_id=None: "")


class TheQuestion(unittest.TestCase):
    """Whether a hole is fine is the person's to say, once, never the model's (_series_question)."""

    def test_asked_once_naming_what_is_missing(self):
        run = run_with("1058; 1059; 1061")
        with said():
            q = skill_task._series_question(run)
            self.assertIn("1060 is missing", q)
            self.assertIn("Voucher No. 1058, 1059, 1061", q)
            self.assertNotIn("Reply `done` if it is done", q)  # (not the done question the eval expert reads as such)
            self.assertEqual(skill_task._series_question(run), "", "asked once per run")

    def test_settled_when_the_person_already_named_the_missing_number(self):
        with said("1060 was cancelled, the series skips it"):
            self.assertEqual(skill_task._series_question(run_with("1058; 1059; 1061")), "")
        with said("10600 is another bill"):
            self.assertIn("1060", skill_task._series_question(run_with("1058; 1059; 1061")))

    def test_nothing_to_ask_for_one_document_or_two_series(self):
        with said():
            for numbers in ("1058", "A-1; B-3", "INV-1; CR-3"):
                self.assertEqual(skill_task._series_question(run_with(numbers)), "", numbers)

    def test_a_run_with_a_hole_is_put_to_the_person_not_reported_done(self):
        run = run_with("1058; 1059; 1061")
        paused = []
        with said(), mock.patch.object(skill_task, "_pause", lambda *a, **k: paused.append((a, k))),                 mock.patch.object(skill_task, "write_final_reply") as wrote:
            skill_task._succeed(run, {"text": "Entered the three bills."}, "model")
        self.assertEqual(paused[0][0][1], "confirm")
        self.assertIn("1060 is missing", paused[0][0][2])
        wrote.assert_not_called()

    def test_the_gate_reaches_the_same_question(self):
        run = run_with("1058; 1059; 1061")
        with said():
            self.assertIn("1060", final_gate._series_question(run))


if __name__ == "__main__":
    unittest.main()


class WhoSettlesItCounts(unittest.TestCase):
    """An answer to a question the rules make code ask is not help; a correction is (experience.counts)."""

    def paused(self, run):
        return {"run": run, "claim": "Entered the three bills.", "kind": "confirm", "result": {"text": "Entered."},
                "verdict": {"verdict": "unsure", "confidence": 0.0, "seen": "1060 is missing", "why": ""},
                "series": {"checked_by": "screen", "seen": "", "helped": False}}

    def test_as_it_should_be_keeps_the_runs_own_check_and_counts(self):
        run = run_with("1058; 1059; 1061")
        with mock.patch.object(skill_task, "_succeed") as succeed:
            final_gate._confirm_reply(self.paused(run), "done")
        succeed.assert_called_once()
        self.assertEqual(succeed.call_args.args[2], "screen")
        self.assertFalse(succeed.call_args.kwargs["helped"])
        from decurix_agent.computer import experience
        self.assertTrue(experience.counts({"ok": True, "helped": False, "basis": ["ledger"]}))

    def test_a_correction_is_help(self):
        run = run_with("1058; 1059; 1061")
        with mock.patch.object(skill_task, "_succeed") as succeed, \
                mock.patch.object(skill_task, "_by_model"), mock.patch.object(skill_task, "_correct"), \
                mock.patch.object(skill_task, "_record_verdict") as recorded, \
                mock.patch.object(final_gate, "_done_reading",
                                  return_value={"choice": "correction", "instruction": "enter bill 1060 from the drive"}):
            final_gate._confirm_reply(self.paused(run), "1060 is on the shared drive - enter it too")
        succeed.assert_not_called()
        self.assertTrue(run.get("asked_person"))
        self.assertFalse(recorded.call_args.args[1], "the month was not whole: this run is no success")
