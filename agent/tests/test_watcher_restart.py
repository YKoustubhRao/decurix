"""
A desktop whose agent restarts under a task: a reply that arrives for a
question the new process never asked, the task the old process never finished,
and the ordering that lets a fast reply find the agent waiting. Found live: a
"keep" to a lesson question sat unhandled, the desktop's X session ended, and
the agent that came back ran "keep" as a request to TheBot while the chat said
"Working..." over a task nothing was doing any more.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import inspect
import tempfile
import unittest
from pathlib import Path
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

from decurix_agent import filelock, messaging, state, task_context, watchers
from decurix_agent.computer import approval

TASK = "t0123456789abcdef"


class FakeThread:
    started = []

    def __init__(self, target=None, args=(), daemon=None, name=None):
        self.target, self.args = target, args

    def start(self):
        # The handler chosen for the request (watchers._guarded wraps it, to give the task slot back if it raises).
        FakeThread.started.append((inspect.unwrap(self.target), self.args))


class Base(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        (root / "command").mkdir()
        self.command_dir = root / "command"
        self.active = root / "active-task.json"

        self.saved = (state.current_task, state.current_task_id, state.waiting_for_query, state.backend_ready,
                      state.paused_skill, state.paused_replay, set(state.processed_commands))
        state.current_task = None
        state.current_task_id = ""
        state.waiting_for_query = False
        state.backend_ready = True
        state.paused_skill = state.paused_replay = None
        state.processed_commands.clear()

        self.finals = []
        FakeThread.started = []
        patches = [
            mock.patch.object(watchers.config, "COMMAND_DIR", self.command_dir),
            mock.patch.object(messaging.config, "COMMAND_DIR", self.command_dir),
            mock.patch.object(task_context, "_active_path", lambda: self.active),
            mock.patch.object(task_context, "CURRENT_FILE", str(root / "current-task")),
            mock.patch.object(watchers, "write_final_reply",
                              lambda text, kind="final", engine=None, quiet=False, interrupted=False:
                              self.finals.append((text, kind, interrupted, state.current_task, task_context.current()))),
            mock.patch.object(watchers.threading, "Thread", FakeThread),
            # Never a real browser in a test, and the request's handler stays the first thread started.
            mock.patch.object(watchers.browser, "warm_soon"),
            mock.patch.object(watchers.approval, "reset"),
            mock.patch.object(watchers.approval, "resolve_reply", return_value=None),
            mock.patch.object(watchers, "usage"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def tearDown(self):
        (state.current_task, state.current_task_id, state.waiting_for_query, state.backend_ready,
         state.paused_skill, state.paused_replay, processed) = self.saved
        state.processed_commands.clear()
        state.processed_commands.update(processed)
        self.tmp.cleanup()

    def request(self, text, reply=False):
        name = f"20260923_212023_645f715b{messaging.REPLY_MARK if reply else ''}.{TASK}.txt"
        path = self.command_dir / name
        path.write_text(text, encoding="utf-8")
        return path


class Replies(Base):

    def test_queued_task_is_not_consumed_as_a_later_query_reply(self):
        path = self.request("process the next bill")
        state.current_task = "process the first bill"
        state.current_task_id = TASK
        state.waiting_for_query = True
        watchers._pass()
        self.assertEqual(FakeThread.started, [])
        self.assertTrue(path.exists())
        self.assertNotIn(path.name, state.processed_commands)

        state.waiting_for_query = False
        state.current_task = None
        watchers._pass()
        self.assertEqual(FakeThread.started[0][0], watchers._perform_following_procedure)

    def test_different_task_id_is_not_marked_as_a_reply(self):
        state.waiting_for_query = True
        state.current_task_id = TASK
        new = messaging.save_request("another bill", "t1111111111111111")
        self.assertFalse(watchers.is_reply_file(new.name))
        reply = messaging.save_request("retry", TASK)
        self.assertTrue(watchers.is_reply_file(reply.name))

    def test_a_reply_saved_while_waiting_is_marked_as_one(self):
        state.waiting_for_query = True
        state.current_task_id = TASK
        path = messaging.save_request("keep", TASK)
        self.assertTrue(watchers.is_reply_file(path.name))
        self.assertEqual(task_context.task_id_from_filename(path.name), TASK, "the task ID still reads back")

        state.waiting_for_query = False
        self.assertFalse(watchers.is_reply_file(messaging.save_request("a new request", TASK).name))

    def test_a_reply_that_only_calls_the_task_off_never_reaches_the_engine(self):
        for words, target in (("stop", "_called_off"), ("Cancel it.", "_called_off"),
                              ("stop and use the first one", "continue_after_query")):
            with self.subTest(words=words):
                FakeThread.started = []
                state.processed_commands.clear()
                state.waiting_for_query = True
                state.current_task = "enter the attached document"
                state.current_task_id = TASK
                path = self.request(words, reply=True)
                with mock.patch.object(watchers.lessons, "note"), mock.patch.object(watchers.skill_task, "learn_answer"):
                    watchers._pass()
                self.assertEqual(FakeThread.started[0][0], getattr(watchers, target))
                for leftover in self.command_dir.iterdir():
                    leftover.unlink()

        watchers._called_off("stop")
        self.assertIn("nothing more", self.finals[-1][0])
        self.assertFalse(state.waiting_for_query)

    def _stop_while_paused(self, paused, workflow_active=False):
        FakeThread.started = []
        state.processed_commands.clear()
        state.waiting_for_query = True
        state.current_task = "/skill add-a-customer"
        state.current_task_id = TASK
        state.paused_skill, state.paused_replay = paused
        self.request("stop", reply=True)
        cancelled = []
        with mock.patch.object(watchers.lessons, "note"), \
                mock.patch.object(watchers.workflow_task, "has_active", lambda: workflow_active and not cancelled), \
                mock.patch.object(watchers.workflow_task, "cancel_active", lambda: cancelled.append(True)):
            watchers._pass()
        return FakeThread.started[0], cancelled

    def test_a_paused_skill_or_replay_reads_stop_itself(self):
        # Found in review (2026-10-14): a bare "stop" went to the generic
        # call-off even when a lesson or a skill run had asked, so a lesson's
        # draft was not kept and a stopped run never recorded that it stopped.
        (target, args), cancelled = self._stop_while_paused(({"mode": "learn"}, None))
        self.assertEqual(target, watchers.skill_task.resume)
        self.assertEqual(args[0], "stop")
        self.assertEqual(cancelled, [])
        (target, _), _ = self._stop_while_paused((None, {"step": 3}))
        self.assertEqual(target, watchers.replay_task.resume)

    def test_stop_at_a_workflow_stages_skill_question_ends_the_workflow_too(self):
        (target, _), cancelled = self._stop_while_paused(({"mode": "run"}, None), workflow_active=True)
        self.assertEqual(target, watchers.skill_task.resume, "the skill still reports where it stopped")
        self.assertEqual(cancelled, [True], "and the next stage is not started on its report")

    def test_an_empty_reply_is_retried_when_its_writer_finishes(self):
        state.waiting_for_query = True
        state.current_task = "/learn demo-x"
        state.paused_skill = {"mode": "learn"}
        path = self.request("", reply=True)
        watchers._pass()
        self.assertEqual(FakeThread.started, [])
        self.assertEqual(state.processed_commands, set())
        path.write_text("skip", encoding="utf-8")
        watchers._pass()
        self.assertEqual(FakeThread.started[0][0], watchers.skill_task.resume)

    def test_a_reply_is_invisible_to_watcher_until_its_text_is_complete(self):
        state.waiting_for_query = True
        state.current_task_id = TASK  # a reply names the task whose question is open
        state.current_task = "/learn demo-x"
        state.paused_skill = {"mode": "learn"}
        original_write = Path.write_text
        checked = []

        def slow_write(path, text, **kwargs):
            if path.parent == self.command_dir and path.name.endswith(".tmp"):
                watchers._pass()
                checked.append(list(self.command_dir.glob("*.txt")))
                self.assertEqual(state.processed_commands, set())
            return original_write(path, text, **kwargs)

        with mock.patch.object(Path, "write_text", slow_write):
            path = messaging.save_request("skip", TASK)

        self.assertEqual(checked, [[]])
        self.assertEqual(path.read_text(), "skip")
        watchers._pass()
        self.assertEqual(FakeThread.started[0][0], watchers.skill_task.resume)
        self.assertFalse(path.exists())

    def test_a_reply_nothing_is_waiting_for_is_answered_not_run(self):
        path = self.request("keep", reply=True)

        watchers._pass()

        self.assertEqual(FakeThread.started, [], "not handed to an engine as a request")
        self.assertEqual(len(self.finals), 1)
        text, kind, _, task, reported = self.finals[0]
        self.assertEqual(kind, "error")
        self.assertIn("after it had restarted", text)
        self.assertEqual((task, reported), ("keep", TASK), "said on the task it belongs to")
        self.assertFalse(path.exists())
        self.assertTrue(path.with_name(path.name + watchers.HANDLED_SUFFIX).exists())
        self.assertIsNone(state.current_task, "the slot is free again")

    def test_a_reply_to_a_live_question_resumes_it(self):
        state.waiting_for_query = True
        state.current_task = "/learn demo-x"
        state.paused_skill = {"mode": "learn"}
        self.request("keep", reply=True)

        watchers._pass()

        self.assertEqual(self.finals, [])
        self.assertEqual(FakeThread.started[0][0], watchers.skill_task.resume)

    def test_a_marked_reply_behind_a_running_task_waits_its_turn(self):
        state.current_task = "something running"
        path = self.request("keep", reply=True)

        watchers._pass()

        self.assertEqual((FakeThread.started, self.finals), ([], []))
        self.assertTrue(path.exists(), "left for a later pass - the running task may pause and take it")

    def test_finding_a_kept_procedure_never_holds_the_watcher(self):
        # Found in review (2026-10-14): procedures.apply asked the hosted model
        # inside the watcher's pass.
        self.request("bank rec for August, usual procedure")
        with mock.patch.object(watchers.procedures, "apply", side_effect=AssertionError("asked in the pass")):
            watchers._pass()
        target, args = FakeThread.started[0]
        self.assertEqual(target, watchers._perform_following_procedure)

        ran = []
        with mock.patch.object(watchers.procedures, "apply", lambda c: c + "\\n\\nThe standing procedure ..."), \
                mock.patch.object(watchers, "perform_task", lambda c, f: ran.append(c)):
            target(*args)
        self.assertTrue(ran[0].endswith("The standing procedure ..."))
        self.assertEqual(state.current_task, ran[0], "the final check reads the procedure too")

    def test_a_new_request_is_remembered_until_it_finishes(self):
        self.request("what is the kernel version?")

        watchers._pass()

        self.assertEqual(FakeThread.started[0][0], watchers._perform_following_procedure)
        self.assertEqual(task_context.take_interrupted()["task_id"], TASK)


class Interrupted(Base):

    def test_a_task_the_last_process_never_finished_is_reported_once(self):
        task_context.mark_active(TASK, "search flamingos")

        watchers._report_interrupted()
        watchers._report_interrupted()

        self.assertEqual(len(self.finals), 1)
        text, kind, interrupted, task, reported = self.finals[0]
        self.assertEqual((kind, interrupted, task, reported), ("error", True, "search flamingos", TASK))
        self.assertIn("restarted while it was working on this", text)
        self.assertIsNone(state.current_task)
        self.assertEqual(task_context.current(), "", "later reports do not land on the dead task")

    def test_a_lesson_is_told_it_carries_on(self):
        task_context.mark_active(TASK, "/learn demo-20260923T211809-b176b6")
        watchers._report_interrupted()
        self.assertIn("same `/learn` carries on", self.finals[0][0])

    def test_nothing_to_report_after_a_task_finished(self):
        task_context.mark_active(TASK, "done soon")
        with mock.patch.object(messaging, "send_query", return_value=True), \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", Path(self.tmp.name)):
            messaging.write_final_reply("done")
        watchers._report_interrupted()
        self.assertEqual(self.finals, [])

    def test_a_garbled_record_is_dropped(self):
        self.active.write_text("{not json", encoding="utf-8")
        self.assertIsNone(task_context.take_interrupted())
        self.assertFalse(self.active.exists())


class Ordering(Base):

    def test_the_agent_is_waiting_before_the_question_goes_out(self):
        seen = []
        with mock.patch.object(messaging, "send_query", lambda *a, **k: seen.append(state.waiting_for_query) or True), \
                mock.patch.object(messaging.config, "QUERYTEXT_DIR", Path(self.tmp.name)):
            messaging.ask_for_query("which one?")
        self.assertEqual(seen, [True], "a reply that lands while the question is being sent is a reply")


class TheSlotIsGivenBack(Base):
    """Review 2026-11-02: a request whose start or handler raised kept the task slot - "Task already running" for the
    life of the agent."""

    def test_a_start_that_raises_gives_the_slot_back(self):
        self.request("process the next bill")
        with mock.patch.object(watchers.attachments, "expand_archives", side_effect=OSError("the archive is damaged")):
            with self.assertRaises(OSError):
                watchers._pass()
        self.assertIsNone(state.current_task)
        self.assertEqual(FakeThread.started, [])

    def test_a_handler_that_raises_is_reported_not_done_and_gives_the_slot_back(self):
        said = []

        def broken(content, path):
            raise RuntimeError("the handler broke")
        state.current_task = "process the next bill"
        with mock.patch.object(watchers, "write_final_reply", lambda text, **kw: said.append((text, kw))), \
                mock.patch.object(watchers.task_context, "current", return_value=TASK):
            watchers._guarded(broken, TASK)("process the next bill", None)
        self.assertIsNone(state.current_task)
        self.assertEqual(len(said), 1)
        self.assertIn("it is NOT done", said[0][0])
        self.assertEqual(said[0][1]["kind"], "error")
        self.assertNotIn("checked", said[0][1])  # not checked: nothing judged it

    def test_a_handler_that_ends_normally_keeps_its_own_ending(self):
        state.current_task = "process the next bill"
        with mock.patch.object(watchers.task_context, "current", return_value=TASK):
            watchers._guarded(lambda content, path: None, TASK)("process the next bill", None)
        # (A handler that asked a question and returned holds the slot until the answer: untouched.)
        self.assertEqual(state.current_task, "process the next bill")

    def test_an_interrupt_is_not_swallowed_and_still_gives_the_slot_back(self):
        # Recheck 2026-11-02: BaseException was caught - a KeyboardInterrupt or SystemExit was reported as a task error.
        said = []
        for stop in (KeyboardInterrupt, SystemExit):
            def ended(content, path, stop=stop):
                raise stop()
            state.current_task = "process the next bill"
            with mock.patch.object(watchers, "write_final_reply", lambda text, **kw: said.append(text)), \
                    mock.patch.object(watchers.task_context, "current", return_value=TASK):
                with self.assertRaises(stop):
                    watchers._guarded(ended, TASK)("process the next bill", None)
            self.assertIsNone(state.current_task)
        self.assertEqual(said, [])


class ApprovalLock(unittest.TestCase):

    def test_a_stuck_holder_is_a_timeout_not_a_hang(self):
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(approval, "STATE_DIR", Path(tmp)), \
                mock.patch.object(approval, "STATE_FILE", Path(tmp) / "approval.json"), \
                mock.patch.object(approval, "LOCK_TIMEOUT_SECONDS", 0.2):
            with open(Path(tmp) / "approval.lock", "w") as holder:
                filelock.exclusive(holder)
                with self.assertRaises(TimeoutError):
                    approval.resolve_reply("yes")
            self.assertIsNone(approval.resolve_reply("yes"), "free again once the holder lets go")


if __name__ == "__main__":
    unittest.main()
