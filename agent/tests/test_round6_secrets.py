"""
The secrets review's round 6 (REVIEW-PY-A-5, R5-B1), 2026-11-04. Input class: a long marked message that carries an
amount and a date, and whose secret is an all-letter word.

Round 5 stopped holding such a message whole, so its task's checks could read its figures - and the handled command
file, the request copy, the active-task record and the job to soul held it verbatim again (F-S1, F-S2). The message is
always held whole now; the checks read the request from a raw copy in the task's secret store (tmpfs, 0600, let go at
the final report).
"""

import json
import unittest
from unittest import mock

import config_stub  # noqa: F401

from test_secret_first import SecretFirst, TASK  # noqa: E402

from decurix_agent import config, messaging, state, task_context, thebot_harness, watchers, workflow_task  # noqa: E402
from decurix_agent.computer import privacy, records  # noqa: E402

WORD = "lanternquartzmeadow"
MARKED = f"Log in to the vendor portal as staff with {WORD} and file the return for total 1,250.00 due 05/11/2024"


class TheMessageItself(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)

    def everything_kept(self):
        """Every file under the test's folders but the task's own secret store (what withholds, in tmpfs)."""
        return {p.relative_to(self.root).as_posix(): p.read_text(encoding="utf-8", errors="replace")
                for p in self.root.rglob("*") if p.is_file() and not p.name.startswith("current-task.secrets")}

    def test_no_kept_copy_holds_it(self):
        self.path = self.send(MARKED)
        watchers._mark_handled(self.path)
        task_context.start(TASK, request=MARKED)
        task_context.mark_active(TASK, MARKED)
        kept = self.everything_kept()
        self.assertTrue(any(name.endswith(".done") for name in kept))
        self.assertIn("current-task.request", kept)
        self.assertIn("active-task.json", kept)
        for name, text in kept.items():
            self.assertNotIn(WORD, text, name)
        self.assertNotIn(WORD, "\n".join(self.logged))

    def test_the_job_to_soul_names_the_whole_message(self):
        self.path = self.send(MARKED)
        bodies = []
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        with mock.patch.object(thebot_harness, "_post_json", side_effect=lambda url, body: bodies.append(body) or {}), \
                mock.patch.object(thebot_harness, "_build_context", return_value=""), \
                mock.patch.object(workflow_task, "_active", None):
            thebot_harness._run_thebot(MARKED, reply=False)
        self.assertIn(MARKED, bodies[-1]["secrets"])

    def test_the_checks_read_the_raw_copy_in_another_process(self):
        self.path = self.send(MARKED)
        task_context.start(TASK, request=MARKED)
        self.assertNotIn("1,250.00", (self.root / "current-task.request").read_text(encoding="utf-8"))
        with mock.patch.object(state, "current_task", None):  # another process: no request in memory
            self.assertEqual(task_context.current_request(), MARKED)
        privacy.clear_literals(TASK)  # the task's final report lets it go
        with mock.patch.object(state, "current_task", None):
            self.assertNotIn(WORD, task_context.current_request())

    def test_the_ledger_and_presave_still_pass_on_its_figures(self):
        self.path = self.send(MARKED)
        task_context.start(TASK, request=MARKED)
        bill = self.root / "bill.txt"
        bill.write_text("Return R-1\nTotal: 1,250.00\nDue: 05/11/2024\n", encoding="utf-8")
        with mock.patch.object(records, "LEDGER_DIR", self.root / "ledger"), \
                mock.patch.object(state, "current_task", None), \
                mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: ""), \
                mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []), \
                mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"), \
                mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day"):
            records.declare("R-1", str(bill), {"Total": "1,250.00", "Due": "05/11/2024"})
            kept = (self.root / "ledger").glob("*.json")
            text = "".join(p.read_text(encoding="utf-8") for p in kept)
            self.assertIn("1,250.00", text)
            self.assertIn("05/11/2024", text)
            blocking, _, _ = records.presave(records.open_record(records.load()),
                                             [("Total", "1250.00"), ("Due", "05-11-2024")])
        self.assertEqual(blocking, [])

    def test_the_manifest_the_ledger_the_follow_up_and_the_recording_title(self):
        self.path = self.send(MARKED)
        from decurix_agent import long_task, shared, source_guard, workflow_runner
        from decurix_agent.computer import audit, recording
        with mock.patch.object(shared, "active", lambda: True), mock.patch.object(shared, "walk", lambda: []), \
                mock.patch.object(source_guard, "_folder", lambda task_id: self.root / "guard" / task_id):
            source_guard.take(MARKED, TASK)
        runner = workflow_runner.WorkflowRunner(self.root / "runs-wf")
        with mock.patch.object(long_task, "_runner", lambda: runner):
            long_task._save_ledger("r1", {"request": MARKED, "task_id": TASK})
        long_task._remember_reported({"task_id": TASK, "request": MARKED}, "Filed.", [])
        task_context.start(TASK, request=MARKED)
        with mock.patch.object(recording, "ROOT", self.root / "recordings"):
            audit.record({"kind": "click", "actor": "agent", "summary": "click Save"})
        for name, text in self.everything_kept().items():
            if "guard/" in name or "runs-wf/" in name or "last-reported-run" in name or "recordings/" in name:
                self.assertNotIn(WORD, text, name)
        self.assertTrue(list((self.root / "recordings").rglob("meta.json")), "a recording was made")


if __name__ == "__main__":
    unittest.main()
