"""
Review round 9's rework (REVIEW-PY-R9, 2026-11-05): each class fails on the code before.

- R9-B1: only a real identifier match backs a pick - a structured fact core.act records, never an action's summary
  words (a plain pick on a custom dropdown wrote "took the entry X", and the worker's guess backed itself).
- R9-B2: only selects into a form the task then saved are picks; a list's filter, pager or sort order is not.
- The words rule: an option the request names in another form is backed when it is picked out among what the list
  offered; a person-authored source (an answer, a taught skill, the organisation's knowledge, a kept answer about the
  field) backs it too.
- R9-B3: one shared check (ledger_checks.findings) on every route that can say done: the general gate, a planned
  run's final report and its stage check, a taught skill's _succeed, a replay.
- R9-B4: a check that could not be made, then a reply that settles nothing, never runs the work again - on any route;
  after the bounded rechecks the task ends not checked; such a reply is never a lesson; with no declaration a second
  save of one record is still found by its own number field.
- The decisions: fields added to a saved record's declaration; "a record was made that no declaration covers"; the
  duplicate block on the record's own key field only; C's false questions (a search's echo, --from screen); the
  person's answers let go with the task's other secret stores.
"""

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import config_stub  # noqa: F401

import test_skills as ts  # noqa: E402
import test_watcher_restart  # noqa: E402
from test_long_requests import LONG, LongRunBase  # noqa: E402
from test_round9_false_done import NOT_BACKED_REPORT, TASK, Ledger  # noqa: E402

from decurix_agent import (guidance, ledger_checks, lessons, long_task, replay_task, skill_task, state,  # noqa: E402
                           task_context, verify, watchers)
from decurix_agent.computer import approval, audit, core, evidence, privacy, records  # noqa: E402
from decurix_agent.computer.errors import ComputerError  # noqa: E402

NEW = {"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"}
SAVE = {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/3"}


def pick(option, label="Cost centre", **extra):
    return dict({"kind": "select", "outcome": "done", "option": option, "element": {"name": label},
                 "url": "http://app/bills/new"}, **extra)


def saved_form(*selects):
    return [NEW, *selects, SAVE]


# ------------------------------------------------------------------ R9-B1

class B1_OnlyAnIdentifierBacksAPick(Ledger):
    def test_a_plain_pick_on_a_custom_dropdown_backs_nothing(self):
        # What core.act records for a plain pick on a dropdown the page drew itself: its summary names the entry taken.
        plain = pick("General", backend="cdp:custom", taken="General",
                     summary='Selected "General" in Cost centre - took the entry "General" for "General" - shown')
        self.events = saved_form(plain)
        self.assertEqual(records.resolved_names(TASK), [])
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        with self.assertRaises(ComputerError):
            self.declare({"Bill no.": "INV-0001", "Cost centre": "General"})

    def test_an_identifier_match_backs_its_pick(self):
        # (Pins review round 9's surviving mutation "a resolved party never backs its pick".)
        party = pick("Example Vendor", label="Vendor", backend="cdp:custom", taken="Example Vendor Pvt Ltd",
                     resolved_by="identifier")
        self.events = saved_form(party)
        self.assertEqual(records.resolved_names(TASK), ["Example Vendor Pvt Ltd"])
        self.assertEqual(records.unbacked_picks(TASK), [])
        rec, _ = self.declare({"Bill no.": "INV-0001", "Vendor": "Example Vendor Pvt Ltd"})
        self.assertEqual(rec["fields"]["Vendor"]["from"], "chosen")


class B1_TheActionRecordsTheFact(unittest.TestCase):
    """core.act writes who chose the entry and what the list offered as structure."""

    FIELD = {"role": "combobox", "name": "Cost centre", "source": "cdp", "ref": "e3", "path": "site x > combobox",
             "box": [10, 10, 40, 20], "states": [], "group": "g", "window_title": "Site"}

    def entry_of(self, selected):
        page = {"elements": [self.FIELD], "frame": None, "windows": [], "size": [1280, 720], "active_title": "Site",
                "groups": [{"id": "g", "source": "cdp", "url": "https://site.example/new", "window_title": "Site"}]}
        kept = []

        def perform(*a, **k):
            core._selected.update(selected)
            return "cdp:custom"
        patches = [mock.patch.object(core, "observe", side_effect=lambda *a, **k: dict(page)),
                   mock.patch.object(core, "_perform", side_effect=perform),
                   mock.patch.object(core, "_wait_for_settle", return_value=None),
                   mock.patch.object(audit, "record", side_effect=lambda entry, **k: kept.append(dict(entry))),
                   mock.patch.object(core, "_downloads_snapshot", return_value={}),
                   mock.patch.object(core.cdp, "submit_form_fields", return_value=None),
                   mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False))]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        try:
            core.act("select", ref="e3", option="General")
        except Exception:  # noqa: BLE001 - only the record of the action matters here
            pass
        for p in patches:
            p.stop()
        self.assertTrue(kept, "the action was recorded")
        return kept[-1]

    def test_a_plain_pick_says_no_identifier_and_keeps_the_offer(self):
        entry = self.entry_of({"name": "General", "why": "an entry of the field's own list", "by_id": False,
                               "offered": ["General", "Marketing"]})
        self.assertNotIn("resolved_by", entry)
        self.assertEqual(entry.get("taken"), "General")
        self.assertEqual(entry.get("offered"), ["General", "Marketing"])

    def test_an_identifier_match_says_so(self):
        entry = self.entry_of({"name": "Example Vendor Pvt Ltd", "why": "matched by identifier 29AAAAA0000A1Z5",
                               "by_id": True, "offered": None})
        self.assertEqual(entry.get("resolved_by"), "identifier")
        self.assertNotIn("offered", entry)


