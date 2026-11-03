"""
Whether a press asks is decided by the control's structure, never by the words after a committing verb (review
2026-11-03, D-04).

On a button a committing word anywhere in its label asks, whatever words stand beside it: a committing verb paired
with a word from each vocabulary that reads a label as a name (_READ_AFTER, _ATTRIBUTES, _ALSO_NOUNS, _OF_WORDS) is
asked. On a link or an item of the site's navigation the same label is read as a place, as before.
"""

import unittest

from decurix_agent.computer import approval

VERBS = ("Pay", "Send", "Publish", "Transfer")


def control(role, name, path=None):
    return {"role": role, "name": name, "value": "", "states": [], "box": [0, 0, 10, 10],
            "path": path or f'site x > {role} "{name}"', "source": "cdp", "group": "g", "window_title": "Site"}


class ACommittingButtonAsks(unittest.TestCase):
    def test_a_committing_verb_with_a_word_from_each_name_vocabulary_asks_on_a_button(self):
        lists = {"read after": approval._READ_AFTER, "attributes": approval._ATTRIBUTES}
        for verb in VERBS:
            for which, words in lists.items():
                for word in sorted(words)[:6]:
                    name = f"{verb} {word}"
                    with self.subTest(name=name, list=which):
                        self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_a_committing_noun_after_another_word_asks_on_a_button(self):
        for name in ("Instant transfer", "Express transfer", "Stock Transfer"):
            with self.subTest(name=name):
                self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_continue_with_payment_asks(self):
        self.assertEqual(approval.press_effect(control("button", "Continue with payment")), "asks")
        self.assertEqual(approval.press_effect(control("button", "Proceed with transfer")), "asks")

    def test_the_same_labels_on_a_link_or_in_navigation_are_unchanged(self):
        for name in ("Pay slip", "Send report settings", "Transfer history", "Stock Transfer"):
            with self.subTest(name=name):
                self.assertNotEqual(approval.press_effect(control("link", name)), "asks")
                self.assertNotEqual(approval.press_effect(
                    control("menuitem", name, path=f'site x > navigation > menuitem "{name}"')), "asks")
                self.assertEqual(approval.press_effect(control("button", name)), "asks")

    def test_structure_still_exempts(self):
        self.assertNotEqual(approval.press_effect(control("button", "Send to back")), "asks")
        self.assertIsNone(approval.press_effect(control("button", "Delete row")))
        self.assertIsNone(approval.press_effect(control("button", "Purchase Invoice")))  # DOCUMENT_NAME
        self.assertIsNone(approval.press_effect(control("option", "Pay tax")))

    def test_whether_a_control_goes_somewhere_is_read_from_role_and_container(self):
        self.assertTrue(approval.goes_somewhere(control("link", "x")))
        self.assertTrue(approval.goes_somewhere(control("tab", "x")))
        self.assertTrue(approval.goes_somewhere(control("menuitem", "x", path='a > navigation > menuitem "x"')))
        self.assertFalse(approval.goes_somewhere(control("menuitem", "x")))
        self.assertFalse(approval.goes_somewhere(control("button", "x")))


if __name__ == "__main__":
    unittest.main()
