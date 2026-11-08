"""
The settle wait by drawing reports (core._settle_by_damage, screen.damage_watch; docs/computer-use.md, "Waits").

No X server here: the drawing reports, the screen's pictures and the clock are stand-ins. What is held: a quiet screen
settles after the same quiet window with one picture; drawing keeps it waiting; a caret's blink does not; a screen
redrawn the same settles; with no reports, or when they stop, pictures are compared exactly as before.
"""

import random
import sys
import unittest
from types import SimpleNamespace
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import core, screen  # noqa: E402
from decurix_agent.computer.screen import Frame  # noqa: E402

W, H = 640, 360


def frame(shade=255, box=None, box_shade=0):
    """A plain screen, with one [x, y, w, h] box painted another shade."""

    rgb = bytearray([shade]) * (W * H * 3)
    if box:
        x, y, w, h = box
        for row in range(y, y + h):
            rgb[(row * W + x) * 3:(row * W + x + w) * 3] = bytes([box_shade]) * (w * 3)
    return Frame(W, H, rgb)


class Clock:
    def __init__(self):
        self.now = 0.0

    def sleep(self, seconds):
        self.now += max(0.0, seconds)

    def monotonic(self):
        return self.now


class Watch:
    """Drawing reports: [(time, [(x, y, w, h), ...]), ...], each handed over once its time has come."""

    def __init__(self, clock, drawn=(), lose_at=None):
        self.clock, self.drawn, self.lose_at = clock, list(drawn), lose_at
        self.size = (W, H)
        self.closed = False

    def take(self):
        if self.lose_at is not None and self.clock.now >= self.lose_at:
            raise screen.DamageLost("gone")
        due = [r for t, rects in self.drawn if t <= self.clock.now for r in rects]
        self.drawn = [(t, rects) for t, rects in self.drawn if t > self.clock.now]
        return due, None

    def close(self):
        self.closed = True


