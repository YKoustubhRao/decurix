"""
A document the request names by its path, of any kind, is one of the run's inputs (stage_checks.run_inputs), so the
figures it prints back what a run typed from it. Found live (2026-11-08): a bill's PDF named in the request was no
source - only tables are found by name - and a code replay's save was refused for "invented" figures the PDF prints.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_named_document_source.py
"""

import tempfile
import unittest
from pathlib import Path

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import stage_checks


class ANamedDocument(unittest.TestCase):

    def test_a_pdf_named_by_path_is_an_input(self):
        with tempfile.TemporaryDirectory() as tmp:
            pdf = Path(tmp) / "BILL_12_Airtel_November.pdf"
            pdf.write_bytes(b"%PDF-1.4")
            request = f"Enter this bill into ERPNext as a draft purchase invoice: {pdf.as_posix()}"
            inputs = [entry.split(" (= ")[0] for entry in stage_checks.run_inputs(request)]
        self.assertIn(pdf.as_posix(), [Path(p).as_posix() for p in inputs])


if __name__ == "__main__":
    unittest.main()
