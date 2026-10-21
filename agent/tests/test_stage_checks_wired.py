"""
Every code check a planned stage goes through is wired, by name, with its maturity. Found live (round 5 P5 on pc3,
2026-10-21): the repeated-rows check (BK3) sat inside another check and was switched off by a report saying "Removed
the prior note sheet" - the inflated quarter was reported done. A check dropped or renamed in a refactor shows here.
"""

import inspect
import re
import unittest

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task, produced

# Every check, as stage_code_findings names it.
CHECKS = {
    "file_facts", "file_notes", "repeated_rows", "coverage", "removed_residue", "removed_still_there", "status_conflicts",
    "filler_values", "lost_findings", "reply_coverage", "system_entry", "lost_sheets", "app_emptied", "files_open", "summary_totals",
    "draft_facts", "draft_fields", "source_guard", "repeated_rows_quiet", "typed_doubtful", "payee_approval",
    # system_entry_findings' (certain, and evidence)
    "system_entry_text",
    # _left_undone_found's
    "deferred_actions", "unsupported_references", "working", "working_factors", "unworked_amounts", "self_contradictions",
}
# The ones that fail a stage (harness audit, 2026-10-21): only domain-free invariants - the person's files and changes,
# provenance, own arithmetic, seen in the system, hygiene and the file invariants. Everything else is evidence.
BLOCKING = {"file_facts", "removed_residue", "removed_still_there", "filler_values", "draft_facts", "app_emptied",
            "files_open", "system_entry", "working", "source_guard"}
# The ones put to the person as an approval before the stage counts (independent review, 2026-10-21, #7).
ASKED = {"payee_approval"}


class EveryCheckIsWired(unittest.TestCase):
    def test_the_checks_by_name(self):
        source = (inspect.getsource(long_task.stage_code_findings) + inspect.getsource(long_task._left_undone_found)
                  + inspect.getsource(long_task.system_entry_findings))
        named = (set(re.findall(r"""add\("([a-z_]+)\"""", source)) | set(re.findall(r"""\(\("([a-z_]+)",""", source))
                 | set(re.findall(r"""check = "([a-z_]+)" if certain else "([a-z_]+)\"""", source)[0]))
        self.assertIn("found.extend(system_entry_findings(", inspect.getsource(long_task.stage_code_findings))
        self.assertEqual(named, CHECKS)

    def test_the_maturity_of_each(self):
        self.assertEqual({c for c in CHECKS if long_task.is_blocking(c)}, BLOCKING)
        self.assertEqual({c for c in CHECKS if long_task.asks_person(c)}, ASKED)
        self.assertFalse(BLOCKING & ASKED)

    def test_the_file_facts_hold_the_older_checks(self):
        # AU/AT accounts and recipients, BD/BE drafts, false sums: inside produced.file_facts.
        source = inspect.getsource(produced.file_facts)
        for part in ("_payee_mismatch", "_unknown_recipients", "draft_disclosures"):
            self.assertIn(part, source, part)
        self.assertIn("false_sums", inspect.getsource(long_task.code_problems))


if __name__ == "__main__":
    unittest.main()
