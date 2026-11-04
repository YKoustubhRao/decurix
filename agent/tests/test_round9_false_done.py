"""
False successes in an eval run (2026-11-04; review round 9). Each class is a mechanism the investigation
found, and a test that fails on the code before.

- A: a value no source gives (a cost centre the worker picked as a "default"), saved and passed as checked. The
  worker's own report marked it "| not backed"; presave only advised about the undeclared field; declare took the
  worker's own pick as its own backing; the judge passed it.
- B: a cut-off judge (max_tokens=600, a reasoning model) made the check not made; the person's "I don't know that"
  re-ran the whole task, and a second bill was saved outside the ledger, which still read back "every declared record".
- C: a record already in the application before the task was entered again.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

import test_skills as ts  # noqa: E402

from decurix_agent import (guidance, llm_client, long_task, person_record, skill_task, state, task_context,  # noqa: E402
                           verify)
from decurix_agent.computer import core, evidence, records  # noqa: E402
from decurix_agent.computer.errors import ComputerError  # noqa: E402

TASK = "t0123456789abcdef"
NOT_BACKED_REPORT = """Entered bill INV-0001 and saved it.

JUDGEMENTS:
- Bill date 03/04/2026 read as 3 April | backed by: "28/09/2026" in the bill
- Cost centre set to "General" | not backed: the source document gives no cost centre, but the form requires one
DONE: saved INV-0001
"""


def verdict(kind, seen="", why=""):
    return {"verdict": kind, "confidence": 0.95 if kind == verify.DONE else 0.5, "seen": seen, "why": why}


def save_events(*pages):
    """Recorded events: for each page, a form opened at .../new, a field typed, and Save answered as saved."""
    out = []
    for n, page in enumerate(pages):
        out += [{"kind": "click", "outcome": "done", "change": f"page is now http://app/{page}/new"},
                {"kind": "type", "outcome": "done", "text": f"INV-010{n}", "element": {"name": "Bill no."}},
                {"kind": "click", "outcome": "done", "change": f"saved; page is now http://app/{page}/{n + 3}?saved=1"}]
    return out


class Ledger(unittest.TestCase):
    """A ledger, a bill and the task's events, all this test's own."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bill = self.root / "bill.txt"
        self.bill.write_text("Example Vendor Ltd\nBill INV-0001\nTotal 3,540.00\n", encoding="utf-8")
        self.events, self.looks, self.request, self.said = [], [], "Enter bill.pdf in the books", ""
        for p in (mock.patch.object(records, "LEDGER_DIR", self.root / "ledger"),
                  mock.patch("decurix_agent.task_context.current", lambda: TASK),
                  mock.patch("decurix_agent.task_context.current_request", lambda full=False: self.request),
                  mock.patch("decurix_agent.task_context.current_files", lambda: []),
                  mock.patch.object(evidence, "_task_events", lambda task_id=None: self.events),
                  mock.patch.object(evidence, "read", lambda task_id=None: "combobox Cost centre: General | "
                                                                        "Project Alder | Project Birch"),
                  mock.patch.object(evidence, "looks", lambda task_id=None: self.looks),
                  mock.patch.object(guidance, "answers", lambda task_id=None: self.said),
                  mock.patch.object(guidance, "exchanges", lambda task_id=None: [], create=True),
                  # (The person's word comes from the control plane - person_record: none read here, and a reply
                  # this test's person gave is one the control plane confirmed.)
                  mock.patch.object(person_record, "_get", lambda path, query: None),
                  mock.patch.object(person_record, "answers", lambda task_id=None: (
                      [{"id": "j_111111111111", "asked": "", "text": self.said}] if self.said else [])),
                  mock.patch.object(records, "_org_text", lambda: "", create=True),
                  mock.patch.object(core, "_task_events_now", lambda: self.events),
                  mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day")):
            p.start()
            self.addCleanup(p.stop)

    def declare(self, fields, key="INV-0001"):
        return records.declare(key, str(self.bill), fields)


class A_APickNothingBacks(Ledger):
    PICK = {"kind": "select", "outcome": "done", "option": "General", "element": {"name": "Cost centre"}}

    def test_declare_refuses_the_workers_own_pick(self):
        self.events = [self.PICK]
        with self.assertRaises(ComputerError) as caught:
            self.declare({"Bill no.": "INV-0001", "Cost centre": "General"})
        self.assertIn("ask the person (HELP) which one", str(caught.exception))

    def test_a_pick_the_request_or_the_person_gives_is_backed(self):
        self.events = [self.PICK]
        self.request = "Enter bill.pdf in the books, charged to General"
        rec, _ = self.declare({"Bill no.": "INV-0001", "Cost centre": "General"})
        self.assertEqual(rec["fields"]["Cost centre"]["from"], "chosen")
        self.request, self.said = "Enter bill.pdf in the books", "Charge it to General."
        rec, _ = self.declare({"Bill no.": "INV-0001", "Cost centre": "General"})
        self.assertEqual(rec["fields"]["Cost centre"]["from"], "chosen")

    def test_the_final_checks_typing_check_names_the_pick(self):
        # (A pick into a form the task then saved: round 10, R9-B2.)
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"}, self.PICK,
                       {"kind": "click", "outcome": "done", "change": "saved; page is now http://app/bills/3"}]
        found = long_task.invented_typing([], self.request, chosen=records.unbacked_picks(TASK))
        self.assertTrue(found and found[0].startswith(long_task.CHOSEN_UNBACKED), found)
        self.request = "Enter bill.pdf in the books, charged to General"
        self.assertEqual(records.unbacked_picks(TASK), [])

    def test_an_undeclared_field_the_task_set_is_held_against_the_saved_record(self):
        rec, _ = self.declare({"Bill no.": "INV-0001", "Total": "3,540.00"})
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"},
                       {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}},
                       {"kind": "type", "outcome": "done", "text": "3540.00", "element": {"name": "Total"}},
                       self.PICK]
        form = [("Bill no.", "INV-0001"), ("Total", "3540.00"), ("Cost centre", "General")]
        held, clean, lines = core._ledger_check(form)
        self.assertEqual(held, "")  # ADVISORY at the save (records.UNDECLARED_FIELD_BLOCKS)
        self.assertTrue(any("this task set Cost centre=General and no declaration covers it" in l for l in lines))
        records.readback(records.open_record(records.load()), "Bill INV-0001", "INV-0001 3,540.00 General", "saved")
        self.assertTrue(any("Cost centre=General" in f for f in records.gate_faults(TASK)))


