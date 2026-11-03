"""
Keeping a task's facts never delays its report (review 2026-11-03, E-30): the report is sent first, then the facts are
kept (still before the task lets go of the desktop), from the whole request.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import kept_facts, messaging, state, task_context, workflow_task


class FactsAfterTheReport(unittest.TestCase):
    def test_the_report_is_sent_before_the_facts_are_kept_from_the_whole_request(self):
        order = []
        request = "Reconcile the bank. " + "x " * 800 + "This is for the client North Mills."
        saved = (state.current_task, workflow_task._active)
        self.addCleanup(lambda: (setattr(state, "current_task", saved[0]), setattr(workflow_task, "_active", saved[1])))
        state.current_task, workflow_task._active = request, None
        with mock.patch.object(messaging, "send_query", side_effect=lambda *a, **k: order.append("sent") or True), \
                mock.patch.object(messaging, "progress"), \
                mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", mock.MagicMock()), \
                mock.patch.object(task_context, "current_request", return_value=request), \
                mock.patch.object(kept_facts, "after_task",
                                  side_effect=lambda t, r: order.append(("facts", r)) or None):
            messaging.write_final_reply("Done.", kind="final", checked=True)
        self.assertEqual(order[0], "sent")
        self.assertEqual(order[1][0], "facts")
        self.assertIn("North Mills", order[1][1], "the whole request, not its first lines")

    def test_another_tasks_report_keeps_no_facts_against_this_request(self):
        saved = state.current_task
        self.addCleanup(setattr, state, "current_task", saved)
        state.current_task = "the request in hand"
        with mock.patch.object(messaging, "send_query", return_value=True), \
                mock.patch.object(messaging, "progress"), mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", mock.MagicMock()), \
                mock.patch.object(task_context, "current", return_value="t1111111111111111"), \
                mock.patch.object(kept_facts, "after_task") as after:
            messaging.write_final_reply("Stopped, as you said.", kind="error", gated=True, task_id="t2222222222222222")
        after.assert_not_called()

    def test_a_quiet_ending_keeps_no_facts(self):
        saved = state.current_task
        self.addCleanup(setattr, state, "current_task", saved)
        state.current_task = "a request"
        with mock.patch.object(messaging, "send_query", return_value=True), \
                mock.patch.object(messaging, "progress"), mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(kept_facts, "after_task") as after:
            messaging.write_final_reply("Recorded.", quiet=True, checked=True)
        after.assert_not_called()


if __name__ == "__main__":
    unittest.main()
