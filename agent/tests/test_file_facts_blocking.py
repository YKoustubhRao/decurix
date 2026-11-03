"""
Which file facts fail the work, and which leave it not checked, is decided in one place (FileFacts.blocking and
not_made) for every route (review 2026-11-03, E-27: four routes kept four lists, and they had drifted).
"""

import unittest

from decurix_agent import produced


class WhatBlocks(unittest.TestCase):
    def test_each_blocking_fact_blocks_and_evidence_does_not(self):
        for name in produced.FileFacts.BLOCKING:
            value = "a finding" if name == "missing_files" else ["a finding"]
            facts = produced.FileFacts(**{name: value})
            self.assertEqual(facts.blocking(), ["a finding"], name)
            self.assertEqual(facts.not_made(), [], name)
        evidence = produced.FileFacts(gap_notes=["n"], layout=["n"], typed_totals=["n"], request_faults=["n"],
                                      disclosures=["n"], draft_fields=["n"], duplicates=["n"], pattern_breaks=["n"],
                                      code_notes=["n"], totals_note=["n"])
        self.assertEqual(evidence.blocking(), [])
        self.assertEqual(evidence.not_made(), [])

    def test_a_check_that_raised_and_a_file_not_opened_are_not_made(self):
        facts = produced.FileFacts(could_not=["formula_errors could not be made"], unchecked=["a.xlsx is protected"])
        self.assertEqual(facts.not_made(), ["formula_errors could not be made", "a.xlsx is protected"])
        self.assertEqual(facts.blocking(), [])

    def test_the_blocking_set_is_pinned(self):
        self.assertEqual(set(produced.FileFacts.BLOCKING),
                         {"unstored", "gaps", "missing_files", "file_faults", "payee_mismatch", "source_mismatch",
                          "unknown_recipients", "draft_facts"})


if __name__ == "__main__":
    unittest.main()
