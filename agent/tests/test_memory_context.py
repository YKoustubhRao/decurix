import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import config
from decurix_agent.memory_context import get_memory_context


class TestMemoryContextBudget(unittest.TestCase):
    def _write(self, tmp, name, content):
        Path(tmp, name).write_text(content, encoding="utf-8")

    def test_stays_within_budget_even_with_one_oversized_entry(self):
        with tempfile.TemporaryDirectory() as tmp:
            # get_memory_context reads most-recent-first (reverse
            # alphabetical by filename); "z-recent" sorts first, so it's
            # already included by the time "a-old" (the oversized legacy
            # file) is considered - that one must be skipped rather than
            # blowing the whole budget just because it's the one being
            # looked at.
            self._write(tmp, "z-recent.txt", "small and normal")
            self._write(tmp, "a-old.txt", "x" * 500_000)

            with patch.object(config, "MEMORY_DIR", Path(tmp)), patch.object(
                config, "MEMORY_CONTEXT_CHARS", 6000
            ):
                out = get_memory_context()

        self.assertLessEqual(len(out), 6000)
        self.assertIn("small and normal", out)

    def test_returns_the_one_entry_alone_when_even_it_exceeds_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            self._write(tmp, "only.txt", "y" * 10_000)

            with patch.object(config, "MEMORY_DIR", Path(tmp)), patch.object(
                config, "MEMORY_CONTEXT_CHARS", 100
            ):
                out = get_memory_context()

        self.assertIn("y" * 10_000, out)

    def test_normal_entries_all_fit_under_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            self._write(tmp, "a.txt", "first")
            self._write(tmp, "b.txt", "second")

            with patch.object(config, "MEMORY_DIR", Path(tmp)), patch.object(
                config, "MEMORY_CONTEXT_CHARS", 6000
            ):
                out = get_memory_context()

        self.assertIn("first", out)
        self.assertIn("second", out)


if __name__ == "__main__":
    unittest.main()
