"""
Found live (eval F1, 2026-10-26, takes 7 and 8): a bill named its customer otherwise than the books did - a trade name
for the firm the books held under another, a title before a person's name. The worker searched the books' ledgers by
the bill's name only, found nothing, and made a second ledger holding the same tax number; a taught rule (match by the
identifier first) did not land twice. A save of a new party's record holding an identifier is now stopped in code -
once per identifier and reason - until the task has looked the identifier up in the application.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import core, evidence


def opened(url):
    return {"kind": "click", "outcome": "done", "change": f"page is now {url}; 12 element(s) appeared"}


def typed(label, text):
    return {"kind": "type", "outcome": "done", "text": text, "change": "", "element": {"name": label}}


def look(fields="(none)", words="(none)", listing=""):
    return (f"{evidence.LOOK_LEAD}. What every field of the page held (in view or not):\n{fields}\n"
            f"The words the page showed outside its fields, in its order (in view or not):\n{words}\n"
            f"The part in view, as the tool listed it:\n{listing}")


GSTIN = "29AAGFC5512K1ZC"
SEARCH_BY_NAME = [opened("http://h:8704/books/ledgers"), typed("Find", "Coastal Infra Projects"),
                  opened("http://h:8704/books/ledgers?q=Coastal+Infra+Projects&group=")]
NEW_LEDGER = [opened("http://h:8704/books/ledger/new"), typed("Name", "Coastal Infra Projects"),
              typed("Address", "Plot 12, Baikampady"), typed("GSTIN/UIN", GSTIN)]
# The books' whole list of ledgers, as a look keeps it: its cells in order, a table's rows one after another.
LEDGER_LIST = look(words=" | ".join([
    "Chart of Accounts - Ledgers", "Name", "Under", "GSTIN/UIN", "State", "Closing balance",
    "Coastal Builders", "Sundry Debtors", GSTIN, "Karnataka", "0.00",
    "Hebri Agro Industries", "Sundry Debtors", "29AAHFH2231Q1Z8", "Karnataka", "0.00",
    "Naveen Prabhu", "Sundry Debtors", "29ABCPN4417M1ZQ", "Karnataka", "0.00",
    "Udupi Fasteners", "Sundry Debtors", "29AADFU7781C1Z2", "Karnataka", "0.00"]))


class TheCheck(unittest.TestCase):
    def test_a_party_looked_for_only_by_the_bills_name_is_stopped(self):
        self.assertEqual(core.unsearched_identifier(SEARCH_BY_NAME + NEW_LEDGER, []), [(GSTIN, "")])

    def test_the_identifier_beside_another_name_on_a_page_seen_is_named(self):
        found = core.unsearched_identifier(SEARCH_BY_NAME + NEW_LEDGER, [LEDGER_LIST])
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0][0], GSTIN)
        self.assertIn("Coastal Builders | Sundry Debtors | 29AAGFC5512K1ZC", found[0][1])

    def test_a_new_party_looked_up_by_its_identifier_passes(self):
        searched = [opened("http://h:8704/books/ledgers"), typed("Find", GSTIN),
                    opened(f"http://h:8704/books/ledgers?q={GSTIN}&group=")]
        self.assertEqual(core.unsearched_identifier(searched + NEW_LEDGER, []), [])

    def test_an_application_without_a_search_its_list_seen_counts(self):
        other = NEW_LEDGER[:-1] + [typed("GSTIN/UIN", "32AAKFK7764R1ZH")]
        self.assertEqual(core.unsearched_identifier(other, [LEDGER_LIST]), [])
        # (A document shows one code or two - its own and its party's: no list of such records.)
        bill = look(words="Tax Invoice | Karkala Steel Fabricators | GSTIN 29AAKCK1234L1Z5 | Bill to | GSTIN 29AAHFH2231Q1Z8")
        self.assertEqual(core.unsearched_identifier(other, [bill]), [("32AAKFK7764R1ZH", "")])

    def test_the_forms_own_field_is_not_a_page_showing_it(self):
        form = look(fields=f"Window with the keyboard: Ledger Creation\ntextbox \"GSTIN/UIN\" holds: {GSTIN}",
                    listing=f"e477 textbox \"GSTIN/UIN\" value=\"{GSTIN}\" [focused]\ne479 button \"Accept\"")
        self.assertEqual(core.unsearched_identifier(SEARCH_BY_NAME + NEW_LEDGER, [form]), [(GSTIN, "")])

    def test_a_document_with_its_own_number_is_not_a_partys_record(self):
        invoice = [opened("http://h:8703/gstr1/b2b/add?period=102026"), typed("Receiver name", "Coastal Builders"),
                   typed("Recipient GSTIN", GSTIN), typed("Invoice No.", "INV-1188"), typed("Value", "76700")]
        self.assertEqual(core.unsearched_identifier(invoice, []), [])

    def test_an_existing_records_form_is_never_stopped(self):
        alter = [opened("http://h:8704/books/ledger/alter?id=12"), typed("Name", "Coastal Builders"),
                 typed("GSTIN/UIN", GSTIN)]
        self.assertEqual(core.unsearched_identifier(alter, []), [])

    def test_an_email_is_an_identifier_too(self):
        joiner = [opened("http://h:8701/people/new"), typed("First name", "Sneha"), typed("Last name", "Iyer"),
                  typed("Personal email", "sneha.iyer@example.com")]
        self.assertEqual(core.unsearched_identifier(joiner, []), [("sneha.iyer@example.com", "")])
        people = look(words="People | Ananya Rao | ananya.rao@example.com | Arif Khan | arif.khan95@example.com | "
                            "Rohit Kulkarni | rohit.kulkarni@example.com")
        self.assertEqual(core.unsearched_identifier(joiner, [people]), [])


class TheStop(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        patches = [mock.patch.object(core, "ID_REFUSALS_DIR", Path(tmp.name) / "id-refusals"),
                   mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef")]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def click(self, events, looks):
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, events)), \
                mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: looks):
            return core._unsearched_record({"name": "Accept"})

    def test_each_stop_is_made_once(self):
        events = SEARCH_BY_NAME + NEW_LEDGER
        said = self.click(events, [])
        self.assertIn(f"Not pressed: this makes a new record holding {GSTIN}, but you have not looked for", said)
        # It looked, and the list shows the code under another name: named, once.
        said = self.click(events, [LEDGER_LIST])
        self.assertIn('a page you looked at shows 29AAGFC5512K1ZC already: "', said)
        self.assertIn("Coastal Builders", said)
        self.assertEqual(self.click(events, [LEDGER_LIST]), "")
        self.assertEqual(self.click(events, []), "")

    def test_a_control_that_saves_nothing_is_not_judged(self):
        with mock.patch("decurix_agent.computer.recording.load", lambda s: ({}, SEARCH_BY_NAME + NEW_LEDGER)):
            self.assertEqual(core._unsearched_record({"name": "Quit"}), "")

    def test_accept_saves(self):
        self.assertTrue(core._SAVE_CONTROL.search("Accept"))


class TheLooks(unittest.TestCase):
    def test_only_looks_at_the_screen_never_a_file_read(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(evidence, "NOTES_DIR", Path(tmp)):
            with open(Path(tmp) / "t1.jsonl", "w", encoding="utf-8") as f:
                for text in (f"{evidence.FILE_READ_LEAD}/work/1188.docx: GSTIN {GSTIN}", LEDGER_LIST,
                             "a check of the screen then - the form shows the code"):
                    f.write(json.dumps({"at": 0, "text": text}) + "\n")
            self.assertEqual(evidence.looks("t1"), [LEDGER_LIST])


if __name__ == "__main__":
    unittest.main()
