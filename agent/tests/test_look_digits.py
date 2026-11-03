"""
A look whose picture shows numbers acts only where a text read shows the same numbers (review 2026-11-03, D-15). A row
one digit off passes the picture compare, so the picture alone makes a candidate: other numbers refuse the step, and a
place that cannot be read leaves the run unverified.
"""

import base64
import unittest
from types import SimpleNamespace
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.computer import replay, visual

LOOK = {"png": base64.b64encode(visual.encode_png(4, 2, bytes(24))).decode(), "w": 4, "h": 2, "at": [1, 1]}
FRAME = SimpleNamespace(width=40, height=20, rgb=bytes(40 * 20 * 3))


def reads(*lines):
    """_line_digits giving the recorded look's digits, then the found place's."""
    return mock.patch.object(visual, "_line_digits", side_effect=list(lines))


class TheNumbersAreRead(unittest.TestCase):
    def test_the_same_numbers_hold(self):
        with reads(["1042"], ["1042"]):
            self.assertTrue(visual.digits_hold(LOOK, FRAME, (10, 10)))

    def test_other_numbers_do_not(self):
        with reads(["1042"], ["1041"]):
            self.assertIs(visual.digits_hold(LOOK, FRAME, (10, 10)), False)

    def test_no_numbers_need_no_read(self):
        with reads([]):
            self.assertTrue(visual.digits_hold(LOOK, FRAME, (10, 10)))

    def test_nothing_read_is_not_known(self):
        with reads(None):
            self.assertIsNone(visual.digits_hold(LOOK, FRAME, (10, 10)))


class AReplayTakesOnlyWhatReadsTheSame(unittest.TestCase):
    STEP = {"n": 2, "kind": "click", "point": [10, 10], "look": LOOK}
    OBS = {"active_title": "x", "elements": [], "frame": FRAME}
    SEEN = {"status": visual.FOUND, "point": (12, 10), "offset": (2, 0)}

    def resolve(self, held):
        with mock.patch.object(visual, "find", return_value=dict(self.SEEN)), \
                mock.patch.object(visual, "digits_hold", return_value=held):
            obs = dict(self.OBS)
            return replay.resolve(self.STEP, obs), obs

    def test_other_numbers_refuse_the_step(self):
        (_, point, why), _ = self.resolve(False)
        self.assertIsNone(point)
        self.assertIn("other numbers", why)

    def test_unread_numbers_act_but_mark_the_look_unconfirmed(self):
        (_, point, _), obs = self.resolve(None)
        self.assertEqual(point, (12, 10))
        self.assertTrue(obs["visual"][2]["unconfirmed"])

    def test_the_same_numbers_act(self):
        (_, point, _), obs = self.resolve(True)
        self.assertEqual(point, (12, 10))
        self.assertFalse(obs["visual"][2]["unconfirmed"])


if __name__ == "__main__":
    unittest.main()
