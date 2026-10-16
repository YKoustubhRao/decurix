"""Text on a slide is read back and typed again slowly when the keys arrived out of order; a number box takes a
number by typing it."""

import unittest
from unittest import mock

from decurix_agent.computer import core
from decurix_agent.computer.errors import ComputerError


SLIDE = {"role": "paragraph", "name": "Click to add Text", "source": "atspi",
         "window_title": "Untitled 1 - LibreOffice Impress", "handle": {}}


class SlideTextIsReadBack(unittest.TestCase):
    def run_keep(self, readings, text="Northwind Traders - prepared", append=False):
        typed, keys = [], []
        reads = iter(readings)
        with mock.patch.object(core.atspi, "text_now", lambda e: next(reads)), \
                mock.patch.object(core.xinput, "type_text", lambda t, delay=12: typed.append((t, delay))), \
                mock.patch.object(core.xinput, "key", keys.append), \
                mock.patch.object(core.time, "sleep"):
            core._keep_slide_text(SLIDE, text, append)
        return typed, keys

    def test_text_that_came_out_right_is_left_alone(self):
        # AutoCorrect's en dash is the same text.
        typed, keys = self.run_keep(["Northwind Traders – prepared"])
        self.assertEqual((typed, keys), ([], []))

    def test_scrambled_text_is_selected_and_typed_again_slowly(self):
        scrambled = "Ns – preparedort for thwhined Tlraeders"
        typed, keys = self.run_keep([scrambled] * 6 + ["Northwind Traders - prepared"])
        self.assertEqual(keys, ["ctrl+a"])
        self.assertEqual(typed, [("Northwind Traders - prepared", 90)])

    def test_text_still_wrong_after_the_slow_typing_is_reported(self):
        with self.assertRaises(ComputerError):
            self.run_keep(["Nrthw"] * 12)

    def test_appended_text_is_never_retyped(self):
        with self.assertRaises(ComputerError):
            self.run_keep(["Nrthw"] * 6, append=True)

    def test_an_unreadable_paragraph_is_not_judged(self):
        typed, keys = self.run_keep([None])
        self.assertEqual((typed, keys), ([], []))


class ANumberBoxTakesANumber(unittest.TestCase):
    def test_select_of_a_number_types_it(self):
        box = {"role": "spinbutton", "name": "Number of rows", "source": "atspi", "handle": {}}
        calls = []
        real = core._perform

        def spy(kind, element, *rest, **kw):
            if kind == "type":
                calls.append(rest[1])  # (point, text, ...)
                return "xdotool"
            return real(kind, element, *rest, **kw)

        with mock.patch.object(core, "_perform", spy):
            self.assertEqual(real("select", box, None, None, None, None, None, "5", False), "xdotool")
        self.assertEqual(calls, ["5"])


if __name__ == "__main__":
    unittest.main()
