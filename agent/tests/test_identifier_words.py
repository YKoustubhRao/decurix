"""
Two findings of a live run (2026-11-08), with this test's own fictional bill.

- A key field typed with a phrase no source held as a number: a supplier's copy printed a phrase pointing to another
  paper where its invoice number belongs (the right action is to ask). The worker declared the phrase as the bill's
  number and the ledger took it as backed - it stands in the document, beside the field's own label. A record's number,
  reference or id given as words (no digit, two or more words) is refused at the declaration, at the save, and at the
  final check, unless the people's own words give it; a code the document prints is accepted as before.
- A worker tried to curl a bill straight into the application's form endpoint. The gate held that one (the host was
  not this machine), but a send to this machine runs without a person, and a yes is no check of what was saved. A
  record sent around the screen is never declared or read back, so the task cannot be reported done.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_round9_false_done as r9  # noqa: E402 - stubs config first

import test_skills as ts  # noqa: E402

from decurix_agent import attachments, ledger_checks, playbooks, skill_task, state  # noqa: E402
from decurix_agent.computer import cdp, core, evidence, records, recording  # noqa: E402
from decurix_agent.computer.errors import ComputerError  # noqa: E402

TASK = r9.TASK
PHRASE = "refer to remittance advice"
NUMBER = "BW-7720"
HOST = "ledger.example.test"
BILL = f"""Brightfern Window Care
12 Orchard Lane, Fernbridge
TAX INVOICE
Invoice number: {PHRASE}
Invoice date: 2026-06-11
Due date: 2026-06-25
Window cleaning, 3 floors: 2,900.00
Total due: 3,422.00
Remittance advice sent separately.
"""
FIELDS = {"Vendor": "Brightfern Window Care", "Date": "2026-06-11", "Due": "2026-06-25", "Total": "3,422.00"}


def typed(label, text):
    return {"kind": "type", "outcome": "done", "text": text, "element": {"role": "textbox", "name": label}}


def bill_saved(number):
    """A bill's form opened, its number and total typed, and Save answered as saved."""

    return [{"kind": "click", "outcome": "done", "change": f"page is now http://{HOST}/bills/new"},
            typed("Invoice number", number), typed("Total", "3,422.00"),
            {"kind": "click", "outcome": "done", "change": f"saved; page is now http://{HOST}/bills/3"}]


class Bill(r9.Ledger):
    def setUp(self):
        super().setUp()
        self.bill.write_text(BILL, encoding="utf-8")
        self.request = f"Enter the bill in bill.txt into the Ledger app (http://{HOST}/bills/new)."


class WordsForTheNumber(Bill):
    def test_the_live_declaration_is_refused_and_names_the_field(self):
        with self.assertRaises(ComputerError) as caught:
            self.declare(dict(FIELDS, **{"Invoice number": PHRASE}), key="Brightfern 2026-06-11")
        said = str(caught.exception)
        self.assertIn(f"Invoice number: '{PHRASE}'", said)
        self.assertIn("Ask the person (HELP) for the Invoice number", said)
        self.assertEqual(records.load(), [])
        # (The words do stand in the document: standing in a source was the whole of the old check.)
        self.assertTrue(records.stands_in(PHRASE, BILL))

    def test_a_number_the_document_prints_is_accepted(self):
        self.bill.write_text(BILL.replace(PHRASE, NUMBER), encoding="utf-8")
        rec, _ = self.declare(dict(FIELDS, **{"Invoice number": NUMBER}), key=NUMBER)
        self.assertEqual(rec["fields"]["Invoice number"], {"intent": NUMBER, "from": "source"})

    def test_words_the_person_gives_for_it_are_theirs(self):
        self.said = f"There is no number; enter it as '{PHRASE}'."
        rec, _ = self.declare(dict(FIELDS, **{"Invoice number": PHRASE}), key="Brightfern")
        self.assertEqual(rec["fields"]["Invoice number"]["intent"], PHRASE)

    def test_what_is_a_code_field_and_what_is_words(self):
        for label in ("Invoice number", "Bill No.", "Ref", "Supplier reference", "Invoice #", "Order ID"):
            self.assertTrue(records.identifier_field(label), label)
        for label in ("Vendor", "Notes", "Cost centre", "Due date"):
            self.assertFalse(records.identifier_field(label), label)
        for value in (PHRASE, "to follow", "see attached"):
            self.assertTrue(records.words_not_code(value), value)
        for value in (NUMBER, "INV 2026 01", "No", "KS1102", "", "Letter 12"):
            self.assertFalse(records.words_not_code(value), value)
        # A field that is not a code may hold words.
        self.assertEqual(records.wordy_identifiers([("Notes", PHRASE)], free=""), [])

    def test_the_save_is_not_pressed(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        form = {"form": "New bill", "fields": [["Invoice number", PHRASE], ["Vendor", "Brightfern Window Care"],
                                               ["Total", "3,422.00"]]}
        self.events = [typed("Invoice number", PHRASE), typed("Vendor", "Brightfern Window Care"),
                       typed("Total", "3,422.00")]
        save = {"ref": "e12", "name": "Save bill", "path": "/form/save", "source": "cdp"}
        self.request += "\n\n" + attachments.MARK + f"\n  {self.bill}\n"  # (the bill attached: its figures are sourced)
        with mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "check.json"), \
                mock.patch.object(cdp, "submit_form_fields", return_value=form), \
                mock.patch.object(recording, "ROOT", Path(tmp.name) / "recordings"), \
                mock.patch("decurix_agent.task_context.current_files", return_value=[str(self.bill)]):
            for checked in (False, True):
                said = core._form_check(save, checked=checked) or ""
                self.assertIn("Not pressed", said)
                self.assertIn("Ask the person (HELP) for the Invoice number", said)

    def test_a_record_saved_with_them_is_not_done_declared_or_not(self):
        self.events = bill_saved(PHRASE)
        found = ledger_checks.faults(ledger_checks.findings(TASK))
        self.assertEqual([f.kind for f in found], [ledger_checks.WORDY_KEY])
        self.assertIn("Invoice number", found[0])
        self.events = bill_saved(NUMBER)
        self.assertEqual(ledger_checks.findings(TASK), [])
        # (Typed and then corrected before the save: the form held the number.)
        self.events = bill_saved(NUMBER)
        self.events.insert(1, typed("Invoice number", PHRASE))
        self.assertEqual(ledger_checks.findings(TASK), [])


