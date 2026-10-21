"""
A message too long for the control plane still reaches the person. Found live (round 5 P3, 2026-10-20): a long run's
18 KB final report - most of it the last stage's prompt, echoed as the note's "Task:" - was refused with a 400 (the
control plane takes 16,384 bytes of content), and the chat said "running" for good.

    PYTHONPATH="agent;agent/tests" python -m unittest test_long_messages
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401  (stubs decurix_agent.config on a machine with no desktop)

from decurix_agent import config, messaging, state


def control_plane(posted, refuse_over=True, refuse_all=False):
    """A stand-in for POST /api/querytext with the control plane's own limits (internal/httpapi/querynotes.go)."""

    def post(payload):
        posted.append(payload)
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        if refuse_all and len(posted) == 1:
            return "refused", "HTTP Error 400: Bad Request"
        if refuse_over and (len(payload["content"].encode("utf-8")) > 16384 or len(body) > 32768):
            return "refused", "HTTP Error 400: Bad Request"
        return "sent", ""
    return post


REPORT = "\n\n".join(
    [f"WARNING - The message \"URGENT: change of bank account\" comes from another domain than the one on record."]
    + [f"{n}. Stage {n}: made outputs/file_{n}.xlsx (checked by reading the files back against the stage). It reported: "
       + "Matched every bill line to its order and receipt, and carried the figures forward. " * 12
       for n in range(1, 21)]
    + ["Cost: 8 min, 1,725,813 model tokens."])


class TooLongForTheChat(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        for patch in (mock.patch.object(config, "DECURIX_DIR", root / "decurix"),
                      mock.patch.object(config, "WORK_DIR", root / "work"),
                      mock.patch.object(config, "QUERYTEXT_DIR", root / "querytext"),
                      mock.patch.object(messaging, "SEND_RETRY_SECONDS", 0),
                      mock.patch.object(messaging.progress, "flush", lambda *a, **k: None)):
            patch.start()
            self.addCleanup(patch.stop)
        (root / "querytext").mkdir()
        self.posted = []
        self.addCleanup(setattr, state, "current_task", state.current_task)

    def test_an_18_kb_report_is_sent_short_and_whole_in_a_file(self):
        self.assertGreater(len(REPORT.encode("utf-8")), 18000)
        state.current_task = "Stage 4 of 4 of a longer request a person made: ask for approval.\n" + "x " * 6000
        with mock.patch.object(messaging, "_post_querytext", control_plane(self.posted)):
            messaging.write_final_reply(REPORT, kind="error", checked=True)
        self.assertEqual(len(self.posted), 1)  # it fitted the first time
        sent = self.posted[0]
        self.assertLessEqual(len(sent["content"].encode("utf-8")), 16384)
        self.assertTrue(sent["reply"].startswith("WARNING - "))
        self.assertIn("Too long for the chat: the whole of it is in", sent["reply"])
        saved = Path(sent["reply"].rsplit(" is in ", 1)[1].rstrip(".)"))
        self.assertEqual(saved.read_text(encoding="utf-8"), REPORT)
        self.assertEqual(saved.parent, config.WORK_DIR / messaging.REPORTS_DIR)
        # Cut where a line or a sentence ends, never mid-sentence.
        kept = sent["reply"].split("\n\n(Too long")[0]
        self.assertTrue(kept.endswith((".", ":")), kept[-80:])
        # The note names the task in a line or two, not with a stage's whole prompt.
        self.assertLess(sent["content"].index("Final reply:"), 1400)

    def test_a_refused_outcome_is_sent_again_short_and_plain(self):
        with mock.patch.object(messaging, "_post_querytext", control_plane(self.posted, refuse_all=True)):
            self.assertTrue(messaging.send_query("f.txt", "Done. " * 50, kind="final", reply="Done. " * 50,
                                                 fallback="Finished: 3 of 4 stages done"))
        self.assertEqual(len(self.posted), 2)
        self.assertTrue(self.posted[1]["reply"].startswith("Finished: 3 of 4 stages done"))
        self.assertIn(str(config.WORK_DIR / messaging.REPORTS_DIR), self.posted[1]["reply"])

    def test_a_long_question_is_still_asked(self):
        question = "Please decide whether to approve this payment run.\n\n" + "- a bill line with its figures.\n" * 900
        with mock.patch.object(messaging, "_post_querytext", control_plane(self.posted)):
            self.assertTrue(messaging.post_question(question))
        self.assertEqual(len(self.posted), 1)
        self.assertTrue(self.posted[0]["reply"].startswith("Please decide whether to approve this payment run."))
        self.assertLessEqual(len(self.posted[0]["content"].encode("utf-8")), 16384)

    def test_a_short_message_is_untouched(self):
        with mock.patch.object(messaging, "_post_querytext", control_plane(self.posted)):
            messaging.send_query("f.txt", "Done.", kind="final", reply="Done.")
        self.assertEqual(self.posted[0]["reply"], "Done.")
        self.assertFalse((config.WORK_DIR / messaging.REPORTS_DIR).exists())

    def test_cut_text_ends_where_a_sentence_ends(self):
        text = "First sentence here. Second one is longer than the limit allows for sure."
        self.assertEqual(messaging.cut_text(text, 40), "First sentence here.")
        self.assertEqual(messaging.cut_text("short", 40), "short")


class EachStageItsOwnConversation(unittest.TestCase):
    """Found live (round 5 P3): 43% of a run's tokens were earlier stages' prompts and replies in soul's session."""

    def test_a_workflow_stage_gets_its_own_soul_session(self):
        from decurix_agent import thebot_harness, workflow_task

        class Runner:
            attempts = 1

            def status(self, run_id):
                return {"steps": {"s4": {"attempts": self.attempts}}}
        runner = Runner()
        with mock.patch.object(thebot_harness.config, "DESKTOP_ID", "pc2"), \
                mock.patch.object(workflow_task, "runner", lambda: runner), \
                mock.patch.object(workflow_task, "_active", None):
            self.assertEqual(thebot_harness._session_id(), "pc2")
            workflow_task._active = ("0123456789abcdef0123", "s4", None)
            first = thebot_harness._session_id()
            self.assertEqual(first, thebot_harness._session_id())  # the same while it waits on an approval
            runner.attempts = 2
            self.assertNotEqual(first, thebot_harness._session_id())
            self.assertTrue(first.startswith("pc2:"))


if __name__ == "__main__":
    unittest.main()
