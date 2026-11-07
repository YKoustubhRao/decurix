"""
The person's files stay as they were (docs/learning-and-verification.md): a given file the run changed is reported
with an offer to put it back, and it is put back only on a structured word - the person's `/restore <offer id>`, the
reply that names that offer, or the judge's verdict naming the file (`put_back`) - never on a word list over a message.
Every restore is checked (the bytes in place hash to the copy kept before the task), and a file that changed after the
run is never put back over without its own yes.
"""

import hashlib
import os
import sys
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

from test_shared_folder import SharedBase

from decurix_agent import skill_task, source_guard, state, verify

TASK = "t0restore0000001"
REQUEST = "Record today's payments in ledger.csv."
BEFORE = b"date,payee,amount\n2026-10-01,Alpha Supplies,100\n"
AFTER = BEFORE + b"2026-10-02,Beta Traders,250\n"


class Offer(SharedBase):
    def setUp(self):
        super().setUp()
        self.put("ledger.csv", BEFORE, age=3600)
        source_guard.take(REQUEST, TASK)
        self.change("ledger.csv", AFTER)
        self.facts = source_guard.changed_sources(TASK, REQUEST)
        self.offer_id = source_guard.offer(TASK, self.facts)
        self.finals, self.asked = [], []
        state.paused_skill = None
        for patch in (mock.patch.object(skill_task, "write_final_reply",
                                        lambda text, kind="final", engine=None, **kw: self.finals.append(
                                            dict(kw, text=text, kind=kind))),
                      mock.patch.object(skill_task, "ask_for_query",
                                        lambda text, kind="query", engine=None, **kw: self.asked.append((text, kind))),
                      mock.patch.object(skill_task.usage, "add"),
                      mock.patch.object(skill_task.messaging, "write_stopped",
                                        lambda text, engine=None, send=None: self.finals.append(
                                            {"text": text, "kind": "error", "checked": False}))):
            patch.start()
            self.addCleanup(patch.stop)
        self.addCleanup(setattr, state, "paused_skill", None)

    def change(self, name, data):
        path = self.mount / name
        path.write_bytes(data)
        os.utime(path, (time.time(), time.time()))

    def test_the_report_names_the_file_and_the_reply_that_puts_it_back(self):
        self.assertRegex(self.offer_id, r"^[0-9a-f]{8}$")
        line = source_guard.report_lines(self.facts, offer_id=self.offer_id)[0]
        self.assertTrue(line.startswith("Changed your file "))
        self.assertIn(f"send `/restore {self.offer_id}`", line)
        # One id per task: the next attempt's gate offers the same reply.
        self.assertEqual(source_guard.offer(TASK, self.facts), self.offer_id)
        self.assertEqual(skill_task.parse_restore(f"/restore {self.offer_id}"), (self.offer_id, []))
        self.assertEqual(skill_task.parse_restore(f'/restore {self.offer_id} "Q3 review.xlsx"'),
                         (self.offer_id, ["Q3 review.xlsx"]))
        self.assertTrue(skill_task.handles(f"/restore {self.offer_id}"))
        self.assertIsNone(skill_task.parse_restore("please restore my file"))

    def test_restore_puts_the_original_back_and_checks_its_bytes(self):
        skill_task.perform(f"/restore {self.offer_id}")
        self.assertEqual((self.mount / "ledger.csv").read_bytes(), BEFORE)
        self.assertEqual((self.mount / "ledger (changed by worker).csv").read_bytes(), AFTER)
        final = self.finals[-1]
        self.assertEqual(final["kind"], "final", final["text"])
        self.assertIs(final["checked"], True)
        self.assertEqual(final["verdict"]["verdict"], "done")
        self.assertIn("Put back your file", final["text"])
        self.assertIn("its bytes match the copy kept before the task (sha256 "
                      + hashlib.sha256(BEFORE).hexdigest()[:12], final["text"])
        self.assertIn("ledger (changed by worker).csv", final["text"])
        # Taken out of the offer: a second reply never writes the original over the worker's version kept beside it.
        skill_task.perform(f"/restore {self.offer_id}")
        self.assertEqual(self.finals[-1]["kind"], "error")
        self.assertIn("There is no offer", self.finals[-1]["text"])
        self.assertEqual((self.mount / "ledger (changed by worker).csv").read_bytes(), AFTER)

    def test_a_restore_whose_bytes_do_not_match_is_not_done(self):
        real = source_guard.restore

        def corrupt(task_id, rels, kept=None):
            done, failed = real(task_id, rels, kept=kept)
            (self.mount / "ledger.csv").write_bytes(b"half written")
            return done, failed

        with mock.patch.object(source_guard, "restore", corrupt):
            skill_task.perform(f"/restore {self.offer_id}")
        final = self.finals[-1]
        self.assertEqual(final["kind"], "error")
        self.assertEqual(final["verdict"]["verdict"], "not_done")
        self.assertIn("does not match the copy kept before the task", final["text"])

    def test_a_file_the_person_changed_after_the_run_is_not_put_back_without_a_yes(self):
        self.change("ledger.csv", AFTER + b"2026-10-03,Gamma Ltd,75\n")
        skill_task.perform(f"/restore {self.offer_id}")
        self.assertEqual(self.finals, [])
        self.assertIn("changed after the task ended", self.asked[-1][0])
        self.assertIn("Reply `yes` to put it back, or `no`", self.asked[-1][0])
        self.assertIn(b"Gamma Ltd", (self.mount / "ledger.csv").read_bytes(), "nothing written before the answer")
        skill_task.resume("no")
        self.assertIn(b"Gamma Ltd", (self.mount / "ledger.csv").read_bytes())
        self.assertIn("Stopped, as you said", self.finals[-1]["text"])
        self.assertFalse(self.finals[-1]["checked"])

    def test_a_yes_to_that_puts_it_back_and_keeps_the_newer_version_beside_it(self):
        newer = AFTER + b"2026-10-03,Gamma Ltd,75\n"
        self.change("ledger.csv", newer)
        skill_task.perform(f"/restore {self.offer_id}")
        skill_task.resume("yes")
        self.assertEqual((self.mount / "ledger.csv").read_bytes(), BEFORE)
        self.assertEqual((self.mount / "ledger (changed by worker).csv").read_bytes(), newer)
        self.assertEqual(self.finals[-1]["kind"], "final", self.finals[-1]["text"])

    def test_a_name_the_offer_does_not_hold_is_refused(self):
        skill_task.perform(f"/restore {self.offer_id} other.csv")
        self.assertEqual(self.finals[-1]["kind"], "error")
        self.assertIn("That offer is for `ledger.csv` only", self.finals[-1]["text"])
        self.assertEqual((self.mount / "ledger.csv").read_bytes(), AFTER)
        skill_task.perform("/restore 00000000")
        self.assertIn("There is no offer", self.finals[-1]["text"])


