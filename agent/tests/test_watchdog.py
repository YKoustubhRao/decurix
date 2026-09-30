"""The command watcher's watchdog: a real stall is reported, a freeze is not."""

import unittest
from unittest import mock

from decurix_agent import watchers


class _Stop(Exception):
    pass


def run_watchdog(times, heartbeat):
    """Run the watchdog over a scripted clock; return what it told the chat."""
    clock = iter(times)
    emitted = []

    def monotonic():
        try:
            return next(clock)
        except StopIteration:
            raise _Stop()

    with mock.patch.object(watchers.time, "sleep", lambda _: None), \
            mock.patch.object(watchers.time, "monotonic", monotonic), \
            mock.patch.object(watchers, "_heartbeat", heartbeat), \
            mock.patch.object(watchers, "log", lambda *_: None), \
            mock.patch.object(watchers, "_thread_stacks", lambda: ""), \
            mock.patch.object(watchers.progress, "emit", emitted.append):
        try:
            watchers.watchdog()
        except _Stop:
            pass
    return emitted


class Watchdog(unittest.TestCase):
    def test_a_loop_that_stops_passing_is_reported(self):
        self.assertEqual(len(run_watchdog([0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50], heartbeat=0)), 1)

    # docker pause freezes the process while the monotonic clock runs on.
    def test_waking_from_a_pause_is_not_a_stall(self):
        self.assertEqual(run_watchdog([0, 5, 300, 305, 310], heartbeat=0), [])

    def test_a_stall_after_waking_is_still_reported(self):
        times = [0, 5, 300] + [300 + 5 * i for i in range(1, 11)]
        self.assertEqual(len(run_watchdog(times, heartbeat=0)), 1)


if __name__ == "__main__":
    unittest.main()
