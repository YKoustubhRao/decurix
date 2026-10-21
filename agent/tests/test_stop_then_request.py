"""
Finding BO (2026-10-20): "stop" to a planned run waiting on a question, then a new request 21 seconds later. The stopped
run's report was still being put together; it went out as the new request's answer, under the new request's task, and
the new request was never worked. Now new requests wait while a stopped run's report is put together, and that report
goes out under the stopped run's own task.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import long_task, state, task_context

TASK, NEW_TASK = base.TASK, "t63fb9d48eea9ec15"


class StopThenANewRequest(base.LongRunBase):
    def test_the_report_goes_out_on_the_runs_own_task_and_holds_new_requests_until_then(self):
        self.engine.asks["s1-register"] = "Which rate is right for bill B-7?"
        long_task.perform(base.LONG, None)
        self.assertTrue(state.waiting_for_query)
        held_while_reporting = []
        original = long_task.final_report

        def report(run_state):
            # The new request arrived meanwhile and became the task in hand (the live order of events).
            held_while_reporting.append(long_task.holding())
            task_context.start(NEW_TASK, request="Onboard the November joiners.")
            state.current_task = "Onboard the November joiners."
            return original(run_state)
        with mock.patch.object(long_task, "final_report", report):
            self.message("stop", task_id="t11d5cf0f4b0cd194")
        self.assertEqual(held_while_reporting, [True])
        self.assertFalse(long_task.holding())
        report_sent = [s for s in self.sent if s["kind"] in ("final", "error") and "Stopped, as you said" in s["reply"]]
        self.assertEqual(len(report_sent), 1)
        self.assertEqual(report_sent[0].get("task_id"), TASK)
        # The new request's task keeps its state: it is worked next, not ended by another task's report.
        self.assertEqual(state.current_task, "Onboard the November joiners.")

    def test_a_run_interrupted_by_a_restart_waits_and_stop_reports_on_its_own_task(self):
        # The second live case: a run resumed after a restart, then "stop", then a new request.
        self.plan["stages"][1]["depends_on"] = ["register"]
        with mock.patch.object(base.workflow_task, "_dispatch"):
            long_task.perform(base.LONG, None)
        run_id = self.run_id()
        self.runner.claim(run_id)
        long_task._run = None
        base.workflow_task._active = None
        state.current_task = None
        with mock.patch("decurix_agent.workflow_runner._alive", return_value=False):
            self.assertTrue(long_task.resume_interrupted({"task_id": TASK, "request": base.LONG}))
        self.assertEqual(self.engine.prompts, [])  # nothing runs behind the person's back
        self.message("stop", task_id="t8302dee60a29bf5e")
        report = [s for s in self.sent if s["kind"] in ("final", "error") and "Stopped, as you said" in s["reply"]]
        self.assertEqual([s.get("task_id") for s in report], [TASK])
        self.assertFalse(long_task.holding())
        self.assertIsNone(long_task._run)

    def test_a_message_to_a_stopped_run_waits_for_its_report(self):
        self.engine.asks["s1-register"] = "Which rate is right for bill B-7?"
        long_task.perform(base.LONG, None)
        self.runner.cancel(self.run_id())
        path = self.message("Onboard the November joiners.", task_id=NEW_TASK)
        self.assertIn(path.name, long_task._deferred)  # not asked "finish that one first?" about a stopped run
        self.assertFalse(any("in progress" in s["reply"] for s in self.sent if s.get("task_id") == NEW_TASK))

    def test_the_hold_never_outlives_a_report_that_never_comes(self):
        long_task._stopping = "r"
        long_task._stopping_since[0] = 0.0
        self.addCleanup(setattr, long_task, "_stopping", None)
        self.assertFalse(long_task.holding())


class TheWatcherWaits(base.WatcherRoutes):
    def test_a_request_during_a_stopped_runs_report_is_left_for_later(self):
        long_task._run = {"run_id": "r", "task_id": TASK, "task_file": None, "request": base.LONG}
        long_task._stopping = "r"
        long_task._stopping_since[0] = __import__("time").time()
        self.addCleanup(setattr, long_task, "_stopping", None)
        path = self.drop("Onboard the November joiners.", task_id=NEW_TASK)
        self.watchers._pass()
        self.assertEqual(self.FakeThread.started, [])
        self.assertTrue(path.exists())
        self.assertNotIn(path.name, state.processed_commands)
        # Once the report is out, the request is started as the new request it is.
        long_task._stopping = None
        long_task._run = None
        state.current_task = None
        self.watchers._pass()
        self.assertTrue(self.FakeThread.started)
        self.assertIsNot(self.FakeThread.started[-1][0], long_task.take_message)


if __name__ == "__main__":
    unittest.main()
