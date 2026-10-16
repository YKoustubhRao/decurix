"""
Teaching from a messy demonstration: presses a person repeats while a slow app
starts, typing the recorder split in two, and a recording made of clicks at
bare positions (an app shown as a picture). Modelled on a real recording of
"search flamingo breeding season" in Brave shown from the Windows host, which
was learned as ten steps and asked about six of them one at a time.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import unittest

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import event

from decurix_agent.computer import learning


def press(i, t, kind, point, **extra):
    return event(i, kind, None, t=t, point=point, window="", **extra)


def brave_demo():
    """What the recorder kept, event for event (points, kinds and times as recorded)."""

    events = [
        press(0, 9.53, "click", [72, 270]),
        press(1, 13.05, "drag", [72, 270], end=[71, 259]),
        press(2, 16.86, "double_click", [71, 259]),
        press(3, 18.27, "click", [71, 259]),
        press(4, 19.27, "click", [71, 259], expect={"url": "http://decurix:8080/host-gui/view"}),
        event(5, "click", None, t=33.97, point=[1004, 48], window="Brave"),
        event(6, "type", None, t=45.7, text="fl", window="Brave"),
        event(7, "type", None, t=47.33, text="amingo breeding season", window="Brave"),
        event(8, "key", None, t=59.46, keys=["Return"], window="Brave"),
        event(9, "double_click", None, t=68.7, point=[676, 522], window="Brave"),
    ]
    meta = {"id": "demo-20260923T211809-b176b6", "title": 'search "flamingo breeding season"', "kind": "demonstration",
            "status": "complete", "started_at": 1790198289, "actors": ["human"], "screen": [1527, 727]}
    return meta, events


class Tidying(unittest.TestCase):

    def setUp(self):
        self.skill = learning.build([brave_demo()])

    def test_a_burst_of_presses_while_an_app_starts_is_one_double_click(self):
        first = self.skill["steps"][0]
        self.assertEqual(first["kind"], "double_click")
        self.assertEqual(first["point"], [71, 259], "where the double click that opened it was")
        self.assertEqual(first["event"], 2, "its picture of the screen is the double click's")
        self.assertNotIn("end", first)
        self.assertEqual(first["expect"], [{"type": "url", "value": "http://decurix:8080/host-gui/view"}],
                         "expects what the last press produced")

    def test_typing_split_by_the_recorder_is_one_value(self):
        typed = [s for s in self.skill["steps"] if s["kind"] == "type"]
        self.assertEqual(len(typed), 1)
        self.assertEqual(len(self.skill["params"]), 1)
        self.assertEqual(self.skill["params"][0]["example"], "flamingo breeding season")

    def test_ten_recorded_steps_become_five_and_the_person_is_told_why(self):
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["double_click", "click", "type", "key", "double_click"])
        text = learning.next_question(self.skill)["text"]
        self.assertIn("learned 5 steps", text)
        self.assertIn("5 quick presses on the same spot", text)
        self.assertIn('("fl" + "amingo breeding season")', text)
        self.assertIn("The recording itself is unchanged", text)

    def test_two_single_clicks_on_one_spot_are_left_alone(self):
        """A counter or a toggle is clicked twice on purpose; only a burst with a double-click in it opens something."""

        meta, _ = brave_demo()
        events = [press(0, 1.0, "click", [300, 300], expect={"url": "http://x/1"}),
                  press(1, 2.0, "click", [300, 300], expect={"url": "http://x/2"}),
                  press(2, 3.0, "click", [300, 300])]
        skill = learning.build([(meta, events)])
        self.assertEqual([s["kind"] for s in skill["steps"]], ["click", "click", "click"])
        self.assertEqual(skill["tidied"], [])

    def test_presses_far_apart_in_time_or_space_are_separate(self):
        meta, _ = brave_demo()
        events = [press(0, 1.0, "click", [300, 300]),
                  press(1, 9.0, "double_click", [300, 300]),
                  press(2, 9.5, "click", [600, 300])]
        skill = learning.build([(meta, events)])
        self.assertEqual([s["kind"] for s in skill["steps"]], ["click", "double_click", "click"])

    def test_a_press_that_had_a_result_of_its_own_is_not_folded_into_the_next(self):
        meta, _ = brave_demo()
        events = [press(0, 1.0, "click", [300, 300], expect={"url": "http://x/selected"}),
                  press(1, 1.8, "double_click", [300, 300])]
        skill = learning.build([(meta, events)])
        self.assertEqual(len(skill["steps"]), 2)

    def test_a_drag_that_only_nudged_an_icon_is_part_of_the_burst(self):
        """Measured on a real desktop: an 11 px drag moved the icon, which the recorder saw as its result."""

        meta, _ = brave_demo()
        events = [press(0, 1.0, "drag", [69, 463], end=[68, 452], expect={"url": "http://x/nudged"}),
                  press(1, 2.5, "double_click", [68, 452]),
                  press(2, 3.4, "click", [68, 452], expect={"url": "http://x/opened"})]
        skill = learning.build([(meta, events)])
        self.assertEqual([(s["kind"], s["point"]) for s in skill["steps"]], [("double_click", [68, 452])])
        self.assertEqual(skill["steps"][0]["expect"], [{"type": "url", "value": "http://x/opened"}])

    def test_a_real_drag_is_never_taken_for_a_press(self):
        meta, _ = brave_demo()
        events = [press(0, 1.0, "drag", [300, 300], end=[500, 300]),
                  press(1, 1.5, "double_click", [300, 300])]
        skill = learning.build([(meta, events)])
        self.assertEqual([s["kind"] for s in skill["steps"]], ["drag", "double_click"])

    def test_typing_into_a_secret_field_is_never_joined(self):
        meta, _ = brave_demo()
        events = [event(0, "type", None, t=1.0, text="first", window="Brave"),
                  event(1, "type", None, t=2.0, text=None, withheld={"text": "password field"}, window="Brave")]
        skill = learning.build([(meta, events)])
        self.assertEqual(skill["tidied"], [])


class GroupedPositions(unittest.TestCase):

    def setUp(self):
        self.skill = learning.build([brave_demo()])
        while (question := learning.next_question(self.skill))["kind"] != "coordinates":
            learning.answer(self.skill, question, {"goal": "skip", "start": "no"}[question["kind"]])
        self.question = question

    def test_every_positional_click_is_one_question(self):
        self.assertEqual(self.question["kind"], "coordinates")
        self.assertEqual([s["n"] for s in self.question["steps"]], [1, 2, 5])
        self.assertIn("3 steps were recorded as clicks", self.question["text"])
        self.assertIn("1. double click 71,259\n2. click 1004,48\n5. double click 676,522", self.question["text"])

    def test_keep_keeps_them_all_and_the_next_question_is_not_about_positions(self):
        result = learning.answer(self.skill, self.question, "keep")
        self.assertTrue(result["understood"])
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["double_click", "click", "type", "key", "double_click"])
        self.assertEqual(learning.next_question(self.skill)["kind"], "variable")

    def test_manual_hands_each_to_the_person_in_plain_words(self):
        learning.answer(self.skill, self.question, "manual")
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["manual", "manual", "type", "key", "manual"])
        self.assertEqual(self.skill["steps"][0]["summary"], "you do this by hand: double click 71,259")

    def test_exceptions_are_named_and_the_rest_kept(self):
        result = learning.answer(self.skill, self.question, "manual 1, skip 5")
        self.assertTrue(result["understood"])
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["manual", "click", "type", "key"])
        self.assertIn("keeping 2", result["note"])

    def test_bare_numbers_carry_the_verb_before_them(self):
        learning.answer(self.skill, self.question, "skip 2 and 5")
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["double_click", "type", "key"])

    def test_ranges_are_steps_too(self):
        learning.answer(self.skill, self.question, "manual 1-2")
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["manual", "manual", "type", "key", "double_click"])

    def test_a_number_that_is_not_in_the_question_is_asked_again_never_guessed(self):
        result = learning.answer(self.skill, self.question, "skip 3")
        self.assertFalse(result["understood"])
        self.assertEqual(len(self.skill["steps"]), 5)

    def test_conflicting_answers_for_one_step_are_asked_again(self):
        self.assertFalse(learning.answer(self.skill, self.question, "skip 2, manual 2")["understood"])

    def test_unclear_twice_stops_at_each_for_the_person(self):
        learning.answer(self.skill, self.question, "hmm")
        result = learning.answer(self.skill, self.question, "what?")
        self.assertTrue(result["understood"])
        self.assertEqual([s["kind"] for s in self.skill["steps"]], ["manual", "manual", "type", "key", "manual"])

    def test_answers_are_kept_per_step_so_a_half_taught_lesson_resumes(self):
        """A draft taught before the grouped question has per-step answers; only the rest are asked."""

        self.skill["answers"]["coordinates:s1"] = "keep"
        question = learning.next_question(self.skill)
        self.assertEqual([s["n"] for s in question["steps"]], [2, 5])

        self.skill["answers"]["coordinates:s2"] = "keep"
        question = next(q for q in learning.questions(self.skill) if q["kind"] == "coordinates")
        self.assertEqual(question["id"], "coordinates:s5", "one left is asked the way a single one always was")
        self.assertIn("Step 5 (double click 676,522)", question["text"])

    def test_a_step_a_person_names_is_found_by_that_control(self):
        result = learning.answer(self.skill, self.question, "keep (2 is the Search button)")
        self.assertTrue(result["understood"])
        step = self.skill["steps"][1]
        self.assertEqual(step["target"], {"role": "button", "name": "Search"})
        self.assertNotIn("point", step)
        self.assertNotIn("look", step)

    def test_naming_the_one_step_left_is_an_answer(self):
        self.skill["answers"]["coordinates:s1"] = "keep"
        self.skill["answers"]["coordinates:s5"] = "keep"
        question = next(q for q in learning.questions(self.skill) if q["kind"] == "coordinates")
        result = learning.answer(self.skill, question, "It's the Search button at the top right.")
        self.assertTrue(result["understood"])
        self.assertIn('button "Search"', result["note"])
        self.assertEqual(self.skill["steps"][1]["target"], {"role": "button", "name": "Search"})

    def test_a_reply_that_names_more_than_one_thing_is_not_taken_for_a_control(self):
        self.assertIsNone(learning._named_control("the Next button, not the Back one"))
        self.assertIsNone(learning._named_control("keep"))
        self.assertEqual(learning._named_control('the button called "Pay now"'), ("button", "Pay now"))


if __name__ == "__main__":
    unittest.main()
