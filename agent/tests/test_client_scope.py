"""
What one client's sources said, and where they are, is never shown to another client's task (review 2026-11-03,
E-29): places are scoped like facts, a client is named only whole (not as part of a longer name), and kept_facts
refuses a truncated client name.
"""

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent import config, kept_facts, memory_context
from decurix_agent.memory_context import get_memory_context, names_whole


def entry(title, category, body):
    return f"Title: {title}\nCategory: {category}\nDesktopID: w\nCreatedAt: 2026-11-01\n\n{body}"


class Scoped(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        p = patch.object(config, "MEMORY_DIR", Path(self.tmp.name))
        p.start()
        self.addCleanup(p.stop)

    def keep(self, n, title, category, body):
        Path(self.tmp.name, f"memory-{n:06d}.txt").write_text(entry(title, category, body), encoding="utf-8")

    def test_a_place_kept_for_one_client_is_not_shown_to_another(self):
        self.keep(1, "Where (for North Mills): chats of North Mills", "place", "chats: http://chat.example/north")
        self.assertNotIn("chat.example/north", get_memory_context("File the October return for South Works"))
        self.assertIn("chat.example/north", get_memory_context("File the October return for North Mills"))

    def test_a_place_kept_with_no_client_is_shown_to_no_task(self):
        self.keep(1, "Where: chats of a client", "place", "chats: http://chat.example/any")
        self.assertNotIn("chat.example/any", get_memory_context("File the October return for South Works"))

    def test_a_fact_about_a_short_name_is_not_shown_to_a_longer_name_holding_it(self):
        self.keep(1, "About Kappa", "fact", '"invoice 12 is cancelled"')
        self.assertNotIn("invoice 12", get_memory_context("File the return for Kappa Traders Pvt Ltd"))
        self.assertIn("invoice 12", get_memory_context("File the return for Kappa this month"))
        self.assertIn("invoice 12", get_memory_context("File Kappa's return"))

    def test_a_lower_case_request_sees_only_the_longest_party_it_names(self):
        # Review 2026-11-03 rework: with no capitals to go by, both parties' facts were shown.
        self.keep(1, "About Kappa Traders", "fact", '"invoice 12 is cancelled"')
        self.keep(2, "About Kappa Traders Pvt Ltd", "fact", '"invoice 99 is on hold"')
        out = get_memory_context("file the return for kappa traders pvt ltd")
        self.assertIn("invoice 99", out)
        self.assertNotIn("invoice 12", out)
        out = get_memory_context("file the return for kappa traders")
        self.assertIn("invoice 12", out)
        self.assertNotIn("invoice 99", out)

    def test_a_lower_case_request_sees_only_the_longest_partys_places(self):
        self.keep(1, "Where (for Ram): chats", "place", "chats: http://chat.example/ram")
        self.keep(2, "Where (for Ram Kumar): chats", "place", "chats: http://chat.example/ram-kumar")
        out = get_memory_context("send the po to ram kumar")
        self.assertIn("chat.example/ram-kumar", out)
        self.assertNotIn("chat.example/ram\n", out + "\n")
        self.assertNotIn("http://chat.example/ram ", out.replace("\n", " ") + " ")

    def test_lessons_and_other_entries_are_shared_as_before(self):
        self.keep(1, "Lesson", "lesson", "Always ask before saving")
        self.assertIn("Always ask before saving", get_memory_context("anything for South Works"))

    def test_an_all_capitals_request_sees_its_clients_facts(self):
        self.keep(1, "About ABC Traders", "fact", "ABC Traders files quarterly")
        self.assertIn("ABC Traders files quarterly", get_memory_context("PLEASE FILE GSTR1 FOR ABC TRADERS"))


class NamedWhole(unittest.TestCase):
    def test_a_name_continued_by_a_capitalised_word_is_another_name(self):
        self.assertFalse(names_whole("Ram", "Send the PO to Ram Kumar"))
        self.assertFalse(names_whole("Kumar", "Send the PO to Ram Kumar"))
        self.assertTrue(names_whole("Ram Kumar", "Send the PO to Ram Kumar."))

    def test_a_capital_that_starts_the_sentence_does_not_continue_the_name(self):
        self.assertTrue(names_whole("Kappa", "File Kappa's return"))
        self.assertTrue(names_whole("Kappa", "Done. Kappa asked for it"))

    def test_an_all_capitals_request_names_its_client(self):
        # (follow-ups: every word of an all-capitals line is capitalised, so the capital said "a longer name" and the
        # client's own facts were hidden)
        self.assertTrue(names_whole("ABC Traders", "PLEASE FILE GSTR1 FOR ABC TRADERS BY FRIDAY"))
        self.assertTrue(names_whole("Ram", "SEND THE PO TO RAM\nThanks, Asha"))
        self.assertFalse(names_whole("Ram", "Send the PO to Ram Kumar"), "a line with case still reads its capitals")
        self.assertFalse(names_whole("ABC", "Please file for ABC TRADERS PVT LTD"))

    def test_a_word_inside_another_word_is_not_the_name(self):
        self.assertFalse(names_whole("Ram", "Ramesh asked for it"))


class ClientOf(unittest.TestCase):
    def test_a_truncated_client_name_is_refused(self):
        self.assertEqual(kept_facts.client_of({"client": "Ram"}, "Send the PO to Ram Kumar"), "")
        self.assertEqual(kept_facts.client_of({"client": "Ram Kumar"}, "Send the PO to Ram Kumar"), "Ram Kumar")

    def test_a_place_is_kept_for_the_client(self):
        kept = []
        answer = {"client": "North Mills", "facts": [], "places": [{"what": "chats of North Mills",
                                                                    "where": "http://chat.example/north"}]}
        with patch.object(kept_facts, "_answer", return_value=answer), \
                patch.object(kept_facts, "what_was_read", return_value="read http://chat.example/north"), \
                patch("decurix_agent.messaging.remember", side_effect=lambda t, b, category: kept.append(t) or True):
            kept_facts.keep("t1", "File North Mills' return")
        self.assertEqual(kept, ["Where (for North Mills): chats of North Mills"])


if __name__ == "__main__":
    unittest.main()
