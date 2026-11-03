"""
DE (round 5 P4 on pc3, 2026-10-21, series 13, reported "Done"): the four welcome drafts named managers no source has
(Rakesh Menon, Meera Sethi, Arjun Rao) or another joiner's (Jenny Wu), and joining dates of 04 and 11 Nov for joiners
starting 02 Nov - the tracker and the portal were right. Since the harness audit (2026-10-21) two checks: provenance
(blocking) - a fact a draft states that no source and no recorded work holds; and the draft's facts against its
recipient's own row, found by the words of the table's own column headers (evidence) - no business word in code.
"""

import json
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths

from decurix_agent import long_task, produced, stage_facts

TRACKER = Path(r"C:\Users\Administrator\decurix-tests\wide\process\data\P4\joiners_tracker_Nov2026.xlsx")


def draft_fact_problems(drafts, tables, source_text=""):
    """stage_facts.draft_field_facts as sentences; a name in no source at all is said to be in none."""

    out = []
    for f in stage_facts.draft_field_facts(drafts, tables):
        note = ""
        if f["kind"] == "name" and source_text and f["said"].lower() not in source_text.lower():
            note = " - a name in no source the run has"
        out.append(f"the draft to {f['who']} says {f['header']} is \"{f['said']}\"{note}; the source gives \"{f['own']}\"")
    return out


@fixture_paths.skip_unless(TRACKER.is_file(), "round 5 P4's tracker is not on this machine")
class TheLiveDrafts(unittest.TestCase):
    def setUp(self):
        self.tables = [(h, r) for _, h, r in stage_facts.tables_of([str(TRACKER)])]
        self.source = "\n".join(" ".join(str(c) for c in row if c is not None) for _, rows in self.tables for row in rows)

    def drafts(self, name):
        return json.loads(fixture_paths.need("p4_welcome_drafts", name).read_text(encoding="utf-8"))

    def test_the_series_13_drafts_are_caught(self):
        found = draft_fact_problems(self.drafts("series13_wrong.json"), self.tables, self.source)
        text = "\n".join(found)
        self.assertEqual(len(found), 6, text)
        self.assertIn('ananya.deshpande@example.com says Reporting manager is "Rakesh Menon" - a name in no source', text)
        self.assertIn('the source gives "Sanjay Patel"', text)
        self.assertIn('rohit.kulkarni@example.com says Joining date is "11 Nov 2026"; the source gives "02 Nov 2026"', text)
        self.assertIn('arif.khan95@example.com says Joining date is "04 Nov 2026"', text)
        self.assertIn('arif.khan95@example.com says Reporting manager is "Jenny Wu"; the source gives "Omar Haddad"', text)

    def test_the_right_drafts_pass(self):
        self.assertEqual(draft_fact_problems(self.drafts("p4_right.json"), self.tables, self.source), [])


class Shapes(unittest.TestCase):
    HEADER = ["Full name", "Personal email", "Joining date", "Reporting manager", "Annual CTC (Rs)"]
    ROWS = [["Ananya Deshpande", "ananya@example.com", "02 Nov 2026", "Sanjay Patel", 480000],
            ["Sneha Iyer", "sneha@example.com", "16/11/2026", "Maria Lopez", 720000]]

    def check(self, to, body):
        return draft_fact_problems([{"to": [to], "subject": "Welcome", "body": body}],
                                   [(self.HEADER, self.ROWS)], "Sanjay Patel Maria Lopez")

    def test_the_same_facts_in_other_forms_pass(self):
        self.assertEqual(self.check("sneha@example.com", "You join us on 2026-11-16. Your reporting manager is Maria "
                                                         "Lopez. Your CTC will be Rs 7,20,000."), [])

    def test_a_figure_is_provenance_and_a_draft_tied_to_no_record_decides_nothing(self):
        # A figure near a column's word may be a line's or a sum's: only dates and names are compared with the row.
        self.assertEqual(self.check("ananya@example.com", "Your annual CTC will be Rs 5,00,000."), [])
        self.assertEqual(self.check("someone@else.example", "Your reporting manager is Rakesh Menon."), [])

    def test_a_column_named_in_other_words_is_found_by_its_header(self):
        header = ["Name", "E-mail", "Start date", "Buddy"]
        rows = [["Ravi", "ravi@x.example", "05/01/2027", "Lata Rao"]]
        found = draft_fact_problems([{"to": ["ravi@x.example"], "subject": "Hi",
                                      "body": "Your start date is 07/01/2027 and your buddy is Lata Rao."}],
                                    [(header, rows)])
        self.assertEqual(len(found), 1, found)
        self.assertIn('Start date is "07/01/2027"', found[0])

    def test_wired_provenance_blocks_and_the_field_comparison_is_evidence(self):
        self.assertTrue(long_task.is_blocking("draft_facts"))
        self.assertFalse(long_task.is_blocking("draft_fields"))
        self.assertIn("draft_facts", produced.FileFacts().__dict__)
        self.assertIn("draft_fields", produced.FileFacts().__dict__)


if __name__ == "__main__":
    unittest.main()
