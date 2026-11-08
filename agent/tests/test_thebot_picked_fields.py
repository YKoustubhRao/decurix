"""
Every job of a taught task sent to soul carries the fields filled by picking an existing entry from the application's
own list, with the words this run searches with (pickedFields, from skill_task._chosen_for), for soul's review. Found
live (2026-11-08): told only the run's values, the review sent a saved bill back twice to put the document's line
description where the application's item belonged. Data beside the prompt: the prompt's bytes stay as they are.
"""

import unittest
from unittest.mock import patch

import test_recordings  # noqa: F401 - stubs decurix_agent.config first
from test_taught_record import bill_skill, field_step

from decurix_agent import guidance, person_record, thebot_harness, workflow_task


def picked_skill():
    """The bill form, its Item typed and then picked from the list the typing brought up."""

    skill = bill_skill()
    steps = skill["steps"]
    option = field_step(0, "option", "Copier Paper", kind="click")
    steps.insert(5, option)
    for n, step in enumerate(steps, 1):
        step["n"] = n
    return skill


class ThePickedFieldsField(unittest.TestCase):
    def setUp(self):
        self.bodies = []
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        patches = [
            patch.object(thebot_harness, "_post_json", side_effect=lambda url, body: self.bodies.append(body) or {}),
            patch.object(thebot_harness, "_build_context", return_value=""),
            patch.object(workflow_task, "_active", None),
            patch.object(thebot_harness, "_request_id", return_value="t1"),
            patch.object(person_record, "answers", return_value=[]),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(guidance.end)

    def sent(self):
        thebot_harness._run_thebot("enter the bill", reply=False)
        return self.bodies[-1]

    def test_a_picked_field_goes_with_its_search_words(self):
        run = {"skill": picked_skill(), "values": {"item": "Executive Copier Paper A4 75 GSM", "qty": "20"}}
        guidance.begin(skill="enter-bill", task="enter the bill", text="", values=run["values"], run=run)
        picked = self.sent()["pickedFields"]
        self.assertEqual(picked, ['Item (item): searched with "Executive Copier Paper A4 75 GSM"'])
        self.assertEqual(self.bodies[-1]["prompt"], "enter the bill")

    def test_no_taught_task_sends_none(self):
        guidance.end()
        self.assertEqual(self.sent()["pickedFields"], [])


    def test_the_gather_phase_is_sent_as_collect_only(self):
        from decurix_agent import skill_task
        run = {"skill": picked_skill(), "values": {}}
        guidance.begin(skill="enter-bill", task="collect", text="", values={}, run=run, phase=skill_task.GATHER_PHASE)
        self.assertTrue(self.sent()["collectOnly"])
        guidance.begin(skill="enter-bill", task="enter", text="", values={}, run=run)
        self.assertFalse(self.sent()["collectOnly"])

if __name__ == "__main__":
    unittest.main()
