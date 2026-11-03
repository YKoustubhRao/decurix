"""
Optional packages a test needs, and the skip that says which one is missing.

The office tools (tools/office.py) read and write workbooks and documents with openpyxl, python-docx and lxml, which
the desktop image installs. A development Python may lack them (WSL's system python3 does): a test that needs one is
then skipped with the reason, never failed - and production code is not changed to work without them.

    from needs import needs

    @needs("openpyxl")
    class Workbooks(unittest.TestCase): ...

Not a test module (no test_ prefix): unittest discovery never imports it on its own.
"""

import importlib
import unittest

# The import name of each package, by the name a person installs it as.
_IMPORT = {"openpyxl": "openpyxl", "python-docx": "docx", "docx": "docx", "lxml": "lxml"}


def have(name):
    """Whether the package `name` can be imported in this Python."""

    try:
        importlib.import_module(_IMPORT.get(name, name))
        return True
    except ImportError:
        return False


def missing(*names):
    """The packages of `names` this Python lacks, in order."""

    return [name for name in names if not have(name)]


def needs(*names):
    """A skip decorator (for a test or a TestCase) for when any of the packages `names` is missing."""

    lacking = missing(*names)
    return unittest.skipIf(bool(lacking), f"needs {', '.join(lacking)} (not installed in this Python)")
