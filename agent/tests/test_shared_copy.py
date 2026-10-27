import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import shared
from decurix_agent.tools import office


# ============================================================
# Found live (2026-10-21): on the person's shared folder (a Windows drive,
# every file owned by root) setting a copy's mode is refused, so copy2 failed
# after writing - and the first `office update` of a person's file failed.
# ============================================================

def refused(*a, **kw):
    raise PermissionError(1, "Operation not permitted")


class TestCopyingIntoTheSharedFolder(unittest.TestCase):

    def test_a_copy_whose_mode_cannot_be_set_is_still_a_copy(self):
        d = Path(tempfile.mkdtemp())
        (d / "a.xlsx").write_bytes(b"book")
        with mock.patch.object(shutil, "copystat", refused):
            shared.copy_file(d / "a.xlsx", d / "a.orig.xlsx")
        self.assertEqual((d / "a.orig.xlsx").read_bytes(), b"book")

    def test_keeping_the_original_before_an_update_survives_it_too(self):
        d = Path(tempfile.mkdtemp())
        src = d / "book.xlsx"
        src.write_bytes(b"book")
        with mock.patch.object(shutil, "copystat", refused), mock.patch.object(office, "_task_made", return_value=False):
            office._keep_original(src)
        self.assertEqual((d / "book.orig.xlsx").read_bytes(), b"book")


if __name__ == "__main__":
    unittest.main()
