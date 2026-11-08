"""
Trust only from independent checks; spot-judge trusted replays (W0.5 of the plan of 2026-11-03).

A skill used to become trusted - replayed with no model - after three clean runs checked by anything, the screen or the
model included, and a trusted replay was then reported done on the screen's word alone with no judge at all. A run now
counts towards trust only when a check independent of the worker passed: its files read back by code, every record it
declared read back matching, or a person. And a deterministic share of trusted replays is judged all the same.

This only makes trust stricter - it cannot create a done - so it ships on these tests and the no-regression matrix; an
eval case of its own waits for a suite that keeps memory between cases (PLAN-P2 §9, the learning-curve suite).
"""

import hashlib
import os
import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import produced, skill_task, verify
from decurix_agent.computer import experience, replay


def runs(skill, n, **kw):
    for _ in range(n):
        experience.note_verdict(skill, kw.pop("mode", "replay"), True, **kw)


class TheStreak(unittest.TestCase):
    def test_screen_only_runs_leave_a_skill_apprentice(self):
        skill = {}
        runs(skill, 3, checked_by="screen", basis=["screen"])
        self.assertEqual(experience.stage(skill), experience.APPRENTICE)
        self.assertEqual(experience.clean_streak(skill), 0)

    def test_model_only_runs_leave_it_apprentice(self):
        skill = {}
        runs(skill, 3, checked_by="model", basis=["model"])
        self.assertEqual(experience.stage(skill), experience.APPRENTICE)

    def test_each_independent_basis_counts(self):
        for basis in (["code"], ["ledger"], ["person"], ["screen", "ledger"], ["model", "code"]):
            with self.subTest(basis):
                skill = {}
                runs(skill, 3, checked_by=basis[0], basis=basis)
                self.assertEqual(experience.stage(skill), experience.TRUSTED)

    def test_a_verdict_written_before_basis_breaks_the_streak(self):
        skill = {"experience": {"runs": [], "steps": {}, "verdicts": [
            {"at": 1, "mode": "replay", "ok": True, "checked_by": "screen", "helped": False, "why": ""}] * 5}}
        self.assertEqual(experience.clean_streak(skill), 0)
        runs(skill, 2, checked_by="screen", basis=["ledger"])
        self.assertEqual(experience.clean_streak(skill), 2)

    def test_a_helped_run_never_counts(self):
        skill = {}
        runs(skill, 3, checked_by="person", basis=["person"], helped=True)
        self.assertEqual(experience.clean_streak(skill), 0)

    def test_the_basis_is_kept_and_shown(self):
        skill = {"steps": []}
        experience.note_verdict(skill, "replay", True, checked_by="screen", basis=["screen", "ledger", "ledger", ""])
        self.assertEqual(skill["experience"]["verdicts"][-1]["basis"], ["ledger", "screen"])
        self.assertEqual(experience.health(skill)["basis"], [["ledger", "screen"]])


class TheBasisOfATaughtRun(unittest.TestCase):
    def run_dict(self):
        return {"skill": {"name": "s", "steps": [], "version": 1}, "patches": [], "asked_ids": [], "ladder": mock.Mock(),
                "one_off": False}

    def basis(self, checked_by="screen", files=("/tmp/out.xlsx",), facts=None, ledger=False):
        noted = {}
        with mock.patch.object(skill_task.produced, "files", lambda *a, **k: list(files)), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.source_guard, "is_source", lambda *a: False), \
                mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts or produced.FileFacts()), \
                mock.patch("decurix_agent.computer.records.all_read_back", lambda task_id=None: ledger), \
                mock.patch.object(skill_task, "_own_run"), \
                mock.patch.object(skill_task.experience, "commit", lambda *a, **k: []), \
                mock.patch.object(skill_task.experience, "note_verdict", lambda *a, **k: noted.update(k)), \
                mock.patch.object(skill_task.experience, "advice", lambda s: ""), \
                mock.patch.object(skill_task.skills, "save"), \
                mock.patch.object(skill_task, "write_final_reply", lambda *a, **k: None):
            skill_task._succeed(self.run_dict(), {"text": "Saved it."}, checked_by)
        return noted.get("basis")

    @staticmethod
    def compared(n, held, source_compared=1):
        facts = produced.FileFacts()
        facts.compared, facts.held, facts.source_compared = n, held, source_compared
        return facts

    def test_files_whose_stated_facts_were_compared_and_held_are_code(self):
        self.assertEqual(self.basis(facts=self.compared(2, 2)), ["code", "screen"])

    def test_files_that_only_opened_are_no_code(self):
        # Review round 2, B5: a read-back that compared nothing is no check of what the files say.
        self.assertEqual(self.basis(facts=self.compared(0, 0, source_compared=0)), ["screen"])
        self.assertEqual(self.basis(facts=self.compared(2, 1)), ["screen"])

    def test_the_comparison_reads_the_file_against_the_request(self):
        import os
        import tempfile
        folder = tempfile.mkdtemp()
        self.addCleanup(__import__("shutil").rmtree, folder, True)
        path = os.path.join(folder, "summary.txt")
        request = "Pay the vendor 12,500.00 and write summary.txt"
        with open(path, "w", encoding="utf-8") as f:
            f.write("Total paid: 99,999.00")
        self.assertEqual(produced.compared_facts(request, [path])[:2], (1, 0))
        with open(path, "w", encoding="utf-8") as f:
            f.write("Total paid: 12,500.00")
        self.assertEqual(produced.compared_facts(request, [path])[:2], (1, 1))
        with open(path, "w", encoding="utf-8") as f:
            f.write("done")
        self.assertEqual(produced.compared_facts("Write a note.", [path])[:2], (0, 0))

    def test_the_requests_own_facts_are_never_the_basis(self):
        # Review round 3, R2: every fact the request states held, nothing from a source - no code.
        self.assertEqual(self.basis(facts=self.compared(2, 2, source_compared=0)), ["screen"])
        self.assertEqual(self.basis(facts=self.compared(0, 0, source_compared=1)), ["code", "screen"])

    def test_no_file_made_is_no_code(self):
        self.assertEqual(self.basis(files=()), ["screen"])

    def test_the_ledger_read_back(self):
        self.assertEqual(self.basis(files=(), ledger=True), ["ledger", "screen"])

    def test_a_person(self):
        self.assertEqual(self.basis(checked_by="person", files=()), ["person"])


