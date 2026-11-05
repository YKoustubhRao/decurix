"""
Where a produced table's bank identifiers come from (payees.py), whatever its headers say.

Review 2026-11-03 (F-01): whether a table was a payment file was read from header words, so a payroll ("Net Salary",
"Wages", "Fees") was never checked while some customer reports were blocked. Now every bank identifier a produced table
holds must come from an input - a record, a document, a message the request gave, or the request itself.
(F-04): "A.B.C. Traders" is "ABC Traders"; a payee named otherwise than the record's holder of its account is asked
about with both names, and blocked only when it is plainly another party's account. (F-03): a party's address is taken
only for a row of that very name.

The parties and numbers are invented.
"""

import csv
import tempfile
import unittest
from pathlib import Path

from decurix_agent import payees


def table(path, rows):
    with open(path, "w", newline="", encoding="utf-8") as f:
        csv.writer(f).writerows(rows)
    return str(path)


class _Dir(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)


class BankColumns(unittest.TestCase):
    def test_bank_detail_and_account_columns(self):
        self.assertEqual(payees.bank_columns(["Vendor", "Account No", "IFSC", "Amount"]), [1, 2])
        self.assertEqual(payees.bank_columns(["Customer", "Account"]), [1])
        self.assertEqual(payees.bank_columns(["Payee", "UPI", "Amount"]), [1])

    def test_a_ledgers_account_is_no_banks(self):
        for header in (["GL Account", "Debit", "Credit"], ["Account Code", "Account Name", "Opening Balance"],
                       ["Item", "Stock Account", "Qty"]):
            self.assertEqual(payees.bank_columns(header), [], header)


class AnIdentifierMustComeFromAnInput(_Dir):
    def setUp(self):
        super().setUp()
        self.master = table(self.dir / "staff.csv", [["Employee", "Bank Account", "IFSC"],
                                                     ["Party One", "50100000000011", "BANK0000011"],
                                                     ["Party Two", "50100000000022", "BANK0000022"]])

    def test_a_payout_to_an_account_in_no_input_is_found_under_any_headers(self):
        for i, header in enumerate((["Employee Name", "Bank Account No", "IFSC", "Net Salary"],
                                    ["Worker", "Account", "IFSC", "Wages"],
                                    ["Consultant", "A/c No", "IFSC", "Fees"],
                                    ["Name", "Account No", "IFSC", "Commission"],
                                    ["नाम", "Account", "IFSC", "राशि"],
                                    ["Employee", "Account No", "IFSC"])):
            run = table(self.dir / f"run{i}.csv", [header, ["Party One", "50100000000011", "BANK0000011"] + ["100"],
                                                   ["Party Two", "77700000000099", "BANK0000022"] + ["100"]])
            with self.subTest(header=header):
                found = payees.mismatches("", [run], found=([self.master], []))
                self.assertEqual(len(found), 1, found)
                self.assertTrue(found[0].startswith("PAYMENT TO AN ACCOUNT ON NO RECORD:"), found)
                self.assertIn("77700000000099", found[0])

    def test_a_statement_whose_accounts_come_from_the_inputs_is_not_found(self):
        statement = table(self.dir / "statement.csv", [["Customer", "Bank Account", "Total Sales"],
                                                       ["Party One", "50100000000011", "45000"]])
        self.assertEqual(payees.mismatches("", [statement], found=([self.master], [])), [])
        self.assertEqual(payees.unchecked("", [statement], found=([self.master], [])), [])

    def test_an_account_the_request_gives_is_from_an_input(self):
        run = table(self.dir / "run.csv", [["Payee", "Account No", "Amount"], ["Party Three", "60600000000033", "100"]])
        request = "Pay Party Three 100 to account 60600000000033"
        self.assertEqual(payees.mismatches(request, [run], found=([self.master], [])), [])

    def test_an_account_from_a_document_no_record_confirms_is_not_checked(self):
        document = self.dir / "bill.txt"
        document.write_text("Bill from Party Three. Pay to account 60600000000033.", encoding="utf-8")
        run = table(self.dir / "run.csv", [["Payee", "Account No", "Amount"], ["Party Three", "60600000000033", "100"]])
        found = ([self.master], [], [str(document)])
        self.assertEqual(payees.mismatches("", [run], found=found), [])
        said = payees.unchecked("", [run], found=found)
        self.assertTrue(said and said[0].startswith("PAYEE NOT CONFIRMED:"), said)
        self.assertIn("bill.txt", said[0])

    def test_with_nothing_to_check_against_the_payments_are_not_checked(self):
        run = table(self.dir / "run.csv", [["Vendor", "Account No", "IFSC", "Amount"],
                                          ["Party One", "50100000000011", "BANK0000011", "45000"]])
        said = payees.unchecked("", [run], found=([], []))
        self.assertEqual(len(said), 1)
        self.assertTrue(said[0].startswith("PAYEES NOT CHECKED: run.csv pays Party One"))
        self.assertEqual(payees.mismatches("", [run], found=([], [])), [])

    def test_the_approval_card_shows_where_each_account_came_from(self):
        run = table(self.dir / "run.csv", [["Employee", "Account", "Net Salary"],
                                          ["Party One", "50100000000011", "100"],
                                          ["Party Two", "77700000000099", "100"]])
        summary = payees.payments_summary("", [run], found=([self.master], []))
        self.assertIn("- Party One: 50100000000011 (from staff.csv)", summary)
        self.assertIn("- Party Two: 77700000000099 (NOT IN ANY INPUT)", summary)


