"""
A record a task saved that nothing read back against a source is named in the report and never under a checked done
(records.saved_unread, final_gate). Found live (2026-11-09): a worker that declared nothing created a party's ledger with
its state left unset, and the run ended done on the model's word.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import records

SAVED = "\"Ledger saved.\" appeared; page is now http://books.example.test/company/1/ledgers/31"


def events(*changes):
    out = []
    for change in changes:
        out += [{"kind": "open", "outcome": "done", "target": "http://books.example.test/company/1/ledgers/new",
                 "change": "page is now http://books.example.test/company/1/ledgers/new"},
                {"kind": "click", "outcome": "done", "change": change}]
    return out


def ledger(readback):
    return [{"key": "Hampi Residency", "saved_at": 1.0, "saved": " ".join(SAVED.split()), "readback": readback}]


class SavedUnread(unittest.TestCase):
    def run_with(self, ledger_records, *changes):
        with mock.patch.object(records, "load", return_value=ledger_records), \
                mock.patch("decurix_agent.computer.core._saved", lambda change: "saved" in change.lower()):
            return records.saved_unread("t1", events=events(*changes))

    def test_a_save_no_declaration_covers_is_unread(self):
        self.assertEqual(self.run_with([], SAVED), ["http://books.example.test/company/1/ledgers/31"])

    def test_a_declared_record_read_back_whole_is_read(self):
        self.assertEqual(self.run_with(ledger({"seen": ["Name"], "absent": []}), SAVED), [])

    def test_a_record_read_back_with_a_value_missing_is_unread(self):
        self.assertEqual(len(self.run_with(ledger({"seen": ["Name"], "absent": ["State"]}), SAVED)), 1)

    def test_nothing_saved_is_nothing_unread(self):
        self.assertEqual(self.run_with([]), [])

    def test_a_long_save_cut_on_a_space_is_still_the_records(self):
        # Found live (2026-11-15): the 300-character cut ended on a space; the event kept it and the ledger lost it.
        long = SAVED + "; " + "x" * (299 - len(SAVED) - 2) + " (+8 more)"
        self.assertEqual(" ".join(long.split())[:300][-1], " ")
        stored = [{"key": "Hampi Residency", "saved_at": 1.0, "saved": " ".join(long.split())[:300],
                   "readback": {"seen": ["Name"], "absent": []}}]
        self.assertEqual(self.run_with(stored, long), [])


class TheReportIsNotCheckedAgain(unittest.TestCase):
    """Found live (2026-11-15, an eval run): the "NOT checked" report went back through the final gate, which judged it done
    again and wrote it again - 63 judge calls until the task's time ran out. The gate settled it: it goes out gated."""

    def test_judged_done_with_a_save_unread_goes_out_once_not_checked(self):
        from decurix_agent import final_gate, skill_task, verify
        judged = []
        with mock.patch.object(skill_task.guidance, "get",
                               lambda: {"mode": "engine", "run": None, "task": "Enter the bill", "attempt": 1}), \
                mock.patch.object(final_gate, "_did_screen_work", lambda: True), \
                mock.patch.object(final_gate, "_saved_unread", lambda: ["http://records.example.test/bills/3"]), \
                mock.patch.object(skill_task.produced, "files", lambda *a, **k: []), \
                mock.patch.object(skill_task.produced, "drafts", lambda: []), \
                mock.patch.object(skill_task.guidance, "refused", lambda: None), \
                mock.patch.object(skill_task.guidance, "end", lambda: None), \
                mock.patch.object(skill_task.source_guard, "changed_sources", lambda *a: []), \
                mock.patch.object(skill_task.source_guard, "tidy_own_backups", lambda *a: None), \
                mock.patch.object(skill_task.source_guard, "resaved", lambda *a: []), \
                mock.patch.object(skill_task.verify, "judge", lambda *a, **k: judged.append(1) or {
                    "verdict": verify.DONE, "confidence": 1.0, "seen": "Saved.", "why": "saved"}), \
                mock.patch.object(skill_task, "write_final_reply") as sent:
            self.assertTrue(skill_task._gate("Saved bill AB-1234.", "final", "thebot"))
        self.assertEqual(len(judged), 1)
        self.assertTrue(sent.call_args.kwargs.get("gated"), sent.call_args)
        self.assertFalse(sent.call_args.kwargs["checked"])
        self.assertIn("NOT checked", sent.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