class A_TheReportsOwnMarker(unittest.TestCase):
    def test_only_the_marker_decides(self):
        self.assertEqual(skill_task.not_backed_items(NOT_BACKED_REPORT),
                         ['Cost centre set to "General" | not backed: the source document gives no cost centre, but '
                          'the form requires one'])
        self.assertEqual(skill_task.not_backed_items("JUDGEMENTS:\n- the date is not backed up anywhere | backed by: "
                                                     "\"28/09/2026\"\nDONE: ok"), [])
        self.assertEqual(skill_task.not_backed_items("no list | not backed"), [])  # (outside the list)

    def test_the_gates_faults_carry_it(self):
        with mock.patch.object(records, "gate_faults", lambda task_id=None, created=None: []),                 mock.patch.object(core, "_task_events_now", lambda: []):
            found = skill_task._ledger_faults(NOT_BACKED_REPORT)
        self.assertEqual(len(found), 1)
        self.assertIn('Cost centre set to "General" | not backed', found[0])


class TheFinalGate(ts.TeachingBase):
    """The gate refuses done on what code decides, whatever the judge would say - and never asks it."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        state.current_task = "Enter bill.pdf in the books"
        for p in (mock.patch.object(skill_task.verify, "available", lambda: True),
                  mock.patch.object(skill_task.verify, "judge",
                                    side_effect=AssertionError("the judge decides nothing code has decided")),
                  mock.patch.object(skill_task.evidence, "live", lambda scale=1: b"now"),
                  mock.patch.object(skill_task, "_did_screen_work", lambda: True),
                  mock.patch.object(skill_task, "_engine_can_drive", lambda: False)):
            p.start()
            self.addCleanup(p.stop)

    def test_a_not_backed_item_is_not_done(self):
        with mock.patch.object(skill_task, "_ledger_faults",
                               lambda reply="": [f"the report itself says: {i}"
                                                 for i in skill_task.not_backed_items(reply)]):
            self.assertTrue(skill_task._gate(NOT_BACKED_REPORT, "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn('Cost centre set to "General" | not backed', self.asked[-1][0])

    def test_a_second_save_outside_the_ledger_is_not_done_and_no_ledger_basis(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(records, "LEDGER_DIR", Path(tmp)):
            records._save(task_context.current() or TASK, [{"key": "INV-0002", "saved_at": 1.0, "fields": {
                "Bill no.": {"intent": "INV-0002", "from": "source"}}, "readback": {"seen": ["Bill no."], "absent": []}}])
            with mock.patch.object(core, "_task_events_now", lambda: save_events("bills", "bills")), \
                    mock.patch("decurix_agent.task_context.current", lambda: TASK):
                self.assertNotIn("ledger", skill_task._basis("model"))
                self.assertTrue(skill_task._gate("Saved INV-0002.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("made 2 new record(s), and 1 declared record(s) were saved: a record was made that no "
                      "declaration covers", self.asked[-1][0])


class B_ACutOffCheck(unittest.TestCase):
    def test_an_answer_cut_off_at_the_limit_is_asked_again(self):
        self.assertTrue(_retries({"content": '{"verdict": "done", "conf'}, "length", 600))  # (only an empty one was)
        self.assertTrue(_retries({"content": ""}, "length", 600))
        self.assertFalse(_retries({"content": '{"verdict": "done"}'}, "stop", 600))

    def test_the_judge_has_room_and_a_cut_off_is_not_made(self):
        asked = []

        def chat(messages, max_tokens=None, **k):
            asked.append(max_tokens)
            return {"content": '```json\n{"verdict": "done", "confidence": 1.0, "seen": "Bill INV-0002 sav'}, {}
        with mock.patch.object(verify.llm_client, "chat", chat), \
                mock.patch.object(verify.llm_client, "available", lambda: True):
            found = verify.judge("Enter the bill", claim="Saved it.")
        self.assertGreaterEqual(asked[0], 2000)
        self.assertNotEqual(found["verdict"], verify.DONE)
        self.assertTrue(found["why"].startswith("the check could not be made"), found["why"])
        self.assertIn("cut off", found["why"])


def _retries(message, finish_reason, max_tokens):
    """Whether llm_client.chat asks again for this answer (its own condition, run on a stand-in transport)."""

    answers = [{"choices": [{"message": message, "finish_reason": finish_reason}], "usage": {}},
               {"choices": [{"message": {"content": "again"}, "finish_reason": "stop"}], "usage": {}}]
    sent = []

    class Ok:
        def __init__(self, body):
            self.body = json.dumps(body).encode()

        def read(self):
            return self.body

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def urlopen(request, timeout=None):
        sent.append(json.loads(request.data)["max_completion_tokens"])
        return Ok(answers[len(sent) - 1])
    with mock.patch.object(llm_client.urllib.request, "urlopen", urlopen), \
            mock.patch.object(llm_client, "available", lambda: True):
        llm_client.chat([{"role": "user", "content": "x"}], max_tokens=max_tokens, purpose="py.verify")
    return len(sent) == 2 and sent[1] == llm_client.EMPTY_RETRY_TOKENS


class B_AReplyThatSettlesNothing(ts.TeachingBase):
    def test_the_check_is_made_again_never_the_work(self):
        made_again = []
        state.current_task = "Enter bill.pdf"
        paused = {"mode": "confirm", "run": None, "claim": "Saved INV-0002.", "engine": "thebot",
                  "task": "Enter bill.pdf",
                  "verdict": {"verdict": verify.UNSURE, "seen": "", "why": "the check could not be made (cut off)"}}
        skill_task._rechecks.clear()
        with mock.patch.object(skill_task, "write_final_reply",
                               lambda text, kind="final", engine=None, **k: made_again.append((text, kind))), \
                mock.patch.object(skill_task, "_retry_engine", side_effect=AssertionError("the work was run again")), \
                mock.patch.object(skill_task, "_done_reading",
                                  lambda reply, doubt="": {"choice": "unclear", "instruction": ""}), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True):
            skill_task._confirm_reply(paused, "I don't know that. Use what you have, or stop and say what is missing.")
        self.assertEqual(made_again, [("Saved INV-0002.", "final")])  # through the final gate again

    def test_a_check_that_looked_and_doubted_still_takes_the_correction(self):
        retried = []
        state.current_task = "Enter bill.pdf"
        paused = {"mode": "confirm", "run": None, "claim": "Saved.", "engine": "thebot", "task": "Enter bill.pdf",
                  "verdict": {"verdict": verify.UNSURE, "seen": "the total is not shown", "why": "not sure"}}
        with mock.patch.object(skill_task, "_retry_engine", lambda guided, note: retried.append(note)), \
                mock.patch.object(skill_task, "_done_reading",
                                  lambda reply, doubt="": {"choice": "correction", "instruction": "the total is wrong"}), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True):
            skill_task._confirm_reply(paused, "the total is wrong")
        self.assertEqual(len(retried), 1)


class B_ASecondSave(Ledger):
    def test_a_saved_key_is_not_declared_again(self):
        rec, _ = self.declare({"Bill no.": "INV-0001"})
        records.readback(rec, "Bill INV-0001", "INV-0001", "saved; page is now http://app/bills/3")
        with self.assertRaises(ComputerError) as caught:
            self.declare({"Bill no.": "INV-0001"})
        self.assertIn("was saved in this task already", str(caught.exception))

    def test_a_new_form_holding_a_saved_key_is_not_pressed(self):
        rec, _ = self.declare({"Bill no.": "INV-0001"})
        records.readback(rec, "Bill INV-0001", "INV-0001", "saved; page is now http://app/bills/3")
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"},
                       {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}}]
        held, clean, _ = core._ledger_check([("Bill no.", "INV-0001"), ("Total", "3540.00")])
        self.assertIn("was saved in this task already", held)
        # Its own saved record's edit page is no second record.
        self.events = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills/3/edit"},
                       {"kind": "type", "outcome": "done", "text": "INV-0001", "element": {"name": "Bill no."}}]
        self.assertIsNone(core._ledger_check([("Bill no.", "INV-0001"), ("Total", "3540.00")]))

    def test_the_saves_that_made_a_record_are_counted(self):
        self.assertEqual(core.records_created(save_events("bills")), 1)
        self.assertEqual(core.records_created(save_events("bills", "bills")), 2)
        refused = [{"kind": "click", "outcome": "done", "change": "page is now http://app/bills/new"},
                   {"kind": "click", "outcome": "done", "change": "Cost centre is required"}]
        self.assertEqual(core.records_created(refused), 0)


class C_AKeyTheApplicationShowedAlready(Ledger):
    def test_stops_once_and_asks(self):
        self.looks = [evidence.LOOK_LEAD + ". What every field of the page held (in view or not):\n(none)\nThe words "
                      "the page showed outside its fields, in its order (in view or not):\nBills | INV-0003 | Example Vendor "
                      "| 3,540.00\nThe part in view, as the tool listed it:\n..."]
        with self.assertRaises(ComputerError) as caught:
            self.bill.write_text("Bill INV-0003\nTotal 3,540.00\n", encoding="utf-8")
            records.declare("INV-0003", str(self.bill), {"Bill no.": "INV-0003"})
        self.assertIn("Ask the person (HELP) whether it is already in the application", str(caught.exception))
        rec, _ = records.declare("INV-0003", str(self.bill), {"Bill no.": "INV-0003"})  # their answer: once only
        self.assertEqual(rec["key"], "INV-0003")

    def test_the_document_on_the_screen_is_not_the_application(self):
        self.looks = [evidence.LOOK_LEAD + ". What every field of the page held (in view or not):\n(none)\nThe words "
                      "the page showed outside its fields, in its order (in view or not):\nbill.txt | Bill INV-0001\n"
                      "The part in view, as the tool listed it:\n..."]
        rec, _ = self.declare({"Bill no.": "INV-0001"})
        self.assertEqual(rec["key"], "INV-0001")


if __name__ == "__main__":
    unittest.main()
