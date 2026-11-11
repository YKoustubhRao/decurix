"""
Code-checkable effects of work that writes no record (effects.py): the file a taught step writes (a typed path saved
by the press after it) and the status the application shows after a consequential press. A skill whose every
consequential step has one may replay while still being learned; a run whose export file is missing, stale, broken or
without what it entered fails; a run whose effects were all read back by code is checked on the `code` basis.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_effects.py
"""

import json
import os
import tempfile
import time
import unittest
from pathlib import Path

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import effects


def export_program(folder):
    """Enter a total, export the register to a file, upload it on a portal and file it."""

    return {"steps": [
        {"n": 1, "id": "a", "kind": "type", "param": "invoice_no", "target": {"role": "textbox", "name": "Invoice No"}},
        {"n": 2, "id": "b", "kind": "type", "param": "total", "target": {"role": "textbox", "name": "Total"}},
        {"n": 3, "id": "c", "kind": "click", "target": {"role": "button", "name": "Export"}},
        {"n": 4, "id": "d", "kind": "type", "param": "export_path", "target": {"role": "textbox", "name": "File name"}},
        {"n": 5, "id": "e", "kind": "click", "target": {"role": "button", "name": "Save"}},
        {"n": 6, "id": "f", "kind": "click", "target": {"role": "button", "name": "Submit return"},
         "consequential": True, "expect": [{"type": "element", "role": "status", "name": "Return submitted"}]},
    ]}


def values(path):
    return {"invoice_no": "INV-2026-118", "total": "12,500.00", "export_path": str(path)}


class Effects(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "register.json"
        self.began = time.time()

    def write(self, payload, age=0):
        self.path.write_text(payload, encoding="utf-8")
        if age:
            os.utime(self.path, (time.time() - age, time.time() - age))

    def test_a_typed_path_saved_by_the_next_press_is_a_file_effect(self):
        found = effects.file_effects(export_program(self.tmp.name), values(self.path))
        self.assertEqual(found, [{"kind": "file", "path": str(self.path), "step": 4, "commit": 5}])

    def test_every_consequential_step_covered_lets_it_replay_while_learned(self):
        ok, why = effects.replayable(export_program(self.tmp.name), values(self.path))
        self.assertTrue(ok, why)
        program = export_program(self.tmp.name)
        program["steps"][5]["expect"] = []
        ok, why = effects.replayable(program, values(self.path))
        self.assertFalse(ok)
        self.assertIn("step 6", why)

    def test_apprentice_replay_is_refused_when_a_commit_leaves_nothing_to_read_back(self):
        program = export_program(self.tmp.name)
        program["steps"][3]["param"] = "invoice_no"  # (no path typed: the Save writes nothing code can find)
        ok, why = effects.replayable(program, values(self.path))
        self.assertFalse(ok)
        self.assertIn("step 5", why)

    def test_a_fresh_file_holding_what_was_entered_and_the_status_seen_is_all_code(self):
        self.write(json.dumps({"invoices": [{"no": "INV-2026-118", "total": 12500.0}]}))
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed={"f"})
        self.assertEqual(found["problems"], [])
        self.assertEqual(found["not_made"], [])
        self.assertTrue(found["all_code"])
        self.assertIn("CHECKED IN CODE", effects.report(found))

    def test_a_missing_export_file_fails_the_run(self):
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed={"f"})
        self.assertTrue(any("is not there" in p for p in found["problems"]), found)
        self.assertFalse(found["all_code"])

    def test_a_stale_export_file_fails_the_run(self):
        self.write(json.dumps({"invoices": [{"no": "INV-2026-118", "total": 12500.0}]}), age=3600)
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed={"f"})
        self.assertTrue(any("earlier copy" in p for p in found["problems"]), found)

    def test_an_export_with_the_wrong_total_or_broken_fails_the_run(self):
        self.write(json.dumps({"invoices": [{"no": "INV-2026-118", "total": 1250.0}]}))
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed={"f"})
        self.assertTrue(any("12,500.00" in p for p in found["problems"]), found)
        self.write("{not json")
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed={"f"})
        self.assertTrue(any("does not open" in p for p in found["problems"]), found)

    def test_a_status_the_replay_did_not_see_is_not_checked_never_passed(self):
        self.write(json.dumps({"invoices": [{"no": "INV-2026-118", "total": 12500.0}]}))
        found = effects.check(export_program(self.tmp.name), values(self.path), self.began, confirmed=set())
        self.assertEqual(found["problems"], [])
        self.assertTrue(found["not_made"])
        self.assertFalse(found["all_code"])


if __name__ == "__main__":
    unittest.main()
