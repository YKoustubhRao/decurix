"""
Finding BR (2026-10-20, round 5 P4): a joiner was onboarded in a portal with phone 9999999999; the attached tracker had
her number, "+91 90040 55318". A filler value that is in no source is refused before a form is sent (gui), and is not
done if it was typed anyway (a planned stage's check, the final gate).
"""

from needs import needs
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import attachments, filler, long_task
from decurix_agent.computer import cdp, core

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
    def check(self, typed, checked):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        form = {"form": "Onboard new employee", "fields": [["First name", "Sneha"], ["Last name", "Iyer"],
                                                           ["Date of birth", "18/09/1999"], ["Mobile phone", "9999999999"]]}
        save = {"ref": "e1961", "name": "Next", "path": "/form/next", "source": "cdp"}
        with mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "check.json"), \
                mock.patch.object(cdp, "submit_form_fields", return_value=form), \
                mock.patch("decurix_agent.computer.evidence.entered", lambda task_id=None: list(typed)), \
                mock.patch("decurix_agent.task_context.current_request", return_value="Onboard the joiners."), \
                mock.patch("decurix_agent.task_context.current_files", return_value=[str(self.tracker)]):
            return core._form_check(save, checked=checked) or ""

    def test_the_live_form_is_refused_even_checked(self):
        for checked in (False, True):
            said = self.check(["Sneha", "Iyer", "18/09/1999", "9999999999"], checked)
            self.assertIn("Not pressed: these values look invented", said)
            self.assertIn("Mobile phone: '9999999999' (Mobile is a column of joiners_tracker_Nov2026.xlsx)", said)

    def test_a_filler_the_application_filled_in_itself_is_not_the_workers(self):
        # An existing record's own value on its edit form (found in the GUI bench, 2026-11-03): the worker never typed
        # it, so it is not judged as invented.
        said = self.check(["Sneha", "Iyer"], checked=True)
        self.assertNotIn("look invented", said)


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
