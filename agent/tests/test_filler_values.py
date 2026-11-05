"""
Finding BR (2026-10-20, round 5 P4): a joiner was onboarded in a portal with phone 9999999999; the attached tracker had
her number, "+91 90040 55318". A filler value that is in no source is refused before a form is sent (gui), and is not
done if it was typed anyway (a planned stage's check, the final gate).
"""

from needs import needs
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import attachments, filler, long_task
from decurix_agent.computer import cdp, core, recording

HEADERS = ["Employee", "Department", "Mobile", "Personal email"]


class Tracker(unittest.TestCase):
    def setUp(self):
        import openpyxl
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.tracker = self.dir / "joiners_tracker_Nov2026.xlsx"
        book = openpyxl.Workbook()
        book.active.append(HEADERS)
        book.active.append(["Sneha Iyer", "Marketing", "+91 90040 55318", "sneha.iyer@gmail.com"])
        book.save(self.tracker)
        self.request = "Onboard the joiners in the portal.\n\n" + attachments.MARK + f"\n  {self.tracker}\n"


@needs("openpyxl")
class WhatIsAFiller(Tracker):
    def test_fillers_and_real_values(self):
        for v in ("9999999999", "0000000000", "1234567890", "987654", "test", "N/A", "xxx", "a@example.com", "TBD"):
            self.assertTrue(filler.looks_filler(v), v)
        for v in ("9004055318", "+91 90040 55318", "720000", "16/11/2026", "Sneha Iyer", "INV-11111", "45,000"):
            self.assertFalse(filler.looks_filler(v), v)

    def test_a_filler_a_source_gives_is_not_invented_and_a_phone_matches_by_its_digits(self):
        fields = [["First name", "Sneha"], ["Mobile phone", "9999999999"], ["Email", "sneha.iyer@example.com"]]
        self.assertEqual(filler.invented(fields, self.request),
                         [("Mobile phone", "9999999999"), ("Email", "sneha.iyer@example.com")])
        self.assertEqual(filler.invented([["Mobile phone", "9004055318"]], self.request), [])
        said = filler.refusal([("Mobile phone", "9999999999")], [str(self.tracker)])
        self.assertIn("Mobile phone: '9999999999' (Mobile is a column of joiners_tracker_Nov2026.xlsx)", said)
        self.assertIn("digits only, without the country code", said)


@needs("openpyxl")
class TheFormIsNotSent(Tracker):
    def check(self, events, checked, mobile="9999999999", shown=None, obs_at=0):
        """_form_check on a form whose Mobile phone holds `mobile`; `events` the task's recorded actions, `shown` what
        the observation kept with the first of them showed (label: value)."""

        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        form = {"form": "Onboard new employee", "fields": [["First name", "Sneha"], ["Last name", "Iyer"],
                                                           ["Date of birth", "18/09/1999"], ["Mobile phone", mobile]]}
        save = {"ref": "e1961", "name": "Next", "path": "/form/next", "source": "cdp"}
        session = Path(tmp.name) / "recordings" / "task-t0123456789abcdef"
        (session / "obs").mkdir(parents=True)
        if shown is not None:
            (session / "obs" / "0123456789abcdef.json").write_text(json.dumps(
                {"elements": [{"role": "textbox", "name": k, "value": v} for k, v in shown.items()]}), encoding="utf-8")
            events = [dict(e, obs="0123456789abcdef") if i == obs_at else e for i, e in enumerate(events)]
        with mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "check.json"), \
                mock.patch.object(cdp, "submit_form_fields", return_value=form), \
                mock.patch.object(recording, "ROOT", Path(tmp.name) / "recordings"), \
                mock.patch("decurix_agent.computer.evidence._task_events", lambda task_id=None: list(events)), \
                mock.patch("decurix_agent.task_context.current", return_value="t0123456789abcdef"), \
                mock.patch("decurix_agent.task_context.current_request", return_value="Onboard the joiners."), \
                mock.patch("decurix_agent.task_context.current_files", return_value=[str(self.tracker)]):
            return core._form_check(save, checked=checked) or ""

    @staticmethod
    def typed(label, text):
        return {"kind": "type", "outcome": "done", "text": text, "element": {"role": "textbox", "name": label}}

    def test_the_live_form_is_refused_even_checked(self):
        for checked in (False, True):
            said = self.check([self.typed("Mobile phone", "9999999999")], checked)
            self.assertIn("Not pressed: these values look invented", said)
            self.assertIn("Mobile phone: '9999999999' (Mobile is a column of joiners_tracker_Nov2026.xlsx)", said)

    def test_a_filler_the_application_showed_before_the_worker_acted_is_the_applications(self):
        # An existing record's own value on its edit form (found in the GUI bench, 2026-11-03), seen in the observation
        # the task's first action was resolved against; compared by value, so the app's own reformatting is the same.
        click = {"kind": "click", "outcome": "done", "element": {"role": "button", "name": "Edit"}}
        said = self.check([click], checked=True, mobile="99999 99999", shown={"Mobile phone": "9999999999"})
        self.assertNotIn("look invented", said)

    def test_every_other_way_in_is_judged(self):
        # Review round 2, B9: reformatted by the app, re-cased, pasted, or picked from a list - none takes the value
        # out of judgement any more.
        cases = {
            "reformatted": [self.typed("Mobile phone", "99999-99999")],
            "pasted": [{"kind": "key", "outcome": "done", "keys": ["ctrl+v"], "element": {"role": "textbox",
                                                                                        "name": "Mobile phone"}}],
            "picked": [self.typed("Mobile phone", "99"), {"kind": "click", "outcome": "done",
                                                          "element": {"role": "option", "name": "9999999999"}}],
            "nothing recorded": [],
        }
        for case, events in cases.items():
            with self.subTest(case):
                self.assertIn("look invented", self.check(events, checked=True))

    def test_a_value_shown_after_the_worker_typed_into_the_field_is_the_workers(self):
        events = [self.typed("Mobile phone", "9999999999"),
                  {"kind": "click", "outcome": "done", "element": {"role": "button", "name": "Check"}}]
        # The observation kept with the second action shows the worker's own value: never the application's.
        said = self.check(events, checked=True, shown={"Mobile phone": "9999999999"}, obs_at=1)
        self.assertIn("look invented", said)


@needs("openpyxl")
class TypedAnyway(Tracker):
    def test_a_stage_or_task_that_typed_a_filler_is_not_done(self):
        found = long_task.invented_typing(["Sneha", "Iyer", "9999999999", "9004055318"], self.request)
        self.assertEqual(len(found), 1)
        self.assertIn("'9999999999'", found[0])
        self.assertNotIn("9004055318", found[0])
        self.assertEqual(long_task.invented_typing(["Sneha", "9004055318"], self.request), [])


if __name__ == "__main__":
    unittest.main()
