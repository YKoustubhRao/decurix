"""
A private value the person typed when they taught a task (a one-time code) is asked of them when the replay reaches it
and typed for them - never a hole for the model (skill_task._type_given, skills.asks_value / entry_end). Measured in the
learning curves (2026-11-17): the code's part cost 6-19 model turns on every filing. A reply that is the value is read
in code only (it is a secret: no model sees it).
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import skill_task  # noqa: E402
from decurix_agent.computer import replay, skills  # noqa: E402

FIELD = {"role": "textbox", "name": "Enter the one-time password (OTP)"}
STEPS = [
    {"n": 1, "kind": "click", "target": {"role": "button", "name": "File with EVC"}, "consequential": True},
    {"n": 2, "kind": "click", "target": dict(FIELD)},
    {"n": 3, "kind": "key", "keys": ["ctrl+a"], "target": dict(FIELD)},
    {"n": 4, "kind": "manual", "target": dict(FIELD)},
    {"n": 5, "kind": "click", "target": dict(FIELD)},
    {"n": 6, "kind": "manual", "target": dict(FIELD), "append": True},
    {"n": 7, "kind": "click", "target": {"role": "button", "name": "Verify"}},
]


class AValueTheyTyped(unittest.TestCase):
    def test_a_private_value_in_a_text_field_is_asked_not_by_hand(self):
        self.assertTrue(skills.asks_value(STEPS[3]))
        self.assertFalse(skills.asks_value({"kind": "manual", "target": {"role": "button", "name": "Approve"}}))

    def test_the_entry_ends_where_another_control_begins(self):
        self.assertEqual(skills.entry_end(STEPS, 4), 6)

    def test_a_reply_that_is_the_value_is_typed_and_the_run_goes_on_after_the_entry(self):
        run = {"skill": {"name": "f", "steps": STEPS, "params": []}, "values": {}, "start": 4}
        paused = {"run": run, "kind": "human", "step": 4, "resume_from": 5, "asks_value": True}
        typed = {}

        def fake_run(program, params=None, **_):
            typed.update(step=program["steps"][0], value=params["__given__"])
            return {"outcome": replay.VERIFIED, "text": "typed"}

        with mock.patch.object(skill_task.skills, "expand", return_value=({"steps": STEPS, "params": []}, {})), \
                mock.patch.object(skill_task, "_replay_values", return_value={}), \
                mock.patch.object(replay, "run", side_effect=fake_run), \
                mock.patch.object(skill_task, "_execute") as execute, \
                mock.patch.object(skill_task.approval, "read_choice", side_effect=AssertionError("no model reads it")):
            skill_task._run_reply(paused, "604217", None)
        self.assertEqual(typed["value"], "604217")
        self.assertEqual(typed["step"]["kind"], "type")
        self.assertEqual(typed["step"]["target"]["name"], FIELD["name"])
        self.assertEqual(run["start"], 7)
        execute.assert_called_once()

    def test_done_still_means_they_typed_it_themselves(self):
        self.assertEqual(skill_task._plain_hand("Done."), "done")
        self.assertEqual(skill_task._plain_hand("604217"), "")


class TheValueInAReply(unittest.TestCase):
    """Found in the learning curves (2026-11-17): "The client says the code is 512334." was typed whole into the
    OTP field, the portal refused it, and the return went unfiled. Read in code only - the reply may be secret."""

    def test_one_word_is_the_value_whole(self):
        self.assertEqual(skill_task.given_value("604217"), "604217")
        self.assertEqual(skill_task.given_value("s3cr3t!"), "s3cr3t!")

    def test_the_one_code_of_a_sentence(self):
        self.assertEqual(skill_task.given_value("The client says the code is 512334."), "512334")
        self.assertEqual(skill_task.given_value('the code is "AB12CD"'), "AB12CD")

    def test_none_or_two_is_asked_again_never_typed_whole(self):
        for reply in ("I do not know", "codes 123456 and 654321", "Year 2026 OTP 604217", ""):
            self.assertIsNone(skill_task.given_value(reply), reply)


if __name__ == "__main__":
    unittest.main()