def sampled(every, want, n=200):
    """Task ids (t + 16 hex) that the spot check does (want=True) or does not sample, 1 in `every`."""

    out = []
    for i in range(n):
        task = "t" + hashlib.sha256(str(i).encode()).hexdigest()[:16]
        if (int(hashlib.sha256(task.encode()).hexdigest()[:8], 16) % every == 0) == want:
            out.append(task)
    return out


class TheSpotJudgement(unittest.TestCase):
    def finish(self, task, stage=experience.TRUSTED, verdict=verify.DONE, env=None):
        calls = {"judge": [], "succeed": [], "recorded": []}

        def judge(run, claim, note="", purpose="py.judge"):
            calls["judge"].append(purpose)
            return {"verdict": verdict, "confidence": 0.9, "seen": "the form is empty", "why": ""}
        with mock.patch.dict(os.environ, env or {}), \
                mock.patch.object(skill_task.task_context, "current", lambda: task), \
                mock.patch.object(skill_task, "_stage", lambda run: stage), \
                mock.patch.object(skill_task.verify, "available", lambda: True), \
                mock.patch.object(skill_task, "_judge", judge), \
                mock.patch.object(skill_task, "_succeed", lambda run, result, by, **k: calls["succeed"].append(by)), \
                mock.patch.object(skill_task, "_record_verdict", lambda run, ok, **k: calls["recorded"].append((ok, k))), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: False), \
                mock.patch.object(skill_task, "_pause", lambda *a, **k: None), \
                mock.patch.object(skill_task.progress, "emit", lambda *a, **k: None):
            skill_task._finish({"skill": {"name": "s", "steps": []}}, {"outcome": replay.VERIFIED, "text": "ran"})
        return calls

    def test_a_sampled_trusted_replay_is_judged(self):
        for task in sampled(5, True)[:3]:
            calls = self.finish(task)
            self.assertEqual(calls["judge"], ["py.judge.spot"])
            self.assertEqual(calls["succeed"], ["model"])

    def test_an_unsampled_one_is_not(self):
        for task in sampled(5, False)[:3]:
            calls = self.finish(task)
            self.assertEqual(calls["judge"], [])
            self.assertEqual(calls["succeed"], ["screen"])

    def test_every_0_never_samples_and_1_always(self):
        task = sampled(5, True)[0]
        self.assertEqual(self.finish(task, env={"DECURIX_SPOT_JUDGE_EVERY": "0"})["judge"], [])
        other = sampled(5, False)[0]
        self.assertEqual(self.finish(other, env={"DECURIX_SPOT_JUDGE_EVERY": "1"})["judge"], ["py.judge.spot"])

    def test_the_share_is_about_one_in_every(self):
        self.assertAlmostEqual(len(sampled(5, True, n=2000)) / 2000, 0.2, delta=0.04)

    def test_a_spot_not_done_resets_the_streak(self):
        calls = self.finish(sampled(5, True)[0], verdict=verify.NOT_DONE)
        self.assertEqual(calls["succeed"], [])
        self.assertEqual(calls["recorded"][0][0], False)
        self.assertIn("spot check", calls["recorded"][0][1]["why"])

    def test_an_apprentice_run_is_judged_as_before(self):
        calls = self.finish(sampled(5, False)[0], stage=experience.APPRENTICE)
        self.assertEqual(calls["judge"], ["py.judge"])


