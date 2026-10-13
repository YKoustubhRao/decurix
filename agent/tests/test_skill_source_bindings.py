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


if __name__ == "__main__":
    unittest.main()