class B1_TheSelectsSetIt(unittest.TestCase):
    ELEMENT = {"role": "combobox", "name": "Vendor", "source": "cdp", "handle": {"backend_id": 1}}

    def test_a_native_select_by_identifier_and_a_plain_one(self):
        labels = ["Example Vendor Pvt Ltd (29AAAAA0000A1Z5)", "Other Supplier (27BBBBB1111B1Z5)"]
        with mock.patch.object(core.cdp, "native_options", lambda element: list(labels)), \
                mock.patch.object(core.cdp, "select_option", lambda element, option: option), \
                mock.patch("decurix_agent.computer.resolve.note"):
            core._selected.update(name=None, why="", by_id=False, offered=None)
            self.assertEqual(core._select_native_by_id(self.ELEMENT, "Example Vendor", ["29AAAAA0000A1Z5"]), "cdp")
            self.assertTrue(core._selected["by_id"])
            self.assertEqual(core._selected["offered"], labels)

    def test_a_datalist_pick_by_name_is_no_identifiers(self):
        with mock.patch.object(core.cdp, "datalist_options", lambda element: ["General", "Marketing"]), \
                mock.patch.object(core.cdp, "type_text", lambda element, text: None):
            core._selected.update(name=None, why="", by_id=False, offered=None)
            core._resolved.clear()
            self.assertEqual(core._select_datalist(self.ELEMENT, "General"), "cdp")
            self.assertFalse(core._selected["by_id"])
            self.assertEqual(core._selected["offered"], ["General", "Marketing"])


# ------------------------------------------------------------------ R9-B2

