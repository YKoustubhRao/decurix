"""The command watcher's watchdog: a real stall is reported, a freeze is not."""

import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

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
            mock.patch.object(watchers, "_in_flight", "req.txt"), \
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


class Stop(Exception):
    pass


class Clock:
    """A fake monotonic clock; sleep() plays a script of how long each nap
    really took (a frozen container naps for hours)."""

    def __init__(self, naps, beats=None):
        self.now = 1000.0
        self.naps = list(naps)
        self.beats = beats or {}  # nap index -> advance the heartbeat
        self.i = 0

    def monotonic(self):
        return self.now

    def sleep(self, _seconds):
        if self.i >= len(self.naps):
            raise Stop
        self.now += self.naps[self.i]
        if self.i in self.beats:
            watchers._heartbeat = self.now
        self.i += 1


def run(naps, in_flight=None, beats=None):
    clock = Clock(naps, beats)
    watchers._heartbeat = clock.now
    watchers._in_flight = in_flight
    emitted, logged = [], []
    with mock.patch.object(watchers.time, "monotonic", clock.monotonic), \
            mock.patch.object(watchers.time, "sleep", clock.sleep), \
            mock.patch.object(watchers.progress, "emit", emitted.append), \
            mock.patch.object(watchers, "log", logged.append):
        try:
            watchers.watchdog()
        except Stop:
            pass
    watchers._in_flight = None
    return emitted, logged


class WatchdogTest(unittest.TestCase):

    def test_a_frozen_container_is_not_a_stall(self):
        # Paused for 15 hours, then thawed: one enormous nap, then normal ones
        # during which the loop has not had its first second yet.
        emitted, logged = run([5, 54000, 5], in_flight="req.txt")
        self.assertEqual(emitted, [])
        self.assertEqual(logged, [])

    def test_a_real_stall_on_a_message_is_reported_once(self):
        emitted, logged = run([5] * 30, in_flight="req.txt")
        self.assertEqual(len(emitted), 1)
        self.assertIn("stuck handling a message", emitted[0])
        self.assertEqual(len(logged), 1)

    def test_a_stall_after_a_freeze_still_counts_only_running_time(self):
        # Frozen, thawed, then genuinely stuck: reported after 45s of running
        # time, not the instant it thaws.
        emitted, _ = run([54000] + [5] * 8, in_flight="req.txt")
        self.assertEqual(emitted, [])
        emitted, _ = run([54000] + [5] * 10, in_flight="req.txt")
        self.assertEqual(len(emitted), 1)

    def test_a_stall_between_messages_is_only_logged(self):
        emitted, logged = run([5] * 30, in_flight=None)
        self.assertEqual(emitted, [])
        self.assertEqual(len(logged), 1)

    def test_a_loop_that_passes_is_quiet(self):
        emitted, logged = run([5] * 30, in_flight="req.txt", beats={i: True for i in range(30)})
        self.assertEqual((emitted, logged), ([], []))


if __name__ == "__main__":
    unittest.main()