class SentAroundTheScreen(Bill):
    CURL = (f"curl -fsS -o /dev/null --data-urlencode number={NUMBER} --data-urlencode 'vendor=Brightfern Window "
            f"Care' http://{HOST}/bills/new")

    def setUp(self):
        super().setUp()
        self.commands = []
        p = mock.patch.object(playbooks, "task_commands", lambda: list(self.commands))
        p.start()
        self.addCleanup(p.stop)

    def test_what_sends(self):
        for c in (self.CURL, "curl -X POST http://localhost:8000/api/bill", "curl -sd @bill.json http://app/x",
                  "wget --post-data 'a=1' http://app/x", "curl --json '{}' http://app/x",
                  "python3 - <<'PY'\nimport requests\nrequests.post('http://app/x', data={})\nPY"):
            self.assertTrue(playbooks.sends_data(c), c)
        for c in (f"curl -fsS http://{HOST}/bills", "curl -sSf -o page.html http://app/x",
                  "wget http://app/file.pdf", "grep -d skip curl notes.txt | head", "office read bill.txt"):
            self.assertFalse(playbooks.sends_data(c), c)

    def test_a_curl_saved_record_with_no_declaration_is_not_done(self):
        # The task opened the application's new-bill form on the screen, then sent the bill with curl.
        self.events = [{"kind": "click", "outcome": "done", "change": f"page is now http://{HOST}/bills/new"}]
        self.commands = ["office read bill.txt", self.CURL]
        found = ledger_checks.faults(ledger_checks.findings(TASK))
        self.assertEqual([f.kind for f in found], [ledger_checks.SENT_AROUND])
        self.assertIn("ask the person (HELP)", found[0])
        self.assertNotIn("Brightfern", found[0])  # (the command is never quoted: it may carry a credential)

    def test_with_a_declared_record_any_send_is_around_the_ledger(self):
        self.bill.write_text(BILL.replace(PHRASE, NUMBER), encoding="utf-8")
        self.declare(dict(FIELDS, **{"Invoice number": NUMBER}), key=NUMBER)
        self.commands = [f"curl -d number={NUMBER} http://10.0.0.5:8080/bills/new"]
        self.assertEqual([f.kind for f in ledger_checks.findings(TASK)], [ledger_checks.SENT_AROUND])

    def test_a_read_or_a_send_elsewhere_is_not_held(self):
        self.events = [{"kind": "click", "outcome": "done", "change": f"page is now http://{HOST}/bills/new"}]
        self.commands = [f"curl -fsS http://{HOST}/bills", "curl -d q=1 http://other.example.test/search"]
        self.assertEqual(ledger_checks.findings(TASK), [])


class TheFinalGateRefusesIt(ts.TeachingBase):
    """The gate refuses done on both, whatever the judge would say - and never asks it."""

    def setUp(self):
        super().setUp()
        state.current_task = "Enter the bill in bill.txt into the Ledger app"
        self.events, self.tmp2 = [], tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp2.cleanup)
        for p in (mock.patch.object(skill_task.verify, "available", lambda: True),
                  mock.patch.object(skill_task.verify, "judge",
                                    side_effect=AssertionError("the judge decides nothing code has decided")),
                  mock.patch.object(skill_task.evidence, "live", lambda scale=1: b"now"),
                  mock.patch("decurix_agent.final_gate._did_screen_work", lambda: True),
                  mock.patch.object(skill_task, "_engine_can_drive", lambda: False),
                  mock.patch.object(records, "LEDGER_DIR", Path(self.tmp2.name)),
                  mock.patch.object(records, "_free_text", lambda task_id=None: state.current_task),
                  mock.patch.object(evidence, "_task_events", lambda task_id=None: self.events),
                  mock.patch.object(core, "_task_events_now", lambda: self.events),
                  mock.patch("decurix_agent.task_context.current", lambda: TASK)):
            p.start()
            self.addCleanup(p.stop)

    def test_words_saved_as_the_number_are_not_done(self):
        self.events = bill_saved(PHRASE)
        self.assertNotIn("ledger", skill_task._basis("model"))
        self.assertTrue(skill_task._gate("Saved the Brightfern bill.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("Ask the person (HELP) for the Invoice number", self.asked[-1][0])

    def test_a_curl_saved_record_is_not_done(self):
        self.events = [{"kind": "click", "outcome": "done", "change": f"page is now http://{HOST}/bills/new"}]
        playbooks.note_command(SentAroundTheScreen.CURL)
        self.assertTrue(skill_task._gate("Sent the bill to the Ledger app.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("sent data straight to the application", self.asked[-1][0])


if __name__ == "__main__":
    unittest.main()