class B2_OnlyASavedFormsSelectsArePicks(Ledger):
    def test_a_filter_and_a_pager_on_a_list_page_are_not_picks(self):
        self.request = "Enter bill.pdf in the books, charged to General"
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills"},
                       pick("Unpaid", "Status filter", url="http://app/bills"),
                       dict(pick("50", "Rows per page", url="http://app/bills"),
                            change="page is now http://app/bills?per=50"),
                       dict(pick("2", "Page", url="http://app/bills"), change="page is now http://app/bills/page/2"),
                       NEW, pick("General"), SAVE]
        self.assertEqual([p["option"] for p in records.picks(TASK)], ["General"])
        self.assertEqual(records.unbacked_picks(TASK), [])
        self.assertEqual(ledger_checks.faults(ledger_checks.findings(TASK)), [])

    def test_a_filter_before_an_existing_records_edit_page_is_not_one(self):
        # (No new form opens between them: leaving the list page is what lets its filter go.)
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills"},
                       pick("Unpaid", "Status filter", url="http://app/bills"),
                       {"kind": "click", "outcome": "done", "change": "page is now http://app/bills/3/edit"},
                       pick("General", url="http://app/bills/3/edit"),
                       {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/3"}]
        self.assertEqual([p["option"] for p in records.picks(TASK)], ["General"])

    def test_a_select_into_the_saved_form_is_one(self):
        self.events = saved_form(pick("General"))
        self.assertEqual([p["option"] for p in records.picks(TASK)], ["General"])
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_a_form_left_unsaved_has_no_picks_and_the_open_one_counts_only_for_a_declaration(self):
        self.events = [NEW, pick("General"), {"kind": "click", "outcome": "done", "change": "page is now http://app/x"}]
        self.assertEqual(records.picks(TASK), [])
        self.events = [NEW, pick("General")]
        self.assertEqual(records.picks(TASK), [])
        self.assertEqual([p["option"] for p in records.picks(TASK, open_form=True)], ["General"])


# ------------------------------------------------------------------ what backs a pick

class APickNamedInAnotherForm(unittest.TestCase):
    def test_the_requests_words_pick_it_out_among_what_was_offered(self):
        backed = records.option_backed
        self.assertTrue(backed("Project Cedar", "Enter it, charge it to Cedar", ["Project Maple", "Project Cedar"]))
        self.assertTrue(backed("Project Cedar", "under the Cedar project", ["Project Maple", "Project Cedar"]))
        self.assertFalse(backed("Project Cedar", "the project bill", ["Project Maple", "Project Cedar"]))
        self.assertTrue(backed("29-Karnataka", "place of supply Karnataka", ["29-Karnataka", "32-Kerala"]))
        self.assertTrue(backed("Karnataka (29)", "place of supply Karnataka"))  # (a code in brackets, list unread)
        self.assertTrue(backed("GST 18%", "tax at 18%", ["GST 5%", "GST 12%", "GST 18%"]))
        self.assertFalse(backed("Sales @ 18%", "tax at 18%", ["Sales @ 18%", "Purchase @ 18%"]))
        self.assertFalse(backed("GST 18%", "Qty 18, rate 250", ["GST 5%", "GST 12%", "GST 18%"]))  # a bare figure
        self.assertTrue(backed("General", "cost centre: general"))

    def test_with_the_list_unread_every_word_must_be_given(self):
        self.assertFalse(records.option_backed("Project Cedar", "charge it to Cedar"))
        self.assertTrue(records.option_backed("Project Cedar", "charge it to Cedar Project"))


class APersonAuthoredSourceBacksAPick(Ledger):
    def setUp(self):
        super().setUp()
        self.events = saved_form(pick("General"))

    def test_a_taught_skill_and_the_organisations_knowledge(self):
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        with mock.patch.object(guidance, "taught_words", lambda: '{"answers": [{"cost centre": "General"}]}'):
            self.assertEqual(records.unbacked_picks(TASK), [])
        with mock.patch.object(records, "_org_text", lambda: "Rule (org): bills of this kind go to cost centre General"):
            self.assertEqual(records.unbacked_picks(TASK), [])

    def test_a_persons_answer_about_the_field_is_kept_for_that_field(self):
        asked = [("Which cost centre should the bill go to?", "General, please.")]
        with mock.patch.object(guidance, "exchanges", lambda task_id=None: asked):
            self.said = "General, please."
            self.assertEqual(ledger_checks.faults(ledger_checks.findings(TASK)), [])
        kept = records.field_answers()
        self.assertEqual([(a["label"], a["option"], a["host"]) for a in kept], [("Cost centre", "General", "app")])
        # The next task: nothing said, nothing in the request - the kept answer backs the same field, in that app only.
        self.said = ""
        self.assertEqual(records.unbacked_picks(TASK), [])
        self.events = saved_form(pick("General", label="Department"))
        self.assertEqual(records.unbacked_picks(TASK), ["General"])
        self.events = saved_form(pick("General", url="http://other-app/bills/new"))
        self.assertEqual(records.unbacked_picks(TASK), ["General"])

    def test_an_answer_to_another_question_is_not_kept_for_the_field(self):
        asked = [("Is the bill dated 3 April?", "Yes - and General.")]
        with mock.patch.object(guidance, "exchanges", lambda task_id=None: asked):
            records.keep_field_answers(TASK)
        self.assertEqual(records.field_answers(), [])


# ------------------------------------------------------------------ R9-B3

class B3_TheSharedCheck(Ledger):
    def test_case_a_on_its_own_structure(self):
        rec, _ = self.declare({"Bill no.": "INV-0001", "Total": "3,540.00"})
        self.events = [NEW, {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}},
                       pick("General")]
        core._ledger_check([("Bill no.", "INV-0001"), ("Total", "3540.00"), ("Cost centre", "General")])
        records.readback(records.open_record(records.load()), "Bill INV-0001", "INV-0001 3,540.00 General", "saved")
        self.events.append(SAVE)
        found = ledger_checks.findings(TASK, NOT_BACKED_REPORT)
        self.assertEqual(sorted({f.kind for f in found}),
                         sorted({ledger_checks.UNDECLARED, ledger_checks.UNBACKED_PICK, ledger_checks.NOT_BACKED}))

    def test_a_check_that_raises_is_not_made(self):
        with mock.patch.object(records, "gate_faults", side_effect=OSError("torn")):
            found = ledger_checks.findings(TASK)
        self.assertEqual([f.kind for f in found], [ledger_checks.NOT_MADE])
        self.assertEqual(ledger_checks.faults(found), [])


