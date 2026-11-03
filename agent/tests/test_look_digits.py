"""
A look whose picture shows numbers acts only where a text read shows the same numbers (review 2026-11-03, D-15). A row
one digit off passes the picture compare, so the picture alone makes a candidate: other numbers refuse the step, text
that cannot be read refuses it too, and a desktop that reads no text at all leaves the run unverified.

OCR itself (ocr.read) is patched here, so what digits_hold makes of "no line read" is what is tested.
"""

import base64
import unittest
from types import SimpleNamespace
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.computer import ocr, replay, visual

W, H = 8, 4
LOOK = {"png": base64.b64encode(visual.encode_png(W, H, bytes(W * H * 3))).decode(), "w": W, "h": H, "at": [4, 2]}
FRAME = SimpleNamespace(width=40, height=20, rgb=bytes(40 * 20 * 3))


def line(text):
    return [{"text": text, "box": [0, 0, W, H]}]


def reads(recorded, now):
    """ocr.read giving the recorded look's lines, then the found place's."""
    return [mock.patch.object(ocr, "available", return_value=True),
            mock.patch.object(ocr, "read", side_effect=[recorded, now])]


class TheNumbersAreRead(unittest.TestCase):
    def hold(self, recorded, now):
        patches = reads(recorded, now)
        for p in patches:
            p.start()
        try:
            return visual.digits_hold(LOOK, FRAME, (10, 10))
        finally:
            for p in patches:
                p.stop()

    def test_the_same_numbers_hold(self):
        self.assertEqual(self.hold(line("Invoice 1042"), line("Invoice 1042")), visual.SAME)

    def test_other_numbers_do_not(self):
        self.assertEqual(self.hold(line("Invoice 1042"), line("Invoice 1041")), visual.OTHER)

    def test_a_look_whose_line_was_not_read_confirms_nothing(self):
        # The rework case: no line read at the look, the place reads other numbers - never SAME.
        self.assertEqual(self.hold([{"text": "Total", "box": [100, 100, 5, 5]}], line("Invoice 1041")), visual.UNREAD)
        self.assertEqual(self.hold([], line("Invoice 1041")), visual.UNREAD)

    def test_a_place_whose_line_was_not_read_confirms_nothing(self):
        self.assertEqual(self.hold(line("Invoice 1042"), []), visual.UNREAD)

    def test_no_text_in_either_picture_is_the_same(self):
        self.assertEqual(self.hold([], []), visual.SAME)

    def test_a_desktop_with_no_ocr_says_so(self):
        with mock.patch.object(ocr, "available", return_value=False):
            self.assertEqual(visual.digits_hold(LOOK, FRAME, (10, 10)), visual.NO_OCR)


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
        (_, point, why), _ = self.resolve(visual.OTHER)
        self.assertIsNone(point)
        self.assertIn("other numbers", why)

    def test_unread_text_refuses_the_step(self):
        (_, point, why), _ = self.resolve(visual.UNREAD)
        self.assertIsNone(point)
        self.assertIn("could not be read", why)

    def test_no_ocr_acts_but_marks_the_look_unconfirmed(self):
        (_, point, _), obs = self.resolve(visual.NO_OCR)
        self.assertEqual(point, (12, 10))
        self.assertTrue(obs["visual"][2]["unconfirmed"])

    def test_the_same_numbers_act(self):
        (_, point, _), obs = self.resolve(visual.SAME)
        self.assertEqual(point, (12, 10))
        self.assertFalse(obs["visual"][2]["unconfirmed"])


if __name__ == "__main__":
    unittest.main()
