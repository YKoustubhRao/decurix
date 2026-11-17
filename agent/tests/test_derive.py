"""
A value no document writes as it is, given by arithmetic over the documents (a month's B2C total): the model proposes it
with the figures it uses, and code keeps it only when every figure stands in the document it names and the operation
gives the value (binding.checked_derivation). Measured in the learning curves (2026-11-17): such a total made a whole
part of a filing the model's - 43 turns, 1.2M tokens - for one number.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_derive
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent import binding  # noqa: E402

TEXTS = {"201.pdf": "Invoice 201 ... Taxable value 40,000.00 ... Total 47,200.00",
         "202.pdf": "Invoice 202 ... Taxable value 1,20,000.00 ... Total 1,41,600.00",
         "203.pdf": "Invoice 203 to Coastal Agro Foods LLP (GSTIN 29AAMFC4471R1ZK) Taxable value 3,00,000.00"}


class ADerivedValue(unittest.TestCase):
    def test_a_sum_over_some_documents_is_a_choice_code_cannot_check(self):
        # Found (2026-11-17): a B2C total summed in a B2B invoice - which documents count is the model's, at the work.
        answer = {"op": "sum", "value": "160000.00", "figures": [{"document": "201.pdf", "figure": "40,000.00"},
                                                                 {"document": "202.pdf", "figure": "1,20,000.00"}]}
        self.assertIsNone(binding.checked_derivation(answer, TEXTS))

    def test_even_a_sum_over_every_document_is_not_kept(self):
        # (every invoice of the month can still hold one of another table: B2B in a B2C total)
        answer = {"op": "sum", "value": "460000.00", "figures": [{"document": "201.pdf", "figure": "40,000.00"},
                                                                 {"document": "202.pdf", "figure": "1,20,000.00"},
                                                                 {"document": "203.pdf", "figure": "3,00,000.00"}]}
        self.assertIsNone(binding.checked_derivation(answer, TEXTS))
        one = {"op": "sum", "value": "40000", "figures": [{"document": "201.pdf", "figure": "40,000.00"}]}
        self.assertEqual(binding.checked_derivation(one, TEXTS), "40000")

    def test_a_wrong_sum_or_a_figure_not_in_its_document_is_not(self):
        wrong = {"op": "sum", "value": "470000", "figures": [{"document": "201.pdf", "figure": "40,000.00"},
                                                             {"document": "202.pdf", "figure": "1,20,000.00"},
                                                             {"document": "203.pdf", "figure": "3,00,000.00"}]}
        self.assertIsNone(binding.checked_derivation(wrong, TEXTS))
        elsewhere = {"op": "sum", "value": "160000", "figures": [{"document": "203.pdf", "figure": "40,000.00"},
                                                                 {"document": "202.pdf", "figure": "1,20,000.00"}]}
        self.assertIsNone(binding.checked_derivation(elsewhere, TEXTS))
        unknown = {"op": "sum", "value": "40000", "figures": [{"document": "999.pdf", "figure": "40,000.00"}]}
        self.assertIsNone(binding.checked_derivation(unknown, TEXTS))

    def test_a_count_and_a_copy(self):
        count = {"op": "count", "value": "2", "figures": [{"document": "201.pdf", "figure": "201"},
                                                          {"document": "202.pdf", "figure": "202"}]}
        self.assertEqual(binding.checked_derivation(count, TEXTS), "2")
        copy = {"op": "copy", "value": "300000", "figures": [{"document": "203.pdf", "figure": "3,00,000.00"}]}
        self.assertEqual(binding.checked_derivation(copy, TEXTS), "300000")

    def test_nothing_proposed_is_nothing_kept(self):
        for answer in (None, {}, {"op": "guess", "value": "5", "figures": []}):
            self.assertIsNone(binding.checked_derivation(answer, TEXTS))

    def test_zero_when_no_document_has_such_a_figure_and_says_why(self):
        self.assertEqual(binding.checked_derivation({"op": "none", "value": "0", "figures": [],
                                                     "why": "no invoice charges cess"}, TEXTS), "0")
        self.assertIsNone(binding.checked_derivation({"op": "none", "value": "5", "figures": [], "why": "x"}, TEXTS))
        self.assertIsNone(binding.checked_derivation({"op": "none", "value": "0", "figures": []}, TEXTS))


if __name__ == "__main__":
    unittest.main()
