import unittest
from unittest import mock

from decurix_agent import skill_task


# GST suite T9 (2026-10-21): an engine that ran out of steps with three of eight invoices saved asked the person to
# "start a new run". It carries on by itself now, a few times a task, never in a planned run's stage.
class TestAnEngineThatRanOutOfStepsCarriesOn(unittest.TestCase):

    OUT = "I ran out of steps (77 commands) before this was finished, and stopped rather than start over."

    def setUp(self):
        skill_task._carried_on.update(task=None, count=0)
        for patcher in (mock.patch.object(skill_task.task_context, "current", return_value="t9"),
                        mock.patch.object(skill_task.task_context, "current_stage", return_value=("", 0)),
                        mock.patch.object(skill_task.progress, "emit"),
                        mock.patch.object(skill_task.threading, "Thread")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_it_carries_on_three_times_then_asks(self):
        with mock.patch("decurix_agent.long_task.active", return_value=False):
            self.assertEqual([skill_task._carries_on(self.OUT) for _ in range(4)], [True, True, True, False])

    def test_other_questions_and_planned_stages_are_not_its_business(self):
        with mock.patch("decurix_agent.long_task.active", return_value=False):
            self.assertFalse(skill_task._carries_on("Which customer is this for?"))
        with mock.patch("decurix_agent.long_task.active", return_value=True):
            self.assertFalse(skill_task._carries_on(self.OUT))


if __name__ == "__main__":
    unittest.main()
