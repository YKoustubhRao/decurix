"""
A person's answer that names the control a spot-only press meant re-aims that press (study._aim_spots). Found live
(2026-11-02): the lesson asked which control saves each item, as the ring of a recorded press was on empty panel
space; the person said "the Save button on the form", the answer reached the rules only, and the replay pressed the
panel once per item, typing every item into one unsaved form until a guard stopped the last step.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_study_aims_spots
"""

import json
import unittest
from unittest import mock

import test_skills  # noqa: F401  (imported first: it stubs decurix_agent.config on a machine with no desktop)

from decurix_agent import study

ANSWER = "The Save button on the form itself, not the empty panel space."


def skill():
    spot = {"kind": "click", "point": [129, 719], "window": "Voucher - Browser", "summary": "click 129,719"}
    return {"name": "enter-items", "steps": [
        {"n": 1, "id": "s1", "kind": "type", "target": {"role": "textbox", "name": "Amount"}, "summary": "type"},
        dict(spot, n=2, id="s2"),
        {"n": 3, "id": "s3", "kind": "type", "target": {"role": "textbox", "name": "Amount"}, "summary": "type"},
        dict(spot, n=4, id="s4"),
        {"n": 5, "id": "s5", "kind": "click", "point": [600, 300], "window": "Voucher - Browser", "summary": "click 600,300"},
        {"n": 6, "id": "s6", "kind": "click", "target": {"role": "link", "name": "Back"}, "summary": "click link"},
    ]}


class AnAnswerThatNamesTheControlOfASpot(unittest.TestCase):

    def test_the_steps_it_names_and_their_repetitions_find_that_control(self):
        s = skill()
        aimed = study._aim_spots(s, [{"steps": [2], "role": "button", "name": "Save"}], said=ANSWER)
        # Step 4 is the same spot of the same window - the next repetition of the block - and goes with step 2.
        self.assertEqual(aimed, [2, 4])
        for n in (2, 4):
            step = s["steps"][n - 1]
            self.assertEqual(step["target"], {"role": "button", "name": "Save"})
            self.assertNotIn("point", step)
            self.assertIn("The person said: The Save button on the form itself", step["intent"])
        # Another spot, and a step that already has its control, are not touched.
        self.assertEqual(s["steps"][4]["point"], [600, 300])
        self.assertEqual(s["steps"][5]["target"], {"role": "link", "name": "Back"})

    def test_nothing_is_aimed_without_a_control_it_can_name(self):
        for controls in ([], None, "Save", [{"steps": [2], "role": "thing", "name": "Save"}],
                         [{"steps": [2], "role": "button", "name": ""}],
                         [{"steps": [2], "role": "button", "name": "the one at the bottom of the long form"}],
                         [{"steps": [6], "role": "button", "name": "Save"}],   # not a spot
                         [{"steps": [99], "role": "button", "name": "Save"}]):
            s = skill()
            self.assertEqual(study._aim_spots(s, controls, said=ANSWER), [], controls)
            self.assertEqual(s["steps"][1]["point"], [129, 719])

    def test_absorb_applies_what_the_model_read_from_the_answer(self):
        s = skill()
        s["understanding"] = {"summary": "Enter the items.", "rules": []}
        reply = {"summary": "Enter the items.", "rules": ["Save each item with the form's Save button."],
                 "controls": [{"steps": [2, 4], "role": "button", "name": "Save"}]}
        with mock.patch.object(study, "available", return_value=True), \
                mock.patch.object(study, "_call", return_value=reply):
            study.absorb(s, "Which control saves each item?", ANSWER)
        self.assertEqual(s["steps"][1]["target"]["name"], "Save")
        self.assertEqual(s["steps"][3]["target"]["name"], "Save")
        self.assertIn('"controls"', study.ABSORB_SYSTEM)
        json.dumps(s)  # (still a skill that can be saved)


if __name__ == "__main__":
    unittest.main()
