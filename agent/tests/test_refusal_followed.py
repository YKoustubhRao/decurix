"""A no at an approval ends the task only when the work stopped there (2026-10-23)."""
import unittest
from unittest import mock

from decurix_agent import guidance, task_context
from decurix_agent.computer import evidence


class ANoThatTheWorkWentOnFrom(unittest.TestCase):

    def setUp(self):
        self.events = []
        for patch in (mock.patch.object(task_context, "current", lambda: "t1"),
                      mock.patch.object(evidence, "_task_events", lambda task_id: self.events)):
            patch.start()
            self.addCleanup(patch.stop)
        self.addCleanup(guidance.note_reply, "", "approved")

    def test_a_no_the_work_stopped_at_stands(self):
        self.events = [{"kind": "click"}]
        guidance.note_reply("No, do not send it.", "declined")
        self.assertEqual(guidance.refused(), "No, do not send it.")

    def test_a_no_with_what_to_do_first_that_the_work_carried_out_does_not_end_the_task(self):
        self.events = [{"kind": "click"}]
        guidance.note_reply("No - don't file yet. Open invoice 455's PDF and redo Table 13, then ask me again.", "declined")
        self.events.append({"kind": "type", "text": "1"})   # the worker went on, as told
        self.assertIsNone(guidance.refused())


class ANoAnsweredWithFileWork(unittest.TestCase):

    def test_a_workbook_corrected_after_the_no_is_the_work_going_on(self):
        import tempfile
        from pathlib import Path
        notes = Path(tempfile.mkdtemp())
        with mock.patch.object(task_context, "current", lambda: "t2"), \
                mock.patch.object(evidence, "_task_events", lambda task_id: []), \
                mock.patch.object(evidence, "NOTES_DIR", notes):
            guidance.note_reply("No - fix the workbook first, then ask me again.", "declined")
            (notes / "t2.outputs").write_text('{"at": 1, "text": "Updated SALES.xlsx: 3 cell(s)"}\n', encoding="utf-8")
            self.assertIsNone(guidance.refused())
            guidance.note_reply("", "approved")


if __name__ == "__main__":
    unittest.main()
