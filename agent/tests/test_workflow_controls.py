"""Explicit workflow controls cannot be consumed as paused-agent answers."""
from unittest import mock

from test_watcher_restart import Base, FakeThread, TASK
from decurix_agent import state, watchers, workflow_task


class WorkflowQueryControls(Base):
    def check_control(self, action):
        state.waiting_for_query, state.backend_ready = True, False
        state.current_task, state.current_task_id = "paused model stage", TASK
        path = self.request("/workflow " + action + " runabc", reply=True)
        with mock.patch.object(workflow_task, "_active", ("runabc", "stage", None)), \
             mock.patch.object(watchers.lessons, "note") as note, \
             mock.patch.object(watchers.approval, "resolve_reply") as resolve:
            watchers._pass()
        self.assertEqual(FakeThread.started[0], (workflow_task.perform_control, ("/workflow " + action + " runabc",)))
        note.assert_not_called()
        resolve.assert_not_called()
        self.assertTrue(path.with_name(path.name + watchers.HANDLED_SUFFIX).exists())

    def test_cancel_while_model_query_paused_and_backend_down_uses_control_handler(self):
        self.check_control("cancel")

    def test_status_while_model_query_paused_preserves_question_without_approval_decision(self):
        self.check_control("status")
