import unittest
from unittest import mock

from decurix_agent import final_gate, skill_task
from decurix_agent.computer import approval


# ============================================================
# One reading of a reply to a question with set answers (approval.read_choice).
# Found (audit, 2026-10-24): word lists anchored at a reply's first word read
# "retry but use the second file" as a plain retry, "no, use option 2" as stop,
# "correct except the total, fix that" as done, and "yes, after you take a
# backup" released a command that cannot be undone.
# ============================================================

CHOICES = {"retry": "do it again", "skip": "leave it out", "stop": "stop here"}
PLAIN = {"retry": ("retry", "try again"), "skip": ("skip",), "stop": ("stop",)}


class TheReadingOfAChoice(unittest.TestCase):
    def setUp(self):
        approval._CHOSEN.clear()

    def test_a_reply_that_is_only_a_choice_needs_no_model(self):
        with mock.patch("decurix_agent.llm_client.available", return_value=False):
            for reply, choice in (("retry", "retry"), ("Retry please", "retry"), ("try again!", "retry"),
                                  ("SKIP", "skip"), ("stop, thanks", "stop")):
                self.assertEqual(approval.read_choice(reply, "Step 3 failed.", CHOICES, PLAIN),
                                 {"choice": choice, "instruction": ""}, reply)

    def test_with_no_model_anything_more_is_unclear_never_guessed(self):
        with mock.patch("decurix_agent.llm_client.available", return_value=False):
            for reply in ("retry but use the second file", "no, use option 2", "stop after this one"):
                self.assertEqual(approval.read_choice(reply, "q", CHOICES, PLAIN)["choice"], "unclear", reply)

    def test_the_model_reads_the_rest_and_keeps_it(self):
        answer = {"content": '{"choice": "retry", "instruction": "use the second file"}'}
        with mock.patch("decurix_agent.llm_client.available", return_value=True), \
                mock.patch("decurix_agent.llm_client.chat", return_value=(answer, {})) as chat:
            said = approval.read_choice("retry but use the second file", "Step 3 failed.", CHOICES, PLAIN)
            self.assertEqual(said, {"choice": "retry", "instruction": "use the second file"})
            # One reply, one reading: asked again, it is not read twice.
            approval.read_choice("retry but use the second file", "Step 3 failed.", CHOICES, PLAIN)
        self.assertEqual(chat.call_count, 1)
        sent = chat.call_args[0][0][1]["content"]
        self.assertIn("- retry: do it again", sent)
        self.assertIn("Step 3 failed.", sent)

    def test_a_choice_the_model_makes_up_is_unclear(self):
        answer = {"content": '{"choice": "delete everything", "instruction": ""}'}
        with mock.patch("decurix_agent.llm_client.available", return_value=True), \
                mock.patch("decurix_agent.llm_client.chat", return_value=(answer, {})):
            self.assertEqual(approval.read_choice("hmm", "q", CHOICES, PLAIN)["choice"], "unclear")


