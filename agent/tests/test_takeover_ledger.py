"""
A taught replay that stops part way and hands the rest to the model (skill_task._by_model, from_step): found live
(2026-11-10, a month of bills, eval office-books). The replay saved bill 1072, then stopped at the next bill because
its customer was not in the "Party A/c name" list. Then:

- the takeover note said only "carry on from step 68" - nothing of what was saved - and the model declared 1072 again
  as "V1072" and saved it a second time (records.ledger_lines, records.saved_from_same_document, core._saved_again);
- nothing resolved the new customer's tax number, so the model's ledger save was refused (replay._run_step with the
  item's identifier, skill_task._lookup_ids, skills.identifier_in);
- the how-to told the model a missing list entry is "a question for the person", though the lesson itself made such a
  ledger before the bills (skills.made_before, skill_task._made_how).

    cd agent/tests && PYTHONPATH=.. python3 -m unittest test_takeover_ledger
"""

import unittest
from unittest import mock

from test_round9_false_done import TASK, Ledger  # noqa: F401 - stubs config first

from decurix_agent import person_record, skill_task
from decurix_agent.computer import core, records, replay, skills
from decurix_agent.computer.errors import ComputerError

NEW = {"kind": "click", "outcome": "done", "change": "page is now http://app/vouchers/new"}

SELLER = "29AAACK1111A1Z1"
CUSTOMER_1, CUSTOMER_2 = "29AAGFC5512K1ZC", "29AABCH7777B1Z2"


# ------------------------------------------------------------------ 1: the same record, saved once

class TheSameRecordIsNotEnteredTwice(Ledger):
    def setUp(self):
        super().setUp()
        self.bill.write_text("Example Vendor Ltd\nBill No. 1072\nDate 02-04-2027\nTotal 3,540.00\n", encoding="utf-8")
        self.other = self.root / "receipt.txt"
        self.other.write_text("Receipt No. 1072\nReceived 500.00\n", encoding="utf-8")

    def saved_1072(self):
        rec, _ = records.declare("1072", str(self.bill), {"Voucher No.": "1072", "Total": "3,540.00"})
        records.readback(rec, "Voucher 1072", "Voucher 1072 | Total 3,540.00", "saved; page is now http://app/vouchers/3")
        return rec

    def test_declared_again_under_another_key_from_its_document_is_the_saved_record(self):
        self.saved_1072()
        with self.assertRaises(ComputerError) as caught:
            records.declare("V1072", str(self.bill), {"Voucher No.": "1072", "Total": "3,540.00"})
        self.assertIn("record 1072 was saved in this task already", str(caught.exception))
        self.assertEqual([r["key"] for r in records.load(TASK)], ["1072"])
        # The path written another way (~, ./) is the same document.
        same = str(self.bill.parent / "." / self.bill.name)
        with self.assertRaises(ComputerError):
            records.declare("V1072", same, {"Voucher No.": "1072"})

    def test_another_document_with_the_same_number_is_another_record(self):
        self.saved_1072()
        rec, _ = records.declare("R1072", str(self.other), {"Voucher No.": "1072", "Amount": "500.00"})
        self.assertEqual(rec["key"], "R1072")
        self.assertEqual(len(records.load(TASK)), 2)

    def test_the_save_of_a_saved_record_is_refused_while_another_record_is_open(self):
        self.saved_1072()
        records._save(TASK, records.load(TASK) + [{
            "key": "V1072", "source": str(self.bill), "fields": {"Voucher No.": {"intent": "1072", "from": "source"}},
            "declared_at": 1.0, "saved_at": None, "readback": {}, "checks": 0}])
        self.events = [NEW, {"kind": "type", "outcome": "done", "text": "1072", "element": {"name": "Voucher No."}}]
        held = core._ledger_check([("Voucher No.", "1072"), ("Total", "3540.00")])
        self.assertIn("record 1072 was saved in this task already", held[0])
        self.assertIn("ask them (HELP) in a question that names 1072", held[0])

    def test_a_record_the_form_shows_that_comes_from_another_document_is_saved(self):
        self.saved_1072()
        records.declare("R1072", str(self.other), {"Voucher No.": "1072", "Amount": "500.00"})
        self.events = [NEW, {"kind": "type", "outcome": "done", "text": "1072", "element": {"name": "Voucher No."}}]
        held = core._ledger_check([("Voucher No.", "1072"), ("Amount", "500.00")])
        self.assertFalse(held and held[0])

    def test_the_persons_yes_to_a_question_naming_it_lets_it_through(self):
        self.saved_1072()
        records._save(TASK, records.load(TASK) + [{
            "key": "V1072", "source": str(self.bill), "fields": {"Voucher No.": {"intent": "1072", "from": "source"}},
            "declared_at": 1.0, "saved_at": None, "readback": {}, "checks": 0}])
        self.events = [NEW, {"kind": "type", "outcome": "done", "text": "1072", "element": {"name": "Voucher No."}}]
        answers = [{"id": "j1", "asked": "Bill 1072 is saved already - enter it a second time?", "text": "yes"}]
        with mock.patch.object(person_record, "answers", lambda task_id=None: answers), \
                mock.patch("decurix_agent.computer.approval.says_yes", lambda reply, action="": reply == "yes"):
            held = core._ledger_check([("Voucher No.", "1072"), ("Total", "3540.00")])
        self.assertFalse(held and "saved in this task already" in held[0])

    def test_the_ledger_says_what_was_saved_and_what_is_left(self):
        self.saved_1072()
        records.declare("R1072", str(self.other), {"Voucher No.": "1072", "Amount": "500.00"})
        lines = records.ledger_lines(TASK)
        self.assertEqual(lines[0], "- saved: 1072 (from bill.txt) - saved and read back off its page - never enter it again")
        self.assertEqual(lines[1], "- left: R1072 (from receipt.txt) - declared from its document, not saved yet")
        with mock.patch.object(skill_task.task_context, "current", lambda: TASK):
            note = skill_task._takeover_ledger_note()
        self.assertIn("never enter it again, do not declare it again under another key", note)
        self.assertIn("- saved: 1072", note)


