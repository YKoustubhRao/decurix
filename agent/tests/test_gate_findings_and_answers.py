"""
The final gate (skill_task._gate), review 2026-11-03:
- E-03: a check that could not be made never hides a definite finding the code did make;
- E-38: "Not checked" is said once;
- E-02 / T-01: an engine's answer that touched no screen and wrote no file is checked in code (a sum it writes out),
  judged when the tools read anything, and otherwise goes out said NOT checked - never as a checked result;
- E-12: the person's given file that changed is never put back while checking; the fact reaches the judge and the
  person;
- E-36: a run the person stopped is never reported as a finished task.
"""

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_skills as ts

from decurix_agent import guidance, playbooks, procedures, produced, skill_task, source_guard, state, verify
from decurix_agent.computer import evidence


def verdict(kind, seen="", why=""):
    return {"verdict": kind, "confidence": 0.9 if kind == verify.DONE else 0.5, "seen": seen, "why": why}


class _Reports(ts.TeachingBase):
    """The final gate for a task that touched no screen and wrote a file (as test_produced_files.FileWorkIsChecked),
    keeping what each report says about itself (checked, gated)."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.verdicts, self.judged, self.reports = [], [], []

        def judge(*args, **kwargs):
            self.judged.append(kwargs)
            return self.verdicts.pop(0)

        self.made = ["/work/result.xlsx"]
        for patch in (mock.patch.object(skill_task.verify, "available", lambda: True),
                      mock.patch.object(skill_task.verify, "judge", judge),
                      mock.patch.object(skill_task, "_did_screen_work", lambda: False),
                      mock.patch.object(skill_task.produced, "files", lambda report="", **kw: list(self.made)),
                      mock.patch.object(skill_task.produced, "text", lambda paths, focus=(): "A7='a row'"),
                      mock.patch.object(skill_task.produced, "missing_on_host", lambda request: ""),
                      mock.patch.object(skill_task.produced, "saved_on_host", lambda request: []),
                      mock.patch.object(skill_task, "_engine_can_drive", lambda: False),
                      mock.patch.object(skill_task, "write_final_reply",
                                        lambda text, kind="final", engine=None, **kw: (
                                            self.finals.append((text, kind)),
                                            self.reports.append(dict(kw, kind=kind, text=text))))):
            patch.start()
            self.addCleanup(patch.stop)
        state.current_task = "compare the two lists into result.xlsx"


class ADefiniteFindingIsNeverHiddenByACheckNotMade(_Reports):
    def test_a_payee_mismatch_beside_a_check_that_raised_goes_back_as_not_done(self):
        facts = produced.FileFacts(could_not=["the formula check could not be made"],
                                   payee_mismatch=["Northgate Supplies is paid to account 99887766 - on record: 11223344"])
        sent = []
        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts), \
                mock.patch.object(skill_task, "_engine_can_drive", lambda: True), \
                mock.patch.object(skill_task, "_engine", lambda task, task_file: sent.append(guidance.get()["text"])):
            self.assertTrue(skill_task._gate("Payment file made: payments.csv.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("99887766", sent[0])

    def test_with_no_definite_finding_it_is_not_checked_said_once(self):
        facts = produced.FileFacts(could_not=["the formula check could not be made"])
        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        text = self.reports[-1]["text"]
        self.assertTrue(text.startswith("Not checked - "), text[:120])
        self.assertEqual(text.lower().count("not checked"), 1, text[:200])
        self.assertIs(self.reports[-1].get("checked", False), False)

    def test_a_file_that_could_not_be_opened_leaves_the_work_not_checked(self):
        facts = produced.FileFacts(unchecked=["result.xlsx is password-protected or encrypted: it was not checked"])
        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: facts):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertIs(self.reports[-1].get("checked", False), False)
        self.assertIn("password-protected", self.reports[-1]["text"])


class AnAnswerIsCheckedOrSaidNotChecked(ts.TeachingBase):
    """An engine's answer: no screen work, no file, no draft."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.reports, self.judged, self.sent = [], [], []
        self.observed, self.read = [], []
        patches = [
            mock.patch.object(skill_task, "write_final_reply",
                              lambda text, kind="final", engine=None, **kw: self.reports.append(dict(kw, kind=kind,
                                                                                                     text=text))),
            mock.patch.object(skill_task, "_did_screen_work", lambda: False),
            mock.patch.object(produced, "files", lambda *a, **k: []),
            mock.patch.object(produced, "drafts", lambda *a, **k: []),
            mock.patch.object(produced, "drafts_text", lambda *a, **k: ""),
            mock.patch.object(produced, "missing_on_host", lambda request: ""),
            mock.patch.object(produced, "saved_on_host", lambda request: []),
            mock.patch.object(procedures, "in_request", lambda *a, **k: False),
            mock.patch.object(produced, "requested_names", lambda *a, **k: []),
            mock.patch.object(playbooks, "changed_things", lambda *a, **k: []),
            mock.patch.object(evidence, "observed", lambda *a, **k: list(self.observed)),
            mock.patch.object(evidence, "files_read", lambda *a, **k: list(self.read)),
            mock.patch.object(skill_task, "_commands_text", lambda: ""),
            mock.patch.object(skill_task.verify, "available", lambda: True),
            mock.patch.object(skill_task.verify, "judge",
                              lambda *a, **k: (self.judged.append(k), verdict(verify.DONE, seen="the ledger says so"))[1]),
            mock.patch.object(skill_task, "_engine_can_drive", lambda: True),
            mock.patch.object(skill_task, "_engine", lambda task, task_file: self.sent.append(guidance.get()["text"])),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        state.current_task = "How much does the client owe across invoices 101 to 103?"

    def test_a_wrong_sum_in_an_answer_goes_back_to_the_worker(self):
        self.assertTrue(skill_task._gate("The invoices come to 750.00 + 300.00 + 84.00 = 1143.00, all unpaid.", "final", "thebot"))
        self.assertEqual(self.reports, [])
        self.assertIn("1,134.00", self.sent[0])

    def test_an_answer_with_nothing_to_check_it_against_goes_out_said_not_checked(self):
        self.assertTrue(skill_task._gate("The invoices come to 750.00 + 300.00 + 84.00 = 1134.00.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(len(self.reports), 1)
        self.assertTrue(self.reports[0]["text"].startswith(skill_task.UNCHECKED_ANSWER))
        self.assertIs(self.reports[0].get("checked", False), False)
        self.assertIn("1134.00", self.reports[0]["text"])

    def test_an_answer_drawn_from_what_the_tools_read_is_judged_with_that_evidence(self):
        self.observed = ["office read ledger.xlsx: B2=750 B3=300 B4=84"]
        self.assertTrue(skill_task._gate("The invoices come to 1,134.00.", "final", "thebot"))
        self.assertEqual(len(self.judged), 1)
        self.assertIn("office read ledger.xlsx", "\n".join(self.judged[0]["observed"]))
        self.assertIs(self.reports[-1].get("checked"), True)


class AChangedGivenFileIsTold(_Reports):
    def test_the_gate_puts_nothing_back_and_the_judge_and_the_person_see_it(self):
        change = {"rel": "ledger.xlsx", "name": "ledger.xlsx", "path": "C:\\Books\\ledger.xlsx", "diff": "B7: 0 -> 500",
                  "gone": False, "kept": True, "asked": False, "copy": "/kept/ledger.xlsx"}
        self.verdicts = [verdict(verify.DONE, seen="the ledger holds the payment")]
        restored = []
        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: produced.FileFacts()), \
                mock.patch.object(source_guard, "changed_sources", lambda task_id, request: [dict(change)]), \
                mock.patch.object(source_guard, "restore", lambda *a, **k: restored.append(a) or ([], [])), \
                mock.patch.object(source_guard, "restore_sources", lambda *a, **k: restored.append(a)), \
                mock.patch.object(source_guard, "check_and_restore", lambda *a, **k: restored.append(a) or ""):
            self.assertTrue(skill_task._gate("Recorded the payment in ledger.xlsx.", "final", "thebot"))
        self.assertEqual(restored, [])
        self.assertIn("B7: 0 -> 500", self.judged[0]["produced"])
        self.assertIn("Changed your file C:\\Books\\ledger.xlsx", self.reports[-1]["text"])
        self.assertIs(self.reports[-1].get("checked"), True)

    def test_a_source_check_that_cannot_be_made_leaves_the_work_not_checked(self):
        def broken(task_id, request):
            raise source_guard.NotChecked("the notes could not be read")

        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: produced.FileFacts()), \
                mock.patch.object(source_guard, "changed_sources", broken):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertIs(self.reports[-1].get("checked", False), False)
        self.assertIn("could not be checked", self.reports[-1]["text"])


class AStopIsNotAFinishedTask(_Reports):
    def test_a_no_at_an_approval_is_reported_stopped_not_final(self):
        with mock.patch.object(guidance, "refused", lambda: "do not send it"):
            guidance.begin(skill=None, task="send the statement", text="", mode="engine", run=None)
            self.assertTrue(skill_task._gate("Stopped at Send.", "final", "thebot"))
        self.assertEqual(self.reports[-1]["kind"], "error")
        self.assertTrue(self.reports[-1]["text"].startswith("Stopped, as you said"))
        self.assertIs(self.reports[-1].get("checked", False), False)

    def test_a_taught_run_stopped_before_a_step_is_reported_stopped(self):
        skill_task._stop(3)
        self.assertEqual(self.reports[-1]["kind"], "error")
        self.assertTrue(self.reports[-1]["text"].startswith("Stopped, as you said, before step 3"))


class ASecretValueIsAskedAsSecret(ts.TeachingBase):
    def test_a_question_for_a_parameter_the_skill_marks_secret_carries_the_flag(self):
        said = []
        run = {"skill": {"name": "s"}}
        self.addCleanup(setattr, state, "paused_skill", None)
        with mock.patch.object(skill_task, "ask_for_query",
                               lambda text, kind="query", engine=None, secret=False: said.append(secret)):
            skill_task._pause(run, "values", "I need the portal PIN.", secret=True, missing=["pin"])
            skill_task._pause(run, "values", "I need the customer.", missing=["customer"])
        self.assertEqual(said, [True, False])


if __name__ == "__main__":
    unittest.main()
