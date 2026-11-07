"""
A code check that raises is a check NOT made - never a pass (E-01, review 2026-11-03).

Each check had its own `except Exception: return []`, so a blocking check that raised (an unreadable manifest, a file
that could not be opened) counted as passed, and the run said "Done - every stage was made and checked", checked=True.
Every check now runs through stage_checks._checked. This table makes each blocking or ask-the-person check raise in turn
(a new such check with no row here fails the coverage test), and the run must not be reported checked.
"""

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from test_long_requests import LONG, TASK, LongRunBase

from decurix_agent import long_task, stage_checks, stage_facts, verify

# The function each check of CHECK_MATURITY is made by, as stage_code_findings calls it.
MADE_BY = {
    "file_facts": (stage_checks, "code_problems"),
    "source_guard": (stage_checks, "source_guard_problems"),
    "filler_values": (stage_checks, "invented_typing"),
    "draft_facts": (stage_checks, "draft_checks"),
    "app_emptied": (stage_checks, "app_emptied"),
    "payee_approval": (stage_checks, "_payee_approvals"),
    "files_open": (stage_checks, "files_open"),
    "system_entry": (stage_checks, "system_entry_findings"),
    "working": (stage_facts, "formula_contradictions"),
}


def must_be_made():
    return sorted(name for name in long_task.CHECK_MATURITY
                  if long_task.is_blocking(name) or long_task.asks_person(name) or name in long_task.MADE_OR_UNCHECKED)


class EveryCheckThatMustBeMadeIsInTheTable(unittest.TestCase):
    def test_the_table_covers_every_blocking_and_ask_the_person_check(self):
        self.assertEqual(sorted(MADE_BY), must_be_made())


class AWrapperThatRaisesIsNotMade(unittest.TestCase):
    def test_a_blocking_check_that_raises_is_said_not_made(self):
        not_made = []
        self.assertEqual(long_task._checked("files_open", not_made, lambda: 1 / 0), [])
        self.assertEqual(not_made, ["the files_open check could not be made (ZeroDivisionError)"])

    def test_an_advisory_check_that_raises_only_loses_its_evidence(self):
        not_made = []
        self.assertEqual(long_task._checked("coverage", not_made, lambda: 1 / 0), [])
        self.assertEqual(not_made, [])

    def test_a_check_that_works_returns_its_findings(self):
        self.assertEqual(long_task._checked("files_open", [], lambda: ["a finding"]), ["a finding"])


class ARunWhoseCheckRaisesIsNotChecked(LongRunBase):
    def run_with(self, module, name):
        sure = {"verdict": "done", "confidence": 0.95, "seen": "the files are right", "why": ""}
        with mock.patch.object(verify, "judge", lambda task, **k: dict(sure)), \
                mock.patch.object(module, name, side_effect=RuntimeError("cannot be read")):
            long_task.perform(LONG, None)
        return self.runner.status(self.run_id())["steps"], self.finals()[-1]

    def check_row(self, check):
        module, name = MADE_BY[check]
        steps, final = self.run_with(module, name)
        # Every stage completes - the work is there - but none is checked, and the report says what was not.
        self.assertEqual({r["status"] for r in steps.values()}, {"completed"}, final["reply"][:300])
        self.assertFalse(any(r.get("checked_by") == "model" for r in steps.values()),
                         {k: r.get("checked_by") for k, r in steps.items()})
        self.assertIs(final["checked"], False)
        self.assertNotIn("every stage was made and checked", final["reply"])
        self.assertNotIn("stopped during verification", final["reply"])


# The inputs the checks read, as long_task reads them (R-1, review round 3): a read that fails is a check not made, never
# "nothing typed" or "no drafts".
_REAL_STAGE_DRAFTS = long_task._stage_drafts
READS = {
    "typed values": ("decurix_agent.computer.evidence.entered", None),
    "the Outbox": ("decurix_agent.tools.outbox.drafted", _REAL_STAGE_DRAFTS),
}


class AnInputThatCannotBeReadIsNotMade(LongRunBase):
    def read_row(self, what):
        target, stage_drafts = READS[what]
        sure = {"verdict": "done", "confidence": 0.95, "seen": "the files are right", "why": ""}
        patches = [mock.patch.object(verify, "judge", lambda task, **k: dict(sure)),
                   mock.patch(target, side_effect=OSError("cannot be read"))]
        if stage_drafts is not None:
            patches.append(mock.patch.object(long_task, "_stage_drafts", stage_drafts))
            patches.append(mock.patch.object(long_task, "_draft_ids", lambda: []))
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        long_task.perform(LONG, None)
        steps, final = self.runner.status(self.run_id())["steps"], self.finals()[-1]
        self.assertFalse(any(r.get("checked_by") == "model" for r in steps.values()),
                         {k: r.get("checked_by") for k, r in steps.items()})
        self.assertIs(final["checked"], False)
        self.assertNotIn("every stage was made and checked", final["reply"])

    def test_typed_values_that_cannot_be_read_leave_the_run_not_checked(self):
        self.read_row("typed values")

    def test_an_outbox_that_cannot_be_read_leaves_the_run_not_checked(self):
        self.read_row("the Outbox")

    def test_a_start_whose_typed_values_were_not_read_is_not_counted_as_nothing(self):
        with mock.patch.object(long_task, "_entered", side_effect=OSError("cannot be read")):
            self.assertIsNone(long_task._typed_count())

    def test_untagged_drafts_with_no_start_reading_are_unknown(self):
        with mock.patch("decurix_agent.tools.outbox.drafted", lambda: [{"id": "d1", "to": ["a@x.test"]}]):
            with self.assertRaises(LookupError):
                _REAL_STAGE_DRAFTS(None, "run:s1")
            self.assertEqual(_REAL_STAGE_DRAFTS([], "run:s1"), [{"id": "d1", "to": ["a@x.test"]}])


def _row(check):
    return lambda self: self.check_row(check)


for _check in MADE_BY:
    setattr(ARunWhoseCheckRaisesIsNotChecked, f"test_{_check}_raising_leaves_the_run_not_checked", _row(_check))


if __name__ == "__main__":
    unittest.main()
