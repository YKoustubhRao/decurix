"""Adds core/ to sys.path so sibling-package imports (models, atomizer,
reasoning, worker, ...) resolve without turning this into a dotted-import
package -- see playwright_pipeline/_pathfix.py for the full rationale (this
is the extension pipeline's identical counterpart, pointing at the same
core/ directory)."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "core"))
