"""
A taught run whose only fault is entries picked from the application's lists that nothing given backs asks the person
about them once, as an engine's report does, and a plain yes to that question is taken as the answer it asks for.

Found live (2026-11-10, books curve on 83f4a53d): every bill a taught replay entered right ended "Not done" over the
customers picked from the ledger list (the bills write them under "Customer Name and Address", the form's field is
"Party A/c name"); the person was never asked, so no confirmation was kept for the next month. And in review: the
picks question says "Reply yes if each is right", but the confirm reading's vague-yes guard took a plain yes first
and asked for `done`, so the confirmation was never kept on the engine's route either.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

import test_skills as ts  # noqa: E402

from decurix_agent import final_gate, ledger_checks, skill_task, state
from decurix_agent.computer import records

PICK = records.finding(ledger_checks.UNBACKED_PICK, "Party A/c name: 'Pai Textiles' was chosen from the application's "
                       "own list into a saved form, and nothing given backs it")


class AReplayAsksAboutItsPicks(ts.TeachingBase):

    def succeed(self, faults, run=None):
        sent, asked = [], []
        run = run if run is not None else {"skill": {"name": "enter-bills", "steps": []}, "values": {}}
        with mock.patch.object(skill_task, "_read_back", lambda run, text: ([], [])), \
                mock.patch.object(skill_task, "_ledger_faults", lambda reply="": list(faults)), \
                mock.patch.object(skill_task, "_own_run"), mock.patch.object(skill_task, "_record_verdict"), \
                mock.patch.object(final_gate, "_ask_about_picks",
                                  lambda run, reply, engine, guided: asked.append(reply) or True), \
                mock.patch.object(skill_task, "write_final_reply",
                                  lambda text, kind="final", engine=None, **k: sent.append((text, kind))):
            skill_task._succeed(run, {"text": "Replayed: entered 4 bills."}, "screen")
        return sent, asked

    def test_only_unbacked_picks_put_the_question_instead_of_an_error(self):
        sent, asked = self.succeed([PICK])
        self.assertEqual(asked, ["Replayed: entered 4 bills."])
        self.assertEqual(sent, [])

    def test_any_other_fault_still_ends_not_done(self):
        other = records.finding(ledger_checks.SAVED_TWICE, "record 1062 was saved 2 times")
        sent, asked = self.succeed([PICK, other])
        self.assertEqual(asked, [])
        self.assertTrue(sent[0][0].startswith("Not done"), sent[0][0])

    def test_asked_once_then_a_second_finding_ends_not_done(self):
        sent, asked = self.succeed([PICK], run={"skill": {"name": "enter-bills", "steps": []}, "values": {},
                                                "picks_asked": True})
        self.assertEqual(asked, [])
        self.assertTrue(sent[0][0].startswith("Not done"), sent[0][0])


class APlainYesAnswersThePicksQuestion(unittest.TestCase):

    def setUp(self):
        saved = state.paused_skill
        self.addCleanup(setattr, state, "paused_skill", saved)

    def test_yes_reaches_the_picks_reading_not_the_vague_yes_guard(self):
        paused = {"run": {"skill": {"name": "enter-bills"}, "values": {}}, "claim": "Entered.", "task": "enter",
                  "verdict": {"verdict": "unsure", "seen": "", "why": ""}, "picks_question": "Reply yes if each is right",
                  "picks": [{"label": "Party A/c name", "value": "Pai Textiles", "host": "books", "party": "P1"}]}
        with mock.patch.object(final_gate, "_picks_reply", return_value="picked") as picks, \
                mock.patch.object(final_gate, "ask_for_query") as asked:
            self.assertEqual(final_gate._confirm_reply(paused, "yes"), "picked")
        picks.assert_called_once()
        asked.assert_not_called()

    def test_a_plain_yes_to_the_done_question_is_still_held_as_vague(self):
        paused = {"run": {"skill": {"name": "enter-bills"}, "values": {}}, "claim": "Entered.",
                  "verdict": {"verdict": "unsure", "seen": "", "why": ""}}
        with mock.patch.object(final_gate, "ask_for_query") as asked:
            final_gate._confirm_reply(paused, "yes")
        self.assertIn("reply `done`", asked.call_args[0][0])


if __name__ == "__main__":
    unittest.main()
