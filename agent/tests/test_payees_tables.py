"""
Review 2026-11-03 (payees.py): any table with an "account" column and a column of figures was taken for a payment
file, so a sales report, a ledger or a stock table drew "PAYEES NOT CHECKED"; and a payee matched another party's row
by a run of its words ("Ram" in "Sita Ram Traders", "ABC Pvt Ltd" in "ABC Traders"), so another party's account passed
for the payee's. Each case both ways: a real payment file is still checked, the other tables are not payments.
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


class WhatIsAPaymentFile(unittest.TestCase):
    def test_payment_files(self):
        for header in (["Vendor", "Account No", "IFSC", "Amount"],
                       ["Serve Order", "Vendor", "Invoice No", "Amount Payable (Rs)", "Bank", "Account No", "IFSC"],
                       ["Employee", "Bank A/c", "Net Pay"],
                       ["Beneficiary", "Beneficiary Account", "Payment Value"],
                       ["Account Name", "Account Number", "Amount"],
                       ["Payee", "UPI", "Amount"],
                       # Round-3 recheck: bare account columns beside who is paid (round 2 caught these).
                       ["Payee", "A/c", "Amount"],
                       ["Party", "Account", "Amount"],
                       ["Beneficiary", "Acct", "Transfer"]):
            self.assertIsNotNone(payees.payment_table(header), header)

    def test_a_vendor_master_with_a_bare_account_column_holds_bank_accounts(self):
        self.assertEqual(payees.bank_columns(["Vendor", "Account"]), [1])
        self.assertEqual(payees.bank_columns(["Customer", "Account"]), [])

    def test_a_wrong_account_in_a_bare_account_payment_file_is_found(self):
        d = Path(tempfile.mkdtemp())
        master = table(d / "master.csv", [["Vendor", "Account"], ["Acme Traders", "50100123456789"]])
        run = table(d / "run.csv", [["Payee", "A/c", "Amount"], ["Acme Traders", "50100999999999", "45000"]])
        said = payees.mismatches("", [run], found=([master], []))
        self.assertTrue(said, "a wrong account in a bare-account payment file went through unchecked")

    def test_other_tables(self):
        for header in (["Customer", "Bank Account", "Total Sales"],  # a sales report that lists accounts
                       ["GL Account", "Debit", "Credit", "Amount"],  # a ledger
                       ["Account Code", "Account Name", "Group", "Opening Balance"],  # a chart of accounts
                       ["Item", "Stock Account", "Qty", "Value"],  # a stock table
                       ["Account", "Narration", "Amount"],  # a journal
                       ["Party", "GSTIN", "Invoice No", "Taxable Value"],
                       ["Customer", "Phone", "Invoice", "Amount"],
                       ["Vendor", "Account No", "IFSC", "GST Amount"]):
            self.assertIsNone(payees.payment_table(header), header)


class UncheckedOnlyForPayments(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_a_report_with_accounts_is_not_a_payment_run(self):
        report = table(self.dir / "sales_by_customer.csv", [["Customer", "Bank Account", "Total Sales"],
                                                            ["Acme Traders", "50100123456789", "45000"]])
        ledger = table(self.dir / "ledger.csv", [["GL Account", "Narration", "Amount"],
                                                 ["4000123456", "Sales", "45000"]])
        self.assertEqual(payees.unchecked("", [report, ledger], found=([], [])), [])

    def test_a_payment_run_without_a_master_is_still_not_checked(self):
        run = table(self.dir / "run.csv", [["Vendor", "Account No", "IFSC", "Amount"],
                                          ["Acme Traders", "50100123456789", "HDFC0000123", "45000"]])
        said = payees.unchecked("", [run], found=([], []))
        self.assertEqual(len(said), 1)
        self.assertTrue(said[0].startswith("PAYEES NOT CHECKED: run.csv pays Acme Traders"))


class APayeeIsItsWholeName(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_names(self):
        self.assertFalse(payees.names_payee("Ram", "Sita Ram Traders | 50100012345678"))
        self.assertFalse(payees.names_payee("ABC Pvt Ltd", "ABC Traders | 50100012345678"))
        self.assertTrue(payees.names_payee("Shree Ganesh Packaging", "M/s Shree Ganesh Packaging Pvt Ltd | 5010"))
        self.assertTrue(payees.names_payee("A & B Traders", "A and B Traders (Pune) | 5010"))
        self.assertTrue(payees.names_payee("abc private limited", "ABC Pvt. Ltd. | 5010"))

    def test_another_partys_account_is_found(self):
        master = table(self.dir / "master.csv", [["Vendor", "Account No", "IFSC"],
                                                 ["Sita Ram Traders", "50100012345678", "HDFC0000123"],
                                                 ["ABC Traders", "60100012345678", "HDFC0000124"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount Payable", "Account No", "IFSC"],
                                          ["Ram", "5000", "50100012345678", "HDFC0000123"],
                                          ["ABC Pvt Ltd", "7000", "60100012345678", "HDFC0000124"]])
        found = payees.mismatches("", [run], found=([master], []))
        self.assertEqual(len(found), 2, found)
        self.assertTrue(all(f.startswith("PAYMENT TO ANOTHER PARTY'S ACCOUNT:") for f in found), found)

    def test_the_payees_own_account_passes(self):
        master = table(self.dir / "master.csv", [["Vendor", "Account No", "IFSC"],
                                                 ["Sita Ram Traders", "50100012345678", "HDFC0000123"]])
        run = table(self.dir / "run.csv", [["Vendor", "Amount Payable", "Account No", "IFSC"],
                                          ["M/s Sita Ram Traders", "5000", "50100012345678", "HDFC0000123"]])
        self.assertEqual(payees.mismatches("", [run], found=([master], [])), [])


if __name__ == "__main__":
    unittest.main()
