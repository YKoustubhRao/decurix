"""
Review (2026-11-02): ways a worker could corrupt or misread a client's figures and identifiers - Indian number and
address forms above all. Each test is the one case that went wrong: a bank account number written as 1.23E+17, a
docx replace that never ended, two document numbers taken for one, a leading zero dropped and "held", "Rs.78" read as
0.78, Indian digit grouping refused, "Rent" choosing "Rent Paid", "Post" pressing "Repost", a 1% rate read as 100%, a
power in a formula, "po1" taken for "po12.pdf", and an address's PIN code withheld as a secret.
"""

import threading
import unittest

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent import stage_facts
from decurix_agent.computer import cdp, constraints, core, privacy, records
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError
from decurix_agent.tools import office


class LongDigitsStayText(unittest.TestCase):
    def test_identifiers_in_digits_are_not_made_numbers(self):
        for ident in ("123456789012345678", "234567890123", "0012345", "9876543210123456"):
            self.assertEqual(office._cell_value(ident), ident)
        self.assertEqual(office._cell_value("125000000"), 125000000)  # a 12.5 crore amount is still a number
        self.assertEqual(office._cell_value("4500.50"), 4500.5)


class DocxReplaceEnds(unittest.TestCase):
    def test_a_value_holding_its_key_is_replaced_once(self):
        from lxml import etree
        w = office._W[1:-1]
        p = etree.fromstring(f'<w:p xmlns:w="{w}"><w:r><w:t>Q3 report; Q</w:t></w:r><w:r><w:t>3 total</w:t></w:r></w:p>')
        counts = {}
        worker = threading.Thread(target=office._replace_in_paragraph, args=(p, {"Q3": "Q3 FY26"}, counts), daemon=True)
        worker.start()
        worker.join(10)
        self.assertFalse(worker.is_alive(), "the replace never ended")
        self.assertEqual("".join(t.text or "" for t in p.iter(office._W + "t")), "Q3 FY26 report; Q3 FY26 total")
        self.assertEqual(counts, {"Q3": 2})


class IdentifiersAreNotNumbers(unittest.TestCase):
    def test_two_document_numbers_differ(self):
        self.assertFalse(records.same_value("INV-1234", "PO-1234"))
        self.assertIsNone(records.as_number("INV-1234"))
        self.assertFalse(records.same_value("0012345", "12345"))
        self.assertEqual(records.as_number("Rs. 5,850.00"), 5850)
        self.assertEqual(records.as_number("Rs.78"), 78)
        self.assertEqual(records.as_number("1,25,000/-"), 125000)
        self.assertTrue(records.same_value("₹1,25,000", "125000.00"))

    def test_to_date_is_not_date(self):
        self.assertFalse(records.label_matches("Date", "To Date"))
        self.assertTrue(records.label_matches("Invoice Date", "Invoice Date *"))

    def test_a_dropped_leading_zero_is_not_held(self):
        self.assertFalse(core._cell_holds("0012345", "12345"))
        self.assertTrue(core._cell_holds("0012345", "0012345"))
        self.assertTrue(core._cell_holds("1250", "1,250.00"))


class CurrencyAndGrouping(unittest.TestCase):
    def test_the_dot_after_rs_is_no_decimal_point(self):
        for shown in ("Rs.78", "Rs 78", "₹78", "INR 78", "Rs. 78.00"):
            self.assertEqual(core._numeric(shown), 78, shown)

    def test_indian_grouping_is_a_number(self):
        for figure in ("1,25,000", "12,34,56,789", "1,250,000.50", "99,999", "-4,50,000.00"):
            self.assertTrue(constraints.NUMBER_SHAPE.match(figure), figure)
        for not_one in ("1,2,3", "12,3456", "1,25,00"):
            self.assertFalse(constraints.NUMBER_SHAPE.match(not_one), not_one)


class ChoosingByName(unittest.TestCase):
    OPTIONS = [{"label": "-- Select --", "value": ""}, {"label": "Rent Paid", "value": "1"},
               {"label": "Rent Received", "value": "2"}, {"label": "29-Karnataka", "value": "3"},
               {"label": "IT", "value": "4"}, {"label": "Current Assets", "value": "5"}]

    def test_two_that_fit_are_not_chosen_between(self):
        with self.assertRaises(ComputerError) as caught:
            cdp.pick_option(self.OPTIONS, "Rent")
        self.assertIn("Rent Paid", str(caught.exception))
        self.assertEqual(cdp.pick_option(self.OPTIONS, "rent  paid"), 1)
        self.assertEqual(cdp.pick_option(self.OPTIONS, "Karnataka"), 3)
        self.assertEqual(cdp.pick_option(self.OPTIONS, "IT department"), 4)
        with self.assertRaises(ComputerError):
            cdp.pick_option(self.OPTIONS, "Curr")  # part of a word is no option

    def test_a_name_is_matched_as_whole_words(self):
        repost = {"ref": "e1", "role": "button", "name": "Repost", "path": "1"}
        post = {"ref": "e2", "role": "button", "name": "Post", "path": "2"}
        save_new = {"ref": "e3", "role": "button", "name": "Save & New", "path": "3"}
        self.assertIs(el.find([repost, post], role="button", name="Post"), post)
        with self.assertRaises(ComputerError):
            el.find([repost], role="button", name="Post")
        self.assertIs(el.find([repost, save_new], role="button", name="Save"), save_new)


class RatesAndFormulas(unittest.TestCase):
    def test_a_one_percent_rate_is_one_percent(self):
        work = {"qty": [("Qty", 10.0)], "price": [("Rate", 100.0)], "tax": [("GST %", 1.0)], "discount": []}
        self.assertTrue(stage_facts.reproduces(work, 1010.0))
        self.assertFalse(stage_facts.reproduces(work, 2000.0))

    def test_a_power_is_not_worked_out(self):
        self.assertIsNone(stage_facts.evaluate("=2**3", 2, [1, 2]))
        self.assertEqual(stage_facts.evaluate("=A2*B2", 2, [3, 4]), 12)


class FileNames(unittest.TestCase):
    def test_a_name_is_a_whole_token(self):
        self.assertFalse(stage_facts.names_in("po1", "po12"))
        self.assertTrue(stage_facts.names_in("PO-1", "po1_bill"))
        self.assertTrue(stage_facts.names_in("po1", "Bill PO 1"))
        self.assertFalse(stage_facts.names_in("po1", "xpo1"))


class PinCodeIsAnAddress(unittest.TestCase):
    def test_a_pin_code_is_not_a_secret(self):
        box = {"role": "textbox", "name": "PIN Code"}
        self.assertIsNone(privacy.secret_field(box))
        self.assertIsNone(privacy.secret_field({"role": "textbox", "name": "Pincode"}))
        self.assertIsNone(privacy.secret_field({"role": "textbox", "name": "PIN",
                                                "handle": {"autocomplete": "postal-code"}}))
        self.assertEqual(privacy.request_secrets("Ship to 12 MG Road, Udupi, PIN: 576104"), [])
        self.assertEqual(privacy.request_secrets("Address PIN code 576104"), [])

    def test_a_card_or_login_pin_still_is(self):
        self.assertTrue(privacy.secret_field({"role": "textbox", "name": "ATM PIN"}))
        self.assertTrue(privacy.secret_field({"role": "textbox", "name": "PIN"}))
        self.assertIn("4821", privacy.request_secrets("the PIN: 4821"))
        self.assertIn("576104", privacy.request_secrets("login PIN: 576104"))
        self.assertIn("123456", privacy.request_secrets("UPI PIN is 123456"))


if __name__ == "__main__":
    unittest.main()
