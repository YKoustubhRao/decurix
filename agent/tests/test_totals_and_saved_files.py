"""
A Total row is formulas, not typed numbers; and when the request asks for a file, the saved file decides that part,
not what is still on the screen. Found live (2026-10-19, a GST register in Calc): the totals were typed in, and a
correct, cleanly saved file was refused because the screen and the action log showed a range filled by mistake and
cleared.
"""

import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

try:
    import openpyxl
except ImportError:  # pragma: no cover
    openpyxl = None

from decurix_agent import produced, skill_task, verify
from decurix_agent.tools import office


def register(path, totals):
    """A register with a Total row: `totals` is what goes under Taxable and CGST (numbers or formulas)."""

    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Register"
    ws.append(["Invoice", "State", "Taxable", "CGST", "Invoice Total"])
    ws.append(["INV-1", "Maharashtra", 1000, 90, 1180])
    ws.append(["INV-2", "Gujarat", 2000, 0, 2360])
    ws.append([])
    ws.append(["Total", None] + totals)
    wb.save(path)
    return str(path)


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class TypedTotals(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)

    def test_typed_sums_under_a_total_label_are_found(self):
        book = register(self.dir / "gst.xlsx", [3000, 90, 3540])
        found = office.typed_totals(book)
        self.assertEqual(found, ["Register!C5 (3000 = the sum of C2:C3)", "Register!D5 (90 = the sum of D2:D3)",
                                 "Register!E5 (3540 = the sum of E2:E3)"])

    def test_formulas_and_numbers_that_are_not_the_sum_are_not(self):
        book = register(self.dir / "gst.xlsx", ["=SUM(C2:C3)", 91])
        self.assertEqual(office.typed_totals(book), [])
        self.assertEqual(office.typed_totals(str(self.dir / "notes.txt")), [])

    def test_a_request_for_values_takes_typed_totals(self):
        book = register(self.dir / "gst.xlsx", [3000])
        self.assertEqual(len(produced.typed_totals([book], "Add a Total row and save gst.xlsx")), 1)
        self.assertIn("typed numbers, not formulas", produced.typed_totals([book], "Add a Total row")[0])
        for asked in ("Give me the totals as values only.", "Paste values, no formulas.", "Save it without formulas."):
            self.assertEqual(produced.typed_totals([book], asked), [], asked)

    def test_the_files_the_request_names_are_its_deliverables(self):
        made = ["/w/sales_register_gst.xlsx", "/w/spec.json"]
        self.assertEqual(produced.asked_files(made, "Save it as sales_register_gst.xlsx in my work folder."),
                         ["/w/sales_register_gst.xlsx"])


from test_produced_files import FileWorkIsChecked  # noqa: E402  (the gate's harness)
from test_apprentice import verdict  # noqa: E402


class TypedTotalsAtTheGate(FileWorkIsChecked):
    """A typed total is wrong only when formulas were asked for (the PM's rule, 2026-10-19); otherwise it is noted."""

    TYPED = ["gst.xlsx: the Total row holds typed numbers, not formulas - Register!E14"]

    def gate(self, request, report):
        skill_task.state.current_task = request
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "missing_requested", lambda request, made, root=None: ""), \
                mock.patch.object(skill_task.produced, "typed_totals", lambda paths, request="": list(self.TYPED)):
            return skill_task._gate(report, "final", "thebot")

    def test_asked_for_formulas_a_typed_total_is_evidence_for_the_judge(self):
        # Harness audit (2026-10-21): "formulas asked" is a reading of the request - the judge weighs it.
        self.verdicts = [verdict(verify.NOT_DONE, seen="the Total row is typed")]
        self.gate("Use LibreOffice Calc to add the columns CGST, SGST, IGST and Invoice Total with "
                  "formulas, and a Total row. Save it as gst.xlsx in my work folder.", "Saved gst.xlsx.")
        self.assertIn("the Total row holds typed numbers", self.judged[0]["produced"])

    def test_not_asked_a_correct_typed_total_passes_with_a_note(self):
        self.verdicts = [{"verdict": verify.DONE, "confidence": 0.95, "seen": "5 rows, total 14,150", "why": ""}]
        self.assertTrue(self.gate("Make his order sheet in Excel, wa_orders.xlsx in my work folder: one row per item "
                                  "with Item, Qty, Rate and Amount, and a Total row.", "Saved wa_orders.xlsx."))
        self.assertEqual(len(self.judged), 1)
        self.assertEqual(self.asked, [])
        self.assertIn("Note: gst.xlsx: the Total row holds typed numbers", self.finals[-1][0])

    def test_what_asks_for_formulas(self):
        for asked in ("Add the columns with formulas.", "Line Total = Qty x Unit Price as a formula",
                      "keep the totals live", "so it updates when a row changes"):
            self.assertTrue(produced.formulas_asked(asked), asked)
        for plain in ("Make his order sheet with a Total row.", "Summarise sales_oct.csv into summary.xlsx"):
            self.assertFalse(produced.formulas_asked(plain), plain)

    def test_the_judge_is_told_which_saved_files_decide(self):
        self.verdicts = [{"verdict": verify.DONE, "confidence": 0.95, "seen": "ok", "why": ""}]
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: []), \
                mock.patch.object(skill_task.produced, "typed_totals", lambda paths, request="": []):
            skill_task._gate("Made result.xlsx.", "final", "thebot")
        self.assertEqual(self.judged[0]["deliverables"], ["/work/result.xlsx"])


class TheSavedFileDecides(unittest.TestCase):
    def setUp(self):
        self.sent = []

        def chat(messages, max_tokens=None):
            self.sent.append(messages)
            return {"content": '{"verdict": "done", "confidence": 0.9, "seen": "", "why": ""}'}, {}

        for p in (mock.patch.object(verify.llm_client, "available", lambda: True),
                  mock.patch.object(verify.llm_client, "chat", chat)):
            p.start()
            self.addCleanup(p.stop)

    def text(self):
        return "".join(part["text"] for part in self.sent[-1][1]["content"] if part.get("type") == "text")

    def test_with_a_saved_deliverable_screen_leftovers_are_a_note(self):
        verify.judge("Add a Total row and save it as gst.xlsx in my work folder.", claim="Saved.",
                     produced="--- /w/gst.xlsx\nA14='Total'", deliverables=["/w/gst.xlsx"])
        said = self.text()
        self.assertIn("gst.xlsx - are saved", said)
        self.assertIn("unsaved change still on the screen, is not a reason for not_done", said)

    def test_without_one_nothing_is_said(self):
        verify.judge("Fill in the form on the screen.", claim="Saved.")
        self.assertNotIn("Decide that part from the saved file", self.text())


if __name__ == "__main__":
    unittest.main()
