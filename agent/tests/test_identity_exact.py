"""
One rule for "the same" (computer/identity.py, review 2026-11-03): acting, passing a check and reading a value back use
exact names and whole tokens in order; near matches are only candidates.

Covers D-13 (read-back), D-14/N-D1 (`gui verify`), D-35 (an identifier in a row).
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import core, identity, records, resolve

PAGE = "Sales Invoice DOC-0007 | Customer: Ravindra Kumar | Item: Consulting | Status: Unpaid | Total 4,500.00"


def e(ref, role, name, value=""):
    return {"ref": ref, "role": role, "name": name, "value": value, "path": f"w > {role} {name}", "group": "g",
            "states": []}


class TheModule(unittest.TestCase):
    def test_same_is_equal_tokens(self):
        self.assertTrue(identity.same("Ravi  Kumar", "ravi kumar"))
        self.assertFalse(identity.same("Ravi Kumar", "Ravi Kumar Jain"))
        self.assertFalse(identity.same("", ""))

    def test_holds_is_whole_tokens_in_order(self):
        self.assertTrue(identity.holds(PAGE, "Ravindra Kumar"))
        self.assertFalse(identity.holds(PAGE, "Paid"))
        self.assertFalse(identity.holds(PAGE, "Ravi"))
        self.assertFalse(identity.holds(PAGE, "Kumar Ravindra"))

    def test_an_identifier_is_whole(self):
        self.assertTrue(identity.holds_identifier("ref DOC 0007 here", "DOC-0007"))
        self.assertFalse(identity.holds_identifier("DOC-00071", "DOC-0007"))

    def test_candidates_say_why_and_never_decide(self):
        found = identity.candidates("INV-1042", ["INV-1043", "INV-1042", "other"])
        self.assertEqual(found[0]["item"], "INV-1042")
        near = next(c for c in found if c["item"] == "INV-1043")
        self.assertIn("numbers differ", near["reason"])

    def test_a_change_of_digits_or_words_is_another_identity(self):
        self.assertTrue(identity.changes_identity("INV-1042", "INV-1043"))
        self.assertTrue(identity.changes_identity("Ravi Kumar", "Ravi Kumar Jain"))
        self.assertFalse(identity.changes_identity("Save  invoice", "save Invoice"))


class AReadBackIsWhole(unittest.TestCase):
    def test_a_word_inside_a_longer_word_is_not_read_back(self):
        self.assertFalse(records.stands_in("Paid", PAGE))
        self.assertFalse(records.stands_in("Ravi", PAGE))
        self.assertTrue(records.stands_in("Ravindra Kumar", PAGE))

    def test_a_short_identifier_is_not_found_inside_another(self):
        self.assertFalse(resolve.holds("row: ABC1234 Vendor", "C12"))
        self.assertTrue(resolve.holds("row: ABC-1234 Vendor", "abc1234"))


class AScreenCheckIsExact(unittest.TestCase):
    def verify(self, elements, **kwargs):
        obs = {"size": [800, 600], "active_title": "Doc - Browser", "windows": [{"title": "Doc - Browser"}],
               "elements": elements, "groups": [], "frame": None}
        with mock.patch.object(core, "_load_state", return_value=None), mock.patch.object(core, "observe", return_value=obs):
            return core.verify(**kwargs)

    def test_a_name_inside_a_longer_name_does_not_pass(self):
        self.assertFalse(self.verify([e("e1", "status", "Not Submitted")], name="Submitted")["ok"])
        self.assertTrue(self.verify([e("e1", "status", "Submitted")], name="Submitted")["ok"])

    def test_text_inside_a_longer_word_does_not_pass(self):
        self.assertFalse(self.verify([e("e1", "status", "Unpaid")], expect_text="paid")["ok"])
        self.assertTrue(self.verify([e("e1", "status", "Paid")], expect_text="paid")["ok"])

    def test_what_a_field_holds_is_not_what_the_screen_says(self):
        self.assertFalse(self.verify([e("e3", "textbox", "Notes", "payment sent")], expect_text="sent")["ok"])


if __name__ == "__main__":
    unittest.main()
