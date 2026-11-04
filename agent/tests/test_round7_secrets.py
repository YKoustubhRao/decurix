"""
The secrets review's round 7 (REVIEW-PY-A-6, R6-B1), 2026-11-04.

The end-of-task fact distiller (messaging._keep_facts -> kept_facts.keep) read the raw request and kept "facts and
places" in the desktop's cross-task memory: a marked request's unlabelled secret, written as a place, entered later
tasks' prompts. And it waited only WAIT_S for that thread, after which the task's values were let go, so a slow model
call made its secret check with them gone. Now: no facts from a task any message of which was secret; the check uses
the values as they were when the thread started, and the values are let go only after it ends. Also: the recording's
title is withheld before it is cut, for a request longer than REQUEST_CHARS.
"""

import json
import threading
import unittest
from unittest import mock

import config_stub  # noqa: F401

from test_secret_first import SecretFirst, TASK  # noqa: E402

from decurix_agent import config, kept_facts, llm_client, messaging, state, task_context  # noqa: E402
from decurix_agent.computer import privacy  # noqa: E402

WORD = "lanternquartzmeadow"
MARKED = f"File the return for Tunga Pipes on the vendor portal, signing in as staff with {WORD}, total 1,250.00"
READ = f"vendor portal for Tunga Pipes: staff / {WORD}\nregister row 28: 456 | 8,000.00"


def model_says(facts, places=(), client="Tunga Pipes"):
    return [mock.patch.object(llm_client, "available", lambda: True),
            mock.patch.object(llm_client, "chat", lambda *a, **k: ({"content": json.dumps(
                {"client": client, "facts": list(facts), "places": list(places)})}, {}))]


class NoFactsFromASecretTask(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)
        self.remembered = []
        for p in model_says([{"about": "vendor portal", "quote": f"staff / {WORD}", "where": "the portal note"}],
                            [{"what": "vendor portal", "where": f"staff / {WORD}"}]) + [
                mock.patch.object(config, "HOSTED_BRAIN", True, create=True),
                mock.patch.object(kept_facts, "what_was_read", lambda task_id: READ),
                mock.patch.object(messaging, "remember",
                                  lambda title, text, category="": self.remembered.append((title, text)) or True)]:
            p.start()
            self.addCleanup(p.stop)

    def test_a_marked_request_keeps_no_fact(self):
        self.send(MARKED)
        task_context.start(TASK, request=MARKED)
        with mock.patch.object(state, "current_task", MARKED):
            self.assertIsNone(messaging._keep_facts(TASK))
        self.assertEqual(self.remembered, [])
        self.assertIn("Facts: none kept - this task's messages held a secret", self.logged)

    def test_an_ordinary_task_still_keeps_its_facts(self):
        request = "File the return for Tunga Pipes"
        task_context.start(TASK, request=request)
        with mock.patch.object(state, "current_task", request), mock.patch.object(state, "reply_secret", False), \
                mock.patch.object(kept_facts, "what_was_read",
                                  lambda task_id: "Tunga Pipes chat: bill 455 is cancelled, ignore it."), \
                mock.patch.object(llm_client, "chat", lambda *a, **k: ({"content": json.dumps(
                    {"client": "Tunga Pipes", "facts": [{"about": "bill 455", "quote": "bill 455 is cancelled",
                                                         "where": "the chat"}]})}, {})):
            thread = messaging._keep_facts(TASK)
            self.assertIsNotNone(thread)
            thread.join(10)
        self.assertEqual(len(self.remembered), 1)


class TheCheckUsesTheValuesAsTheyWere(unittest.TestCase):
    def test_a_slow_model_after_the_values_went(self):
        held = ["Zebra-42-quiet"]
        went = threading.Event()

        def slow(*a, **k):
            went.wait(5)  # the task's final report let go of its values meanwhile
            return ({"content": json.dumps({"client": "Tunga Pipes", "facts": [
                {"about": "portal", "quote": "staff login Zebra-42-quiet", "where": "the note"}]})}, {})
        kept = []
        with mock.patch.object(config, "HOSTED_BRAIN", True, create=True), \
                mock.patch.object(llm_client, "available", lambda: True), \
                mock.patch.object(llm_client, "chat", slow), \
                mock.patch.object(kept_facts, "what_was_read", lambda task_id: "portal: staff login Zebra-42-quiet"), \
                mock.patch.object(privacy, "_read_literals", lambda: {}), \
                mock.patch.object(messaging, "remember", lambda title, text, category="": kept.append(text) or True):
            thread = kept_facts.after_task("t000000000000a7a1", "File Tunga Pipes' return", held=held)
            went.set()
            thread.join(10)
        self.assertEqual(kept, [])


class TheValuesAreLetGoOnlyAfterTheFactsThread(SecretFirst):
    def test_a_running_facts_thread_keeps_them(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        privacy._literal_cache.clear()
        release = threading.Event()
        facts_thread = threading.Thread(target=lambda: release.wait(10), daemon=True)
        facts_thread.start()
        (self.root / "querytext").mkdir()
        with mock.patch.object(messaging, "_keep_facts", lambda *a, **k: facts_thread), \
                mock.patch.object(messaging, "_post_querytext", lambda p: ("sent", "")), \
                mock.patch.object(config, "QUERYTEXT_DIR", self.root / "querytext"), \
                mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(messaging.progress, "flush", lambda: None):
            messaging.write_final_reply("Done.", task_id=TASK)
            privacy._literal_cache.clear()
            self.assertIn("Copper71meadow", privacy.literals(TASK))  # still in use by the facts thread
            release.set()
            facts_thread.join(10)
            for _ in range(100):
                privacy._literal_cache.clear()
                if not privacy.literals(TASK):
                    break
                threading.Event().wait(0.05)
        self.assertEqual(privacy.literals(TASK), [])


class TheRecordingTitleOfALongMarkedRequest(SecretFirst):
    def test_is_withheld_before_it_is_cut(self):
        from decurix_agent.computer import audit, recording
        message = f"Sign in to the vendor portal with {WORD} and then " + "file every bill of the month. " * 800
        self.assertGreater(len(message), task_context.REQUEST_CHARS)
        task_context.note_secret_values(TASK, privacy.marked_values(message))
        privacy._literal_cache.clear()
        task_context.start(TASK, request=message)
        with mock.patch.object(state, "current_task", None), \
                mock.patch.object(recording, "ROOT", self.root / "recordings"):
            audit.record({"kind": "click", "actor": "agent", "summary": "click Save"})
        metas = list((self.root / "recordings").rglob("meta.json"))
        self.assertTrue(metas)
        self.assertNotIn(WORD, "".join(m.read_text(encoding="utf-8") for m in metas))


if __name__ == "__main__":
    unittest.main()