class Settle(unittest.TestCase):
    def run_settle(self, watch, shown, clock):
        """shown(t) -> the Frame on the screen at time t. Returns (frame, end time, times pictures were taken)."""

        grabs = []

        def grab():
            grabs.append(clock.now)
            clock.now += 0.04        # a picture takes time
            return shown(clock.now)
        with mock.patch.object(core, "time", SimpleNamespace(sleep=clock.sleep, monotonic=clock.monotonic)), \
                mock.patch.object(screen, "damage_watch", return_value=watch), \
                mock.patch.object(screen, "grab", side_effect=grab):
            result = core._wait_for_settle(None)
        return result, clock.now, grabs

    def test_a_quiet_screen_settles_after_the_quiet_window_with_one_picture(self):
        clock = Clock()
        watch = Watch(clock)
        result, end, grabs = self.run_settle(watch, lambda t: frame(), clock)
        self.assertGreaterEqual(end, 0.15 + core.SETTLE_QUIET)
        self.assertLess(end, 0.15 + core.SETTLE_QUIET + 0.15)
        self.assertEqual(len(grabs), 1)          # the picture the caller gets, taken at the end
        self.assertIsInstance(result, Frame)
        self.assertTrue(watch.closed)

    def test_drawing_keeps_it_waiting_until_it_stops(self):
        clock = Clock()
        # A page drawing itself until 1.2 s: a large area changes every 50 ms.
        drawn = [(0.2 + 0.05 * i, [(0, 0, 400, 200)]) for i in range(21)]

        def shown(t):
            return frame(box=(0, 0, 400, 200), box_shade=int(t * 100) % 200) if t < 1.25 else frame(box=(0, 0, 400, 200), box_shade=7)
        result, end, grabs = self.run_settle(Watch(clock, drawn), shown, clock)
        self.assertGreaterEqual(end, 1.2 + core.SETTLE_QUIET)
        self.assertLess(end, core.SETTLE_SECONDS)
        # Never more than one picture each SETTLE_POLL seconds while it draws.
        self.assertTrue(all(b - a >= core.SETTLE_POLL - 1e-9 for a, b in zip(grabs, grabs[1:])))
        self.assertEqual(result.rgb, shown(end).rgb)   # the screen as it settled

    def test_a_dialog_that_opens_late_is_waited_for(self):
        clock = Clock()
        drawn = [(0.4, [(100, 100, 300, 150)])]
        result, end, grabs = self.run_settle(Watch(clock, drawn),
                                             lambda t: frame(box=(100, 100, 300, 150)) if t >= 0.4 else frame(), clock)
        self.assertGreaterEqual(end, 0.4 + core.SETTLE_QUIET)
        self.assertEqual(result.rgb, frame(box=(100, 100, 300, 150)).rgb)

    def test_a_blinking_caret_does_not_keep_it_waiting(self):
        clock = Clock()
        caret = (205, 103, 2, 16)
        drawn = [(0.05 * i, [caret]) for i in range(80)]  # drawn on every read
        result, end, grabs = self.run_settle(Watch(clock, drawn), lambda t: frame(box=caret) if int(t * 2) % 2 else frame(), clock)
        self.assertLess(end, 0.15 + core.SETTLE_QUIET + 0.15)
        self.assertEqual(len(grabs), 1)

    def test_a_screen_redrawn_the_same_settles(self):
        clock = Clock()
        drawn = [(0.05 * i, [(0, 0, W, H)]) for i in range(80)]  # redrawn whole, never changed
        result, end, grabs = self.run_settle(Watch(clock, drawn), lambda t: frame(), clock)
        self.assertLess(end, 1.0)
        self.assertLess(end, core.SETTLE_SECONDS)

    def test_small_changes_add_up(self):
        # Points drawn one at a time, each alone under the tolerance, together a change (as changed_fraction sees it).
        clock = Clock()
        spots = [(i * 50, 200, 12, 12) for i in range(12)]
        drawn = [(0.2 + 0.03 * i, [spot]) for i, spot in enumerate(spots)]

        def shown(t):
            f = frame()
            for i, spot in enumerate(spots):
                if t >= 0.2 + 0.03 * i:
                    f = Frame(W, H, _paint(f, spot))
            return f
        result, end, grabs = self.run_settle(Watch(clock, drawn), shown, clock)
        self.assertGreaterEqual(len(grabs), 1)
        self.assertEqual(result.rgb, shown(end).rgb)
        self.assertGreaterEqual(end, 0.2 + 0.03 * 11 + core.SETTLE_QUIET - 0.05)

    def test_without_reports_pictures_are_compared_as_before(self):
        clock = Clock()
        result, end, grabs = self.run_settle(None, lambda t: frame(), clock)
        self.assertGreaterEqual(len(grabs), 3)     # the old loop: a picture every SETTLE_POLL seconds
        self.assertTrue(all(b - a >= core.SETTLE_POLL for a, b in zip(grabs, grabs[1:])))

    def test_reports_that_stop_fall_back_to_pictures(self):
        clock = Clock()
        watch = Watch(clock, lose_at=0.3)
        result, end, grabs = self.run_settle(watch, lambda t: frame(), clock)
        self.assertTrue(watch.closed)
        self.assertGreaterEqual(len(grabs), 3)
        self.assertIsInstance(result, Frame)


def _paint(f, box, shade=0):
    rgb = bytearray(f.rgb)
    x, y, w, h = box
    for row in range(y, y + h):
        rgb[(row * W + x) * 3:(row * W + x + w) * 3] = bytes([shade]) * (w * 3)
    return rgb


