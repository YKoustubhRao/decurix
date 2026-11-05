"""
Tier-3 items of review 2026-11-03 in long_task: with no model a message is never taken for a change to the task (E-42),
the run's report is claimed once even when a stop's timer and a stage race (E-42), and the engine's own model's tokens
are charged to the run (E-41).
"""

import os
import sys
import threading
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_long_requests as base

import decurix_agent
from decurix_agent import long_task


class WithNoModel(base.LongRunBase):
    def test_a_message_with_no_question_open_is_asked_about_not_applied(self):
        with mock.patch.object(long_task, "drive", lambda *a, **k: None):
            long_task.perform(base.LONG, None)
        run_state = self.runner.status(self.run_id())

        def no_model(*a, **k):
            raise RuntimeError("no model")
        found = long_task.classify("Also add the October numbers.", run_state, long_task.ledger(self.run_id()),
                                   ask=no_model)
        self.assertEqual(found["kind"], "unclear")
        self.assertEqual(found["amendment"], "")
        self.assertTrue(found["ask"])


class TheReportIsClaimedOnce(unittest.TestCase):
    def test_only_one_of_many_racing_callers_claims_it(self):
        run_id = "race-run"
        self.addCleanup(long_task._reported.discard, run_id)
        claimed, start = [], threading.Barrier(8)

        def claim():
            start.wait()
            claimed.append(long_task._claim_report(run_id))
        threads = [threading.Thread(target=claim) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(claimed.count(True), 1)


class TheEnginesModelIsCharged(unittest.TestCase):
    def test_the_fallback_models_tokens_count(self):
        spent = iter([1000, 1240])
        harness = mock.Mock()
        harness.ask_model = lambda system, prompt, purpose=None: '{"ok": true}'
        before = long_task._asked()
        with mock.patch("decurix_agent.llm_client.available", lambda: False), \
                mock.patch.dict(sys.modules, {"decurix_agent.harness": harness}), \
                mock.patch.object(decurix_agent, "harness", harness, create=True), \
                mock.patch.object(long_task.usage, "spent", lambda: next(spent)):
            self.assertEqual(long_task._ask_json("system", "prompt"), {"ok": True})
        self.assertEqual(long_task._asked() - before, 240)


if __name__ == "__main__":
    unittest.main()
