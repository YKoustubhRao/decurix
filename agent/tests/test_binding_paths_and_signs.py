"""
binding: a left-out document is exactly one of the run's files (E-34), and a negative figure is held by a text that
says it (E-45). Review 2026-11-03.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import binding

FILES = ["/in/1.pdf", "/in/11.pdf", "/a/same.pdf", "/b/same.pdf", "/in/memo.pdf"]


class LeftOutDocuments(unittest.TestCase):
    def run_lists(self, left_out):
        answer = {"left_out": left_out}
        with mock.patch.object(binding, "_ask", return_value=(answer, ["text"] * len(FILES))), \
                mock.patch.object(binding, "_clean", return_value=({}, {})):
            return binding.fill_lists({"name": "s"}, [], {}, FILES)[3]

    def test_a_file_name_names_the_one_file_it_is(self):
        out = self.run_lists([{"document": "memo.pdf", "kind": "skip", "why": "cancelled"}])
        self.assertEqual([x["document"] for x in out], ["/in/memo.pdf"])

    def test_a_name_inside_another_name_is_not_that_file(self):
        out = self.run_lists([{"document": "1.pdf", "kind": "by_hand", "why": "odd"}])
        self.assertEqual([x["document"] for x in out], ["/in/1.pdf"])
        out = self.run_lists([{"document": "in/1", "kind": "by_hand", "why": "odd"}])
        self.assertEqual(out, [])

    def test_a_name_two_files_share_is_dropped(self):
        self.assertEqual(self.run_lists([{"document": "same.pdf", "kind": "skip", "why": "dup"}]), [])

    def test_one_file_named_twice_is_left_out_once(self):
        out = self.run_lists([{"document": "/in/11.pdf", "kind": "skip", "why": "a"},
                              {"document": "11.pdf", "kind": "skip", "why": "b"}])
        self.assertEqual([x["document"] for x in out], ["/in/11.pdf"])


class NegativeFigures(unittest.TestCase):
    def test_a_negative_amount_is_held_by_a_text_that_says_it(self):
        self.assertTrue(binding.held_by("-500", ["Adjustment: -500 on the credit note"]))
        self.assertTrue(binding.held_by("-1,250.50", ["Balance -1250.50"]))

    def test_the_amount_is_what_is_held_whatever_sign_either_side_writes(self):
        # (a credit entered as -500 is the document's "500"; a discount a field takes as 500 is the document's "-500")
        self.assertTrue(binding.held_by("-500", ["Credit note amount 500"]))
        self.assertTrue(binding.held_by("500", ["Discount -500"]))

    def test_a_sign_flip_is_said_to_the_check_not_refused(self):
        self.assertTrue(binding.sign_differs("-500", ["Credit note amount 500"]))
        self.assertTrue(binding.sign_differs("500", ["Discount -500"]))
        self.assertFalse(binding.sign_differs("-500", ["Adjustment -500"]))
        self.assertFalse(binding.sign_differs("500", ["Invoice INV-500 for 500"]))
        self.assertFalse(binding.sign_differs("500", ["Invoice INV-500"]), "a hyphen after a word is no minus")
        values, evidence = binding._clean([{"name": "amount", "example": "100"}], {"values": {"amount": "-500"}},
                                          ["Credit note amount 500"], strict=True)
        self.assertEqual(values, {"amount": "-500"})
        self.assertEqual(evidence["amount"], binding.OTHER_SIGN)

    def test_another_amount_is_still_not_held(self):
        self.assertFalse(binding.held_by("-500", ["Amount 5000"]))
        self.assertFalse(binding.held_by("-1,250.50", ["Balance 125.05"]))


if __name__ == "__main__":
    unittest.main()