class AStuckReplay(unittest.TestCase):
    """The reply to "step N could not be done - retry, skip, stop, or tell me which to use"."""

    def setUp(self):
        self.run = {"skill": {"name": "s", "steps": []}, "values": {}}
        self.paused = {"run": self.run, "step": 3, "ran": False, "resume_from": 3, "why": "Step 3 could not be done.",
                       "unclear": 0}
        self.did = []
        for name in ("_execute", "_by_model", "_stop", "_retarget", "_ask_stuck", "_unanchor", "_settle"):
            patcher = mock.patch.object(skill_task, name, side_effect=lambda *a, _n=name, **k: self.did.append((_n, a, k)) or True)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = mock.patch.object(skill_task, "_engine_can_drive", return_value=True)
        patcher.start()
        self.addCleanup(patcher.stop)

    def reply(self, text, reading=None):
        with mock.patch.object(skill_task.approval, "read_choice", wraps=approval.read_choice) as read:
            if reading is not None:
                read.side_effect = None
                read.return_value = reading
            with mock.patch("decurix_agent.llm_client.available", return_value=False):
                skill_task._stuck_reply(self.paused, text)
        return self.did[-1][0]

    def test_plain_replies_do_what_they_say(self):
        self.assertEqual(self.reply("stop"), "_stop")
        self.assertEqual(self.reply("skip"), "_execute")
        self.assertEqual(self.run["start"], 4)
        self.assertEqual(self.reply("retry"), "_execute")
        self.assertEqual(self.run["start"], 3)

    def test_a_choice_with_more_to_it_goes_to_the_model_with_their_words(self):
        self.assertEqual(self.reply("retry but use the second file",
                                    {"choice": "retry", "instruction": "use the second file"}), "_by_model")
        self.assertEqual(self.did[-1][2]["from_step"], 3)
        self.assertIn("retry but use the second file", self.did[-1][2]["note"])
        self.assertEqual(self.reply("skip 3, do 4 with the new rate",
                                    {"choice": "carry_on", "instruction": "do 4 with the new rate"}), "_by_model")
        self.assertEqual(self.did[-1][2]["from_step"], 4)

    def test_no_with_another_control_is_not_a_stop(self):
        self.assertEqual(self.reply("no, use option 2", {"choice": "use", "instruction": "option 2"}), "_execute")
        self.assertEqual(self.did[-2][0], "_retarget")
        self.assertEqual(self.did[-2][1][2], "option 2")


class ADoneThatIsNotDone(unittest.TestCase):
    """The reply to "I cannot confirm this is done": a correction is never a person's confirmation."""

    def test_a_correction_is_tried_again_with_their_words(self):
        paused = {"run": None, "claim": "Entered both.", "verdict": {"seen": "one row is missing"}, "task": "enter both"}
        with mock.patch.object(skill_task.approval, "read_choice",
                               return_value={"choice": "correction", "instruction": "fix the total"}), \
                mock.patch.object(skill_task, "_engine_can_drive", return_value=True), \
                mock.patch.object(final_gate, "_retry_engine") as retry, \
                mock.patch.object(skill_task, "write_final_reply") as final:
            skill_task._confirm_reply(paused, "correct except the total, fix that")
        final.assert_not_called()
        self.assertIn("correct except the total, fix that", retry.call_args[0][1])


if __name__ == "__main__":
    unittest.main()


class WhatThePersonSaidReachesTheFinalCheck(unittest.TestCase):
    """Found live (2026-10-25): told "every bill must have its rows (454 two rows)", the worker reported done with one
    row and the final check, shown only the request, passed it."""

    def setUp(self):
        # (This task's words alone; the earlier tasks' are TheEarlierTasksWords'.)
        from decurix_agent import guidance
        patcher = mock.patch.object(guidance, "said_on_earlier_tasks", return_value="")
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_the_tasks_questions_and_answers_go_with_the_request(self):
        from decurix_agent import guidance
        with mock.patch.object(guidance.task_context, "current", return_value="t1"):
            guidance._exchanges.update(task=None, said=[])
            self.assertEqual(skill_task._with_said("Do the month"), "Do the month")
            guidance.note_exchange("Before I report this as done: 1. bill 454 ...", "454 two rows, one per rate")
            judged = skill_task._with_said("Do the month")
            self.assertTrue(judged.startswith("Do the month\n\nWhat the person said during this task"))
            self.assertIn("they answered: 454 two rows, one per rate", judged)
        with mock.patch.object(guidance.task_context, "current", return_value="t2"):
            self.assertEqual(skill_task._with_said("Another"), "Another")   # another task's words never go with it

    def test_a_taught_runs_check_and_its_second_attempt_are_given_the_answers(self):
        # Found live (2026-10-31, T7): the client gave a bill's right GSTIN in answer to the worker's question; the
        # check, shown only the request, failed the bill for not using the printed one, and the second attempt, shown
        # the check's note but not the answer, undid the correction.
        from decurix_agent import guidance
        run = {"skill": {"name": "gstr1", "title": "GSTR-1", "steps": [{"n": 1, "summary": "open the portal"}]},
               "values": {}, "context": "Prepare September's GSTR-1"}
        answer = "The printed GSTIN has two digits swapped - use 29ADLFB1520R1ZW"
        with mock.patch.object(guidance.task_context, "current", return_value="t7"):
            guidance._exchanges.update(task=None, said=[])
            guidance.note_exchange("Invoice 792's GSTIN fails its check digit - which is right?", answer)
            with mock.patch.object(skill_task.verify, "judge", return_value={"verdict": "done"}) as judge, \
                    mock.patch.multiple(skill_task, _criteria=mock.DEFAULT, _rows_note=mock.DEFAULT,
                                        _chosen_for=mock.DEFAULT) as helpers, \
                    mock.patch.multiple(skill_task.evidence, live=mock.DEFAULT, final_frame=mock.DEFAULT,
                                        observed=mock.DEFAULT, live_text=mock.DEFAULT):
                helpers["_criteria"].return_value, helpers["_rows_note"].return_value = "", ""
                helpers["_chosen_for"].return_value = ()
                skill_task._judge(run, "done")
            task = judge.call_args[0][0]
            self.assertTrue(task.startswith("Prepare September's GSTR-1"))
            self.assertIn(answer, task)
            self.assertNotIn(answer, skill_task._engine_request(run))   # a first attempt: the request as it was
            run.update(attempt_note="the record uses a GSTIN other than the printed one", attempt_report="done")
            request = skill_task._engine_request(run)
            self.assertIn(answer, request)
            self.assertIn("stand over the check's note", request)
            self.assertEqual(skill_task._with_said(request).count(answer), 1)   # never twice


