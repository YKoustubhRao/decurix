"""A short message soon after a planned run was reported, which changes that work, is planned as a request of its own
(the original request, the report, the change) - never answered "Got it" and dropped."""

import json
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import long_task

REQUEST = "Hi - please audit October's travel and expense claims ... 4. audit_note_Oct2026.docx ..."
REPORT = "Done - every stage was made and checked.\nDone:\n1. ... expense_audit_Oct2026.xlsx"
ADMIN = ("One thing from Admin: Hotel Shaniwar Residency in Pune is our corporate tie-up hotel. For stays there the "
         "hotel limit is Rs 5,000 a night including taxes, not the Tier 2 Rs 4,000.")


class FollowUp(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for p in (mock.patch.object(long_task.config, "DECURIX_DIR", Path(self.dir.name)),
                  mock.patch.object(long_task, "enabled", lambda: True)):
            p.start()
            self.addCleanup(p.stop)
        long_task._remember_reported({"task_id": "t1", "request": REQUEST}, REPORT,
                                     ["/home/headless/work/H1/outputs/expense_audit_Oct2026.xlsx"])
        self.asked = []

    def ask(self, kind):
        def fake(system, prompt, max_tokens=None):
            self.asked.append(prompt)
            return {"kind": kind, "what": "the tie-up hotel's limit"}
        return fake

    def test_a_change_to_the_reported_work_becomes_a_request_that_redoes_it(self):
        followed = long_task.follow_up(ADMIN, ask=self.ask("change"))
        self.assertIn(REQUEST, followed)
        self.assertIn(REPORT, followed)
        self.assertIn(ADMIN, followed)
        self.assertIn("expense_audit_Oct2026.xlsx", followed)
        self.assertIn("Redo only what this changes", followed)
        self.assertIn(ADMIN, self.asked[0])

    def test_a_question_or_a_new_request_is_handled_as_always(self):
        self.assertIsNone(long_task.follow_up("Thanks! Which claims did you cut?", ask=self.ask("question")))
        self.assertIsNone(long_task.follow_up("Please book a cab to the airport", ask=self.ask("new")))

    def test_only_a_short_message_without_files_soon_after_a_report_is_read(self):
        never = self.ask("change")
        self.assertIsNone(long_task.follow_up("x" * (long_task.WHOLE_REQUEST_CHARS + 1), ask=never))
        self.assertIsNone(long_task.follow_up("/skill payroll", ask=never))
        old = json.loads(long_task._follow_up_path().read_text(encoding="utf-8"))
        old["at"] = time.time() - (long_task.FOLLOW_UP_HOURS + 1) * 3600
        long_task._follow_up_path().write_text(json.dumps(old), encoding="utf-8")
        self.assertIsNone(long_task.follow_up(ADMIN, ask=never))
        self.assertEqual(self.asked, [])

    def test_the_watcher_wraps_only_a_message_that_could_be_about_the_report(self):
        self.assertTrue(long_task.may_follow_up(ADMIN))
        self.assertFalse(long_task.may_follow_up(REQUEST + "x" * long_task.WHOLE_REQUEST_CHARS))
        long_task._follow_up_path().unlink()
        self.assertFalse(long_task.may_follow_up(ADMIN))

    def test_no_reported_run_or_a_failing_model_changes_nothing(self):
        long_task._follow_up_path().unlink()
        self.assertIsNone(long_task.follow_up(ADMIN, ask=self.ask("change")))
        long_task._remember_reported({"task_id": "t1", "request": REQUEST}, REPORT, [])

        def broken(system, prompt, max_tokens=None):
            raise RuntimeError("model down")
        self.assertIsNone(long_task.follow_up(ADMIN, ask=broken))


if __name__ == "__main__":
    unittest.main()


class TheWatcherRunsTheChange(unittest.TestCase):
    def test_a_change_is_run_as_the_whole_request_and_anything_else_as_before(self):
        from decurix_agent import task_context, watchers
        ran, started = [], []
        with mock.patch.object(watchers.long_task, "follow_up", lambda content: "WHOLE REQUEST + change"), \
                mock.patch.object(task_context, "current", lambda: "t9"), \
                mock.patch.object(watchers.long_task, "reported_task", lambda: "t1"), \
                mock.patch.object(task_context, "start", lambda task_id, request=None, continues="":
                                  started.append((task_id, request, continues))), \
                mock.patch.object(watchers, "_perform_long", lambda content, path: ran.append(("long", content))):
            watchers._follow_up_or(lambda content, path: ran.append(("plain", content)))("the change", "p")
        self.assertEqual(started, [("t9", "WHOLE REQUEST + change", "t1")])
        self.assertEqual(ran, [("long", "WHOLE REQUEST + change")])
        ran.clear()
        with mock.patch.object(watchers.long_task, "follow_up", lambda content: None):
            watchers._follow_up_or(lambda content, path: ran.append(("plain", content)))("thanks!", "p")
        self.assertEqual(ran, [("plain", "thanks!")])
