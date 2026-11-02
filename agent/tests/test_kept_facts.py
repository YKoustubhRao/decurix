"""What the sources said that a later task will need is kept (2026-10-23)."""
import json
import unittest
from unittest import mock

from decurix_agent import kept_facts, llm_client, messaging

READ = ("Chat with the client\n09:41 them: Bill 455 is cancelled - the customer returned the goods the same day. "
        "Ignore it.\n10:02 them: Sending the purchase bills now.\nregister row 28: 456 | 8,000.00")


def model_keeps(facts, client=""):
    return [mock.patch.object(llm_client, "available", lambda: True),
            mock.patch.object(llm_client, "chat",
                              lambda *a, **k: ({"content": json.dumps({"client": client, "facts": facts})}, {}))]


class KeptFacts(unittest.TestCase):
    def use(self, patches):
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_a_sources_own_words_are_kept(self):
        self.use(model_keeps([{"about": "bill 455", "quote": "Bill 455 is cancelled - the customer returned the goods",
                               "where": "the client's chat, 09:41"}]))
        self.assertEqual(kept_facts.distil("save the month's bills", READ),
                         [{"about": "bill 455", "quote": "Bill 455 is cancelled - the customer returned the goods",
                           "where": "the client's chat, 09:41"}])

    def test_a_quote_nothing_read_holds_is_not_kept(self):
        self.use(model_keeps([{"about": "bill 455", "quote": "Bill 455 was voided by the accountant", "where": "x"},
                              {"about": "bill 455", "quote": "cancelled", "where": "x"}]))
        self.assertEqual(kept_facts.distil("save the month's bills", READ), [], "made up, or too short to say anything")

    def test_kept_in_the_workers_memory_as_facts(self):
        self.use(model_keeps([{"about": "bill 455", "quote": "Bill 455 is cancelled - the customer returned the goods",
                               "where": "the client's chat"}], client="Tunga Pipes"))
        kept = []
        self.use([mock.patch.object(kept_facts, "what_was_read", lambda task_id: READ),
                  mock.patch.object(messaging, "remember", lambda title, text, category="": kept.append((title, text, category)) or True)])
        self.assertEqual(kept_facts.keep("t1", "save Tunga Pipes' bills for the month"), 1)
        self.assertEqual(kept[0][2], "fact")
        self.assertEqual(kept[0][0], "About Tunga Pipes", "kept for the client the request names, and no other")
        self.assertIn("Bill 455 is cancelled", kept[0][1])

    def test_no_client_named_no_fact_kept(self):
        # Review 2026-11-02: memory is shared by every task on the desktop; a fact kept with no client was every
        # client's. Nor is a client the request does not name taken for one.
        quote = [{"about": "bill 455", "quote": "Bill 455 is cancelled - the customer returned the goods", "where": "chat"}]
        kept = []
        self.use([mock.patch.object(kept_facts, "what_was_read", lambda task_id: READ),
                  mock.patch.object(messaging, "remember", lambda title, text, category="": kept.append(title) or True)])
        for client in ("", "Hampi Stone"):
            with mock.patch.object(llm_client, "available", lambda: True), \
                    mock.patch.object(llm_client, "chat",
                                      lambda *a, **k: ({"content": json.dumps({"client": client, "facts": quote})}, {})):
                self.assertEqual(kept_facts.keep("t1", "save the month's bills"), 0, client)
        self.assertEqual(kept, [])

    def test_a_fact_that_holds_a_secret_is_never_kept(self):
        read = READ + "\n10:05 them: Our portal password: Lift$2026 - please file it today."
        self.use(model_keeps([{"about": "the portal", "quote": "Our portal password: Lift$2026 - please file it today",
                               "where": "the client's chat"},
                              {"about": "bill 455", "quote": "Bill 455 is cancelled - the customer returned the goods",
                               "where": "the client's chat"}], client="Tunga Pipes"))
        kept = []
        self.use([mock.patch.object(kept_facts, "what_was_read", lambda task_id: read),
                  mock.patch.object(messaging, "remember", lambda title, text, category="": kept.append(text) or True)])
        self.assertEqual(kept_facts.keep("t1", "file Tunga Pipes' return"), 1)
        self.assertNotIn("Lift$2026", " ".join(kept))
        self.assertIn("Bill 455 is cancelled", kept[0])

    def test_where_the_sources_are_is_kept_when_it_was_read(self):
        answer = {"facts": [], "places": [{"what": "the client's messages and bills", "where": "http://desk:8703/chat"},
                                          {"what": "made up", "where": "http://elsewhere/inbox"}]}
        self.use([mock.patch.object(llm_client, "available", lambda: True),
                  mock.patch.object(llm_client, "chat", lambda *a, **k: ({"content": json.dumps(answer)}, {}))])
        kept = []
        self.use([mock.patch.object(kept_facts, "what_was_read", lambda task_id: READ),
                  mock.patch.object(messaging, "remember", lambda title, text, category="": kept.append((text, category)) or True)])
        self.assertEqual(kept_facts.keep("t1", "save the bills from the chat (http://desk:8703/chat)"), 1)
        self.assertEqual(kept, [("the client's messages and bills: http://desk:8703/chat", "place")],
                         "only an address the task had in front of it")

    def test_without_a_model_nothing_is_asked_or_kept(self):
        self.use([mock.patch.object(llm_client, "available", lambda: False),
                  mock.patch.object(llm_client, "chat", side_effect=AssertionError("asked"))])
        self.assertEqual(kept_facts.distil("x", READ), [])


if __name__ == "__main__":
    unittest.main()
