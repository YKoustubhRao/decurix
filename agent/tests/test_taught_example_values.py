import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import binding, guidance, long_task, task_context
from decurix_agent.computer import evidence


# ============================================================
# GST suite T5 (2026-10-22): asked for another client's return "the way I
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


class TestBindingLeavesOutANameNoDocumentHas(unittest.TestCase):
    """GST suite T7 (2026-10-22): with no document read, a client's portal username came back made up from its name."""

    def test_a_made_up_name_is_left_out_and_the_documents_words_are_kept(self):
        params = [{"name": "user", "label": "Username", "example": "manipalint"},
                  {"name": "pos", "label": "POS", "example": "29-Karnataka"},
                  {"name": "taxable", "label": "Taxable", "example": "165000"}]
        request = "Prepare Sri Lakshmi Elevators' GSTR-1; their customers are in Karnataka."
        answer = {"values": {"user": "srilakshmielev", "pos": "29-Karnataka", "taxable": "67909.30"}}
        values, _ = binding._clean(params, answer, [request])
        self.assertEqual(values, {"pos": "29-Karnataka", "taxable": "67909.30"})


if __name__ == "__main__":
    unittest.main()
