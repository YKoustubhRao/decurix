"""
Finding BX (2026-10-20, round 5 P6): "Reply drafts (Outbox) for every email that needs one" over 22 emails - among them
an auto-reply, a phishing mail and duplicates - was planned as "22 email draft(s)"; the check then failed "expected at
least 22 ... found 12", and after the retries "found 0" with 18 drafts of the stage in the Outbox. Drafts "where needed"
carry no count: each email is drafted for or named with why not. And a stage's drafts are all its attempts' drafts.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task

REQUEST = ("Go through the inbox folder. Log every email in a tracker, and put reply drafts (Outbox) for every email "
           "that needs one. Never send anything.")


def write_emails(folder, n=22):
    paths = []
    for i in range(1, n + 1):
        p = Path(folder) / f"email_{i:02d}.eml"
        p.write_text(f"From: Sender {i} <sender{i}@example.com>\nTo: desk@example.com\nSubject: Query number {i}\n\n"
                     f"Body {i}\n", encoding="utf-8")
        paths.append(str(p))
    return paths


class NoCount(unittest.TestCase):
    def test_every_email_that_needs_one_is_no_count(self):
        self.assertTrue(long_task.drafts_as_needed(REQUEST))
        self.assertTrue(long_task.drafts_as_needed("Draft replies where needed."))
        self.assertFalse(long_task.drafts_as_needed("Draft a reminder email to each of the 4 customers."))
        plan = {"stages": [{"id": "log", "does": "Log the emails.", "outputs": ["tracker.csv"]},
                           {"id": "reply", "does": "Draft the replies.", "drafts": 22, "depends_on": ["log"]}]}
        reply = long_task.spec_from_plan(plan, request=REQUEST)["steps"][1]
        self.assertTrue(reply["drafts_as_needed"])
        self.assertEqual([c for c in reply["checks"] if c["format"] == "drafts"],
                         [{"path": "outbox", "format": "drafts", "min_count": 1}])

    def test_a_stated_count_stays(self):
        plan = {"stages": [{"id": "a", "does": "x", "outputs": ["a.csv"]},
                           {"id": "b", "does": "Remind them.", "drafts": 4, "depends_on": ["a"]}]}
        step = long_task.spec_from_plan(plan, request="Draft a reminder email to each of the 4 customers.")["steps"][1]
        self.assertFalse(step["drafts_as_needed"])
        self.assertIn({"path": "outbox", "format": "drafts", "min_count": 4}, step["checks"])


class Coverage(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.emails = write_emails(tmp.name)
        self.drafts = [{"to": [f"sender{i}@example.com"], "subject": f"Re: Query number {i}"} for i in range(1, 13)]

    def test_drafts_and_named_exclusions_cover_all(self):
        report = ("Drafted 12 replies. No draft for: email_13.eml (auto-reply), email_14.eml (phishing - not "
                  "answered), sender15@example.com (duplicate of email_02.eml), 'Query number 16' (no reply needed: "
                  "a newsletter), email_17 (duplicate of email_03.eml), email_18.eml, email_19.eml, email_20.eml, "
                  "email_21.eml, email_22.eml (no reply needed: receipts).")
        self.assertEqual(long_task.reply_coverage(self.emails, self.drafts, report), [])

    def test_unexplained_ones_are_named(self):
        report = ("Drafted 12 replies. No draft for: email_13.eml (auto-reply), email_14.eml (phishing), "
                  "email_15.eml, email_16.eml, email_17.eml, email_18.eml (duplicates).")
        self.assertEqual(long_task.reply_coverage(self.emails, self.drafts, report),
                         ["email_19.eml", "email_20.eml", "email_21.eml", "email_22.eml"])
        three = report.replace("email_18.eml (duplicates)", "email_18.eml, email_19.eml (duplicates)")
        self.assertEqual(long_task.reply_coverage(self.emails, self.drafts, three),
                         ["email_20.eml", "email_21.eml", "email_22.eml"])

    def test_a_folder_input_is_its_emails(self):
        folder = str(Path(self.emails[0]).parent)
        with mock.patch.object(long_task, "run_inputs", return_value=[folder]):
            self.assertEqual(len(long_task.input_emails(REQUEST)), 22)


class AllAttempts(unittest.TestCase):
    """Attempt 1 makes 12 drafts, attempt 2 none new: the stage has 12."""

    def test_the_first_attempts_outbox_is_the_base(self):
        store = {}
        outbox = [["earlier-1"], ["earlier-1"] + [f"d{i}" for i in range(12)]]
        with mock.patch.object(long_task, "_update", side_effect=lambda run_id, change: change(store)), \
                mock.patch.object(long_task, "_draft_ids", side_effect=outbox):
            self.assertEqual(long_task._draft_base("run1", "s3"), ["earlier-1"])   # attempt 1 begins
            self.assertEqual(long_task._draft_base("run1", "s3"), ["earlier-1"])   # attempt 2: unchanged

    def test_drafts_across_attempts_are_counted(self):
        from decurix_agent.tools import outbox
        made = [{"id": f"d{i}", "to": [f"s{i}@example.com"], "subject": "Re", "stage": "run1:s3", "attempt": 1}
                for i in range(12)]
        made.append({"id": "other", "to": ["x@example.com"], "subject": "Re", "stage": "run1:s2", "attempt": 1})
        with mock.patch.object(outbox, "drafted", return_value=made):
            # The base taken at attempt 2's start would hold all 12; the stage's tag still counts them.
            self.assertEqual(len(long_task._stage_drafts([d["id"] for d in made], "run1:s3")), 12)
            # Untagged drafts fall back to the first attempt's base.
            untagged = [{k: v for k, v in d.items() if k != "stage"} for d in made[:12]]
            with mock.patch.object(outbox, "drafted", return_value=untagged):
                self.assertEqual(len(long_task._stage_drafts([], "run1:s3")), 12)


if __name__ == "__main__":
    unittest.main()
