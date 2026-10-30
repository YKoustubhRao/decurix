"""
The browser is not kept running on an idle desktop: a request warms it, and one
warmed for work that never showed a page is closed again (computer/browser.warm).
Nothing here starts a real browser.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import unittest
from unittest import mock

from decurix_agent import task_context
from decurix_agent.computer import browser, cdp, screen, session


class FakeProcess:
    pid = 4242

    def __init__(self, exits_after=None):
        self.polls = 0
        self.exits_after = exits_after

    def poll(self):
        self.polls += 1
        return 0 if self.exits_after is not None and self.polls > self.exits_after else None


class Warm(unittest.TestCase):
    def test_a_running_browser_is_left_alone(self):
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(session, "launch") as launch:
            browser.warm()
        launch.assert_not_called()

    def test_a_missing_browser_is_started_with_no_window(self):
        with mock.patch.object(cdp, "available", return_value=False), \
                mock.patch.object(session, "launch", return_value=FakeProcess()) as launch, \
                mock.patch.object(browser.threading, "Thread") as thread:
            browser.warm()
        launch.assert_called_once_with([browser.COMMAND, "--no-startup-window"])
        self.assertIs(thread.call_args.kwargs["target"], browser._close_if_unused)

    def test_a_browser_that_cannot_start_never_raises(self):
        with mock.patch.object(cdp, "available", return_value=False), \
                mock.patch.object(session, "launch", side_effect=FileNotFoundError("chromium-browser")):
            browser.warm()


class CloseIfUnused(unittest.TestCase):
    def run_check(self, windows, active, process=None):
        process = process or FakeProcess()
        with mock.patch.object(screen, "windows", return_value=windows), \
                mock.patch.object(task_context, "is_active", side_effect=active), \
                mock.patch.object(browser.os, "killpg", create=True) as kill, \
                mock.patch.object(browser.time, "sleep"):
            browser._close_if_unused(process, check_seconds=0)
        return kill

    def test_closed_once_no_task_runs_and_no_page_was_shown(self):
        kill = self.run_check([{"title": "Terminal"}], active=[True, True, False])
        kill.assert_called_once()

    def test_kept_when_a_browser_window_came_of_it(self):
        kill = self.run_check([{"title": "Invoices - Google Chrome"}], active=[False])
        kill.assert_not_called()

    def test_nothing_to_close_when_it_already_exited(self):
        kill = self.run_check([], active=[False], process=FakeProcess(exits_after=0))
        kill.assert_not_called()


if __name__ == "__main__":
    unittest.main()
