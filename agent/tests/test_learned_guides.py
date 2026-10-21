"""Guides a desktop learns to an app nobody wrote one for: kept only when the task that wrote them is checked done,
never with a person's data, never over a shipped guide."""

import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import app_skills

BODY = ("Zoho Books runs in the browser. Invoices: Sales > Invoices > New. The customer field is a search box - type "
        "part of the name and pick the row from the list; pressing Enter alone leaves it empty. Line items: Tab moves "
        "from Item to Quantity to Rate. Save as Draft keeps it unsent; check the invoice number shown at the top.")


class LearnedGuides(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.patch = mock.patch.object(app_skills, "LEARNED", Path(self.dir.name))
        self.patch.start()

    def tearDown(self):
        self.patch.stop()
        self.dir.cleanup()

    def names(self, text):
        return [g["name"] for g in app_skills.matching(text)]

    def test_a_guide_is_used_only_after_its_task_is_checked_done(self):
        said = app_skills.learn("zoho-books", "zoho books, zoho", "Invoices in Zoho Books", BODY, "t1")
        self.assertIn("when this task is checked done", said)
        self.assertEqual(self.names("Raise the invoice in Zoho Books"), [])  # pending: not a guide yet
        self.assertEqual(app_skills.promote("t1"), ["zoho-books"])
        self.assertEqual(self.names("Raise the invoice in Zoho Books"), ["zoho-books"])
        text = app_skills.context("Raise the invoice in Zoho Books")
        self.assertIn("Guide learned on this desktop: zoho-books", text)
        self.assertIn("the screen is right", text)
        self.assertIn("Save as Draft", text)

    def test_a_guide_from_a_task_that_never_ended_done_is_never_kept(self):
        app_skills.learn("zoho-books", "zoho books", "Invoices", BODY, "t1")
        self.assertEqual(app_skills.promote("t2"), [])  # another task's done does not keep it
        self.assertEqual(self.names("zoho books"), [])
        old = Path(self.dir.name) / app_skills.PENDING / "t1"
        stale = time.time() - (app_skills.PENDING_DAYS + 1) * 86400
        os.utime(old, (stale, stale))
        app_skills.promote("t3")
        self.assertFalse(old.exists())

    def test_in_a_planned_run_a_guide_is_kept_when_its_own_stage_passes_its_check(self):
        said = app_skills.learn("zoho-books", "zoho books", "Invoices", BODY, "t1", stage="run9:s2-invoices")
        self.assertIn("when this stage is checked done", said)
        app_skills.learn("odoo", "odoo", "Purchase orders", BODY.replace("Zoho Books", "Odoo"), "t1", stage="run9:s3-po")
        self.assertEqual(app_skills.promote("t1", "run9:s2-invoices"), ["zoho-books"])
        self.assertEqual(self.names("odoo"), [])  # its stage has not passed
        self.assertEqual(self.names("zoho books"), ["zoho-books"])

    def test_a_persons_data_is_never_kept_in_a_guide(self):
        for leak in ("Send it to accounts@acme.example", "Bank account 50100012345678 goes in Payee",
                     "Log in with password: 'Hunter2!'"):
            with self.assertRaises(ValueError):
                app_skills.learn("zoho-books", "zoho books", "Invoices", BODY + " " + leak, "t1")

    def test_a_guide_must_say_how_the_app_works(self):
        with self.assertRaises(ValueError):
            app_skills.learn("zoho-books", "zoho books", "Invoices", "Click save.", "t1")
        with self.assertRaises(ValueError):
            app_skills.learn("Zoho Books!", "zoho books", "Invoices", BODY, "t1")
        with self.assertRaises(ValueError):
            app_skills.learn("zoho-books", "", "Invoices", BODY, "t1")

    def test_a_shipped_guide_is_never_replaced_only_added_to(self):
        app_skills.learn("tallyprime", "tallyprime, tally", "What this desktop found", BODY.replace("Zoho Books", "Tally"),
                         "t1")
        app_skills.promote("t1")
        found = self.names("Post the voucher in TallyPrime")
        self.assertIn("tallyprime", found)
        self.assertIn("tallyprime-notes", found)
        shipped = next(g for g in app_skills.guides() if g["name"] == "tallyprime")
        self.assertEqual(shipped["learned"], "")

    def test_learning_again_keeps_the_previous_version(self):
        app_skills.learn("zoho-books", "zoho books", "Invoices", BODY, "t1")
        app_skills.promote("t1")
        app_skills.learn("zoho-books", "zoho books", "Invoices", BODY + " Credit notes: Sales > Credit Notes.", "t2")
        app_skills.promote("t2")
        folder = Path(self.dir.name) / "zoho-books"
        self.assertIn("Credit notes", (folder / "SKILL.md").read_text(encoding="utf-8"))
        self.assertNotIn("Credit notes", (folder / "SKILL.prev.md").read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
