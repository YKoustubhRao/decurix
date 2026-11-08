"""
Provenance (harness audit, 2026-10-21): a value typed into a form or written into a draft is real only when a source
holds it - the request, its files, a person's message, the system's pages the tools read, or the work's own recorded
computation. One normaliser says "the same value": a date in any form, a figure at its written precision (grouping,
currency, lakh), a long number by its digits, a code by its letters and digits.
"""

import datetime
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, provenance, stage_checks

TRACKER = ("Full name,Personal email,Mobile,Joining date,Reporting manager,Annual CTC (Rs),Order\n"
           "Ananya Deshpande,ananya@example.com,+91 90040 55318,02/11/2026,Sanjay Patel,\"4,80,000\",SO-5101\n")


class TheFactsAText(unittest.TestCase):
    def test_what_is_looked_for_and_what_is_not(self):
        facts = provenance.facts_in("Mail ananya@example.com: join on 2 Nov 2026, CTC Rs 4,80,000, order SO-5101, "
                                    "qty 40 at 10:30 am, 18% GST, 3rd floor, see http://localhost:8701/orders/5101")
        kinds = {(k, v) for k, v in facts}
        self.assertIn(("email", "ananya@example.com"), kinds)
        self.assertIn(("date", "2 Nov 2026"), kinds)
        self.assertIn(("code", "SO-5101"), kinds)
        self.assertTrue(any(k == "number" and "4,80,000" in v for k, v in kinds), kinds)
        written = " ".join(v for _, v in facts)
        for absent in ("40", "10:30", "18", "3rd", "8701", "localhost"):
            self.assertNotIn(absent, written.split(), absent)


class TheSourcesHoldIt(unittest.TestCase):
    def setUp(self):
        self.sources = provenance.Sources([TRACKER, "Refund of Rs 2,100.00 approved (working: 60 x 35)."])

    def test_the_same_fact_in_another_form_is_held(self):
        for text in ("2026-11-02", "02 Nov 2026", "Nov 2, 2026", "480000", "Rs 4.8 lakh", "9004055318", "so-5101",
                     "ANANYA@example.com", "2100", "2,100"):
            self.assertEqual(self.sources.unsourced(text), [], text)

    def test_a_fact_no_source_holds_is_found(self):
        said = "CTC Rs 5,00,000, joins 11 Nov 2026, phone 9999999999, SO-5199"
        self.assertEqual(self.sources.unsourced(said), ["Rs 5,00,000", "9999999999"])  # blocking: an amount, an id
        # A date and a letter-and-digit code are evidence only (independent review, 2026-10-21).
        self.assertEqual(self.sources.doubtful(said), ["11 Nov 2026", "SO-5199"])

    def test_an_amount_in_brackets_is_held_by_its_amount(self):
        # The replay gate (P6, 2026-10-21): "refund for the 40 hoodies (Rs 35,960)" read as a negative figure.
        self.assertEqual(provenance.Sources(["refund for the 40 hoodies (Rs 35,960); 2 boxes (₹480)"]).unsourced(
            "Your refund of Rs 35,960 and ₹480"), [])

    def test_the_day_the_work_is_done_is_held(self):
        sources = provenance.Sources([])
        sources.add_date(datetime.date(2026, 10, 21))
        self.assertEqual(sources.unsourced("As of 21/10/2026"), [])


class TypedIntoAForm(unittest.TestCase):
    """long_task.invented_typing: a placeholder or a fact no source holds, typed into a form."""

    def test_a_value_from_no_source_is_found_and_one_from_the_work_folder_is_not(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / "joiners.csv").write_text(TRACKER, encoding="utf-8")
            (work / "working.csv").write_text("Order,Total\nSO-5101,\"13,456.00\"\n", encoding="utf-8")
            request = f"Onboard the joiners in {work / 'joiners.csv'} into the portal at http://localhost:8701."
            with mock.patch.object(config, "WORK_DIR", work), mock.patch.object(stage_checks, "_page_notes",
                                                                                lambda *a: ["Order SO-5102 saved"]):
                self.assertEqual(long_task.invented_typing(["Ananya Deshpande", "9004055318", "02-11-2026", "13456",
                                                            "SO-5102", "clerk"], request), [])
                found = long_task.invented_typing(["9999999999", "test", "4,90,000"], request)
        self.assertEqual(len(found), 1)
        for value in ("'9999999999'", "'test'", "'4,90,000'"):
            self.assertIn(value, found[0])


class AStageDraft(unittest.TestCase):
    def test_a_draft_value_no_source_holds_blocks_and_a_field_difference_is_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / "joiners.csv").write_text(TRACKER, encoding="utf-8")
            request = f"Draft a welcome email to each joiner in {work / 'joiners.csv'}."
            right = {"to": ["ananya@example.com"], "subject": "Welcome",
                     "body": "Dear Ananya, you join us on 2 Nov 2026. Your reporting manager is Sanjay Patel."}
            invented = {"to": ["ananya@example.com"], "subject": "Welcome",
                        "body": "Dear Ananya, you join us on 2 Nov 2026. Your reporting manager is Rakesh Menon. "
                                "Please reply by 14 Nov 2026."}
            with mock.patch.object(config, "WORK_DIR", work), \
                    mock.patch.object(stage_checks, "_page_notes", lambda *a: []):
                self.assertEqual(long_task.draft_checks([right], request), ([], []))
                unsourced, fields = long_task.draft_checks([invented], request)
        # A date, and a name found by matching a column's words, are evidence, never a verdict (independent review,
        # 2026-10-21): nothing here is certain enough to block.
        self.assertEqual(unsourced, [])
        self.assertIn('Reporting manager is "Rakesh Menon"', " ".join(fields))


class AFigureWorkedOutFromValuesTypedWithIt(unittest.TestCase):
    """Found live (2026-10-24): the tax of each line of a bill that prints only their total (5,200 at 5% = 260,
    36,000 at 18% = 6,480) was typed beside its amount and rate, and the final check refused a finished task."""

    def test_one_step_from_another_typed_value_and_a_source_is_held(self):
        src = provenance.Sources(["Bill 454: Rusk 36,000.00 @ 18%; Rice 5,200.00 @ 5%; IGST 6,740.00"])
        typed = ["36000", "18%", "6480", "5200", "5%", "260", "9999"]
        ops = provenance._figures(" ".join(typed))
        self.assertEqual([v for v in typed if src.unsourced(v, operands=ops)], ["9999"])
        # Alone, as before, a worked-out figure is not held.
        self.assertEqual(src.unsourced("260"), ["260"])


if __name__ == "__main__":
    unittest.main()
