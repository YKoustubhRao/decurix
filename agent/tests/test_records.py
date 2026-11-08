"""
The record ledger (computer/records.py): a record's intent declared from its source, the form checked against it
before the save, the page read back after. Found live (2026-10-26, after ~220 runs): a form's default saved in place
of the document's number, another record's figure in a form, saves reported and never read back, the wrong book open -
none decided in code until now.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import core, records
from decurix_agent.computer.errors import ComputerError

BILL = """TAX INVOICE
Invoice No: 1188          Date: 01/10/2026
Bill to: Coastal Infra Projects, Plot 12, Baikampady, Mangaluru 575011, Karnataka
GSTIN: 29AAGFC5512K1ZC
Description                     HSN     Qty   Rate       Amount
Structural steel fabrication    7308    1     65,000.00  65,000.00
Taxable value                                            65,000.00
CGST @ 9%                                                 5,850.00
SGST @ 9%                                                 5,850.00
Grand total                                              76,700.00
"""

FIELDS = {"No.": "1188", "Date": "01-10-2026", "Party A/c name": "Coastal Infra Projects", "Place of supply": "Karnataka",
          "Particulars 1": "Sales@18%", "Amount 1": "65000.00", "Particulars 2": "CGST@9%", "Amount 2": "5850",
          "Particulars 3": "SGST@9%", "Amount 3": "5850"}


class Values(unittest.TestCase):
    def test_numbers_dates_and_text_compare_by_meaning(self):
        self.assertTrue(records.same_value("76,700.00", "76700"))
        self.assertTrue(records.same_value("Rs. 5,850.00", "5850"))
        self.assertFalse(records.same_value("1", "1188"))
        self.assertTrue(records.same_value("01-10-2026", "1/10/2026", order="day"))
        # (With no order known for either side, a date that reads two ways is never decided by its layout: W0.2.)
        self.assertIsNone(records.same_value("01-10-2026", "1/10/2026"))
        self.assertTrue(records.same_value("1-Oct-26", "2026-10-01"))
        self.assertFalse(records.same_value("01-10-2026", "10-01-2026"))
        self.assertTrue(records.same_value("Coastal  Infra Projects", "coastal infra projects"))
        self.assertFalse(records.same_value("", ""))

    def test_a_value_stands_in_its_source_in_any_form(self):
        self.assertTrue(records.stands_in("76700", BILL))
        self.assertTrue(records.stands_in("5850.00", BILL))
        self.assertTrue(records.stands_in("1188", BILL))
        with mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"), \
                mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day"):
            self.assertTrue(records.stands_in("1-10-2026", BILL))
            self.assertTrue(records.stands_in("2026-10-01", BILL))
        # With no order known, the bill's 01/10/2026 may be 10 January: an ISO date is not taken as standing in it
        # (W0.2 - it was read day first by default).
        with mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: None), \
                mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: None):
            self.assertFalse(records.stands_in("2026-10-01", BILL))
        self.assertTrue(records.stands_in("Coastal Infra Projects", BILL))
        self.assertTrue(records.stands_in("29AAGFC5512K1ZC", BILL))
        self.assertFalse(records.stands_in("76800", BILL))
        self.assertFalse(records.stands_in("1189", BILL))
        self.assertFalse(records.stands_in("Hebri Agro", BILL))
        # (A number inside a longer number is not that number: 850 is not in 5,850.00.)
        self.assertFalse(records.stands_in("850", BILL))

    def test_an_identifier_stands_in_only_as_a_whole_token(self):
        # Recheck (2026-11-02): another document's longer number counted as this one's read back.
        self.assertFalse(records.stands_in("INV-1234", "Invoice INV-12345"))
        self.assertFalse(records.stands_in("INV-1234", "Invoice XINV-1234 saved"))
        self.assertFalse(records.stands_in("B9", "Qty 9 | B91"))
        self.assertTrue(records.stands_in("INV-1234", "Invoice INV-1234 saved"))
        self.assertTrue(records.stands_in("INV-1234", "Invoice No: inv 1234, dated"))
        self.assertTrue(records.stands_in("INV-1234", "INV-1234/A"))
        self.assertTrue(records.stands_in("B9", "Voucher B9 | Alpha Farms"))


class Labels(unittest.TestCase):
    def test_labels_match_by_their_words(self):
        self.assertTrue(records.label_matches("No.", "No."))
        self.assertTrue(records.label_matches("Number", "No."))
        self.assertTrue(records.label_matches("Party", "Party A/c name"))
        self.assertTrue(records.label_matches("Date", "Invoice date (DD/MM/YYYY)"))
        self.assertFalse(records.label_matches("Amount 1", "Amount 2"))
        self.assertTrue(records.label_matches("Amount 2", "Amount 2"))
        self.assertFalse(records.label_matches("Date", "Due date"))  # (a different field: its words are not these)

    def test_each_form_field_is_used_once_exact_first(self):
        intent = {"Date": {"intent": "01-10-2026"}, "Due date": {"intent": "31-10-2026"}, "Amount 1": {"intent": "1"}}
        form = [("Due date", "x"), ("Date", "y"), ("Amount 1", "z"), ("Amount 2", "w")]
        got = records.match_fields(intent, form)
        self.assertEqual(got, [("Date", "Date", "y"), ("Due date", "Due date", "x"), ("Amount 1", "Amount 1", "z")])


class Declaring(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bill = Path(self.tmp.name) / "1188 Coastal Infra Projects.txt"
        self.bill.write_text(BILL, encoding="utf-8")
        for p in (mock.patch.object(records, "LEDGER_DIR", Path(self.tmp.name) / "ledger"),
                  mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"),
                  mock.patch("decurix_agent.task_context.current_request", lambda: "Enter the bills in the books of Karkala Steel Fabricators 2026-27"),
                  mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: "a look at the screen then. combobox \"Particulars 1\" holds: Sales@18% | CGST@9% | SGST@9% | Karnataka"),
                  mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []),
                  # The books of an org whose dates are written day first (the control plane's date_order, W0.2): the
                  # bill's 01/10/2026 could be read either way, and only the order decides it is 1 October.
                  mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day")):
            p.start()
            self.addCleanup(p.stop)

    def test_values_from_the_source_and_the_screen_are_kept_with_where_from(self):
        rec, lines = records.declare("1188", str(self.bill), FIELDS, scope="Karkala Steel Fabricators 2026-27")
        self.assertEqual(rec["fields"]["No."], {"intent": "1188", "from": "source"})
        self.assertEqual(rec["fields"]["Amount 1"]["from"], "source")
        self.assertEqual(rec["fields"]["Particulars 1"]["from"], "seen")  # a ledger name chosen on the screen
        self.assertEqual(rec["fields"]["Place of supply"]["from"], "source")
        self.assertIn("10 field(s)", lines[0])
        self.assertEqual(len(records.load()), 1)

    def test_a_value_in_no_source_refuses_the_whole_declaration(self):
        wrong = dict(FIELDS, **{"Amount 1": "65500.00"})
        with self.assertRaises(ComputerError) as caught:
            records.declare("1188", str(self.bill), wrong)
        self.assertIn("Amount 1: 65500.00", str(caught.exception))
        self.assertEqual(records.load(), [])

    def test_declared_again_replaces_the_open_record(self):
        records.declare("1188", str(self.bill), FIELDS)
        records.declare("1188", str(self.bill), dict(FIELDS, **{"Amount 2": "5850.00"}))
        self.assertEqual(len(records.load()), 1)
        self.assertEqual(records.open_record(records.load())["fields"]["Amount 2"]["intent"], "5850.00")

    def test_presave_blocks_a_default_left_and_another_records_value(self):
        rec, _ = records.declare("1188", str(self.bill), FIELDS)
        form = [("Voucher type", "Sales"), ("No.", "1"), ("Date", "01-10-2026"), ("Party A/c name", "Coastal Infra Projects"),
                ("Place of supply", "Karnataka"), ("Particulars 1", "Sales@18%"), ("Amount 1", "65,000.00"),
                ("Particulars 2", "CGST@9%"), ("Amount 2", "5,850.00"), ("Particulars 3", "SGST@9%"), ("Amount 3", "5,850.00")]
        blocking, advisory, matched = records.presave(rec, form)
        self.assertEqual(blocking, ["No. holds '1' where the record says '1188'"])
        self.assertEqual(matched, 10)
        form[1] = ("No.", "1188")
        form[6] = ("Amount 1", "1,74,640.00")
        blocking, _, _ = records.presave(rec, form)
        self.assertEqual(blocking, ["Amount 1 holds '1,74,640.00' where the record says '65000.00'"])
        form[6] = ("Amount 1", "65000")
        blocking, advisory, matched = records.presave(rec, form)
        self.assertEqual(blocking, [])
        self.assertTrue(any("Voucher type=Sales" in a for a in advisory))

    def test_the_click_is_stopped_then_let_through_without_a_listing(self):
        rec, _ = records.declare("1188", str(self.bill), FIELDS)
        form = [(k, v) for k, v in FIELDS.items()]
        form[0] = ("No.", "1")
        held, clean, lines = core._ledger_check(form)
        self.assertIn("No. holds '1' where the record says '1188'", held)
        self.assertFalse(clean)
        form[0] = ("No.", "1188")
        held, clean, lines = core._ledger_check(form)
        self.assertEqual(held, "")
        self.assertTrue(clean)
        self.assertIn("record 1188: 10 of 10 declared values match the form", lines[0])

    def test_the_scope_absent_from_the_window_stops_the_save_once(self):
        records.declare("1188", str(self.bill), FIELDS, scope="Karkala Steel Fabricators 2026-27")
        form = [(k, v) for k, v in FIELDS.items()]
        wrong = {"active_title": "Accounting Voucher Creation - Books", "elements": [
            {"role": "heading", "name": "Hosmane Pumps & Motors 2026-27", "group": 1}]}
        with mock.patch.object(core, "observe", lambda remember=True: wrong), \
                mock.patch("decurix_agent.computer.core.active_group", lambda obs: 1):
            held, clean, _ = core._ledger_check(form)
            self.assertIn("shows none of: karkala, steel, fabricators", held)
            held, clean, lines = core._ledger_check(form)  # (stopped once: the second time it is advisory)
            self.assertEqual(held, "")
            self.assertTrue(any("scope words" in l for l in lines))
        right = {"active_title": "Accounting Voucher Creation - Karkala Steel Fabricators 2026-27 - Books", "elements": []}
        with mock.patch.object(core, "observe", lambda remember=True: right), \
                mock.patch("decurix_agent.computer.core.active_group", lambda obs: None):
            held, clean, lines = core._ledger_check(form)
            self.assertEqual(held, "")
            self.assertFalse(any("scope words" in l for l in lines))

    def test_the_scope_is_shown_by_a_run_of_its_words(self):
        # Found live: the model wrote the scope long; the window names the client only.
        shown = "Accounting Voucher Creation - Karkala Steel Fabricators 2026-27 - Books"
        self.assertEqual(records.scope_missing("Karkala Steel Fabricators October 2026 sales bill 1188", shown, ""), [])
        self.assertEqual(records.scope_missing("Karkala Steel Fabricators 2026-27", "Voucher", "Company: Karkala Steel Fabricators"), [])
        self.assertTrue(records.scope_missing("Hosmane Pumps 2026-27", shown, ""))  # (a year in common is no name)
        self.assertEqual(records.scope_missing("Karkala", shown, ""), [])
        self.assertEqual(records.scope_missing("Hosmane", shown, ""), ["hosmane"])

    def test_readback_marks_what_the_page_shows(self):
        rec, _ = records.declare("1188", str(self.bill), FIELDS)
        page = ("Sales Voucher No. 1188 | 1-Oct-26 | Coastal Infra Projects | Karnataka | Sales@18% 65,000.00 | "
                "CGST@9% 5,850.00 | SGST@9% 5,850.00 | Total 76,700.00")
        lines = records.readback(rec, "Sales Voucher No. 1188 - Books", page, "page is now /voucher?id=3&msg=saved")
        self.assertIn("shows 10 of 10 declared value(s) - all of them", lines[0])
        kept = records.load()[0]
        self.assertTrue(kept["saved_at"])
        self.assertEqual(kept["readback"]["absent"], [])
        self.assertIsNone(records.open_record(records.load()))
        # A second record declared after it is the open one; its page misses a value.
        rec2, _ = records.declare("1188-again", str(self.bill), FIELDS)
        lines = records.readback(rec2, "Day Book", "Day Book | 1-Oct-26 | Sales | 1188 | 76,700.00", "saved")
        self.assertIn("NOT shown: Party A/c name=Coastal Infra Projects", lines[0])
        self.assertIn("Party A/c name", records.load()[1]["readback"]["absent"])
        listing = "\n".join(records.listing())
        self.assertIn("- 1188 (from", listing)
        self.assertIn("saved and read back", listing)
        self.assertIn("[source; read back: ABSENT]", listing)

    def test_a_party_chosen_from_the_applications_list_is_a_source_and_never_blocks(self):
        # Found live (2026-10-26): the ledger found by its tax number was named otherwise than the bill wrote it.
        chosen = [{"kind": "select", "outcome": "done", "option": "Malnad Agro Foods Pvt Ltd",
                   "summary": 'select combobox "Party A/c name" - took the entry "Malnad Agro Foods Pvt Ltd" for "Malnad Agro Foods & Beverages"',
                   # (Who chose the entry is the action's structured fact since review round 10, R9-B1 - never its
                   # summary's words: found by its tax number, an identifier chose it.)
                   "taken": "Malnad Agro Foods Pvt Ltd", "resolved_by": "identifier"}]
        with mock.patch("decurix_agent.computer.evidence._task_events", lambda task_id=None: chosen):
            rec, lines = records.declare("1188", str(self.bill), dict(FIELDS, **{"Party A/c name": "Malnad Agro Foods Pvt Ltd"}))
            self.assertEqual(rec["fields"]["Party A/c name"]["from"], "chosen")
            # Declared with the document's own spelling, the form holding the application's entry is not a mismatch.
            rec, _ = records.declare("1188", str(self.bill), FIELDS)
            form = [(k, v) for k, v in FIELDS.items()]
            form[2] = ("Party A/c name", "Malnad Agro Foods Pvt Ltd")
            blocking, advisory, _ = records.presave(rec, form)
            self.assertEqual(blocking, [])
            self.assertTrue(any("application's own entry 'Malnad Agro Foods Pvt Ltd'" in a for a in advisory))
            # ... and the read-back looks for the application's entry, which is what the saved page shows.
            records.note_presave(rec, blocking)
            kept = records.open_record(records.load())
            lines = records.readback(kept, "Voucher 1188", "Sales Voucher No. 1188 | Malnad Agro Foods Pvt Ltd | 65,000.00 | 5,850.00 | 76,700.00 | 1-Oct-26 | Karnataka | Sales@18% | CGST@9% | SGST@9%", "saved")
            self.assertNotIn("NOT shown", lines[0], lines)
        # Without the choice on record, the same form is stopped.
        rec, _ = records.declare("1189", str(self.bill), FIELDS)
        blocking, _, _ = records.presave(rec, form)
        self.assertEqual(len(blocking), 1)

    def test_a_bracketed_code_does_not_make_another_value(self):
        self.assertTrue(records.same_value("Karnataka (29)", "Karnataka"))
        self.assertTrue(records.same_value("Sales GST 5%", "Sales GST 5% (S5)"))
        self.assertFalse(records.same_value("Karnataka (29)", "Kerala (32)"))

    def test_a_photographed_bill_is_read_by_ocr_and_unread_values_are_kept_as_pictures(self):
        photo = Path(self.tmp.name) / "IMG-0001.jpg"
        photo.write_bytes(b"\xff\xd8\xff\xe0 not really a picture")
        with mock.patch("decurix_agent.tools.office._ocr_file", lambda p: "Invoice 1188  Total 76,700.00  Coastal Infra"):
            rec, lines = records.declare("1188", str(photo), {"No.": "1188", "Amount 1": "65000", "Party": "Coastal Infra Projects"})
        self.assertEqual(rec["fields"]["No."]["from"], "source")
        self.assertEqual(rec["fields"]["Amount 1"]["from"], "picture")
        self.assertEqual(rec["fields"]["Party"]["from"], "picture")
        self.assertTrue(any("NOT confirmed by OCR" in l and "Amount 1=65000" in l for l in lines))

    def test_fields_parse_from_json_or_lines(self):
        self.assertEqual(records.parse_fields('{"No.": 1188, "Date": "01-10-2026"}'), {"No.": "1188", "Date": "01-10-2026"})
        self.assertEqual(records.parse_fields("No.: 1188\nDate: 01-10-2026"), {"No.": "1188", "Date": "01-10-2026"})
        with self.assertRaises(ComputerError):
            records.parse_fields("[1, 2]")


if __name__ == "__main__":
    unittest.main()


class WholeCodeTests(Declaring):
    """Found live (2026-10-26): '1234' declared for a bill written 'ABC-1234' was accepted and saved under the wrong number."""

    def test_a_code_is_the_documents_only_whole(self):
        text = "TAX INVOICE\nInvoice No: ABC-1234  Date: 02-11-2026\nTotal 54,300.00"
        self.assertEqual(records.written_whole("1234", text), "ABC-1234")
        self.assertEqual(records.written_whole("1188", "Invoice INV1188 of 1 Oct"), "INV1188")
        self.assertIsNone(records.written_whole("ABC-1234", text))
        self.assertIsNone(records.written_whole("1234", "Bill no 1234 dated 2 Nov"))
        self.assertIsNone(records.written_whole("98700", text))  # a figure, not a code
        self.assertIsNone(records.written_whole("2026", text))  # a date's year is not a code written joined

    def test_the_declaration_refuses_a_part_of_a_code(self):
        bill = Path(self.tmp.name) / "ABC-1234.txt"
        bill.write_text("TAX INVOICE\nInvoice No: ABC-1234\nDate: 02-11-2026\nParty: Some Farmers\nTotal 54300.00\n", encoding="utf-8")
        with self.assertRaises(ComputerError) as cm:
            records.declare("ABC1234", str(bill), {"No.": "1234", "Date": "02-11-2026"})
        self.assertIn("the document writes it as 'ABC-1234'", str(cm.exception))
        rec, _ = records.declare("ABC1234", str(bill), {"No.": "ABC-1234", "Date": "02-11-2026"})
        self.assertEqual(rec["fields"]["No."]["from"], "source")


class AUnitTheFieldStates(Declaring):
    """Review round 3, R5: a unit on one side only made every GST rate wait ("18%" against the form's "18"); the form's
    label decides it, and without a label that states the unit it still waits."""

    def _rec(self, fields):
        doc = Path(self.tmp.name) / "line.txt"
        doc.write_text("Rate: 18%\nWeight: 15 kg\nQuantity: 2 Nos\n", encoding="utf-8")
        rec, _ = records.declare("line", str(doc), fields)
        return rec

    def test_the_label_states_the_unit(self):
        rec = self._rec({"Rate": "18%", "Weight": "15 kg", "Quantity": "2 Nos"})
        blocking, _, _ = records.presave(rec, [("Rate (%)", "18.000"), ("Weight (kg)", "15"), ("Quantity (Nos.)", "2")])
        self.assertEqual(blocking, [])
        blocking, _, _ = records.presave(rec, [("Rate (%)", "12"), ("Weight (kg)", "15"), ("Quantity (Nos.)", "2")])
        self.assertEqual(blocking, ["Rate (%) holds '12' where the record says '18%'"])

    def test_no_label_or_another_unit_still_waits_and_says_how(self):
        rec = self._rec({"Rate": "18%", "Weight": "15 kg"})
        blocking, _, _ = records.presave(rec, [("Rate", "18"), ("Weight (lb)", "15")])
        self.assertEqual(len(blocking), 2, blocking)
        # It waits on the person, who is asked once for this field (unit_answers), never told to drop the unit.
        self.assertTrue(all("this needs the person" in b and "asked once" in b for b in blocking), blocking)
        blocking, _, _ = records.presave(rec, [("Rate (%)", "18"), ("Weight kg/lb", "15")])  # alternatives state none
        self.assertEqual(len(blocking), 1, blocking)
        self.assertIn("Weight kg/lb", blocking[0])


class TheApplicationsOwnEntryWhenUndecided(Declaring):
    """Review round 3, nit 4: the application's own entry was tried only on "differs", so an undecided pair - a figure
    against an identifier - blocked without trying it."""

    def test_an_undecided_pair_is_rescued_by_the_applications_entry(self):
        rec, _ = records.declare("1188", str(self.bill), FIELDS)
        form = [(k, v) for k, v in FIELDS.items()]
        form[0] = ("No.", "INV-1188")
        self.assertIsNone(records.compare_shown("INV-1188", "1188").same)
        blocking, _, _ = records.presave(rec, form)
        self.assertEqual(len(blocking), 1, blocking)
        chosen = [{"kind": "select", "outcome": "done", "option": "INV-1188"}]
        with mock.patch("decurix_agent.computer.evidence._task_events", lambda task_id=None: chosen):
            blocking, advisory, _ = records.presave(rec, form)
        self.assertEqual(blocking, [])
        self.assertTrue(any("application's own entry 'INV-1188'" in a for a in advisory), advisory)


class IdentifierOnTheForm(Declaring):
    """Found live (2026-10-27): a customer ledger saved with its identifier left empty; nothing could resolve it after."""

    def test_an_empty_identifier_field_blocks_the_save(self):
        bill = Path(self.tmp.name) / "party.txt"
        bill.write_text("Customer: Some Builders\nGSTIN: 29ABCDE1234F1Z5\nState: Karnataka\n", encoding="utf-8")
        rec, _ = records.declare("party", str(bill), {"Name": "Some Builders", "GSTIN/UIN": "29ABCDE1234F1Z5", "State": "Karnataka"})
        blocking, advisory, _ = records.presave(rec, [("Name", "Some Builders"), ("GSTIN/UIN", ""), ("State", "Karnataka")])
        self.assertTrue(any("GSTIN/UIN is empty" in b and "saved with its identifier" in b for b in blocking), (blocking, advisory))
        blocking, advisory, _ = records.presave(rec, [("Name", "Some Builders"), ("GSTIN/UIN", "29ABCDE1234F1Z5"), ("State", "")])
        self.assertEqual(blocking, [])
        self.assertTrue(any("State is empty" in a for a in advisory))


class DeclaredFromAPicture(Declaring):
    """Found live (2026-10-27): a photographed bill's figures, declared from the photo, were refused as invented."""

    def test_a_value_declared_from_a_picture_is_not_invented(self):
        photo = Path(self.tmp.name) / "IMG-0001.jpg"
        photo.write_bytes(b"\xff\xd8\xff\xe0 not a real picture")
        with mock.patch.object(records, "_source_text", lambda source: ""):
            records.declare("BILL1", str(photo), {"Amount 1": "29500.00", "Amount 2": "737.50"})
        self.assertIn("29500.00", core._declared_values())
        typed = mock.patch("decurix_agent.computer.evidence.entered", lambda *a, **k: ["29,500.00", "737.50", "1,23,456.00"])
        never = mock.patch("decurix_agent.long_task.work_sources", lambda request: mock.Mock(unsourced=lambda v: True))
        with typed, never:
            got = core._unsourced_fields([("Amount 1", "29,500.00"), ("Amount 2", "737.50"), ("Amount 3", "1,23,456.00")])
        self.assertEqual(got, [("Amount 3", "1,23,456.00")])


class TheRecordForTheForm(unittest.TestCase):
    """Found live (2026-10-27): a customer form was held against a bill's declared record."""

    def test_a_form_the_record_does_not_fit_gets_no_record(self):
        bill = {"key": "B9", "saved_at": None, "fields": {
            "No.": {"intent": "9"}, "Date": {"intent": "2026-11-28"}, "Party A/c name": {"intent": "Alpha Farms"},
            "Amount 1": {"intent": "1000"}, "Amount 2": {"intent": "90"}, "Amount 3": {"intent": "90"}}}
        party_form = [("Name", "Beta Growers"), ("Under", "Sundry Debtors"), ("Address", "Market Yard"),
                      ("Pincode", "416312"), ("GSTIN/UIN", "27AAAAA0000A1Z5")]
        self.assertIsNone(records.open_record([bill], "Beta Growers | Sundry Debtors", form=party_form))
        voucher_form = [("Voucher type", "Sales"), ("No.", "1"), ("Date", ""), ("Party A/c name", ""),
                        ("Amount 1", ""), ("Amount 2", ""), ("Amount 3", "")]
        self.assertIs(records.open_record([bill], "Sales | 1", form=voucher_form), bill)
        # Its key's number in its own number field binds it (the form shows the voucher's number, not the key).
        numbered = [("Voucher type", "Sales"), ("No.", "9"), ("Party A/c name", "Alpha Farms")]
        self.assertIs(records.open_record([bill], "Sales | 9 | Alpha Farms", form=numbered), bill)

    def test_a_key_number_in_another_field_binds_nothing(self):
        # Recheck (2026-11-02): record B9 was bound to a form whose Qty was 9.
        bill = {"key": "B9", "saved_at": None, "fields": {
            "No.": {"intent": "9"}, "Date": {"intent": "2026-11-28"}, "Party A/c name": {"intent": "Alpha Farms"},
            "Amount 1": {"intent": "1000"}, "Amount 2": {"intent": "90"}, "Amount 3": {"intent": "90"}}}
        other = [("Item", "Widget"), ("Qty", "9")]
        self.assertIsNone(records.open_record([bill], "Widget | 9", form=other))
        # Two records open: the form numbered 1 with a quantity of 9 is B1's, not B9's.
        b1 = {"key": "B1", "saved_at": None, "fields": {"No.": {"intent": "1"}, "Party A/c name": {"intent": "Beta"}}}
        form = [("No.", "1"), ("Party A/c name", ""), ("Qty", "9")]
        self.assertIs(records.open_record([b1, bill], "1 | 9", form=form), b1)
        # No field (or more than one) holds the key's number on the record: which field it is cannot be told.
        vague = {"key": "B9", "saved_at": None, "fields": {"Qty": {"intent": "9"}, "No.": {"intent": "9"}}}
        self.assertIsNone(records.open_record([vague], "9", form=[("No.", "9"), ("Item", "Widget")]))
