"""
Parameter constraints: what a typed field takes, worked out when a recording is
compiled and checked before a run types anything.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import unittest

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import FakeScreen, event, field
from test_skills import TeachingBase, demo

from decurix_agent import skill_task
from decurix_agent.computer import constraints, learning, replay, skills


class Inference(unittest.TestCase):

    def test_a_numeric_widget_takes_a_number_whatever_was_typed_in_it(self):
        self.assertEqual(constraints.infer("Quantity", "spinbutton", "x"), {"type": "number", "strict": True})

    def test_a_label_and_an_example_that_agree_are_enforced(self):
        self.assertEqual(constraints.infer("Work email", "textbox", "alice@acme.com"), {"type": "email", "strict": True})
        self.assertEqual(constraints.infer("Website", "textbox", "acme.com"), {"type": "url", "strict": True})
        self.assertEqual(constraints.infer("Amount", "textbox", "$1,200.50"), {"type": "number", "strict": True})

    def test_one_of_the_two_alone_is_only_a_hint(self):
        # The example is a number, the field is "Customer": one instance proves nothing about a type.
        self.assertEqual(constraints.infer("Customer", "textbox", "1042"), {"type": "number", "strict": False})
        # The label says email, the example is not one: "Email subject" is not an address.
        self.assertEqual(constraints.infer("Email subject", "textbox", "Hello there"), {"type": "email", "strict": False})

    def test_dates_and_phone_numbers_are_hints_never_enforced(self):
        self.assertEqual(constraints.infer("Due date", "textbox", "2026-09-30"), {"type": "date", "strict": False})
        self.assertEqual(constraints.infer("Phone", "textbox", "+44 20 7946 0958"), {"type": "phone", "strict": False})

    def test_free_text_says_nothing(self):
        self.assertIsNone(constraints.infer("Notes", "textbox", "call back on Tuesday"))


class Checking(unittest.TestCase):

    def test_a_value_the_field_cannot_take_is_named_with_why(self):
        email = {"type": "email", "strict": True}

        self.assertIsNone(constraints.problem(email, "bob@x.io"))
        self.assertIn("is not an email address", constraints.problem(email, "bob at x"))

    def test_a_hint_and_an_empty_value_are_never_refused(self):
        self.assertIsNone(constraints.problem({"type": "date", "strict": False}, "next week"))
        self.assertIsNone(constraints.problem({"type": "email", "strict": True}, ""))
        self.assertIsNone(constraints.problem(None, "anything"))

    def test_numbers_take_the_ways_people_write_them(self):
        number = {"type": "number", "strict": True}

        for good in ("40", "-3.5", "1,200", "1,200.50", "$40", "12%", " 7 "):
            self.assertIsNone(constraints.problem(number, good), good)
        for bad in ("forty", "12 apples", "1,20", ""):
            if bad:
                self.assertIsNotNone(constraints.problem(number, bad), bad)


class Replaying(unittest.TestCase):

    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0
        meta = {"id": "demo-20260914T101500-cccccc", "title": "Email a customer", "started_at": 1789380000}
        events = [event(0, "type", field("textbox", "Work email"), text="alice@acme.com")]
        self.program = replay.compile_program(meta, events)

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved

    def test_compiling_learns_what_the_field_takes(self):
        self.assertEqual(self.program["params"][0]["constraint"], {"type": "email", "strict": True})

    def test_a_wrong_value_is_refused_before_anything_is_typed(self):
        replay.core = fake = FakeScreen([field("textbox", "Work email")])
        result = replay.run(self.program, params={"work_email": "not an email"}, report=lambda line: None)

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("Work email", result["text"])
        self.assertIn("is not an email address", result["text"])
        self.assertEqual(fake.acted, [])

    def test_a_right_value_and_the_recorded_default_are_typed(self):
        replay.core = fake = FakeScreen([field("textbox", "Work email")])
        replay.run(self.program, params={"work_email": "bob@x.io"}, report=lambda line: None)
        replay.run(self.program, report=lambda line: None)

        self.assertEqual([kwargs["text"] for _, kwargs in fake.acted], ["bob@x.io", "alice@acme.com"])

    def test_a_value_for_a_step_before_the_start_is_not_judged(self):
        replay.core = FakeScreen([field("textbox", "Work email")])
        result = replay.run(self.program, params={"work_email": "not an email"}, start=2, report=lambda line: None)

        self.assertNotIn("is wrong", result["text"])


class Skills(TeachingBase):

    def email_demo(self):
        meta, events = demo(self.SESSION, "alice@acme.com")
        events[1]["element"]["name"] = "Work email"
        events[1]["expect"] = {"values": [{"role": "textbox", "name": "Work email", "value": "alice@acme.com"}]}
        return meta, events

    def test_a_skill_carries_what_its_field_takes_through_learning_and_expanding(self):
        skill = learning.build([self.email_demo()])

        self.assertEqual(skill["params"][0]["constraint"], {"type": "email", "strict": True})
        self.assertEqual(skills.program(skill)["params"][0]["constraint"], {"type": "email", "strict": True})

    def test_running_a_skill_asks_again_for_a_value_its_field_cannot_take(self):
        self.record(made=self.email_demo())
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("the customer is in the list", "yes", "changes", "yes"):
            skill_task.resume(reply)

        fake = self.screen(field("textbox", "Work email"))
        self.asked.clear()

        skill_task.perform('/skill add-a-customer work_email="not-an-email"')

        self.assertIn("is not an email address", self.asked[-1][0])
        self.assertIn("I need: work_email", self.asked[-1][0])
        self.assertEqual(fake.acted, [])

        skill_task.resume("bob@x.io")

        self.assertEqual([kwargs.get("text") for kind, kwargs in fake.acted if kind == "type"], ["bob@x.io"])

    def test_a_value_that_stays_wrong_ends_the_run_instead_of_asking_forever(self):
        self.record(made=self.email_demo())
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("the customer is in the list", "yes", "changes", "yes"):
            skill_task.resume(reply)

        self.screen(field("textbox", "Work email"))
        skill_task.perform('/skill add-a-customer work_email="nope"')
        skill_task.resume("still nope")
        skill_task.resume("nope again")

        self.assertEqual(self.finals[-1][1], "error")


if __name__ == "__main__":
    unittest.main()