class B3_APlannedRunWithCaseA(LongRunBase):
    def test_ends_not_done(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        events = [NEW, pick("General"), SAVE]
        with mock.patch.object(records, "LEDGER_DIR", Path(tmp.name)), \
                mock.patch.object(core, "_task_events_now", lambda: events), \
                mock.patch.object(evidence, "_task_events", lambda task_id=None: events), \
                mock.patch.object(records, "_org_text", lambda: ""):
            records._save(TASK, [{"key": "INV-0001", "saved_at": 1.0, "declared_at": 1.0, "undeclared": ["Cost centre=General"],
                                  "fields": {"Bill no.": {"intent": "INV-0001", "from": "source"}},
                                  "readback": {"seen": ["Bill no."], "absent": []}}])
            long_task.perform(LONG, None)
        report = self.finals()[-1]["reply"]
        self.assertIn("Not everything is done", report)
        self.assertIn("Cost centre=General", report)
        self.assertIn("'General' was chosen from the application's own list", report)


class B3_ATaughtSkillsRunAndAReplay(ts.TeachingBase):
    def test_a_runs_read_back_does_not_pass_over_a_ledger_fault(self):
        sent = []
        run = {"skill": {"name": "enter-bill", "steps": []}, "values": {}}
        fault = records.finding(ledger_checks.UNDECLARED, "record INV-0001 was saved with Cost centre=General")
        with mock.patch.object(skill_task, "_read_back", lambda run, text: ([], [])), \
                mock.patch.object(skill_task, "_ledger_faults", lambda reply="": [fault]), \
                mock.patch.object(skill_task, "_own_run"), mock.patch.object(skill_task, "_record_verdict"), \
                mock.patch.object(skill_task, "write_final_reply",
                                  lambda text, kind="final", engine=None, **k: sent.append((text, kind, k))):
            skill_task._succeed(run, {"text": "Saved INV-0001."}, "model")
        self.assertEqual(len(sent), 1)
        self.assertTrue(sent[0][0].startswith("Not done"), sent[0][0])
        self.assertIn("Cost centre=General", sent[0][0])

    def test_a_verified_replay_is_held_back_too_but_not_for_its_picks(self):
        sent, asked_picks = [], []

        def findings(task_id=None, reports=(), events=None, picks=True):
            asked_picks.append(picks)
            return [records.finding(ledger_checks.SAVED_TWICE, "record INV-0001 was saved 2 times")]
        result = {"outcome": replay_task.replay.VERIFIED, "text": "Replayed: saved INV-0001."}
        with mock.patch.object(ledger_checks, "findings", findings), \
                mock.patch.object(replay_task.replay, "run", lambda program, params=None, start=None: dict(result)), \
                mock.patch.object(skill_task, "write_final_reply",
                                  lambda text, kind="final", engine=None, **k: sent.append((text, kind))), \
                mock.patch.object(replay_task, "write_final_reply",
                                  lambda text, kind="final", engine=None, **k: sent.append((text, kind))):
            replay_task._run({"session": "rec-1"}, {}, 0)
        self.assertEqual(asked_picks, [False])
        self.assertEqual(sent[-1][1], "error")
        self.assertIn("saved 2 times", sent[-1][0])


# ------------------------------------------------------------------ R9-B4

class B4_AReplyThatSettlesNothing(ts.TeachingBase):
    QUESTION = "I cannot confirm this is done - the check could not be made. Reply done, retry, or say what is wrong."

    def paused(self, run=None, why="the check could not be made (cut off)"):
        return {"mode": "confirm", "run": run, "claim": "Saved INV-0002.", "engine": "thebot", "task": "Enter bill.pdf",
                "verdict": {"verdict": verify.UNSURE, "seen": "", "why": why}}

    def answer_three_times(self, run=None):
        state.current_task, state.last_query_text = "Enter bill.pdf", self.QUESTION
        skill_task._rechecks.clear()
        out = {"questions": 1, "finals": []}  # (the first question, before the first reply)

        def write_final_reply(text, kind="final", engine=None, **k):
            if "checked" not in k:
                # The final gate again: the check could not be made again, and the person is asked again.
                out["questions"] += 1
                state.paused_skill = self.paused(run)
                return
            out["finals"].append((text, kind, k))
        with mock.patch.object(skill_task, "write_final_reply", write_final_reply), \
                mock.patch.object(skill_task, "_retry_engine", side_effect=AssertionError("the work was run again")), \
                mock.patch.object(skill_task, "_by_model", side_effect=AssertionError("the run was run again")), \
                mock.patch.object(skill_task, "_correct", side_effect=AssertionError("kept as a correction")), \
                mock.patch.object(skill_task.lessons, "note", side_effect=AssertionError("kept as a lesson")), \
                mock.patch.object(skill_task, "_done_reading",
                                  lambda reply, doubt="": {"choice": "unclear", "instruction": ""}), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True), \
                mock.patch.object(skill_task.guidance, "begin") as begun:
            state.paused_skill = self.paused(run)
            for _ in range(3):
                paused, state.paused_skill = state.paused_skill, None
                skill_task._confirm_reply(paused, "I don't know that.")
        return out, begun

    def test_three_questions_then_an_unchecked_final_and_no_run(self):
        out, _ = self.answer_three_times()
        self.assertEqual(out["questions"], 3)
        self.assertEqual(len(out["finals"]), 1)
        text, kind, k = out["finals"][0]
        self.assertFalse(k.get("checked"))
        self.assertTrue(text.startswith("Not checked"), text)
        self.assertIn(self.QUESTION[:40], text)

    def test_a_taught_skills_run_too(self):
        run = {"skill": {"name": "enter-bill", "steps": []}, "values": {}, "attempts": 1}
        out, begun = self.answer_three_times(run)
        self.assertEqual(out["questions"], 3)
        self.assertEqual(len(out["finals"]), 1)
        self.assertEqual(begun.call_count, 2)  # (the run handed back to the gate for each recheck)
        self.assertIs(begun.call_args.kwargs["run"], run)

    def test_the_protocols_own_retry_still_runs_it_again(self):
        retried = []
        state.current_task = "Enter bill.pdf"
        skill_task._rechecks.clear()
        with mock.patch.object(skill_task, "_retry_engine", lambda guided, note: retried.append(note)), \
                mock.patch.object(skill_task, "_done_reading",
                                  lambda reply, doubt="": {"choice": "redo", "instruction": ""}), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True):
            skill_task._confirm_reply(self.paused(), "retry")
        self.assertEqual(len(retried), 1)

    def test_a_check_that_looked_and_doubted_takes_an_empty_reply_as_their_words(self):
        # (Pins review round 9's surviving mutation "recheck for any doubt".)
        retried = []
        state.current_task = "Enter bill.pdf"
        skill_task._rechecks.clear()
        with mock.patch.object(skill_task, "_retry_engine", lambda guided, note: retried.append(note)), \
                mock.patch.object(skill_task, "write_final_reply", side_effect=AssertionError("checked again")), \
                mock.patch.object(skill_task, "_done_reading",
                                  lambda reply, doubt="": {"choice": "unclear", "instruction": ""}), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True):
            skill_task._confirm_reply(self.paused(why="the total is not on the page"), "I don't know that.")
        self.assertEqual(retried, ["the person said: I don't know that."])


