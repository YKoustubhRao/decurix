"""
Where the tests' fixtures made from live runs live: outside the repository (they hold customer data, demo logins and
MBs of files - never committed). DECURIX_TEST_FIXTURES names the folder; by default decurix-tests/fixtures beside the
work tree (C:\\Users\\Administrator\\decurix-tests\\fixtures, or /mnt/c/... under WSL). A test that needs one skips,
naming the missing path, when it is not there - it never fails for that.
"""

import os
import sys
import unittest
from pathlib import Path


def _default():
    if sys.platform == "win32":
        return Path(r"C:\Users\Administrator\decurix-tests\fixtures")
    return Path("/mnt/c/Users/Administrator/decurix-tests/fixtures")


FIXTURES = Path(os.environ.get("DECURIX_TEST_FIXTURES") or _default())


def fixture(*parts):
    """The path of a fixture (whether or not it is there)."""

    return FIXTURES.joinpath(*parts)


def need(*parts):
    """The path of a fixture that must be there; the test skips (naming it) when it is not."""

    path = fixture(*parts)
    if not path.exists():
        raise unittest.SkipTest(f"fixture not here: {path} (set DECURIX_TEST_FIXTURES)")
    return path