class NamedIn(unittest.TestCase):
    FACTS = [{"rel": "a/ledger.csv", "name": "ledger.csv", "path": "C:\\Books\\a\\ledger.csv"},
             {"rel": "b/ledger.csv", "name": "ledger.csv", "path": "C:\\Books\\b\\ledger.csv"},
             {"rel": "rates.xlsx", "name": "rates.xlsx", "path": "C:\\Books\\rates.xlsx"}]

    def test_only_a_file_the_guard_saw_change_and_a_name_that_is_not_ambiguous(self):
        self.assertEqual(source_guard.named_in(self.FACTS, ["rates.xlsx"]), ["rates.xlsx"])
        self.assertEqual(source_guard.named_in(self.FACTS, ["c:/books/b/ledger.csv"]), ["b/ledger.csv"])
        self.assertEqual(source_guard.named_in(self.FACTS, ["ledger.csv"]), [], "two changed files have that name")
        self.assertEqual(source_guard.named_in(self.FACTS, ["notes.docx", ""]), [])


from test_gate_findings_and_answers import _Reports, verdict  # noqa: E402  (the gate's harness)


class TheJudgesWord(_Reports):
    CHANGE = {"rel": "ledger.xlsx", "name": "ledger.xlsx", "path": "C:\\Books\\ledger.xlsx", "diff": "B7: 0 -> 500",
              "gone": False, "kept": True, "asked": False, "copy": "/kept/ledger.xlsx"}

    def gate(self, put_back):
        self.verdicts = [dict(verdict(verify.DONE, seen="the summary is saved"), put_back=put_back)]
        restored = []
        with mock.patch.object(skill_task.produced, "file_facts", lambda *a, **k: skill_task.produced.FileFacts()), \
                mock.patch.object(source_guard, "changed_sources", lambda task_id, request: [dict(self.CHANGE)]), \
                mock.patch.object(source_guard, "offer", lambda task_id, facts: "ab12cd34" if facts else ""), \
                mock.patch.object(source_guard, "kept_digest", lambda task_id, rel: "f" * 64), \
                mock.patch.object(source_guard, "restore_sources",
                                  lambda task_id, rels: restored.append(list(rels)) or (list(rels), [], {})):
            self.assertTrue(skill_task._gate("Made the summary from ledger.xlsx.", "final", "thebot"))
        return restored

    def test_a_file_the_judge_names_is_put_back_and_said_checked(self):
        self.assertEqual(self.gate(["ledger.xlsx"]), [["ledger.xlsx"]])
        text = self.reports[-1]["text"]
        self.assertIn("Put back your file", text)
        self.assertIn("its bytes match the copy kept before the task", text)
        self.assertNotIn("/restore", text, "nothing left to offer once it is back")

    def test_a_name_the_guard_did_not_see_change_puts_nothing_back(self):
        self.assertEqual(self.gate(["rates.xlsx"]), [])
        self.assertIn("send `/restore ab12cd34`", self.reports[-1]["text"])

    def test_the_judges_answer_carries_the_names(self):
        found = verify._parse('{"verdict": "done", "confidence": 0.9, "seen": "s", "why": "w", '
                              '"put_back": ["ledger.xlsx", 3, " "]}')
        self.assertEqual(found["put_back"], ["ledger.xlsx"])
        self.assertNotIn("put_back", verify._parse('{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}'))


if __name__ == "__main__":
    unittest.main()
