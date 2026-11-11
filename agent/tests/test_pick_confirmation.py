"""
An entry picked from an application's list that nothing given backs is put to the person once, with what the documents
show; their yes backs exactly that entry for its record's own party - the identifiers that record's form holds - never
another party's record, never a different entry, and on a record that names no party, nothing beyond that record
(owner's rulings 2026-11-09: a run was sent back over "29-Karnataka" and "18" that every invoice supported; a
client-wide answer would back a wrong entry for another buyer). And where the skill's taught rules and the person's
later words are shown together, the later words hold.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import final_gate, skill_task, state
from decurix_agent.computer import records

ASKED = ("Before I report this, please confirm the entries I chose from the application's own lists:\n"
         "- Place of supply: 29-Karnataka\nReply yes if each is right for the record it is on.")
PICK = {"label": "Place of supply", "option": "29-Karnataka", "taken": "", "offered": None, "host": "taxnet"}
B1, B2 = "29AAACB1111K1Z5", "32AAACK2222L1Z9"  # (a Karnataka buyer and a Kerala buyer of the same client)
NONE = {"free": "", "files": []}


def kept_for(party):
    return [{"label": "Place of supply", "host": "taxnet", "option": "29-Karnataka", "party": party,
             "confirmed": True, "source_text": "yes", "source_asked": ASKED, "judgement": "j1"}]


def form_events(*forms):
    """A form per record: the values typed into it, then its pick of 29-Karnataka, then its save."""

    events = []
    for typed in forms:
        events += [{"kind": "type", "outcome": "done", "text": t, "element": {"name": "field"}} for t in typed]
        events += [{"kind": "select", "outcome": "done", "option": "29-Karnataka", "element": {"name": "Place of supply"}},
                   {"kind": "click", "outcome": "done", "change": "SAVED"}]
    return events


def picks_of(events):
    with mock.patch("decurix_agent.computer.core._saved", lambda change: change == "SAVED"), \
            mock.patch("decurix_agent.computer.core._FORM_START") as start, \
            mock.patch("decurix_agent.task_context.current", lambda: "t1111111111111111"):
        start.search.return_value = None
        return records.picks(events=events)


class EachRecordsOwnParty(unittest.TestCase):
    def setUp(self):
        records._confirmed_now.clear()
        self.addCleanup(records._confirmed_now.clear)

    def test_a_records_party_is_the_identifiers_its_form_holds(self):
        self.assertEqual(records.form_party(["1053", B1, "Pai Textiles", "29-Karnataka"]), B1)
        self.assertEqual(records.form_party(["1053", "Pai Textiles"]), "")

    def test_buyer_b1_confirmed_backs_b1_again_but_not_the_kerala_buyer_of_the_same_client(self):
        with mock.patch("decurix_agent.person_record.field_answers", return_value=kept_for(B1)):
            self.assertTrue(records.pick_backed(dict(PICK, party=B1), NONE))
            self.assertFalse(records.pick_backed(dict(PICK, party=B2), NONE))

    def test_a_row_kept_with_no_party_backs_no_partys_pick(self):
        # Review of freeze 2 (A12): a legacy row kept before answers carried their party backed every party's pick.
        with mock.patch("decurix_agent.person_record.field_answers", return_value=kept_for("")):
            self.assertFalse(records.pick_backed(dict(PICK, party=B1), NONE))
            self.assertFalse(records.pick_backed(dict(PICK, party=B2), NONE))
            self.assertTrue(records.pick_backed(dict(PICK, party=""), NONE))  # a pick with no party: as kept

    def test_a_different_entry_is_still_held(self):
        with mock.patch("decurix_agent.person_record.field_answers", return_value=kept_for(B1)):
            self.assertFalse(records.pick_backed(dict(PICK, party=B1, option="33-Tamil Nadu"), NONE))

    def test_a_yes_in_this_run_is_kept_for_each_records_own_party(self):
        with mock.patch("decurix_agent.person_record.keep_field_answer") as kept, \
                mock.patch("decurix_agent.person_record.field_answers", return_value=[]):
            records.confirm_picks("t1", "j1", [{"label": "Place of supply", "value": "29-Karnataka", "host": "taxnet",
                                                "party": B1}], ASKED)
            self.assertTrue(records.pick_backed(dict(PICK, party=B1), NONE))
            self.assertFalse(records.pick_backed(dict(PICK, party=B2), NONE))
        self.assertEqual(kept.call_args.kwargs["party"], B1)
        self.assertTrue(kept.call_args.kwargs["confirmed"])

    def test_the_same_entry_on_two_buyers_records_is_asked_for_each(self):
        self.assertEqual([p["party"] for p in picks_of(form_events(["1053", B1], ["1054", B2]))], [B1, B2])

    def test_two_records_that_name_no_party_are_each_their_own(self):
        # (A sale to a consumer has no buyer number: its place of supply is its own, invoice by invoice.)
        first, second = picks_of(form_events(["1053", "Walk-in customer"], ["1054", "Walk-in customer"]))
        self.assertNotEqual(first["party"], second["party"])
        with mock.patch("decurix_agent.person_record.keep_field_answer"), \
                mock.patch("decurix_agent.person_record.field_answers", return_value=[]):
            records.confirm_picks("t1", "j1", [{"label": "Place of supply", "value": "29-Karnataka", "host": "taxnet",
                                                "party": first["party"]}], ASKED)
            self.assertTrue(records.pick_backed(first, NONE))
            self.assertFalse(records.pick_backed(second, NONE), "a yes about one such record backs no other")
        # Nor does what was kept for it on the control plane back a later run's record.
        with mock.patch("decurix_agent.person_record.field_answers", return_value=kept_for(first["party"])):
            records._confirmed_now.clear()
            self.assertFalse(records.pick_backed(first, NONE))


class TheQuestion(unittest.TestCase):
    def setUp(self):
        saved = state.paused_skill
        self.addCleanup(setattr, state, "paused_skill", saved)

    def test_unbacked_picks_are_put_to_the_person_once_with_what_the_documents_show(self):
        run = {"skill": {"name": "file"}, "values": {}}
        asked = []
        with mock.patch.object(records, "unbacked_pick_entries", return_value=[dict(PICK, party=B1)]), \
                mock.patch.object(final_gate, "_pick_evidence", return_value=['"Karnataka 576101" (inv1.pdf)']), \
                mock.patch.object(final_gate, "ask_for_query", lambda text, **k: asked.append(text)):
            self.assertTrue(final_gate._ask_about_picks(run, "Filed.", "thebot", {"task": "file"}))
        self.assertTrue(run["picks_asked"])
        self.assertIn("- Place of supply: 29-Karnataka (for " + B1 + ")", asked[0])
        self.assertIn("Karnataka 576101", asked[0])
        self.assertEqual(state.paused_skill["picks"][0]["party"], B1)

    def test_a_yes_is_kept_and_the_result_checked_again_and_anything_else_is_help(self):
        paused = {"run": {"skill": {"name": "file"}, "values": {}}, "task": "file", "picks_question": ASKED,
                  "picks": [{"label": "Place of supply", "value": "29-Karnataka", "host": "taxnet", "party": B1}]}
        with mock.patch.object(records, "confirm_picks") as confirm, \
                mock.patch.object(final_gate, "write_final_reply") as wrote, \
                mock.patch("decurix_agent.guidance.begin"):
            final_gate._picks_reply(paused, "yes", "Filed.", "thebot")
        confirm.assert_called_once()
        wrote.assert_called_once()
        self.assertFalse(paused["run"].get("asked_person"))
        with mock.patch.object(records, "confirm_picks") as confirm, \
                mock.patch.object(skill_task, "_by_model") as by_model:
            final_gate._picks_reply(paused, "No - it should be 33-Tamil Nadu", "Filed.", "thebot")
        confirm.assert_not_called()
        by_model.assert_called_once()
        self.assertTrue(paused["run"]["asked_person"])


class LaterWordsHold(unittest.TestCase):
    def test_the_person_s_later_words_are_said_to_hold_over_a_taught_rule(self):
        skill = {"name": "file", "understanding": {"rules": ["Stop for approval before Proceed to file."]},
                 "run_answers": [{"q": "Plan?", "a": "Ask only before File with EVC and Verify.", "at": 1}]}
        with mock.patch("decurix_agent.org_knowledge.cached", return_value=[]):
            text = skill_task._taught(skill)
            plain = skill_task._taught({"name": "file", "understanding": {"rules": ["A rule."]}})
        self.assertIn(skill_task.LATER_WORDS_HOLD, text)
        self.assertLess(text.index("Rule: Stop"), text.index(skill_task.LATER_WORDS_HOLD))
        self.assertLess(text.index(skill_task.LATER_WORDS_HOLD), text.index("Ask only before"))
        self.assertNotIn(skill_task.LATER_WORDS_HOLD, plain)


if __name__ == "__main__":
    unittest.main()