# ------------------------------------------------------------------ 2 and 3: a record the lesson made before the items

def lesson():
    """Make a customer's ledger (name, tax number, save), then per bill: a fresh voucher, its number, its party, save."""

    def step(n, kind, role, name, param=None, **extra):
        return dict({"id": f"s{n}", "n": n, "kind": kind, "target": {"role": role, "name": name}, "param": param,
                     "summary": f"{kind} {role} \"{name}\"" + (f" {{{param}}}" if param else "")}, **extra)

    steps = [step(1, "click", "link", "Create ledger"),
             step(2, "type", "textbox", "Name", "customer_name"),
             step(3, "type", "textbox", "GSTIN/UIN", "gstin"),
             step(4, "click", "button", "Save ledger"),
             step(5, "click", "link", "Create voucher"),
             step(6, "type", "textbox", "Voucher No.", "number"),
             step(7, "select", "combobox", "Party A/c name", "party"),
             step(8, "click", "button", "Save voucher")]
    params = [
        {"name": "customer_name", "label": "Name", "step": 2, "example": "Bhadra Traders", "default": "Bhadra Traders"},
        {"name": "gstin", "label": "GSTIN/UIN", "step": 3, "example": "29AAGFB1234C1Z5", "default": "29AAGFB1234C1Z5"},
        {"name": "number", "label": "Voucher No.", "step": 6, "list": True, "examples": ["412", "413"]},
        {"name": "party", "label": "Party A/c name", "step": 7, "list": True, "examples": ["Tunga Mills", "Bhadra Traders"]},
    ]
    return {"name": "enter-bills", "title": "Enter the month's bills", "steps": steps, "params": params,
            "loops": [{"id": "L1", "enabled": True, "blocks": [["s5", "s6", "s7", "s8"]], "length": 4, "reps": 2}]}


