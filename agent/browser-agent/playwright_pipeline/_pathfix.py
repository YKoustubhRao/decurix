"""Adds core/ to sys.path so sibling-package imports (models, atomizer,
reasoning, worker, ...) resolve without turning this into a dotted-import
package -- these scripts are run directly (`python server.py`) or as
subprocesses (`[sys.executable, WORKER_PATH, ...]`), not via `python -m`, so
a __file__-relative sys.path insert is the simplest thing that works
regardless of the caller's cwd. Import this before any `import <core module>`
line, e.g. `import _pathfix  # noqa: F401`."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "core"))
