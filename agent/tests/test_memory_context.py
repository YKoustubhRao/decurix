import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import config, memory_context
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


def entry(when, title, category, text):
    return f"Title: {title}\nCategory: {category}\nDesktopID: w\nCreatedAt: {when}\n\n{text}"


class TestMemoryChosenForTheTask(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        for patcher in (patch.object(config, "MEMORY_DIR", Path(self.tmp.name)), patch.object(config, "MEMORY_CONTEXT_CHARS", 1500)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def _keep(self, n, *args):
        Path(self.tmp.name, f"memory-{n:06d}.txt").write_text(entry(*args), encoding="utf-8")

    def _noise(self, first, count=40):
        for i in range(first, first + count):
            self._keep(i, "2026-09-01", "About Another Trader", "fact", f'ClientChat: "Bill {i} went by NEFT, do not chase it"')

    def test_everything_that_fits_is_shown_as_it_always_was(self):
        self._keep(1, "2026-01-01", "About Tunga Pipes", "fact", "older")
        self._keep(2, "2026-02-01", "Lesson", "lesson", "newer")
        out = get_memory_context("anything at all for Tunga Pipes")
        self.assertFalse(out.startswith("("))
        self.assertEqual(out, get_memory_context("something else for Tunga Pipes"))
        self.assertLess(out.index("older"), out.index("newer"))

    def test_a_fact_about_one_client_never_reaches_a_task_for_another(self):
        # Review 2026-11-02: kept facts went into every later task's prompt, whichever client they were about.
        self._keep(1, "2026-01-01", "About Tunga Pipes", "fact", 'ClientChat: "Bill 455 is cancelled, ignore it"')
        self._keep(2, "2026-01-02", "A fact a source stated", "fact", '"Pay the courier by NEFT from now on"')
        self._keep(3, "2026-02-01", "Lesson", "lesson", "Always ask before saving")
        for request in ("Enter Hampi Stone's bills for September", "anything at all"):
            out = get_memory_context(request)
            self.assertNotIn("Bill 455", out, request)
            self.assertNotIn("courier", out, "a fact kept with no party is no task's")
            self.assertIn("Always ask before saving", out)
            self.assertIn("2 fact(s) or place(s) kept about parties this request does not name are not shown", out)
        self.assertIn("Bill 455", get_memory_context("Enter Tunga Pipes' bills for September"))

    def test_an_old_fact_about_the_party_the_request_names_survives_many_newer_entries(self):
        self._keep(1, "2025-01-01", "About Tunga Pipes", "fact", 'ClientChat: "Bill 455 is cancelled, ignore it"')
        self._noise(2)
        self.assertIn("Bill 455 is cancelled", get_memory_context("File Tunga Pipes' September returns"))
        self.assertNotIn("Bill 455 is cancelled", get_memory_context("Enter Hampi Stone's bills"))

    def test_a_party_with_a_name_alike_does_not_take_the_room(self):
        self._keep(1, "2025-01-01", "About Sagar Elevators", "fact", 'ClientChat: "Our username is U1111"')
        for i in range(2, 42):
            self._keep(i, "2025-06-01", "About Sagar Electricals", "fact", f'ClientChat: "Our username is U{i} sagar elevators elevators"')
        out = get_memory_context("What is Sagar Elevators' username?")
        self.assertIn("U1111", out)

    def test_what_the_person_taught_is_kept_whatever_the_request_says(self):
        self._keep(1, "2025-01-01", "Lesson", "lesson", "Always ask before saving")
        self._noise(2)
        self.assertIn("Always ask before saving", get_memory_context("Bill 12 NEFT chase"))

    def test_a_later_correction_is_shown_after_the_statement_it_corrects(self):
        self._keep(1, "2025-01-01", "About Tunga Pipes", "fact", "Account is 111")
        self._noise(2)
        self._keep(90, "2026-01-01", "About Tunga Pipes", "fact", "Account changed, new one is 222")
        out = get_memory_context("Tunga Pipes account")
        self.assertLess(out.index("Account is 111"), out.index("new one is 222"))

    def test_it_says_what_is_not_shown_stays_in_the_room_and_does_not_change_within_a_task(self):
        self._noise(1)
        out = get_memory_context("Another Trader: Bill 7 NEFT")
        self.assertRegex(out, r"^\(\d+ of 40 remembered entries are shown .* gui recall")
        self.assertLessEqual(len(out), 1500)
        self.assertEqual(out, get_memory_context("Another Trader: Bill 7 NEFT"))

    def test_search_reaches_what_the_view_left_out(self):
        self._keep(1, "2025-01-01", "About Tunga Pipes", "fact", "Courier account is CX9")
        self._noise(2)
        self.assertNotIn("CX9", get_memory_context("Enter the bills"))
        self.assertIn("CX9", memory_context.search("courier"))
        self.assertEqual(memory_context.search("xyzzy"), "")


if __name__ == "__main__":
    unittest.main()