class AnIbanIsOneByItsChecksum(_Dir):
    """Review 2026-11-03 (round 2): any token of IBAN shape counted, so a reference of letters and digits in a summary
    blocked correct work. An IBAN is one only when its country's length and the ISO 13616 mod-97 check hold."""

    def test_the_checksum_and_the_length_decide(self):
        self.assertTrue(payees._is_iban("GB82WEST12345698765432"))     # the standard's example
        self.assertTrue(payees._is_iban("DE89370400440532013000"))
        self.assertFalse(payees._is_iban("GB82WEST12345698765433"))    # one digit off
        self.assertFalse(payees._is_iban("AB12345678901234567"))       # no such country
        self.assertFalse(payees._is_iban("GB82WEST123456987654"))      # GB's length is 22

    def test_a_reference_of_letters_and_digits_in_a_summary_is_correct_work(self):
        master = table(self.dir / "staff.csv", [["Employee", "Bank Account"], ["Party One", "50100000000011"]])
        summary = table(self.dir / "summary.csv", [["Order Ref", "Customer", "Amount"],
                                                   ["OR20260000012345", "Party One", "4500"],
                                                   ["SO11202600004431", "Party Two", "1200"]])
        self.assertEqual(payees.mismatches("", [summary], found=([master], [])), [])
        self.assertEqual(payees.unchecked("", [summary], found=([master], [])), [])

    def test_an_iban_in_no_input_is_still_found(self):
        master = table(self.dir / "staff.csv", [["Employee", "IBAN"], ["Party One", "DE89370400440532013000"]])
        run = table(self.dir / "run.csv", [["Payee", "Reference", "Amount"], ["Party Two", "GB82WEST12345698765432", "9"]])
        found = payees.mismatches("", [run], found=([master], []))
        self.assertTrue(found and "GB82WEST12345698765432" in found[0], found)


class AnAccountReadOffTheScreen(_Dir):
    """Review 2026-11-03 (round 2): an account the worker read off an application's screen was "in no input" and
    blocked. It is a source that does not say whose the account is: not checked, never refused for that."""

    def test_it_is_not_checked_never_refused(self):
        from unittest import mock
        master = table(self.dir / "staff.csv", [["Employee", "Bank Account"], ["Party One", "50100000000011"]])
        run = table(self.dir / "run.csv", [["Payee", "Account No", "Amount"], ["Party Three", "60600000000033", "100"]])
        with mock.patch("decurix_agent.computer.evidence.observed",
                        lambda task_id=None: ["read off the screen: Party Three | A/c 60600000000033"]):
            self.assertEqual(payees.mismatches("", [run], found=([master], [])), [])
            said = payees.unchecked("", [run], found=([master], []))
            summary = payees.payments_summary("", [run], found=([master], []))
        self.assertTrue(said and "read off the screen" in said[0], said)
        self.assertIn("read off the screen", summary)


