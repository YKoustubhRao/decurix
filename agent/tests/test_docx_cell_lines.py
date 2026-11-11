"""
A Word table cell of several paragraphs reads as its lines joined by a space, never run together (office.docx_structure):
found live (2026-11-08), a bill's customer cell "Hampi Residency" / "12 Kunjibettu, Udupi" read as "Hampi
Residency12 Kunjibettu", and the customer the worker typed was "never read" four times over.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_docx_cell_lines.py
"""

import tempfile
import unittest
import zipfile
from pathlib import Path

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.tools import office

CELL = ("<w:tc><w:p><w:r><w:t>Hampi Residency</w:t></w:r></w:p>"
        "<w:p><w:r><w:t>12 Kunjibettu, </w:t></w:r><w:r><w:t>Udupi</w:t></w:r></w:p></w:tc>")
DOC = ('<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
       "<w:body><w:tbl><w:tr>" + CELL + "<w:tc><w:p><w:r><w:t>Invoice No : 1049</w:t></w:r></w:p></w:tc>"
       "</w:tr></w:tbl></w:body></w:document>")


class ACellOfSeveralLines(unittest.TestCase):

    def test_its_lines_are_joined_by_a_space(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "bill.docx"
            with zipfile.ZipFile(path, "w") as z:
                z.writestr("word/document.xml", DOC)
            text = "\n".join(office.docx_structure(path)["lines"])
        self.assertIn("Hampi Residency 12 Kunjibettu, Udupi | Invoice No : 1049", text)
        self.assertNotIn("Residency12", text)


if __name__ == "__main__":
    unittest.main()
