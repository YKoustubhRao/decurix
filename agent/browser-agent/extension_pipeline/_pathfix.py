"""Adds core/ to sys.path so sibling-package imports (models, atomizer,
reasoning, worker, ...) resolve without turning this into a dotted-import
package: ext_server.py is run directly as a script, not via python -m."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "core"))