if __name__ == "__main__":
    unittest.main()




class OnlyAComparisonThatCouldHaveFailed(unittest.TestCase):
    """Review rounds 3 and 4, R2: `code` trust came from the request's own figures found in a file (a file restating the
    request held them all), then from any of an input's facts found in a file - a byte copy of the input workbook, a
    renamed or cut copy, a partial copy, a failure note quoting one figure and a file with one figure wrong all held
    some. It now comes only from a figure a file presents as a source's own, compared with what that source states."""

    REQUEST = "Enter every bill in bills.xlsx into the books and write a summary"
    ROWS = [("BL-20931", "Coastal Infra", 45210.50), ("BL-20932", "Malnad Agro", 7342.25),
            ("BL-20933", "Karkala Steel", 12000.00)]
    TOTAL = 64552.75

    def setUp(self):
        import shutil
        import tempfile
        from pathlib import Path
        from decurix_agent import config
        self.folder = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.folder, True)
        p = mock.patch.object(config, "WORK_DIR", self.folder)
        p.start()
        self.addCleanup(p.stop)
        self.source = self.folder / "bills.xlsx"
        self.book(self.source, [("Bill", "Party", "Amount")] + self.ROWS + [("Total", "", self.TOTAL)])

    @staticmethod
    def book(path, rows):
        import openpyxl
        wb = openpyxl.Workbook()
        for row in rows:
            wb.active.append(list(row))
        wb.save(str(path))
        return str(path)

    def facts(self, *made):
        return produced.file_facts(self.REQUEST, [str(m) for m in made])

    def assert_no_code(self, facts):
        self.assertEqual(facts.source_compared, 0)
        self.assertFalse(facts.compared_clean())

    def test_copies_and_notes_are_no_code(self):
        import shutil
        copy = self.folder / "copy.xlsx"
        shutil.copyfile(self.source, copy)
        self.assert_no_code(self.facts(copy))
        renamed = self.book(self.folder / "renamed.xlsx", [("No.", "Name", "Value")] + self.ROWS)
        self.assert_no_code(self.facts(renamed))
        cut = self.book(self.folder / "cut.xlsx", [("Bill", "Party", "Amount")] + self.ROWS[:2])
        self.assert_no_code(self.facts(cut))
        partial = self.folder / "partial.csv"
        partial.write_text("Bill,Party,Amount\nBL-20931,Coastal Infra,45210.50\nBL-20932,Malnad Agro,7342.25\n",
                           encoding="utf-8")
        self.assert_no_code(self.facts(partial))
        note = self.folder / "fail.txt"
        note.write_text("Could not log in to the books. Nothing was entered. First bill seen: BL-20931 for 45,210.50.",
                        encoding="utf-8")
        self.assert_no_code(self.facts(note))

    def test_a_wrong_figure_is_a_mismatch_and_no_code(self):
        wrong = self.book(self.folder / "summary.xlsx", [("Bill", "Amount")] + [(r[0], r[2]) for r in self.ROWS]
                          + [("Total as per bills", 64642.75)])
        facts = self.facts(wrong)
        self.assertEqual(facts.source_compared, 1)
        self.assertTrue(facts.source_mismatch, "the wrong total is said")
        self.assertFalse(facts.compared_clean())

    def test_a_figure_compared_with_its_source_and_matching_is_code(self):
        right = self.book(self.folder / "summary.xlsx", [("Bill", "Amount")] + [(r[0], r[2]) for r in self.ROWS]
                          + [("Total as per bills", self.TOTAL)])
        facts = self.facts(right)
        self.assertEqual(facts.source_compared, 1)
        self.assertEqual(facts.source_mismatch, [])
        self.assertTrue(facts.compared_clean())

    def test_a_figure_one_paisa_off_is_a_mismatch(self):
        """Review round 5, N2: a 0.01 tolerance let a miscopy of the last digit pass as the source's own figure."""
        off = self.book(self.folder / "summary.xlsx", [("Total as per bills", self.TOTAL + 0.01)])
        facts = self.facts(off)
        self.assertTrue(facts.source_mismatch)
        self.assertFalse(facts.compared_clean())

    def test_a_figure_of_a_kind_the_source_does_not_state_holds_code_back(self):
        """Review round 5, N1: next to a right compared total, a "Closing balance as per bills" the bills do not
        state was skipped, and the run earned code."""
        made = self.book(self.folder / "summary.xlsx", [("Total as per bills", self.TOTAL),
                                                         ("Closing balance as per bills", 5.00)])
        facts = self.facts(made)
        self.assertEqual(facts.source_compared, 1)
        self.assertEqual(facts.source_unbacked, 1)
        self.assertFalse(facts.compared_clean())