class B4_TheWatcherKeepsNoLesson(test_watcher_restart.Base):
    def test_a_reply_to_a_check_not_made_is_not_noted_as_a_lesson(self):
        state.waiting_for_query = True
        state.current_task_id = TASK
        state.current_task = "Enter bill.pdf"
        self.addCleanup(setattr, state, "last_query_text", getattr(state, "last_query_text", ""))
        self.addCleanup(setattr, state, "last_query_kind", getattr(state, "last_query_kind", ""))
        state.last_query_text, state.last_query_kind = "I cannot confirm this is done.", "query"
        state.paused_skill = {"mode": "confirm", "run": None, "claim": "Saved.", "engine": "thebot",
                              "verdict": {"verdict": verify.UNSURE, "why": "the check could not be made (cut off)"}}
        path = self.command_dir / f"20261105_101500_ab12cd34-reply.{TASK}.txt"
        path.write_text("I don't know that.", encoding="utf-8")
        with mock.patch.object(lessons, "note") as noted:
            watchers.answer_waiting(path, "I don't know that.")
        noted.assert_not_called()
        self.assertEqual(FakeStarted.target(), skill_task.resume)


class FakeStarted:
    @staticmethod
    def target():
        return test_watcher_restart.FakeThread.started[-1][0] if test_watcher_restart.FakeThread.started else None


