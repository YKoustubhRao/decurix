"""Explicit learned-file transfer never silently reopens the demonstrated input."""
import copy
import tempfile
import unittest
from pathlib import Path

from test_recordings import event  # Portable config fixture before computer imports.
from decurix_agent.computer import skills


class SourceBindings(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.old, self.new = root / "demo.ods", root / "holdout.ods"
        self.old.write_text("demonstration")
        self.new.write_text("holdout")
        self.program = {"starts": {"check": True, "window": "demo.ods - LibreOffice Calc"}, "steps": [
            {"kind": "open", "open": str(self.old), "expect": [{"type": "active_window", "value": "demo.ods - LibreOffice Calc"}]},
            {"kind": "type", "window": "demo.ods - LibreOffice Calc", "phase_label": "demo.ods", "target": {"role": "cell", "name": "B2"},
             "expect": [{"type": "value", "role": "cell", "name": "B2", "param": "amount"}]},
            {"kind": "type", "target": {"role": "textbox", "name": "Name"}, "text": str(self.old)}]}

    def test_explicit_source_binding_changes_open_and_window_checks_only(self):
        original = copy.deepcopy(self.program)
        bound = skills.bind_sources(self.program, {str(self.old): str(self.new)})
        self.assertEqual(bound["steps"][0]["open"], str(self.new))
        self.assertIn("holdout.ods", bound["steps"][0]["expect"][0]["value"])
        self.assertIn("holdout.ods", bound["steps"][1]["window"])
        self.assertIn("holdout.ods", bound["starts"]["window"])
        self.assertEqual(bound["steps"][1]["expect"][0]["param"], "amount")
        self.assertEqual(bound["steps"][2]["text"], str(self.old), "save destinations must not be implicitly redirected")
        self.assertEqual(self.program, original, "the persisted demonstration is immutable")

    def test_unrecorded_sources_urls_apps_missing_files_and_type_changes_refused(self):
        other = Path(self.temp.name) / "other.docx"
        other.write_text("wrong application")
        for source, target in [("calc", str(self.new)), ("https://example.test/demo.ods", str(self.new)),
                               (str(self.old), "relative.ods"), (str(self.old), str(other)),
                               (str(self.old), str(self.new.with_name("missing.ods")))]:
            with self.subTest(source=source, target=target), self.assertRaises(ValueError):
                skills.bind_sources(self.program, {source: target})


class AFileNameIsReplacedOnlyWhereItStandsWhole(unittest.TestCase):
    def test_bind_sources_leaves_a_longer_name_holding_the_bound_one(self):
        # Through bind_sources itself (review round 4): a window of another file whose name ends with the bound one's.
        with tempfile.TemporaryDirectory() as tmp:
            old, new = Path(tmp) / "bill.ods", Path(tmp) / "october.ods"
            old.write_text("demonstration")
            new.write_text("holdout")
            program = {"starts": {"window": "bill.ods - LibreOffice Calc"}, "steps": [
                {"kind": "open", "open": str(old), "expect": [{"type": "active_window", "value": "bill.ods - Calc"}]},
                {"kind": "click", "window": "prebill.ods - LibreOffice Calc", "phase_label": "bill.ods.bak",
                 "expect": [{"type": "window", "value": "prebill.ods - Calc"}]}]}
            bound = skills.bind_sources(program, {str(old): str(new)})
        self.assertEqual(bound["starts"]["window"], "october.ods - LibreOffice Calc")
        self.assertEqual(bound["steps"][0]["expect"][0]["value"], "october.ods - Calc")
        self.assertEqual(bound["steps"][1]["window"], "prebill.ods - LibreOffice Calc")
        self.assertEqual(bound["steps"][1]["phase_label"], "bill.ods.bak")
        self.assertEqual(bound["steps"][1]["expect"][0]["value"], "prebill.ods - Calc")

    # D-36 (review round 3): a substring replace rewrote another file's name that only ends with the bound one's.
    def test_a_longer_name_holding_it_is_left_alone(self):
        self.assertEqual(skills._rename_file("prebill.xlsx - Calc", "bill.xlsx", "oct.xlsx"), "prebill.xlsx - Calc")
        self.assertEqual(skills._rename_file("bill.xlsx.bak", "bill.xlsx", "oct.xlsx"), "bill.xlsx.bak")

    def test_the_name_itself_is_replaced(self):
        self.assertEqual(skills._rename_file("bill.xlsx - Calc", "bill.xlsx", "oct.xlsx"), "oct.xlsx - Calc")
        self.assertEqual(skills._rename_file("C:/books/bill.xlsx", "bill.xlsx", "oct.xlsx"), "C:/books/oct.xlsx")


if __name__ == "__main__":
    unittest.main()
