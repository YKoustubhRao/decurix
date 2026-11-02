import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import binding, guidance, long_task, task_context
from decurix_agent.computer import evidence


# ============================================================
# Found live (2026-10-22): asked for another client's return "the way I
# showed you", the run's values came back as the teaching client's own
# ("copied from example") and the replay typed them into the other client's
# return. The example says which thing to read, never what it is.
# ============================================================

PARAMS = [
    {"name": "portal_username", "label": "Username", "example": "manipalint", "varies": True},
    {"name": "taxable", "label": "Taxable value", "example": "165000", "varies": True},
    {"name": "rate", "label": "Rate", "example": "18%", "varies": True},
]


class TestBindingLeavesOutTheTaughtExample(unittest.TestCase):

    def test_an_example_value_no_text_of_this_run_holds_is_left_out(self):
        request = "Prepare Kavya Constructions' GSTR-1 for September; their username is kavyacon."
        answer = {"values": {"portal_username": "kavyacon", "taxable": "165000", "rate": "18%"}}
        values, _ = binding._clean(PARAMS, answer, [request])
        self.assertEqual(values, {"portal_username": "kavyacon"})

    def test_an_example_value_this_runs_document_says_is_kept(self):
        document = "B2C taxable value 1,65,000.00 at 18% - same as last month"
        answer = {"values": {"taxable": "165000.00", "rate": "18%"}}
        values, _ = binding._clean(PARAMS, answer, ["request", document.replace("1,65,000.00", "165,000.00")])
        self.assertEqual(values, {"taxable": "165000.00", "rate": "18%"})

    def test_a_number_is_held_only_by_the_same_amount(self):
        self.assertFalse(binding.held_by("165000", ["total 1650000 and 16500"]))
        self.assertTrue(binding.held_by("254237.29", ["Taxable 2,54,237.29".replace("2,54,", "254,")]))
        self.assertTrue(binding.is_example("165,000.00", "165000"))
        self.assertFalse(binding.is_example("254237.29", "254237"))


