"""
Trust only from independent checks; spot-judge trusted replays (W0.5 of the plan of 2026-11-03).

A skill used to become trusted - replayed with no model - after three clean runs checked by anything, the screen or the
model included, and a trusted replay was then reported done on the screen's word alone with no judge at all. A run now
counts towards trust only when a check independent of the worker passed: its files read back by code, every record it
declared read back matching, or a person. And a deterministic share of trusted replays is judged all the same.

This only makes trust stricter - it cannot create a done - so it ships on these tests and the no-regression matrix; an
eval case of its own waits for a suite that keeps memory between cases (PLAN-P2 §9, learn-v1).
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
    def compared(n, held, from_sources=1):
        facts = produced.FileFacts()
        facts.compared, facts.held, facts.from_sources = n, held, from_sources
        return facts

    def test_files_whose_stated_facts_were_compared_and_held_are_code(self):
        self.assertEqual(self.basis(facts=self.compared(2, 2)), ["code", "screen"])

    def test_files_that_only_opened_are_no_code(self):
        # Review round 2, B5: a read-back that compared nothing is no check of what the files say.
        self.assertEqual(self.basis(facts=self.compared(0, 0, from_sources=0)), ["screen"])
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
        self.assertEqual(self.basis(facts=self.compared(2, 2, from_sources=0)), ["screen"])
        self.assertEqual(self.basis(facts=self.compared(0, 0, from_sources=1)), ["code", "screen"])

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


class WhatTheWorkerDidNotWrite(unittest.TestCase):
    """Review round 3, R2 (probe r2p4): the facts compared were the request's own, looked for in the files the worker
    wrote - a file that restates the request held them all and gave the `code` basis. The basis is now a fact of the
    request's input files the worker did not write, found in what it made."""

    REQUEST = "Pay the vendor 12,500.00 against INV-2024-0042 as invoice.csv says and write summary.txt"

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
        (self.folder / "invoice.csv").write_text(
            "Item,Ref,Amount\nSteel angles,INV-2024-0042,10670.00\nFreight,FRT-77310,1830.00\nTotal,,12500.00\n",
            encoding="utf-8")

    def facts(self, text, request=None):
        path = self.folder / "summary.txt"
        path.write_text(text, encoding="utf-8")
        return produced.file_facts(request or self.REQUEST, [str(path)])

    def test_the_request_echoed_is_no_code(self):
        for text in (self.REQUEST, "Task: " + self.REQUEST + "\nStatus: could not log in, nothing paid.",
                     "Paid 12,500.00 for INV-2024-0042"):
            with self.subTest(text):
                facts = self.facts(text)
                self.assertEqual(facts.from_sources, 0)
                self.assertFalse(facts.compared_clean())

    def test_a_sources_figure_carried_into_the_output_is_code(self):
        facts = self.facts("Paid 12,500.00 for INV-2024-0042: steel 10,670.00 and freight 1,830.00 (FRT-77310)")
        self.assertEqual(facts.from_sources, 3)  # 10670.00, 1830.00 and FRT-77310; the request's own are not counted
        self.assertTrue(facts.compared_clean())

    def test_a_copy_of_the_source_checks_nothing(self):
        source = (self.folder / "invoice.csv").read_text(encoding="utf-8")
        facts = self.facts("Copied:\n" + source)
        self.assertEqual(facts.from_sources, 0)

    def test_the_request_not_held_still_withholds_it(self):
        facts = self.facts("Paid 99,999.00: steel 10,670.00 and freight 1,830.00")
        self.assertGreater(facts.from_sources, 0)
        self.assertFalse(facts.compared_clean())
