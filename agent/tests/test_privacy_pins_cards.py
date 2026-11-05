"""
Recheck 2026-11-03: a postal PIN code given in a request ("PIN: 576104", "Bengaluru PIN 560001") was remembered as a
secret and then hidden everywhere, an invoice total that matched it included; "Pin: Mumbai 400001" remembered "Mumbai".
A real PIN must stay withheld ("ATM PIN 4821", "UPI PIN: 482193"). Also: "ATM PIN code: 1234" captured "code:",
"Password Northwind2026" (no colon) and card numbers got through withhold_known, the part of a password after ";" leaked,
and "PO token number: 7781" remembered "number:". Each case is checked both ways: withheld when secret, kept when not.
"""

import os
import tempfile
import unittest
from unittest import mock

from decurix_agent.computer import privacy


class _Fresh(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patch = mock.patch.dict(os.environ, {"DECURIX_DIR": self.tmp.name})
        patch.start()
        self.addCleanup(patch.stop)
        privacy._known_cache.clear()
        privacy._unsaved.clear()


class APostalCodeIsNotASecret(_Fresh):
    def test_a_postal_code_in_a_request_is_never_remembered(self):
        for request in ("PIN: 576104", "Bengaluru PIN 560001", "Pin: Mumbai 400001", "Udupi PIN-576104",
                        "Ship to 12 MG Road, Udupi, PIN: 576104", "Address: Andheri, Mumbai, Pin: 400053",
                        "Pincode: 560001", "PIN code 576104"):
            self.assertEqual(privacy.request_secrets(request), [], request)

    def test_a_remembered_request_does_not_hide_a_matching_total_later(self):
        privacy.remember_secrets("Send the invoice to Acme, Udupi. PIN: 576104")
        self.assertEqual(privacy.withhold_known("Invoice total 576104"), "Invoice total 576104")
        privacy.remember_secrets("Pin: Mumbai 400001")
        self.assertEqual(privacy.withhold_known("Mumbai office"), "Mumbai office")

    def test_an_address_keeps_its_postal_code(self):
        for text in ("Bengaluru PIN 560001", "Manipal, PIN: 576104", "Ship to 12 MG Road, Udupi, PIN: 576104",
                     "Address: Andheri, Mumbai, Pin: 400053", "PIN code 576104", "Pin: Mumbai 400001"):
            self.assertEqual(privacy.withhold_known(text), text, text)

    def test_six_digits_after_a_bare_pin_with_nothing_beside_them_are_withheld_only_there(self):
        self.assertEqual(privacy.withhold_known("PIN: 576104"), "PIN: [withheld]")
        self.assertEqual(privacy.request_secrets("PIN: 576104"), [])


class ARealPinStaysWithheld(_Fresh):
    def test_a_named_pin_is_remembered_and_withheld(self):
        for request, value in (("ATM PIN 4821", "4821"), ("UPI PIN: 482193", "482193"), ("mpin 482193", "482193"),
                               ("TPIN 4455", "4455"), ("PIN: 4821", "4821"), ("pin '4821'", "4821"),
                               ("Log in with user ram, PIN: 482193", "482193"), ("my PIN is 482193", "482193"),
                               ("Card PIN code: 1234", "1234")):
            self.assertIn(value, privacy.request_secrets(request), request)
            self.assertNotIn(value, privacy.withhold_known(request), request)

    def test_atm_pin_code_takes_the_digits_not_the_word(self):
        self.assertEqual(privacy.request_secrets("ATM PIN code: 1234"), ["1234"])
        self.assertEqual(privacy.withhold_known("ATM PIN code: 1234"), "ATM PIN code: [withheld]")

    def test_a_remembered_pin_is_withheld_everywhere_after(self):
        privacy.remember_secrets("Pay with UPI PIN: 482193")
        self.assertEqual(privacy.withhold_known("I typed 482193 into the box"), "I typed [withheld] into the box")

    def test_a_word_is_never_a_pin(self):
        for text in ("the PIN has changed", "the pin 3 on the board", "Pin: Mumbai 400001"):
            self.assertEqual(privacy.request_secrets(text), [], text)


class PasswordsAndCards(_Fresh):
    def test_a_password_after_its_name_without_a_colon(self):
        self.assertEqual(privacy.withhold_known("Password Northwind2026"), "Password [withheld]")
        self.assertEqual(privacy.withhold_known("pwd Lift2026"), "pwd [withheld]")
        for text in ("Password reset link sent", "Password Manager opened", "pwd /home/user/work"):
            self.assertEqual(privacy.withhold_known(text), text, text)
        self.assertEqual(privacy.request_secrets("pwd /home/user/work"), [])

    def test_the_part_after_a_semicolon_is_withheld_too(self):
        self.assertEqual(privacy.withhold_known("password: Abc;123xyz"), "password: [withheld]")
        self.assertEqual(privacy.request_secrets("password: Abc;123xyz"), ["Abc;123xyz"])
        privacy.remember_secrets("password: Abc;123xyz")
        self.assertNotIn("123xyz", privacy.withhold_known("I used Abc;123xyz to log in"))
        self.assertEqual(privacy.withhold_known("password: hunter2, user: ram"), "password: [withheld], user: ram")

    def test_a_sign_in_code_is_withheld(self):
        self.assertEqual(privacy.withhold_known("Login code 552901"), "Login code [withheld]")
        self.assertEqual(privacy.withhold_known("Login code sent to the client"), "Login code sent to the client")

    def test_a_card_number_is_withheld(self):
        self.assertEqual(privacy.withhold_known("Card 4111 1111 1111 1111 exp 12/27"), "Card [withheld] exp 12/27")
        self.assertEqual(privacy.withhold_known("card no 4111111111111111"), "card no [withheld]")
        self.assertEqual(privacy.withhold_known("4111-1111-1111-1111"), "[withheld]")
        privacy.remember_secrets("Pay with card 4111111111111111")
        self.assertEqual(privacy.withhold_known("charged 4111 1111 1111 1111"), "charged [withheld]")

    def test_an_account_or_order_number_that_passes_the_checksum_is_kept(self):
        for text in ("A/c 50100123456789 IFSC HDFC0001234", "Order 4111111111111111 shipped",
                     "Total 4111111111111111", "Invoice INV-2026-00123 for Rs 1,23,456.78"):
            self.assertEqual(privacy.withhold_known(text), text, text)


class ANumberLabelIsNotASecret(_Fresh):
    def test_a_token_number_is_a_queue_number(self):
        for text in ("PO token number: 7781", "Ack token 112233445566", "Token number 45 at the counter"):
            self.assertEqual(privacy.request_secrets(text), [], text)
            self.assertEqual(privacy.withhold_known(text), text, text)

    def test_an_identifier_named_after_a_token_word_is_that_identifiers(self):
        irn = "6f2b9a1c7d3e5f80a9b1c2d3e4f5061728394a5b6c7d8e9f0a1b2c3d4e5f6071"
        text = f"e-invoice token generated, IRN {irn}"
        self.assertEqual(privacy.withhold_known(text), text)
        self.assertNotIn(irn, privacy.withhold_known(f"auth token for e-invoice: {irn}"))

    def test_a_machine_token_after_its_name_still_is(self):
        self.assertEqual(privacy.request_secrets("api key sk_live_abc123def456ghi"), ["sk_live_abc123def456ghi"])
        self.assertEqual(privacy.request_secrets("the token is ABC-77812"), ["ABC-77812"])


if __name__ == "__main__":
    unittest.main()
