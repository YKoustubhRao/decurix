"""
Finding BU (2026-10-20, P4 scored 36/36): the run's report said "Not everything is done" and listed, for every joiner,
"Figures that do not agree" - all of which agreed (02/11/2026 in a sheet, a letter and a log; 480000 and "Rs 480,000").
Each place now gives its own figure, compared as what it stands for.
"""

import tempfile
import unittest
from pathlib import Path

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import long_task

REQUEST = "Onboard them. Dates as DD/MM/YYYY."


def outputs(dir_):
    files = {
        "joiners_review_and_salary_working.csv": "Name,Joining,CTC,Basic,HRA\nAnanya Deshpande,02/11/2026,480000,16000,8000\n",
        "offer_letter_Ananya_Deshpande.txt": "Your date of joining will be 02/11/2026. Your CTC will be Rs 480,000. "
                                             "Basic 16,000 a month, HRA 8,000.",
        "portal_onboarding_log.csv": "EMP-2041,Ananya Deshpande,02/11/2026,480000\n",
    }
    out = []
    for name, text in files.items():
        path = Path(dir_) / name
        path.write_text(text, encoding="utf-8")
        out.append(("s", str(path)))
    return out


def places(*rows):
    return [{"file": f, "text": t, "value": v} for f, t, v in rows]


class Values(unittest.TestCase):
    def test_the_same_figure_written_differently_is_the_same(self):
        same = [("480000", "Rs 480,000"), ("480000", "4,80,000"), ("16000", "16,000.00"), ("1800", "INR 1,800")]
        for a, b in same:
            self.assertTrue(long_task.same_fact(long_task.fact_value(a), long_task.fact_value(b)), (a, b))
        self.assertFalse(long_task.same_fact(long_task.fact_value("480000"), long_task.fact_value("4,80,500")))
        dates = ("02/11/2026", "2 Nov 2026", "2026-11-02", "Nov 2, 2026")
        for d in dates:
            self.assertTrue(long_task.same_fact(long_task.fact_value("02/11/2026", True), long_task.fact_value(d, True)), d)
        # With no convention, 02/11/2026 is not guessed into a date - only equal to itself as written.
        self.assertEqual(long_task.fact_value("02/11/2026")[0], "text")
        self.assertTrue(long_task.same_fact(long_task.fact_value("13/11/2026"), long_task.fact_value("13 Nov 2026")))


class UnitsAndRounding(unittest.TestCase):
    """BU2 (2026-10-20, P5): a commentary's exact variance and a deck's rounded lakh figure, a workbook's revenue and a
    deck table's bare lakh figures under "Figures in Rs lakh" - all the same, all reported as disagreeing."""

    def test_the_live_lines_agree(self):
        f, same = long_task.fact_value, long_task.same_fact
        self.assertTrue(same(f("531,169"), f("+5.3 lakh")))   # West Sep variance
        self.assertTrue(same(f("448,679"), f("-4.5 lakh")))   # South Aug: the sign is in words on one side
        self.assertTrue(same(f("364,728"), f("-3.6 lakh")))   # North Jul
        lakh = long_task.declared_unit("MIS Q2 FY27\nFigures in Rs lakh\n| West | 40.07 | 45.22 | 51.31 | 136.60 |")
        self.assertEqual(lakh, 100000)
        self.assertTrue(same(f("13660029"), f("136.60", unit=lakh)))  # West Q2 revenue
        self.assertTrue(same(f("1078139.62"), f("10.78 lakh")))       # West revenue per head
        self.assertEqual(long_task.declared_unit("Revenue (Rs crore)"), 10**7)
        self.assertEqual(long_task.declared_unit("A plain memo."), 1)

    def test_a_real_difference_is_still_one(self):
        f, same = long_task.fact_value, long_task.same_fact
        self.assertFalse(same(f("531,169"), f("5.9 lakh")))
        self.assertFalse(same(f("13660029"), f("137.60", unit=100000)))


