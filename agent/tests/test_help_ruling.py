"""
What counts as a person helping a taught run (owner's ruling 2026-11-10; final_gate._helped, engine_questions.
_required_every_run, note_approval). Found live on the GSTR-1 learning curve: every month asks for the OTP by design,
that answer made every run "helped", the skill never earned trust and every case ran on the model.

Not help - an answer the rules require on every run: an OTP (a value asked for a secret, given and nothing more), the
gate's yes for an approval, the plan's yes, a hole in the documents' own series confirmed, picked entries confirmed, the
date order. Help: a corrected value, "use X instead", data the documents should have held, a takeover, a save after
the run was sent back. Decided from which question was asked and whether the reply carried more - never a word list.
Values are made up.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import engine_questions, final_gate, guidance, skill_task, state, watchers
from decurix_agent.computer import approval, experience, records

OTP_QUESTION = "The portal sent a one-time password (OTP) to the client's registered mobile. What is the OTP?"
ASKED_PICKS = "Before I report this, please confirm the entries I chose from the application's own lists"


def a_run():
    return {"skill": {"name": "gstr1-file"}, "values": {}, "asked_ids": []}


def answered(question, reply, secret=False, reading=None):
    """The run after the engine's question `question` was answered `reply` (learn_answer, as the watcher calls it).
    `reading`: what the one model reading of the reply says, when the reply needs one."""

    run = a_run()
    seen = []
    real = approval.read_choice

    def read(text, asked, choices, plain=None):
        seen.append(text)
        if reading is None:  # (a plain phrase is decided in code; with no model anything else is "unclear")
            return real(text, asked, choices, plain=plain)
        return dict(reading)

    with mock.patch.object(guidance, "get", return_value={"run": run, "asked": question}), \
            mock.patch.object(skill_task.skills, "load", side_effect=FileNotFoundError), \
            mock.patch.object(approval, "read_choice", side_effect=read):
        skill_task.learn_answer(reply, secret=secret)
    run["_read"] = seen
    return run


def helped(run, mode="model"):
    with mock.patch.object(final_gate, "_reworked", return_value=False):
        return final_gate._helped(run, {"mode": mode})


class AnswersTheRulesRequire(unittest.TestCase):
    """Each leaves the run unhelped, so its verdict counts and the streak grows."""

    def test_an_otp_given_alone_is_not_help(self):
        # Today's code counted it as help: no taught skill whose task needs an OTP could ever be trusted.
        run = answered(OTP_QUESTION, "730146", secret=True)
        self.assertFalse(run.get("asked_person"))
        self.assertFalse(helped(run))
        self.assertEqual(run["_read"], [], "a bare code needs no model")

    def test_an_otp_with_where_it_came_from_is_not_help_and_the_code_never_reaches_the_model(self):
        run = answered(OTP_QUESTION, "The client read it out: the OTP is 730146.", secret=True,
                       reading={"choice": "value", "instruction": ""})
        self.assertFalse(run.get("asked_person"))
        self.assertEqual(len(run["_read"]), 1)
        self.assertNotIn("730146", run["_read"][0])

    def test_the_plans_yes_is_not_help(self):
        from decurix_agent.thebot_harness import PLAN_QUESTION
        run = answered(PLAN_QUESTION + "\n- Covering: the month's invoices\n- Then file with EVC", "yes")
        self.assertFalse(run.get("asked_person"))

    def test_the_gates_yes_for_an_approval_is_not_help(self):
        run = a_run()
        with mock.patch.object(guidance, "get", return_value={"run": run}), \
                mock.patch.object(state, "last_query_kind", "approval"):
            engine_questions.note_approval("granted")
        self.assertFalse(helped(run))

    def test_a_series_hole_confirmed_is_not_help(self):
        run = a_run()
        paused = {"run": run, "claim": "Filed the month.", "result": None, "engine": "thebot", "task": "file",
                  "verdict": {"verdict": "unsure", "confidence": 0.0, "seen": "1060 is missing", "why": ""},
                  "series": {"helped": False, "basis": ["ledger"]}}
        with mock.patch.object(skill_task, "_record_verdict") as recorded, \
                mock.patch.object(skill_task, "write_final_reply"), \
                mock.patch.object(final_gate, "_done_reading", return_value={"choice": "done", "instruction": ""}):
            final_gate._confirm_reply(paused, "done")
        self.assertFalse(run.get("asked_person"))
        self.assertFalse(recorded.call_args.kwargs["helped"])

    def test_picked_entries_confirmed_are_not_help(self):
        run = a_run()
        paused = {"run": run, "task": "file", "picks_question": ASKED_PICKS,
                  "picks": [{"label": "Place of supply", "value": "29-Karnataka", "host": "taxnet", "party": "B1"}]}
        with mock.patch.object(records, "confirm_picks"), mock.patch.object(final_gate, "write_final_reply"), \
                mock.patch("decurix_agent.guidance.begin"):
            final_gate._picks_reply(paused, "yes", "Filed.", "thebot")
        self.assertFalse(helped(run))

    def test_the_date_order_answer_is_not_help_though_an_engine_question_is_open(self):
        run = a_run()
        reply_to(self, run, "1", engine_asked="Which ledger should the sale go to?", date_order=True)
        self.assertFalse(run.get("asked_person"))
        self.assertFalse(helped(run))

    def test_runs_answered_only_so_earn_trust(self):
        skill = {"name": "gstr1-file"}
        from decurix_agent.thebot_harness import PLAN_QUESTION
        for run in (answered(OTP_QUESTION, "730146", secret=True),
                    answered(PLAN_QUESTION + "\n- Covering: the month", "go ahead"),
                    answered(OTP_QUESTION, "285913", secret=True)):
            experience.note_verdict(skill, "model", True, checked_by="model", helped=helped(run),
                                    basis=["model", "ledger"])
        self.assertEqual(experience.clean_streak(skill), 3)
        self.assertEqual(experience.stage(skill), experience.TRUSTED if experience.TRUST_AFTER <= 3
                         else experience.APPRENTICE)


class Help(unittest.TestCase):
    """Each makes the run helped: it does not count towards trust."""

    def test_a_corrected_value_is_help(self):
        run = answered("Which tax rate applies to invoice 1105?", "No - it is 12%, not 18%. Change it.")
        self.assertTrue(run["asked_person"])
        self.assertTrue(helped(run))

    def test_data_the_documents_should_have_held_is_help(self):
        run = answered("Invoice 1105 has no date. What date should it carry?", "3 March 2027")
        self.assertTrue(run["asked_person"])

    def test_more_than_the_code_in_a_reply_to_the_otp_question_is_help(self):
        run = answered(OTP_QUESTION, "OTP 730146 - and invoice 1105 is 5,000, not 4,000; fix it first", secret=True,
                       reading={"choice": "more", "instruction": "fix invoice 1105 first"})
        self.assertTrue(run["asked_person"])
        run = answered(OTP_QUESTION, "OTP 730146, and file it only after the client calls back", secret=True,
                       reading={"choice": "value", "instruction": "file only after the client calls back"})
        self.assertTrue(run["asked_person"])

    def test_a_secret_reply_nobody_may_read_or_no_model_to_read_it_is_help(self):
        with mock.patch("decurix_agent.computer.privacy.scrub", return_value="[withheld]"):
            run = answered(OTP_QUESTION, "use the other mobile number and skip invoice five", secret=True)
        self.assertTrue(run["asked_person"])
        run = answered(OTP_QUESTION, "the OTP is 730146", secret=True,
                       reading={"choice": "unclear", "instruction": ""})
        self.assertTrue(run["asked_person"])

    def test_a_value_given_without_the_secret_mark_is_help(self):
        # (A secret is the control plane's or soul's structure, never read off the words of the question.)
        self.assertTrue(answered(OTP_QUESTION, "730146", secret=False)["asked_person"])

    def test_use_x_instead_at_an_approval_is_help(self):
        # Today's code counted it as no help: a declined approval never reached learn_answer.
        run = a_run()
        reply_to(self, run, "No - use Save as draft instead", approval_outcome="declined")
        self.assertTrue(run["asked_person"])
        self.assertTrue(helped(run))

    def test_a_decline_read_off_a_reply_to_another_question_decides_nothing(self):
        run = a_run()
        with mock.patch.object(guidance, "get", return_value={"run": run}), \
                mock.patch.object(state, "last_query_kind", "query"):
            engine_questions.note_approval("declined")
        self.assertFalse(run.get("asked_person"))

    def test_use_x_instead_at_a_stuck_step_is_help(self):
        run = answered("Step 7 could not be done: the Submit button is not on the screen.", 'use "Save as" instead')
        self.assertTrue(run["asked_person"])

    def test_a_takeover_is_help_even_after_a_send_back_put_the_run_back_in_the_models_hands(self):
        run = a_run()
        self.assertTrue(helped(run, mode="takeover"))
        run["taken_over"] = True
        # Today's code saw only the guidance's mode, which a send-back sets to "model" again.
        self.assertTrue(helped(run, mode="model"))
        run = a_run()
        run["asked_ids"] = [7]  # (a stuck replay step put to the person)
        self.assertTrue(helped(run))

    def test_a_yes_to_start_with_a_condition_is_help(self):
        run = dict(a_run(), start=1)
        with mock.patch.object(approval, "reading", return_value={"decision": "if", "condition": "close the popup"}), \
                mock.patch.object(skill_task, "_engine_can_drive", return_value=True), \
                mock.patch.object(skill_task, "_by_model") as by_model:
            skill_task._run_reply({"run": run, "kind": "start"}, "yes, close the popup first", None)
        by_model.assert_called_once()
        self.assertTrue(run["asked_person"])

    def test_a_save_after_a_send_back_is_help(self):
        run = dict(a_run(), retried_at=1000.0)
        with mock.patch.object(records, "load", return_value=[{"saved_at": 1500.0}]):
            self.assertTrue(final_gate._helped(run, {"mode": "model"}))
        with mock.patch.object(records, "load", return_value=[{"saved_at": 500.0}]), \
                mock.patch.object(final_gate.evidence, "_task_events", return_value=[]):
            self.assertFalse(final_gate._helped(run, {"mode": "model"}), "a send-back that saved nothing is no help")


def reply_to(case, run, content, engine_asked=None, approval_outcome=None, date_order=False):
    """The watcher handing `content` on while the engine of `run` waits (watchers.answer_waiting), with the real
    learn_answer and note_approval."""

    path = mock.Mock()
    path.name = "20261110_101500_ab12cd34.reply.t0123456789abcdef.txt"
    saved = (state.waiting_for_query, state.last_query_secret, state.last_query_text, state.last_query_kind,
             state.current_task_id, state.paused_skill, state.paused_replay)

    def restore():
        (state.waiting_for_query, state.last_query_secret, state.last_query_text, state.last_query_kind,
         state.current_task_id, state.paused_skill, state.paused_replay) = saved
    case.addCleanup(restore)
    state.waiting_for_query, state.last_query_secret = True, False
    state.last_query_kind = "approval" if approval_outcome else "query"
    state.last_query_text = "Click Submit to portal?" if approval_outcome else "Which way is 03/04/2027 written?"
    state.current_task_id, state.paused_skill, state.paused_replay = "t0123456789abcdef", None, None
    guided = {"run": run, "asked": engine_asked}
    with mock.patch.object(watchers, "is_reply_file", return_value=True), \
            mock.patch.object(watchers, "_mark_handled"), \
            mock.patch("decurix_agent.retention.mark_labelled"), \
            mock.patch.object(watchers.workflow_task, "query_control", return_value=False), \
            mock.patch.object(watchers.workflow_task, "has_active", return_value=False), \
            mock.patch.object(watchers.workflow_task, "awaiting_start", return_value=False), \
            mock.patch.object(watchers.task_context, "date_query_answered", return_value=date_order), \
            mock.patch.object(watchers.task_context, "date_question", return_value={"value": "03/04/2027"}), \
            mock.patch.object(watchers.task_context, "take_date_answer", return_value="day"), \
            mock.patch("decurix_agent.unit_answers.query_answered", return_value=False), \
            mock.patch.object(watchers.approval, "note_reply"), \
            mock.patch.object(watchers.approval, "resolve_reply", return_value=approval_outcome), \
            mock.patch.object(watchers.lessons, "note"), \
            mock.patch.object(guidance, "get", return_value=guided), \
            mock.patch.object(guidance, "note_exchange"), mock.patch.object(guidance, "note_reply"), \
            mock.patch.object(guidance, "calls_off", return_value=False), \
            mock.patch.object(skill_task.skills, "load", side_effect=FileNotFoundError), \
            mock.patch.object(watchers.threading, "Thread"):
        watchers.answer_waiting(path, content)


if __name__ == "__main__":
    unittest.main()