class ANameWrittenOtherwise(_Dir):
    def test_dotted_initials_are_one_word(self):
        self.assertEqual(payees._name_tokens("A.B.C. Traders"), payees._name_tokens("ABC Traders"))
        self.assertEqual(payees._name_tokens("A. B. C. Traders"), ["abc", "traders"])
        master = table(self.dir / "master.csv", [["Vendor", "Account No", "IFSC"],
                                                 ["ABC Traders", "50100012345678", "BANK0000123"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount", "Account No", "IFSC"],
                                          ["A.B.C. Traders", "5000", "50100012345678", "BANK0000123"]])
        self.assertEqual(payees.mismatches("", [run], found=([master], [])), [])
        self.assertEqual(payees.unchecked("", [run], found=([master], [])), [])

    def test_a_payee_named_otherwise_than_the_accounts_holder_is_asked_about_with_both_names(self):
        master = table(self.dir / "master.csv", [["Vendor", "Account No", "IFSC"],
                                                 ["Shree Sample Packaging", "50100012345678", "BANK0000123"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount", "Account No", "IFSC"],
                                          ["Sample Packaging", "5000", "50100012345678", "BANK0000123"]])
        self.assertEqual(payees.mismatches("", [run], found=([master], [])), [])
        said = payees.unchecked("", [run], found=([master], []))
        self.assertTrue(said and all(s.startswith("PAYEE NOT CONFIRMED:") for s in said), said)
        self.assertIn('"Shree Sample Packaging"', said[0])
        self.assertIn('"Sample Packaging"', said[0])

    def test_a_payee_with_its_own_account_paid_to_another_partys_is_found(self):
        master = table(self.dir / "master.csv", [["Vendor", "Account No", "IFSC"],
                                                 ["Party One", "50100000000011", "BANK0000011"],
                                                 ["Party Two", "50100000000022", "BANK0000022"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount", "Account No", "IFSC"],
                                          ["Party One", "5000", "50100000000022", "BANK0000022"]])
        found = payees.mismatches("", [run], found=([master], []))
        self.assertTrue(found and found[0].startswith("PAYMENT TO ANOTHER PARTY'S ACCOUNT:"), found)
        self.assertIn("master.csv has it for Party Two", found[0])

    def test_one_account_paid_to_its_owner_and_to_another_payee_in_one_file_is_found(self):
        master = table(self.dir / "master.csv", [["Vendor", "Account No"], ["Party One", "50100000000011"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount", "Account No"],
                                          ["Party One", "5000", "50100000000011"],
                                          ["Party Nine", "7000", "50100000000011"]])
        found = payees.mismatches("", [run], found=([master], []))
        self.assertEqual(len(found), 1, found)
        self.assertIn("pays Party Nine", found[0])


class ThePartysAddress(_Dir):
    def test_a_name_that_is_part_of_another_rows_names_candidates_never_a_party(self):
        book = table(self.dir / "parties.csv", [["Party", "Email"],
                                                ["Shahid Enterprises", "accounts@shahid.example"],
                                                ["Lakeside Textiles", "kt@lakeside.example"]])
        with self.assertRaises(payees.PartyError) as caught:
            payees.party_address("Shah", book)
        self.assertIn("Shahid Enterprises", str(caught.exception))
        with self.assertRaises(payees.PartyError):
            payees.party_address("Textiles", book)
        self.assertEqual(payees.party_address("Shahid Enterprises Pvt Ltd", book), "accounts@shahid.example")
        self.assertEqual(payees.party_address("lakeside textiles", book), "kt@lakeside.example")


if __name__ == "__main__":
    unittest.main()