class TheGridBound(unittest.TestCase):
    """What the drawing reports allow is never less than what changed_fraction could see."""

    def test_drawn_points_cover_every_point_that_differs(self):
        rng = random.Random(7)
        for _ in range(40):
            before = frame(shade=rng.randrange(256))
            rects = [(rng.randrange(-20, W), rng.randrange(-20, H), rng.randrange(1, 120), rng.randrange(1, 60))
                     for _ in range(rng.randrange(1, 4))]
            rgb = bytearray(before.rgb)
            for x, y, w, h in rects:
                for row in range(max(0, y), min(H, y + h)):
                    left, right = max(0, x), min(W, x + w)
                    if right > left:
                        rgb[(row * W + left) * 3:(row * W + right) * 3] = bytes([rng.randrange(256)]) * ((right - left) * 3)
            after = Frame(W, H, rgb)
            drawn = screen.grid_points(W, H, rects)
            differing = screen.differing_points(before, after)
            self.assertLessEqual(differing, drawn)
            changed = screen.changed_fraction(before, after) * len(before.sample())
            self.assertLessEqual(changed, 3 * len(differing))

    def test_grid_size_matches_the_sample(self):
        for w, h in ((1280, 720), (1366, 768), (640, 360), (1000, 701)):
            f = Frame(w, h, bytearray(w * h * 3))
            self.assertEqual(3 * screen.grid_size(w, h), len(f.sample()))

    def test_a_caret_is_under_the_tolerance(self):
        points = screen.grid_points(1280, 720, [(400, 300, 2, 18)])
        self.assertLessEqual(len(points), core.SETTLE_CHANGE * screen.grid_size(1280, 720))


class Event(SimpleNamespace):
    pass


class FakeDisplay:
    def __init__(self, events):
        self.events, self.destroyed, self.synced = list(events), [], 0

    def pending_events(self):
        return len(self.events)

    def next_event(self):
        return self.events.pop(0)

    def damage_destroy(self, damage_id):
        self.destroyed.append(damage_id)

    def sync(self):
        self.synced += 1


class TheWatch(unittest.TestCase):
    def setUp(self):
        self.assertTrue(screen._damage_lock.acquire(blocking=False))  # as damage_watch holds it for the watch

    def tearDown(self):
        if screen._damage_lock.locked():
            screen._damage_lock.release()

    def test_takes_its_own_reports_and_is_closed(self):
        ours = Event(type=91, damage=5, area=SimpleNamespace(x=1, y=2, width=3, height=4),
                     drawable_geometry=SimpleNamespace(width=W, height=H))
        theirs = Event(type=91, damage=6, area=ours.area, drawable_geometry=ours.drawable_geometry)
        other = Event(type=12)
        display = FakeDisplay([ours, theirs, other, Event(type=91 | 0x80, damage=5, area=ours.area,
                                                         drawable_geometry=ours.drawable_geometry)])
        watch = screen.DamageWatch(display, 91, 5, (W, H), [])
        rects, size = watch.take()
        self.assertEqual(rects, [(1, 2, 3, 4), (1, 2, 3, 4)])
        self.assertEqual(size, (W, H))
        self.assertEqual(watch.take(), ([], None))
        watch.close()
        self.assertEqual(display.destroyed, [5])
        self.assertFalse(screen._damage_lock.locked())

    def test_a_refused_request_is_lost_reports(self):
        watch = screen.DamageWatch(FakeDisplay([]), 91, 5, (W, H), ["BadDamage"])
        with self.assertRaises(screen.DamageLost):
            watch.take()
        watch.close()

    def test_no_xlib_means_no_watch(self):
        saved = dict(screen._damage)
        screen._damage_lock.release()
        try:
            screen._damage.update(display=None, pid=None, code=None, failed_at=None)
            with mock.patch.dict(sys.modules, {"Xlib": None, "Xlib.display": None}), \
                    mock.patch.object(screen.session, "ensure"):
                self.assertIsNone(screen.damage_watch())
            self.assertFalse(screen._damage_lock.locked())
            self.assertIsNotNone(screen._damage["failed_at"])
            # Not tried again at once.
            with mock.patch.object(screen.session, "ensure", side_effect=AssertionError("tried again")):
                self.assertIsNone(screen.damage_watch())
        finally:
            screen._damage.clear()
            screen._damage.update(saved)
            screen._damage_lock.acquire()

    def test_a_second_watch_at_once_compares_pictures(self):
        self.assertIsNone(screen.damage_watch())   # the lock is held (setUp): the caller compares pictures


if __name__ == "__main__":
    unittest.main()
