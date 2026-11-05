"""
The harness code names no test suite: none of a suite's names, test ids or data, comments included.

The suites (outside the repo) are probes the harness learns from, never the product: code that names one reads as
fitted to it and carries to no other task. Each suite lists its own distinctive words in a leak_terms.txt in its
folder (one regular expression a line, "#" for a note); this scans the harness code for them. Tests may hold example
data; the code that must generalise may not. Skipped where the suites are not on this machine - counted and said at
the end (fixture_paths), and a failure under DECURIX_REQUIRE_FIXTURES=1.
"""

import os
import re
import unittest
from pathlib import Path

import fixture_paths

REPO = Path(__file__).resolve().parents[2]
SUITES = Path(os.environ.get("DECURIX_TESTS") or Path.home() / "decurix-tests")
CODE_DIRS = ("agent/decurix_agent", "agent/thebot", "internal", "cmd")
CODE_FILES = (".py", ".go", ".md", ".txt", ".html", ".tmpl", ".js", ".json", ".yaml", ".yml", ".sh", ".ps1")
SKIP_DIRS = {"__pycache__", "node_modules", "testdata", "tests", ".git"}


def _terms():
    """Every suite's terms, as (regular expression, file it came from) - the root's and each suite's, four deep."""
    terms = []
    for pattern in ("leak_terms.txt", "*/leak_terms.txt", "*/*/leak_terms.txt", "*/*/*/leak_terms.txt"):
        for path in sorted(SUITES.glob(pattern)):
            for line in path.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if line and not line.startswith("#"):
                    terms.append((re.compile(line), path))
    return terms


def _code_files():
    """The harness's own files: its code and what it ships (prompts, pages), never its tests."""
    roots = [REPO / d for d in CODE_DIRS] + [REPO]
    for root in roots:
        walk = os.walk(root) if root != REPO else [(str(REPO), [], [p.name for p in REPO.glob("*.go")])]
        for folder, dirs, files in walk:
            dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
            for name in files:
                if name.endswith(CODE_FILES) and not name.endswith("_test.go"):
                    yield Path(folder) / name


class NoSuiteTerms(unittest.TestCase):
    def test_the_harness_code_names_no_suite(self):
        terms = _terms()
        if not terms:
            raise fixture_paths.missing(f"no leak_terms.txt under {SUITES} (set DECURIX_TESTS)")
        found = []
        for path in _code_files():
            try:
                text = path.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError):
                continue
            for number, line in enumerate(text.splitlines(), 1):
                for term, _ in terms:
                    if term.search(line):
                        found.append(f"{path.relative_to(REPO)}:{number}: {term.pattern!r} in {line.strip()[:100]}")
        self.assertEqual(found, [], "a suite's words in the harness code (write the lesson in domain-free words):\n"
                         + "\n".join(found[:40]))


if __name__ == "__main__":
    unittest.main()
