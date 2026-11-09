"""
A question about an application's date order is asked at most once (owner's ruling 2026-11-09: a month's six saves each
asked "which day is 04-11-2026", and the case ended on the number of questions). The person's answer is kept for that
application's host across tasks (task_context.kept_app_order); a question they could not settle is not put again in the
task (date_left_open).
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import task_context

TASK, NEXT = "t1234567890abcdef", "tfedcba0987654321"
HOST = "books.example.test"


class AskedOncePerApplication(unittest.TestCase):
    def setUp(self):
        self.context = {}
        for p in (mock.patch.object(task_context, "_context", lambda task_id: self.context.setdefault(task_id, {})),
                  mock.patch.object(task_context, "_keep_context",
                                    lambda task_id, **f: self.context.setdefault(task_id, {}).update(f)),
                  mock.patch.object(task_context, "sent_date_order", lambda task_id: None),
                  mock.patch.object(task_context, "valid", lambda task_id: True)):
            p.start()
            self.addCleanup(p.stop)
        task_context._kept_orders.update(at=0.0, rows=[])
        self.addCleanup(task_context._kept_orders.update, at=0.0, rows=[])
        self.addCleanup(task_context._app_host.update, task="", host="")

    def answer(self, task_id, reply="1"):
        question = task_context.ask_date_order(task_id, "app", "04-11-2026")
        self.context[task_id]["date_question"] = dict(question, query_id="q1")
        with mock.patch("decurix_agent.person_record.keep_field_answer") as kept:
            taken = task_context.take_date_answer(task_id, reply, judgement_id="j_0123456789abcdef", query_id="q1")
        return question, taken, kept

    def test_the_second_save_and_the_next_run_on_the_same_application_are_not_asked(self):
        task_context.note_app_host(HOST, TASK)
        question, taken, kept = self.answer(TASK)
        self.assertEqual((question["host"], taken), (HOST, "day"))
        self.assertEqual(task_context.app_date_order(TASK), "day")  # (the second save of this task)
        self.assertEqual(kept.call_args.args[2:5], (task_context.DATE_ORDER_LABEL, HOST, "day"))

        # The next run, on the same application: the control plane gives the kept answer back.
        row = {"label": task_context.DATE_ORDER_LABEL, "host": HOST, "option": "day"}
        task_context._kept_orders.update(at=0.0)
        with mock.patch("decurix_agent.person_record.field_answers", return_value=[row]):
            task_context.note_app_host(HOST, NEXT)
            self.assertEqual(task_context.app_date_order(NEXT), "day")
            task_context.note_app_host("other.example.test", NEXT)
            task_context._kept_orders.update(at=0.0)
            self.assertIsNone(task_context.app_date_order(NEXT), "another application is asked")

    def test_a_question_the_person_could_not_settle_is_not_put_again_in_the_task(self):
        task_context.note_app_host(HOST, TASK)
        question = task_context.ask_date_order(TASK, "app", "04-11-2026")
        self.context[TASK]["date_question"] = dict(question, query_id="q1")
        self.assertIsNone(task_context.take_date_answer(TASK, "I don't know", judgement_id="j_0123456789abcdef",
                                                        query_id="q1"))
        task_context.unmark_date_asked(TASK)
        self.assertTrue(task_context.date_left_open(TASK, "app"))
        self.assertIsNone(task_context.ask_date_order(TASK, "app", "07-12-2026"))
        self.assertIsNone(task_context.date_question(TASK))


if __name__ == "__main__":
    unittest.main()