class B4_ASecondSaveWithNoDeclaration(Ledger):
    def test_is_found_by_its_own_number_field(self):
        twice = [NEW, {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}}, SAVE,
                 NEW, {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}},
                 {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/4"}]
        self.assertEqual(core.saved_twice(twice), {"INV-0001": 2})
        self.events = twice
        found = ledger_checks.findings(TASK)
        self.assertEqual([f.kind for f in found], [ledger_checks.SAVED_TWICE])
        # Two bills, each its own number: two records, nothing found.
        self.events = [e if e.get("text") != "INV-0001" or i < 3 else dict(e, text="INV-0002")
                       for i, e in enumerate(twice)]
        self.assertEqual(ledger_checks.findings(TASK), [])
        # An edit of the saved record on its own page is no new record.
        self.assertEqual(core.saved_twice(twice[:3] + [
            {"kind": "click", "outcome": "done", "change": "page is now http://app/bills/3/edit"},
            {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}},
            {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/3"}]), {})


# ------------------------------------------------------------------ the decisions

class TheDecisions(Ledger):
    def saved(self, fields, key="INV-0001", page="INV-0001 3,540.00"):
        rec, _ = self.declare(fields, key=key)
        records.readback(rec, f"Bill {key}", page, "saved; page is now http://app/bills/3")
        return rec

    def test_fields_can_be_added_to_a_saved_records_declaration(self):
        self.saved({"Bill no.": "INV-0001"})
        records._save(TASK, [dict(r, undeclared=["Total=3540.00"]) for r in records.load(TASK)])
        self.looks = [evidence.LOOK_LEAD + ". What every field of the page held (in view or not):\n(none)\nThe words "
                      "the page showed outside its fields, in its order (in view or not):\nBill INV-0001 | Total "
                      "3,540.00\nThe part in view, as the tool listed it:\n..."]
        rec, lines = self.declare({"Bill no.": "INV-0001", "Total": "3,540.00"})
        self.assertIn("now also covers: Total=3,540.00", lines[0])
        self.assertEqual(rec["undeclared"], [])
        self.assertIn("Total", rec["readback"]["seen"])
        self.assertTrue(rec["saved_at"])
        self.assertEqual(records.gate_faults(TASK), [])
        # Nothing new, or a field given otherwise: refused - a second save would make a second record.
        with self.assertRaises(ComputerError) as caught:
            self.declare({"Bill no.": "INV-0001"})
        self.assertIn("was saved in this task already", str(caught.exception))

    def test_a_new_party_saved_with_no_declaration_is_said_as_that(self):
        self.saved({"Bill no.": "INV-0001"})
        found = records.gate_faults(TASK, created=2)
        self.assertEqual([f.kind for f in found], [ledger_checks.UNCOVERED])
        self.assertIn("a record was made that no declaration covers", found[0])
        self.assertNotIn("twice", found[0])

    def test_a_payment_naming_the_saved_bill_is_pressed(self):
        self.saved({"Bill no.": "INV-0001"})
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/payments/new"},
                       {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Against bill"}}]
        held = core._ledger_check([("Against bill", "INV-0001"), ("Amount", "3540.00"), ("Mode", "Bank")])
        self.assertIsNone(held)
        # The bill's own form again, its number in its own field: not pressed.
        self.events = [NEW, {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}}]
        held = core._ledger_check([("Bill no.", "INV-0001"), ("Total", "3540.00")])
        self.assertIn("was saved in this task already", held[0])

    def test_a_search_that_found_nothing_is_not_the_application_holding_it(self):
        self.looks = [evidence.LOOK_LEAD + ". What every field of the page held (in view or not):\nsearchbox Search: "
                      "INV-0001\nThe words the page showed outside its fields, in its order (in view or not):\nBills | "
                      "No bills match “INV-0001”\nThe part in view, as the tool listed it:\n..."]
        rec, _ = self.declare({"Bill no.": "INV-0001"})
        self.assertEqual(rec["key"], "INV-0001")

    def test_from_screen_the_page_read_is_the_source(self):
        self.looks = [evidence.LOOK_LEAD + ". What every field of the page held (in view or not):\n(none)\nThe words "
                      "the page showed outside its fields, in its order (in view or not):\nInbox | From: Example Vendor "
                      "| Bill INV-0001 attached, total 3,540.00\nThe part in view, as the tool listed it:\n..."]
        rec, _ = records.declare("INV-0001", "screen", {"Bill no.": "INV-0001"})
        self.assertEqual(rec["key"], "INV-0001")


class ThePersonsAnswersOnDisk(unittest.TestCase):
    def test_kept_with_the_question_and_let_go_with_the_tasks_secrets(self):
        tid = "t00000000000000ab"
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(evidence, "NOTES_DIR", Path(tmp)), \
                mock.patch("decurix_agent.task_context.current", lambda: tid):
            guidance.note_exchange("Which cost centre?", "Charge it to General.")
            said = Path(tmp) / f"{tid}.said"
            self.assertEqual(json.loads(said.read_text(encoding="utf-8").splitlines()[0]),
                             {"asked": "Which cost centre?", "reply": "Charge it to General."})
            self.assertEqual(guidance.exchanges(tid), [("Which cost centre?", "Charge it to General.")])
            privacy.clear_literals(tid)
            self.assertFalse(said.exists())
            self.assertEqual(guidance.answers(tid), "")


if __name__ == "__main__":
    unittest.main()
