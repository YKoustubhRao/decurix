"""
Load-aware replay waits (computer/waits.py): a wait for a control or a step's result is the fixed base on an idle host
with an unchanging screen, is stretched by the host's load and while the screen is still changing, and never passes
MAX_FACTOR times the base. Found on the curves (2026-11-11): three of five replay stops were waits that ran out at
load 23-39, each handing the rest of the run to the model.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_replay_waits.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import waits


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


def screen(n):
    return {"active_title": "Books", "elements": [{"role": "row", "name": f"row {i}"} for i in range(n)]}


class Waits(unittest.TestCase):

    def test_an_idle_host_and_a_still_screen_wait_exactly_the_base(self):
        clock = Clock()
        wait = waits.Patience(10, clock=clock, load=lambda: 1.0)
        clock.now = 9.9
        self.assertTrue(wait.more(screen(3)))
        clock.now = 10.0
        self.assertFalse(wait.more(screen(3)))
        self.assertEqual(wait.stretched(), "")

    def test_a_loaded_host_waits_longer(self):
        clock = Clock()
        wait = waits.Patience(10, clock=clock, load=lambda: 3.0)
        clock.now = 25
        self.assertTrue(wait.more(screen(3)))
        clock.now = 30
        self.assertFalse(wait.more(screen(3)))
        self.assertIn("host load x3.0", wait.stretched())

    def test_a_screen_still_drawing_moves_the_deadline_up_to_the_cap(self):
        clock = Clock()
        wait = waits.Patience(10, clock=clock, load=lambda: 1.0)
        wait.more(screen(1))
        for n in range(2, 100):
            clock.now += 3
            if not wait.more(screen(n)):
                break
        cap = 10 * waits.MAX_FACTOR
        self.assertTrue(cap <= clock.now < cap + 3, clock.now)  # (still changing every poll: stopped at the cap)
        self.assertIn("still changing", wait.stretched())

    def test_the_load_never_stretches_past_the_cap(self):
        clock = Clock()
        wait = waits.Patience(10, clock=clock, load=lambda: 50.0)
        clock.now = 10 * waits.MAX_FACTOR
        self.assertFalse(wait.more(screen(3)))


if __name__ == "__main__":
    unittest.main()
