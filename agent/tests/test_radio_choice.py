"""
A click on one radio button of a named group is a choice in that group, like an option of a drop-down. Found live
(2026-10-14): kept as a click on "Full-time", a later run for a contractor entered Full-time and had to ask.
"""

import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import META, event

from decurix_agent.computer import core, replay

GROUP = 'site x > group "Employment type"'


def radio(name, checked=False):
    return {"role": "radio", "name": name, "path": f'{GROUP} > radio "{name}"', "source": "cdp",
            "states": ["checked" if checked else "unchecked"]}


class ARadioClickIsAChoice(unittest.TestCase):
    def test_it_compiles_to_a_choice_named_after_its_group(self):
        program = replay.compile_program(META, [event(0, "click", radio("Full-time"))], frames=mock.Mock(png=lambda f: None))
        step = program["steps"][0]
        self.assertEqual(step["kind"], "select")
        self.assertEqual(step["option"], "Full-time")
        self.assertEqual(program["params"][0]["label"], "Employment type")
        self.assertEqual(program["params"][0]["default"], "Full-time")
        self.assertIn('the radio buttons "Employment type"', step["summary"])

    def test_a_radio_with_no_group_stays_a_click(self):
        lone = {"role": "radio", "name": "Yes", "path": 'site x > radio "Yes"', "source": "cdp"}
        program = replay.compile_program(META, [event(0, "click", lone)], frames=mock.Mock(png=lambda f: None))
        self.assertEqual(program["steps"][0]["kind"], "click")


class ChoosingAmongRadios(unittest.TestCase):
    def setUp(self):
        self.radios = [radio("Full-time", True), radio("Part-time"), radio("Contractor")]
        self.clicked = []
        for patch in (mock.patch.object(core, "observe", lambda remember=True, frame=None: {"elements": self.radios}),
                      mock.patch.object(core.cdp, "click", lambda e, **kw: self.clicked.append(e["name"]) or {})):
            patch.start()
            self.addCleanup(patch.stop)

    def choose(self, option):
        return core._choose_radio(self.radios[0], option)

    def test_the_radio_named_within_the_value_is_chosen(self):
        self.choose("Contractor (12 months)")
        self.assertEqual(self.clicked, ["Contractor"])

    def test_by_name_whatever_the_case(self):
        self.choose("part-time")
        self.assertEqual(self.clicked, ["Part-time"])

    def test_never_one_of_two(self):
        with self.assertRaises(core.ComputerError):
            self.choose("Full-time or Part-time")
        self.assertEqual(self.clicked, [])


if __name__ == "__main__":
    unittest.main()
