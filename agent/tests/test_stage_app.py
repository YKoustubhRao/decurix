"""
Finding CL (2026-10-20, round 5 P1): "3. A 5-slide MIS deck in PowerPoint ... 4. A Word memo" sent the workbook
stage to open, check and save month_end_close_Oct2026.xlsx in PowerPoint, which could not open it, and the person was
asked. A named app binds to its deliverable: a stage is held to an app only for a file of that app's kind it hands in.
"""

import unittest

import test_long_requests as base  # noqa: F401 - stubs config first
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import long_task


class StageApp(unittest.TestCase):
    def setUp(self):
        global P1, P4
        P1 = fixture_paths.need("replay", "P1_20261020-063847", "request.txt").read_text(encoding="utf-8")
        P4 = fixture_paths.need("replay", "P4_20261020-150848", "request.txt").read_text(encoding="utf-8")

    def app(self, request, *outputs):
        return long_task.stage_app({"outputs": list(outputs)}, request)

    def test_the_live_p1_request(self):
        self.assertIn("deck in PowerPoint", " ".join(P1.split()))
        self.assertIsNone(self.app(P1, "P1_month_end/outputs/month_end_close_Oct2026.xlsx"))
        self.assertEqual(self.app(P1, "PC/process/P1/MIS_Oct2026.pptx"), ("PowerPoint", "MIS_Oct2026.pptx"))
        self.assertEqual(self.app(P1, "PC/process/P1/Exceptions_memo_Oct2026.docx"),
                         ("Word", "Exceptions_memo_Oct2026.docx"))
        self.assertIsNone(self.app(P1, "P1_month_end/outputs/overdue_reminder_recipients.csv"))

    def test_a_template_named_word_is_no_app(self):
        self.assertIsNone(self.app(P4, "P4_onboarding_Nov/offer_letter_ananya_deshpande.docx"))

    def test_an_app_given_as_the_format_or_a_choice_is_no_app(self):
        self.assertIsNone(self.app("Make his order sheet in Excel, wa_orders.xlsx in my work folder.", "wa_orders.xlsx"))
        self.assertIsNone(self.app("Save it as a report (in Excel format).", "report.xlsx"))
        self.assertIsNone(self.app("Fill the letters (or in Documents on my PC if you use Word there).", "letter.docx"))
        self.assertEqual(self.app("Use Excel to build the ageing.", "ageing.xlsx"), ("Excel", "ageing.xlsx"))


if __name__ == "__main__":
    unittest.main()
