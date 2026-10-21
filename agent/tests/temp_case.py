"""
A test case with a fresh temp folder per test: self.tmp (removed after the test) and self.dir, its Path.

Not a test module (no test_ prefix): unittest discovery never imports it on its own.
"""

import tempfile
import unittest
from pathlib import Path


class TempDirCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
