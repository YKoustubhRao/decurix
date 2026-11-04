"""
Two findings of an eval probe (W0.3, a slipped account shared its last 10 digits with another payee, 2026-11-05), each failing on the code before:

- A field corrected in place clears its earlier slip: the form's value for a field is its latest. The probe typed
  another payee's account (sharing its last 10 digits), the save was rightly refused, the account was retyped in the
  same form - and four more saves were still refused, because every value ever typed into the form stayed in it.
- A refused action is an event of the task: which guard refused it, and why, is in the task's record (and so in the
  run's trajectory and bundle). The four refused saves left no trace.
"""

import unittest
from types import SimpleNamespace
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import approval, audit, core  # noqa: E402

NEW = {"kind": "click", "outcome": "done", "change": "page is now http://app/payees/new"}
HEADER = ["Name", "Bank", "Account number"]
ROWS = [["Example Holdings", "First Bank", "001234567890"],
        ["Example Payee Services", "First Bank", "991234567890"]]


def typed(label, text, path=None, **extra):
    return dict({"kind": "type", "outcome": "done", "text": text,
                 "element": {"name": label, "path": path or f"form > textbox {label}"}}, **extra)


class ACorrectedFieldClearsItsSlip(unittest.TestCase):
    SLIP = [NEW, typed("Name", "Example Payee Services"), typed("Bank", "First Bank"),
            typed("Account number", "001234567890")]

    def test_the_forms_value_for_a_field_is_its_latest(self):
        self.assertEqual(core._current_form(self.SLIP + [typed("Account number", "991234567890")]),
                         [("Name", "Example Payee Services"), ("Bank", "First Bank"),
                          ("Account number", "991234567890")])
        # Appended to, it holds both parts.
        self.assertEqual(core._current_form([NEW, typed("Ref", "AB"), typed("Ref", "12", append=True)]),
                         [("Ref", "AB12")])

    def refusal(self, events):
        with mock.patch("decurix_agent.computer.recording.load", lambda session: ({}, events)), \
                mock.patch("decurix_agent.computer.recording.session_for_task", lambda task_id: "s"), \
                mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"), \
                mock.patch("decurix_agent.stage_facts.tables_of", lambda files: [("payees.xlsx", HEADER, ROWS)]), \
                mock.patch.object(core, "_source_files", lambda: ["payees.xlsx"]):
            return core._crossed_record({"role": "button", "name": "Save"})

    def test_slip_then_correct_in_place_then_the_save_passes(self):
        self.assertIn("Example Holdings's", self.refusal(self.SLIP))  # (the slip itself: refused, rightly)
        self.assertEqual(self.refusal(self.SLIP + [typed("Account number", "991234567890")]), "")


class ARefusedActionIsAnEvent(unittest.TestCase):
    FIELD = {"role": "button", "name": "Save", "source": "cdp", "ref": "e3", "path": "site x > button Save",
             "box": [10, 10, 40, 20], "states": [], "group": "g", "window_title": "Site"}

    def test_the_guard_and_its_reason_are_recorded(self):
        page = {"elements": [self.FIELD], "frame": None, "windows": [], "size": [1280, 720], "active_title": "Site",
                "groups": [{"id": "g", "source": "cdp", "url": "https://site.example/new", "window_title": "Site"}]}
        kept = []
        reason = "Not pressed: this record's form holds another record's values (Account number is '001234567890')"
        with mock.patch.object(core, "observe", side_effect=lambda *a, **k: dict(page)), \
                mock.patch.object(core, "_commit_check", lambda *a, **k: reason), \
                mock.patch.object(core, "_perform", side_effect=AssertionError("a refused save was pressed")), \
                mock.patch.object(audit, "record", side_effect=lambda entry, **k: kept.append(dict(entry))), \
                mock.patch.object(core, "_downloads_snapshot", return_value={}), \
                mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            with self.assertRaises(core.ComputerError) as caught:
                core.act("click", ref="e3", check_form=True)
        self.assertEqual(str(caught.exception), reason)
        self.assertEqual(len(kept), 1)
        self.assertEqual((kept[0]["outcome"], kept[0]["guard"], kept[0]["reason"]), ("refused", "pre_save", reason))

    def test_a_refused_action_is_no_done_value(self):
        # (The checks that read the task's events read only what was done: a refused save is no save.)
        events = [NEW, typed("Account number", "001234567890"),
                  {"kind": "click", "outcome": "refused", "guard": "pre_save", "change": "saved"}]
        self.assertEqual(core.records_created(events), 0)


if __name__ == "__main__":
    unittest.main()