class ARecordTheLessonMadeBeforeTheItems(unittest.TestCase):
    def test_the_making_is_found_with_its_identifier(self):
        skill = lesson()
        self.assertEqual(skill_task._loop_start(skill), 5)
        self.assertEqual(skills.made_before(skill, 5), [
            {"pick": "party", "name": "customer_name", "identifier": "gstin", "steps": [1, 2, 3, 4]}])
        # A name typed that the block never chose is no making of its record.
        other = lesson()
        other["params"][3]["examples"] = ["Tunga Mills", "Kadri Heights"]
        self.assertEqual(skills.made_before(other, 5), [])

    def test_an_items_identifier_is_its_documents_own_not_the_senders(self):
        bill_1 = f"Suvarna Lifts GSTIN {SELLER}\nBill to Coastal Infra GSTIN {CUSTOMER_1}\n"
        bill_2 = f"Suvarna Lifts GSTIN {SELLER}\nBill to Hebri Agro GSTIN {CUSTOMER_2}\n"
        self.assertEqual(skills.identifier_in("29AAGFB1234C1Z5", bill_1, [bill_2]), CUSTOMER_1)
        self.assertEqual(skills.identifier_in("29AAGFB1234C1Z5", bill_2, [bill_1]), CUSTOMER_2)
        # One document alone, or a customer with no number: which is the sender's cannot be told - none.
        self.assertIsNone(skills.identifier_in("29AAGFB1234C1Z5", bill_1, []))
        self.assertIsNone(skills.identifier_in("29AAGFB1234C1Z5", f"GSTIN {SELLER}\nBill to a walk-in\n", [bill_1]))

    def test_each_items_choice_carries_its_documents_identifier(self):
        skill = lesson()
        docs = ["/w/1188 Coastal.docx", "/w/1189 Hebri.docx"]
        texts = {docs[0]: f"GSTIN {SELLER}\nBill to Coastal GSTIN {CUSTOMER_1}",
                 docs[1]: f"GSTIN {SELLER}\nBill to Hebri GSTIN {CUSTOMER_2}"}
        run = {"skill": skill, "gathered": "done", "documents": docs, "values": {}}
        program, _ = skills.expand(skill, {"number": "1188; 1189", "party": "Coastal Infra; Hebri Agro",
                                           "customer_name": "x", "gstin": "y"})
        with mock.patch.object(skill_task.binding, "_document_text", lambda path: texts[path]):
            ids = skill_task._lookup_ids(run, program)
        chosen = {s["n"]: s["param"] for s in program["steps"] if s["kind"] == "select"}
        self.assertEqual({chosen[n]: v for n, v in ids.items()}, {"party__1": [CUSTOMER_1], "party__2": [CUSTOMER_2]})
        # Not a run done once per document: nothing.
        self.assertEqual(skill_task._lookup_ids(dict(run, gathered=None), program), {})

    def test_the_how_to_says_to_make_it_the_way_the_lesson_did(self):
        how = skill_task._made_how({"skill": lesson()})
        self.assertIn("the person's steps 1-4 MADE such a record", how)
        self.assertIn("--id", how)
        self.assertNotIn("that is a question for the person (HELP)", how)
        plain = lesson()
        plain["params"][3]["examples"] = ["Tunga Mills", "Kadri Heights"]
        self.assertIn("that is a question for the person (HELP)", skill_task._made_how({"skill": plain}))

    def test_the_takeover_step_is_placed_in_the_persons_steps_and_its_item(self):
        skill = lesson()
        program, _ = skills.expand(skill, {"number": "1188; 1189", "party": "Coastal Infra; Hebri Agro",
                                           "customer_name": "x", "gstin": "y"})
        run = {"skill": skill, "expanded": program, "gathered": "done", "documents": ["/w/1188 A.docx", "/w/1189 B.docx"]}
        second = next(s["n"] for s in program["steps"] if s.get("param") == "party__2")
        self.assertEqual(skill_task._where_in_lesson(run, second),
                         f"Step {second} of this run is the person's step 7, for item 2 of 2 (the document 1189 B.docx).")


class TheReplayLooksForTheItemByItsIdentifier(unittest.TestCase):
    STEP = {"n": 7, "kind": "select", "target": {"role": "combobox", "name": "Party A/c name"}, "param": "party__2",
            "summary": "select {party__2} in combobox \"Party A/c name\" (item 2 of 2)"}
    FIELD = {"path": "page > combobox", "role": "combobox", "name": "Party A/c name"}

    def run_step(self, step, act):
        with mock.patch.object(replay, "_find", lambda *a, **k: (self.FIELD, None, {}, None)), \
                mock.patch.object(replay.core, "act", act):
            return replay._run_step(step, {"party__2": "Hebri Agro"}, report=lambda text: None)

    def test_a_miss_by_name_is_searched_again_by_the_items_identifier(self):
        calls = []

        def act(kind, **kw):
            calls.append(kw.get("ids"))
            if not kw.get("ids"):
                raise ComputerError("combobox \"Party A/c name\" has no option 'Hebri Agro'")
            return {"status": "done", "observation": None}

        done = self.run_step(dict(self.STEP, ids=[CUSTOMER_2]), act)
        self.assertEqual(done["status"], "done")
        self.assertEqual(calls, [None, [CUSTOMER_2]])

    def test_without_an_identifier_the_miss_halts_as_before(self):
        def act(kind, **kw):
            raise ComputerError("combobox \"Party A/c name\" has no option 'Hebri Agro'")

        with self.assertRaises(replay._Halt) as caught:
            self.run_step(dict(self.STEP), act)
        self.assertIn("has no option", caught.exception.reason)

    def test_what_the_search_by_identifier_resolved_is_in_the_halt(self):
        def act(kind, **kw):
            if kw.get("ids"):
                raise ComputerError(f"has no option 'Hebri Agro'\nresolved {CUSTOMER_2}: no record holds it (searched: ...)")
            raise ComputerError("has no option 'Hebri Agro'")

        with self.assertRaises(replay._Halt) as caught:
            self.run_step(dict(self.STEP, ids=[CUSTOMER_2]), act)
        self.assertIn(f"resolved {CUSTOMER_2}: no record holds it", caught.exception.reason)


if __name__ == "__main__":
    unittest.main()
