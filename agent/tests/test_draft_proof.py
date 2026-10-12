import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import playbooks, produced
from decurix_agent.tools import outbox

DRAFT = {"to": ["ap@acme.example"], "cc": [], "subject": "Statement for March",
         "body": "Dear Acme,\n\nYour balance is 1,250.00.\n", "attachments": ["statement.txt"]}


class _Desk(unittest.TestCase):
    """A worker in the middle of a task, with a control plane that takes drafts."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.work = self.root / "work"
        self.work.mkdir()
        (self.work / "statement.txt").write_text("balance 1250\n", encoding="utf-8")
        self.task, self.posted = "t00000000000000aa", []

        class Taken:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return b'{"id": 7}'

        def urlopen(request, timeout=0):
            self.posted.append(json.loads(request.data))
            return Taken()

        for p in (patch.object(outbox, "DRAFTS_DIR", self.root / "drafts"),
                  patch.object(outbox.config, "WORK_DIR", self.work),
                  patch.object(outbox.urllib.request, "urlopen", urlopen),
                  patch.object(outbox.task_context, "current", lambda: self.task),
                  patch.object(outbox.progress, "emit", lambda *a, **k: None),
                  patch.dict(os.environ, {}, clear=False)):
            p.start()
            self.addCleanup(p.stop)
        os.environ.pop(outbox.DRY_RUN, None)

    def draft(self, **changed):
        d = dict(DRAFT, **changed)
        return outbox.draft(d["to"], d["subject"], d["body"], cc=d["cc"], attachments=d["attachments"])


class WhatATaskDraftedIsKnownOnTheDesktop(_Desk):
    def test_a_draft_goes_to_the_outbox_and_is_noted_for_its_task_without_its_attachments_data(self):
        self.assertIn("NOT been sent", self.draft())
        self.assertEqual(len(self.posted), 1)
        self.assertEqual(outbox.drafted(), [DRAFT])
        self.assertNotIn("data", json.dumps(outbox.drafted()))

        self.assertEqual(outbox.drafted("t00000000000000bb"), [])        # another task drafted nothing
        self.task = ""
        self.draft()                                                     # run by hand: no task's
        self.assertEqual(outbox.drafted(), [])
        self.assertEqual(sorted(p.name for p in (self.root / "drafts").iterdir()), ["t00000000000000aa.jsonl"])

    def test_the_check_reads_a_draft_back_like_a_file_the_task_wrote(self):
        self.draft()
        shown = produced.drafts_text()
        for part in ("email draft 1 (in the Outbox, not sent)", "To: ap@acme.example", "Subject: Statement for March",
                     "Attached: statement.txt", "Your balance is 1,250.00."):
            self.assertIn(part, shown)

    def test_a_trial_drafts_nowhere_but_its_own_file(self):
        self.draft()
        trial = self.root / "trial.jsonl"
        with patch.dict(os.environ, {outbox.DRY_RUN: str(trial)}):
            self.assertIn("NOT been sent", self.draft(subject="Statement for April"))

        self.assertEqual(len(self.posted), 1)                            # nothing more reached the Outbox
        self.assertEqual(len(outbox.drafted()), 1)                       # nor the task's own note
        self.assertEqual(outbox.read_log(trial)[0]["subject"], "Statement for April")


class AScriptThatDraftsIsProvenByItsDraft(_Desk):
    def trial(self, *drafts):
        sandbox = self.root / "sandbox"
        sandbox.mkdir(exist_ok=True)
        (sandbox / "drafts.jsonl").write_text("".join(json.dumps(d) + "\n" for d in drafts), encoding="utf-8")
        return playbooks._same_drafts(sandbox)

    def test_the_same_email_is_proof_and_a_different_one_is_not(self):
        self.draft()
        # However the figure was printed, it is the same email.
        self.assertEqual(self.trial(dict(DRAFT, body="Dear Acme,\n\nYour balance is 1,250.\n")), "")

        self.assertIn("drafted 1 email(s)", self.trial())
        self.assertIn("drafted 1 email(s)", self.trial(DRAFT, DRAFT))
        self.assertIn("had to ['ap@acme.example']", self.trial(dict(DRAFT, to=["someone@else.example"])))
        self.assertIn("had attachments", self.trial(dict(DRAFT, attachments=[])))
        self.assertIn("its subject does not read the same", self.trial(dict(DRAFT, subject="Statement")))
        self.assertIn("its body does not read the same", self.trial(dict(DRAFT, body="Dear Acme,\n\nYour balance is 9.\n")))

    def test_a_task_that_drafted_nothing_is_not_matched_by_a_script_that_drafts(self):
        self.assertEqual(self.trial(), "")
        self.assertIn("drafted 0 email(s)", self.trial(DRAFT))


if __name__ == "__main__":
    unittest.main()
