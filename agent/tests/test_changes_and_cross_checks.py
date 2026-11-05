"""
A change the person makes reaches every finished stage it may touch (E-18), and the cross-check compares only figures
that are one figure, then says when a disagreement was settled (E-17). Review 2026-11-03.
"""

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_long_requests as base

from decurix_agent import long_task


class TheCrossCheckComparesOneFigure(unittest.TestCase):
    def test_bare_totals_of_two_tables_are_not_compared(self):
        rows = {"/w/register.xlsx": [("Total", ["245000"])], "/w/ageing.xlsx": [("Total", ["112400"])],
                "/w/memo.docx": [("Grand total", ["9"])]}
        found, agreed = long_task.code_disagreements(
            [("s1", "/w/register.xlsx"), ("s2", "/w/ageing.xlsx"), ("s3", "/w/memo.docx")], read=lambda p: rows[p])
        self.assertEqual((found, agreed), ([], 0))

    def test_a_named_total_still_is(self):
        rows = {"/w/a.xlsx": [("Purchases total", ["245000"])], "/w/b.docx": [("Purchases total", ["245900"])]}
        found, _ = long_task.code_disagreements([("s1", "/w/a.xlsx"), ("s2", "/w/b.docx")], read=lambda p: rows[p])
        self.assertEqual([d["figure"] for d in found], ["purchases total"])


class ASettledDisagreementIsResolved(base.LongRunBase):
    def test_after_the_last_round_it_is_compared_once_more(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        old = [{"figure": "purchases total", "sent_back_to": "s1-register", "values": []},
               {"figure": "bank balance", "sent_back_to": "s2-bank", "values": []}]
        long_task._update(run_id, lambda d: d["crosscheck"].update(rounds=long_task.MAX_CROSSCHECK_ROUNDS,
                                                                    disagreements=old))
        still = [{"figure": "bank balance", "values": []}]
        with mock.patch.object(long_task, "_outputs", lambda run_state: [("s1", "/w/a"), ("s2", "/w/b")]), \
                mock.patch.object(long_task, "code_disagreements", lambda outputs: (still, 3)), \
                mock.patch.object(long_task, "model_disagreements", lambda outputs, request="": ([], 0)):
            self.assertFalse(long_task._cross_check(self.runner.status(run_id)))
        cross = long_task.ledger(run_id)["crosscheck"]
        self.assertEqual({d["figure"]: bool(d.get("resolved")) for d in cross["disagreements"]},
                         {"purchases total": True, "bank balance": False})


class AChangeReachesTheFinishedStages(base.LongRunBase):
    def finished_run(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        reopened = []
        patch = mock.patch.object(long_task, "_reopen", lambda r, step_id, reason, amendment=None:
                                  reopened.append(step_id) or True)
        patch.start()
        self.addCleanup(patch.stop)
        status = self.runner.status(run_id)
        for record in status["steps"].values():
            record["status"] = "completed"
        return run_id, status, reopened

    def test_a_change_naming_no_stage_reopens_every_finished_one(self):
        run_id, status, reopened = self.finished_run()
        with mock.patch.object(self.runner, "status", lambda r: status):
            long_task.amend(run_id, "Leave bill X-1 out of everything.", [], records={"removed": ["X-1"]})
        self.assertEqual(sorted(reopened), sorted(status["steps"]))

    def test_a_stage_whose_outputs_hold_a_removed_reference_is_reopened_too(self):
        run_id, status, reopened = self.finished_run()
        status["steps"]["s2-bank"]["evidence"] = [{"path": "bank.csv"}]
        with mock.patch.object(self.runner, "status", lambda r: status), \
                mock.patch("decurix_agent.produced.lines_with",
                           lambda paths, terms: "X-1,500" if any(p.endswith("bank.csv") for p in paths) else ""):
            long_task.amend(run_id, "Leave bill X-1 out.", ["s1-register"], records={"removed": ["X-1"]})
        self.assertIn("s2-bank", reopened)
        self.assertIn("s1-register", reopened)

    def test_a_change_that_keeps_what_is_done_reopens_nothing(self):
        run_id, status, reopened = self.finished_run()
        with mock.patch.object(self.runner, "status", lambda r: status):
            long_task.amend(run_id, "Also cc me on future drafts.", [], keeps=True)
        self.assertEqual(reopened, [])


if __name__ == "__main__":
    unittest.main()
