"""Guides to well-known apps: each is well formed, a task gets the guides of the apps it names and no others."""

import re
import unittest

from decurix_agent import app_skills


class TheGuides(unittest.TestCase):
    def test_every_guide_is_well_formed_and_short(self):
        found = app_skills.guides()
        self.assertGreaterEqual(len(found), 8)
        for g in found:
            self.assertRegex(g["name"], r"^[a-z0-9-]+$")
            self.assertTrue(g["description"], g["name"])
            self.assertTrue(g["terms"], g["name"])
            # Read on every turn of a task that names the app: kept short.
            self.assertLess(len(g["body"]), 5500, g["name"])

    def test_a_task_gets_the_guides_of_the_apps_it_names(self):
        def names(text):
            return [g["name"] for g in app_skills.matching(text)]

        self.assertEqual(names("In TallyPrime (on your desktop), create a company"), ["tallyprime"])
        self.assertIn("microsoft-excel", names("Use Excel (the Excel app on your desktop) to open sales.csv"))
        self.assertEqual(names("Use Word (the Word app on your desktop) to write a reply"), ["microsoft-word"])
        self.assertEqual(names("In Photoshop, resize each attached product photo"), ["gimp"])
        # A file-only task, and words that only look like an app's name, get nothing.
        self.assertEqual(names("Combine the invoices into totals.xlsx and draft an email"), [])
        self.assertEqual(names("Write a short reply letter; keep every word polite"), [])
        self.assertEqual(names("Read the excellent report"), [])

    def test_render_keeps_whole_guides_within_the_limit(self):
        found = app_skills.matching("excel tally gimp")
        text = app_skills.render(found, limit=6000)
        self.assertLessEqual(len(text), 6400)
        for g in found:
            if f"## Guide: {g['name']}" in text:
                self.assertIn(g["body"][-40:], text)  # never cut in the middle

    def test_a_guide_is_found_by_its_name_or_its_app(self):
        self.assertEqual(app_skills.named("tallyprime")["name"], "tallyprime")
        self.assertEqual(app_skills.named("Tally")["name"], "tallyprime")
        self.assertIsNone(app_skills.named("no such app"))

    def test_guides_teach_no_values(self):
        # A guide says how an app works; the values of a task come from its request and documents.
        for g in app_skills.guides():
            self.assertNotRegex(g["body"], re.compile(r"\b(Northwind|Kaveri|INV-\d)", re.I), g["name"])


if __name__ == "__main__":
    unittest.main()
