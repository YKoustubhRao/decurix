"""
A replay's run value is read back whole (R-2, review round 3): any value with a slash was read back as its last part,
so a date "05/03/2026" was confirmed by any "2026" on the page and the run could end VERIFIED. The file-name part is
used only for a value that is a file by structure (given to an upload, or a file field).
"""

import os
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import config_stub  # noqa: F401

from decurix_agent.computer import replay


def unread(program, values, shown):
    page = {"elements": [{"role": "cell", "name": text} for text in shown]}
    with mock.patch.object(replay, "core", mock.Mock(observe=mock.Mock(return_value=page))):
        return replay._not_read_back(program, values, {})


class AValueIsReadBackWhole(unittest.TestCase):
    PROGRAM = {"params": [{"name": "due", "step": 1}], "steps": [{"n": 1, "kind": "type", "param": "due"}]}

    def test_a_date_is_not_confirmed_by_its_year_alone(self):
        self.assertEqual(unread(self.PROGRAM, {"due": "05/03/2026"}, ["Due 09/11/2026"]), ["due"])

    def test_a_name_with_a_slash_is_not_confirmed_by_its_last_part(self):
        self.assertEqual(unread(self.PROGRAM, {"due": "A/B Traders"}, ["Invoice B Traders"]), ["due"])

    def test_the_same_date_is_read_back(self):
        self.assertEqual(unread(self.PROGRAM, {"due": "05/03/2026"}, ["Due 05/03/2026"]), [])


class AFileIsReadBackByItsName(unittest.TestCase):
    def test_an_uploaded_files_name_is_enough(self):
        program = {"params": [{"name": "scan", "step": 1}],
                   "steps": [{"n": 1, "kind": "upload", "param": "scan"}]}
        self.assertEqual(unread(program, {"scan": "/home/headless/work/inbox/bill_042.pdf"}, ["Attached: bill_042.pdf"]),
                         [])

    def test_a_file_fields_value_is_read_by_its_name(self):
        program = {"params": [{"name": "doc", "step": 1}],
                   "steps": [{"n": 1, "kind": "type", "param": "doc", "target": {"input_type": "file"}}]}
        self.assertEqual(unread(program, {"doc": "C:\\docs\\note.txt"}, ["note.txt"]), [])


if __name__ == "__main__":
    unittest.main()