class TestGuiDoRefusesTheTaughtExampleOnce(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.skill = {"name": "gstr1-prepare", "params": PARAMS}
        sources = mock.Mock()
        sources.unsourced = lambda v: [v] if v == "165000" else []
        for patcher in (mock.patch.object(evidence, "NOTES_DIR", self.dir),
                        mock.patch.object(task_context, "current", return_value="t5"),
                        mock.patch.object(task_context, "current_request", return_value="Prepare Kavya's GSTR-1"),
                        mock.patch.object(long_task, "work_sources", return_value=sources),
                        mock.patch.object(guidance, "run_values", return_value={})):
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_an_unheld_example_is_refused_once_then_goes_ahead(self):
        given = {"portal_username": "kavyacon", "taxable": "165000", "rate": "18%"}
        said = guidance.taught_values_check(self.skill, given)
        self.assertIn("taxable=165000", said)
        self.assertIn("TAUGHT", said)
        self.assertIn("rate=18%", said)  # also the example, named as suspect
        self.assertEqual(json.loads((self.dir / "t5.examples").read_text()), ["gstr1-prepare|taxable|165000"])
        self.assertEqual(guidance.taught_values_check(self.skill, given), "")

    def test_values_of_this_run_or_held_examples_go_ahead(self):
        self.assertEqual(guidance.taught_values_check(self.skill, {"taxable": "254237.29", "rate": "18%"}), "")

    def test_a_value_the_harness_handed_this_run_goes_ahead(self):
        with mock.patch.object(guidance, "run_values", return_value={"taxable": "165000"}):
            self.assertEqual(guidance.taught_values_check(self.skill, {"taxable": "165000"}), "")

    def test_outside_a_task_nothing_is_checked(self):
        with mock.patch.object(task_context, "current", return_value=""):
            self.assertEqual(guidance.taught_values_check(self.skill, {"taxable": "165000"}), "")


class TestBindingSaysANameNoDocumentHas(unittest.TestCase):
    """Found live (2026-10-22): with no document read, a client's portal username came back made up from its name.
    Independent review (2026-10-22): dropping every value the texts do not write word for word dropped correct derived
    values too (the request is always among the texts) - so it is said, never dropped; only the taught example is."""

    PARAMS = [{"name": "user", "label": "Username", "example": "manipalint"},
              {"name": "pos", "label": "POS", "example": "29-Karnataka"},
              {"name": "taxable", "label": "Taxable", "example": "165000"}]

    def test_a_name_no_document_writes_is_kept_and_said(self):
        request = "Prepare Sri Lakshmi Elevators' GSTR-1; their customers are in Karnataka."
        answer = {"values": {"user": "srilakshmielev", "pos": "29-Karnataka", "taxable": "67909.30"},
                  "evidence": {"user": "made from the client's name"}}
        values, said = binding._clean(self.PARAMS, answer, [request])
        self.assertEqual(values, {"user": "srilakshmielev", "pos": "29-Karnataka", "taxable": "67909.30"})
        self.assertIn("not written so in this run's documents", said["user"])
        self.assertIn("made from the client's name", said["user"])
        self.assertNotIn("pos", said)

    def test_values_worked_out_from_the_documents_are_kept(self):
        params = [{"name": "date", "label": "Invoice date", "example": "01-Aug-2026"},
                  {"name": "company", "label": "Company", "example": "Acme Private Limited"}]
        texts = ["Enter the bill.", "Invoice date 28/09/2026\nBill to: Wood Corner Pvt Ltd"]
        values, _ = binding._clean(params, {"values": {"date": "28-Sep-2026", "company": "Wood Corner Private Limited"}},
                                   texts)
        self.assertEqual(values, {"date": "28-Sep-2026", "company": "Wood Corner Private Limited"})

    def test_the_taught_example_is_still_left_out(self):
        values, _ = binding._clean(self.PARAMS, {"values": {"user": "manipalint"}}, ["Prepare Kavya's GSTR-1."])
        self.assertEqual(values, {})


class TestAValueWithoutTheTaughtNumberIsLeftOut(unittest.TestCase):
    """Found live (2026-11-01): taught with a folder "6. SEPTEMBER", a run was handed "December" from the request and
    made a folder "December" beside the numbered ones; left out, the work reads the name off the screen."""

    PARAMS = [{"name": "folder", "label": "Enter the name", "example": "6. SEPTEMBER"},
              {"name": "month", "label": "Period", "example": "August"},
              {"name": "pos", "label": "POS", "example": "29-Karnataka"}]

    def test_the_meaning_without_the_number_is_left_out_held_or_not(self):
        request = "The client's December sales bills are in the mail; their customers are in Karnataka."
        answer = {"values": {"folder": "December", "month": "December", "pos": "Karnataka"}}
        for strict in (False, True):
            values, _ = binding._clean(self.PARAMS, answer, [request], strict=strict)
            self.assertEqual(values, {"month": "December"})

    def test_a_value_written_with_its_number_is_kept(self):
        values, _ = binding._clean(self.PARAMS, {"values": {"folder": "9. DECEMBER", "pos": "27-Maharashtra"}},
                                   ["December bills; folders 1. APRIL to 8. NOVEMBER; customers in Maharashtra"])
        self.assertEqual(values, {"folder": "9. DECEMBER", "pos": "27-Maharashtra"})

    def test_a_list_item_without_it_drops_the_list(self):
        params = [{"name": "folders", "label": "Folder", "example": "6. SEPTEMBER", "list": True}]
        values, _ = binding._clean(params, {"values": {"folders": ["9. DECEMBER", "January"]}}, ["x"])
        self.assertEqual(values, {})


class TestARunTheModelDoesIsGivenOnlyWhatItsTextsHold(unittest.TestCase):
    """Found live (2026-10-24): a request naming no document came back with an invoice series KC/SEP/001 to
    KC/SEP/014, a count of 14 and two HSN rows - none of them anywhere. Handed on as the run's values they read as the
    person's, were typed into the return, and the final check held the work to them over the person's "dont invent"."""

    REQUEST = ("Prepare Kavya Constructions' GSTR-1 for September 2026 on the GST portal, the way I showed you for "
               "Manipal Interiors. Their workbook is up to date. Don't file it.")
    PARAMS = [{"name": "month", "label": "Period", "example": "August"},
              {"name": "first_no", "label": "Sr. No. From", "example": "88"},
              {"name": "count", "label": "Total number", "example": "2"},
              {"name": "taxable", "label": "Taxable value", "example": "165000"}]
    ANSWER = {"values": {"month": "September", "first_no": "KC/SEP/001", "count": "14", "taxable": "254237.29"}}

    def test_strict_keeps_only_what_the_texts_hold(self):
        values, _ = binding._clean(self.PARAMS, self.ANSWER, [self.REQUEST], strict=True)
        self.assertEqual(values, {"month": "September"})

    def test_strict_keeps_a_figure_a_document_holds_as_the_same_amount(self):
        values, _ = binding._clean(self.PARAMS, self.ANSWER, [self.REQUEST, "Taxable value 254,237.29"], strict=True)
        self.assertEqual(values, {"month": "September", "taxable": "254237.29"})

    def test_a_replay_still_gets_them_and_says_so(self):
        # A replay types what it is given; there the guess is kept and said, as before (TestBindingSaysANameNoDocumentHas).
        values, said = binding._clean(self.PARAMS, self.ANSWER, [self.REQUEST])
        self.assertEqual(values["first_no"], "KC/SEP/001")
        self.assertIn("a guess", said["first_no"])

    def test_rows_nothing_holds_are_no_rows_and_no_count(self):
        params = [{"name": "hsn", "label": "HSN", "example": "9954", "step": 1, "varies": True},
                  {"name": "value", "label": "Value", "example": "165000", "step": 2, "varies": True},
                  {"name": "hsn_2", "label": "HSN", "example": "9954", "step": 3, "varies": True},
                  {"name": "value_2", "label": "Value", "example": "1000", "step": 4, "varies": True}]
        skill = {"name": "s", "params": params, "steps": [{"kind": "type", "param": p["name"]} for p in params]}
        guessed = {"rows": [{"hsn": "9954", "value": "127118.64"}, {"hsn": "9954", "value": "127118.65"}]}
        # Not strict (a replay): the rows are taken as given, as before.
        self.assertEqual(binding._rows(skill, params, guessed, [self.REQUEST], {}, {})["count"], 2)
        values, evidence = {}, {}
        self.assertIsNone(binding._rows(skill, params, guessed, [self.REQUEST], values, evidence, strict=True))
        self.assertEqual(values, {})
        held = [self.REQUEST, "SAC 9954 Taxable 127,118.64"]
        rows = binding._rows(skill, params, guessed, held, values, evidence, strict=True)
        self.assertEqual(rows["count"], 2)
        self.assertEqual(values, {"hsn": "9954", "value": "127118.64", "hsn_2": "9954"})


class TestThePersonsWordInThisRunDecides(unittest.TestCase):
    """The same run: the person's correction was shown to the check as "asked during an earlier run", beside the
    invented values as "this run's values", and the check sent the corrected work back to them."""

    def test_this_runs_answers_come_last_and_decide(self):
        from decurix_agent import skill_task
        skill = {"name": "s", "params": [{"name": "first_no", "example": "88"}],
                 "run_answers": [{"at": 100, "q": "Which client?", "a": "the first one"},
                                 {"at": 500, "q": "Before I change anything: series KC/SEP/001-014",
                                  "a": "where did KC/SEP/001-014 come from?? dont invent - the numbers are on the bills"}]}
        text = skill_task._criteria(skill, {"first_no": "KC/SEP/001"}, since=400)
        self.assertIn("Asked during an earlier run: Which client?", text)
        self.assertNotIn("Asked during an earlier run: Before I change", text)
        now = text.index("What the person said during THIS run")
        self.assertGreater(now, text.index("Which client?"))
        self.assertIn("dont invent - the numbers are on the bills", text[now:])
        # Without a start every answer is an earlier run's, as before.
        self.assertNotIn("said during THIS run", skill_task._criteria(skill, {"first_no": "1"}))


if __name__ == "__main__":
    unittest.main()
