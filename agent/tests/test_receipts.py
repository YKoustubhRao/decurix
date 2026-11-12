"""receipts.received: whether a request for a task arrived since the control plane began sending it, as asked after a
restart cut a send short (GET /requests/<task>?since=). Only a file that arrived since then counts - a reply or retry
reuses the task id - and past what the command folder keeps the answer is "cannot say", never "no"."""

import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import config, receipts

TASK = "t0123456789abcdef"


class ReceivedTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        patch = mock.patch.object(config, "COMMAND_DIR", self.dir)
        patch.start()
        self.addCleanup(patch.stop)

    def put(self, name, at):
        path = self.dir / name
        path.write_text("x", encoding="utf-8")
        os.utime(path, (at, at))

    def test_a_request_saved_or_handled_since_counts(self):
        now = time.time()
        self.put(f"20260101_000000_ab12cd34.{TASK}.txt", now - 5)
        self.assertTrue(receipts.received(TASK, now - 10, now=now))
        (self.dir / f"20260101_000000_ab12cd34.{TASK}.txt").rename(self.dir / f"x.{TASK}.txt.done")
        self.assertTrue(receipts.received(TASK, now - 10, now=now))

    def test_the_tasks_earlier_request_does_not_count(self):
        now = time.time()
        self.put(f"old.{TASK}.txt.done", now - 600)
        self.assertFalse(receipts.received(TASK, now - 60, now=now))

    def test_a_file_still_being_written_does_not_count(self):
        now = time.time()
        self.put(f"new.{TASK}.txt.tmp", now)
        self.assertFalse(receipts.received(TASK, now - 60, now=now))

    def test_past_what_is_kept_it_cannot_say(self):
        now = time.time()
        self.assertIsNone(receipts.received(TASK, now - 7200, now=now))

    def test_a_bad_task_id_or_time_cannot_say(self):
        now = time.time()
        self.assertIsNone(receipts.received("../etc", now, now=now))
        self.assertIsNone(receipts.received(TASK, "soon", now=now))


if __name__ == "__main__":
    unittest.main()