class Signs(unittest.TestCase):
    """Equal magnitudes are the same figure: no word near it decides a sign, and two written signs that differ are a
    quiet note, never "do not agree" (series 15 H2, 2026-10-21) - a flipped variance is the judge's, from the files."""

    def check(self, commentary, deck, notes=None):
        with tempfile.TemporaryDirectory() as tmp:
            a, b = Path(tmp) / "Q2_commentary.txt", Path(tmp) / "MIS_deck.txt"
            a.write_text(commentary[0], encoding="utf-8")
            b.write_text(deck[0], encoding="utf-8")
            figures = [{"figure": "variance", "places": [
                {"file": a.name, "text": commentary[0], "value": commentary[1]},
                {"file": b.name, "text": deck[0], "value": deck[1]}]}]
            found, _ = long_task.model_disagreements([("s1", str(a)), ("s2", str(b))],
                                                     ask=lambda s, p: {"figures": figures}, request="", notes=notes)
        return found

    def test_the_sign_cases(self):
        self.assertEqual(self.check(("an adverse variance of 448,679", "448,679"), ("South Aug -4.5 lakh", "-4.5 lakh")), [])
        notes = []
        self.assertEqual(self.check(("a favourable variance of 531,169", "531,169"), ("West Sep -5.3 lakh", "-5.3 lakh"),
                                    notes), [])
        self.assertEqual(len(notes), 1)
        self.assertIn("the same amount, shown with opposite signs in Q2_commentary.txt and MIS_deck.txt", notes[0])
        self.assertEqual(self.check(("West Sep 531,169", "531,169"), ("West Sep -5.3 lakh", "-5.3 lakh")), [])
        self.assertEqual(self.check(("South Aug (4,48,679)", "(4,48,679)"), ("South Aug -4.5 lakh", "-4.5 lakh")), [])


class TheLiveCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.outputs = outputs(tmp.name)

    def ask(self, figures):
        return lambda system, prompt: {"figures": figures}

    def test_figures_that_agree_are_not_reported(self):
        figures = [
            {"figure": "Ananya Deshpande joining date", "places": places(
                ("joiners_review_and_salary_working.csv", "Ananya Deshpande,02/11/2026,480000,16000,8000", "02/11/2026"),
                ("offer_letter_Ananya_Deshpande.txt", "Your date of joining will be 02/11/2026.", "02/11/2026"),
                ("portal_onboarding_log.csv", "EMP-2041,Ananya Deshpande,02/11/2026,480000", "02/11/2026"))},
            {"figure": "Ananya Deshpande annual CTC", "places": places(
                ("joiners_review_and_salary_working.csv", "Ananya Deshpande,02/11/2026,480000,16000,8000", "480000"),
                ("offer_letter_Ananya_Deshpande.txt", "Your CTC will be Rs 480,000.", "Rs 480,000"))},
            {"figure": "Ananya Deshpande basic", "places": places(
                ("joiners_review_and_salary_working.csv", "Ananya Deshpande,02/11/2026,480000,16000,8000", "16000"),
                ("offer_letter_Ananya_Deshpande.txt", "Basic 16,000 a month", "16,000"))},
        ]
        found, agreed = long_task.model_disagreements(self.outputs, ask=self.ask(figures), request=REQUEST)
        self.assertEqual(found, [])
        self.assertEqual(agreed, 3)

    def test_a_real_disagreement_is_still_reported_briefly(self):
        figures = [{"figure": "annual CTC", "places": places(
            ("joiners_review_and_salary_working.csv", "Ananya Deshpande,02/11/2026,480000,16000,8000", "480000"),
            ("offer_letter_Ananya_Deshpande.txt", "Your CTC will be Rs 480,000.", "Rs 4,80,500"))}]
        # The letter's value is not in its snippet: that place cannot be compared, so nothing is reported ...
        found, _ = long_task.model_disagreements(self.outputs, ask=self.ask(figures), request=REQUEST)
        self.assertEqual(found, [])
        # ... and with a letter that really says 4,80,500, it is.
        path = Path(self.outputs[1][1])
        path.write_text("Your CTC will be Rs 4,80,500.", encoding="utf-8")
        figures[0]["places"][1] = {"file": path.name, "text": "Your CTC will be Rs 4,80,500.", "value": "Rs 4,80,500"}
        found, _ = long_task.model_disagreements(self.outputs, ask=self.ask(figures), request=REQUEST)
        self.assertEqual(len(found), 1)
        self.assertEqual([v["value"] for v in found[0]["values"]], ["480000", "Rs 4,80,500"])

    def test_a_row_with_several_figures_and_no_value_cannot_disagree(self):
        figures = [{"figure": "basic", "places": [
            {"file": "joiners_review_and_salary_working.csv", "text": "Ananya Deshpande,02/11/2026,480000,16000,8000"},
            {"file": "offer_letter_Ananya_Deshpande.txt", "text": "Basic 16,000 a month"}]}]
        found, _ = long_task.model_disagreements(self.outputs, ask=self.ask(figures), request=REQUEST)
        self.assertEqual(found, [])


if __name__ == "__main__":
    unittest.main()