class TheEarlierTasksWords(unittest.TestCase):
    """Found live (2026-11-08, a reconciliation): the final check, shown none of what the person taught on an earlier
    task ("one row per statement line"), failed a file laid out that way for lacking a row they never asked for; the
    worker added it, and the second check passed the wrong file."""

    RULE = "One row per statement line; a book entry the statement does not show gets no row."

    def rows(self, task="t-current"):
        from decurix_agent import person_record
        return [
            {"id": "j_aaaaaaaaaaaaaaaa", "task_id": "t-earlier", "kind": "answer", "actor": {"kind": "person"},
             "text": self.RULE, "asked": "How should the reconciliation be laid out?", "derived_from": "", "secret": False},
            {"id": "j_bbbbbbbbbbbbbbbb", "task_id": task, "kind": "answer", "actor": {"kind": "person"},
             "text": "this task's own answer", "asked": "q", "derived_from": "", "secret": False},
        ], person_record

    def test_they_go_to_the_check_with_their_questions(self):
        from decurix_agent import guidance
        rows, person_record = self.rows()
        person_record.clear_cache()
        with mock.patch.object(guidance.task_context, "current", return_value="t-current"), \
                mock.patch.object(person_record.task_context, "current", return_value="t-current"), \
                mock.patch.object(person_record, "_get", return_value={"judgements": rows}):
            guidance._exchanges.update(task=None, said=[])
            judged = skill_task._with_said("Reconcile the bank book")
        self.assertIn("What the person told this worker on its earlier tasks", judged)
        self.assertIn("asked: How should the reconciliation be laid out? - they answered: " + self.RULE, judged)
        self.assertIn("holds on this task too", judged)
        self.assertNotIn("this task's own answer", judged)   # this task's are said_this_task's

    def test_none_or_an_unreadable_control_plane_adds_nothing(self):
        from decurix_agent import guidance, person_record
        person_record.clear_cache()
        with mock.patch.object(person_record, "_get", return_value=None):
            self.assertEqual(guidance.said_on_earlier_tasks(), "")
        with mock.patch.object(person_record, "worker_said", side_effect=OSError("down")):
            self.assertEqual(guidance.said_on_earlier_tasks(), "")

    def test_a_workers_own_row_is_not_the_persons(self):
        from decurix_agent import guidance, person_record
        rows, _ = self.rows()
        rows[0]["actor"] = {"kind": "worker"}
        person_record.clear_cache()
        with mock.patch.object(person_record, "_get", return_value={"judgements": rows}), \
                mock.patch.object(person_record.task_context, "current", return_value="t-current"):
            self.assertEqual(guidance.said_on_earlier_tasks(), "")
