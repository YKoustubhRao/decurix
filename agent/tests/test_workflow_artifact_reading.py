"""
workflow_runner's artifact checks read correct files as correct, and a near miss as a miss (review 2026-11-03, E-26
and E-46).
"""

import json
import tempfile
import unittest
from pathlib import Path

import config_stub  # noqa: F401

from decurix_agent.workflow_runner import artifact_evidence


class ArtifactReading(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def passes(self, name, data, check):
        (self.dir / name).write_bytes(data)
        try:
            artifact_evidence({"checks": [dict(check, path=name)]}, {}, {"work_dir": str(self.dir)})
            return True
        except Exception:  # noqa: BLE001 - a refusal is what the test reads
            return False

    # E-26: correct delimited files
    def test_a_csv_with_a_byte_order_mark_has_its_columns(self):
        self.assertTrue(self.passes("a.csv", "﻿Name,Amount\r\nOne,100\r\n".encode("utf-8"),
                                    {"format": "csv", "columns": ["Name", "Amount"]}))

    def test_a_csv_ending_in_a_blank_line_is_consistent(self):
        self.assertTrue(self.passes("b.csv", b"Name,Amount\n1,2\n\n", {"format": "csv", "min_rows": 1}))

    def test_a_windows_1252_csv_is_read(self):
        self.assertTrue(self.passes("c.csv", "Name,Amount\nCaf\xe9,100\n".encode("cp1252"),
                                    {"format": "csv", "contains": ["Caf\xe9"]}))

    def test_a_ragged_csv_is_still_refused(self):
        self.assertFalse(self.passes("d.csv", b"Name,Amount\n1,2,3\n", {"format": "csv"}))

    # E-46: contains is whole
    def test_a_number_is_matched_as_an_amount(self):
        self.assertFalse(self.passes("t.txt", b"Total: 1500", {"contains": [500]}))
        self.assertTrue(self.passes("u.txt", b"Total: 1,500.00", {"contains": [1500]}))
        self.assertTrue(self.passes("v.txt", b"Total: Rs.1500", {"contains": [1500.0]}))
        self.assertFalse(self.passes("w.txt", b"Total: 15000", {"contains": [1500]}))

    def test_a_number_written_as_text_in_the_spec_is_matched_as_an_amount(self):
        # Review rework: a digit string took the word path, and "500" passed on "1,500.00".
        self.assertFalse(self.passes("s1.txt", b"Total: 1,500.00", {"contains": ["500"]}))
        self.assertTrue(self.passes("s2.txt", b"Total: 1,500.00", {"contains": ["1500"]}))
        self.assertTrue(self.passes("s3.txt", b"Total: 1500", {"contains": ["1,500.00"]}))

    def test_a_drafts_contains_is_whole_too(self):
        from decurix_agent.workflow_runner import _draft_evidence
        drafts = [{"id": "d1", "to": "a@example.com", "subject": "Statement", "body": "Balance 1500, status unpaid"}]
        for needle in ("500", "paid"):
            with self.assertRaises(ValueError, msg=needle):
                _draft_evidence({"path": "drafts", "format": "drafts", "contains": [needle]}, {"drafts": drafts}, {})
        _draft_evidence({"path": "drafts", "format": "drafts", "contains": ["1500", "unpaid"]}, {"drafts": drafts}, {})

    def test_words_are_matched_as_whole_tokens(self):
        self.assertFalse(self.passes("x.txt", b"Status: Unpaid", {"contains": ["paid"]}))
        self.assertTrue(self.passes("y.txt", b"Status: paid in full", {"contains": ["paid"]}))
        self.assertTrue(self.passes("z.txt", b"Total: 10", {"contains": ["Total:"]}))

    def test_a_negative_json_index_names_nothing(self):
        data = json.dumps({"items": [1, 2, 3]}).encode()
        self.assertFalse(self.passes("j.json", data, {"format": "json", "json_equals": {"/items/-1": 3}}))
        self.assertTrue(self.passes("k.json", data, {"format": "json", "json_equals": {"/items/2": 3}}))
        self.assertFalse(self.passes("l.json", data, {"format": "json", "json_equals": {"/items/3": 3}}))

    def test_a_mail_header_is_a_header(self):
        self.assertFalse(self.passes("m.eml", b"Subject: hi\n\nTo: whom it may concern", {"format": "eml"}))
        self.assertTrue(self.passes("n.eml", b"To: a@example.com\r\nSubject: hi\r\n\r\nBody", {"format": "eml"}))


if __name__ == "__main__":
    unittest.main()
