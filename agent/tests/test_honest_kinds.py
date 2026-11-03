"""
A report is never a plain "final" unless something judged it (review 2026-11-03: E-35, E-36, E-37, N-F3).

- The final check itself raising: the report goes out not checked, saying so first.
- A run the person stopped: reported as not done (kind "error", "Stopped, as you said"), never "final", never judged.
- /workflow list: not a checked result.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import messaging, replay_task, state, thebot_harness, watchers, workflow_task


class Sent(unittest.TestCase):
    def setUp(self):
        self.sent = []
        patches = [
            mock.patch.object(messaging, "send_query", side_effect=lambda *a, **k: self.sent.append(k) or True),
            mock.patch.object(messaging, "progress"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.saved = (state.current_task, workflow_task._active)
        state.current_task = "/a command, so no facts are kept"
        workflow_task._active = None
        self.addCleanup(self.restore)

    def restore(self):
        state.current_task, workflow_task._active = self.saved
        messaging.set_final_gate(self.gate_before)

    gate_before = messaging._final_gate


class AFinalCheckThatRaises(Sent):
    def test_the_report_goes_out_not_checked_with_the_note_first(self):
        def broken(reply, kind, engine):
            raise RuntimeError("the check broke")

        messaging.set_final_gate(broken)
        messaging.write_final_reply("The total is 1,134.00.", kind="final", quiet=True)
        self.assertEqual(len(self.sent), 1)
        sent = self.sent[0]
        self.assertFalse(sent["checked"])
        self.assertTrue(sent["reply"].startswith("Not checked"), sent["reply"])
        self.assertIn("The total is 1,134.00.", sent["reply"])


class AStopIsNotFinal(unittest.TestCase):
    def test_a_stopped_replay_is_reported_not_done(self):
        finals = []
        state.paused_replay = {"program": {"session": "s"}, "params": {}, "outcome": replay_task.replay.NEEDS_HUMAN,
                               "step": 3, "resume_from": 4, "text": "Sign in, then tell me."}
        self.addCleanup(setattr, state, "paused_replay", None)
        with mock.patch.object(replay_task, "write_final_reply", side_effect=lambda t, **k: finals.append((t, k))), \
                mock.patch.object(replay_task, "by_hand_choice", return_value={"choice": "stop"}):
            replay_task.resume("stop")
        text, kwargs = finals[-1]
        self.assertEqual(kwargs["kind"], "error")
        self.assertFalse(kwargs.get("checked"))
        self.assertTrue(kwargs.get("gated"))
        self.assertTrue(text.startswith("Stopped, as you said"))

    def test_a_task_called_off_at_its_question_is_reported_not_done(self):
        finals = []
        with mock.patch.object(watchers, "write_final_reply", side_effect=lambda t, **k: finals.append((t, k))), \
                mock.patch.object(watchers, "progress"):
            watchers._called_off("stop")
        text, kwargs = finals[-1]
        self.assertEqual(kwargs["kind"], "error")
        self.assertTrue(kwargs.get("gated"))
        self.assertTrue(text.startswith("Stopped, as you said"))

    def test_a_thebot_job_stopped_by_the_person_is_reported_not_done(self):
        finals = []
        job = {"outcome": "needs_input", "finalAnswer": "I stopped.", "question": "Shall I count that as done?"}
        with mock.patch.object(thebot_harness, "write_final_reply", side_effect=lambda t, **k: finals.append((t, k))), \
                mock.patch.object(thebot_harness.guidance, "refused", return_value="stop"), \
                mock.patch.object(thebot_harness, "note_question"):
            thebot_harness._handle_job(job, None)
        text, kwargs = finals[-1]
        self.assertEqual(kwargs["kind"], "error")
        self.assertTrue(kwargs.get("gated"))
        self.assertTrue(text.startswith("Stopped, as you said"))


class AWorkflowListIsNotAChecked(unittest.TestCase):
    def test_the_list_is_not_reported_checked(self):
        finals = []
        with mock.patch("decurix_agent.messaging.write_final_reply", side_effect=lambda t, **k: finals.append(k)):
            workflow_task.perform("/workflow list")
        self.assertFalse(finals[-1].get("checked"))
        self.assertTrue(finals[-1].get("gated"))


if __name__ == "__main__":
    unittest.main()
