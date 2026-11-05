"""
The resource plan's first round for the agent (R1, 2026-11-05): fewer grabs, files and RAM for the same work.

- P1 #5: the after-action look takes the settled frame instead of grabbing the screen again.
- B5: page-picture folders and handled command files are let go.
- B6: cached prompt tokens are counted.
- H-SOUL-9: the embedding model is warmed only when soul embeds through Ollama on a local brain.
"""

import os
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, thebot_harness, usage, watchers  # noqa: E402
from decurix_agent.computer import approval, audit, core, screen, session  # noqa: E402
from decurix_agent.tools import office  # noqa: E402


class TheSettledFrameIsTheAfterLook(unittest.TestCase):
    FIELD = {"role": "button", "name": "Open", "source": "cdp", "ref": "e3", "path": "site x > button Open",
             "box": [10, 10, 40, 20], "states": [], "group": "g", "window_title": "Site"}

    def test_act_hands_observe_the_settled_frame(self):
        settled = object()
        page = {"elements": [self.FIELD], "frame": None, "windows": [], "size": [1280, 720], "active_title": "Site",
                "groups": [{"id": "g", "source": "cdp", "url": "https://site.example/a", "window_title": "Site"}]}
        looked = []

        def observe(*a, **k):
            looked.append(k.get("frame"))
            return dict(page)
        patches = [mock.patch.object(core, "observe", side_effect=observe),
                   mock.patch.object(core, "_perform", return_value="cdp"),
                   mock.patch.object(core, "_wait_for_settle", return_value=settled),
                   mock.patch.object(audit, "record"), mock.patch.object(core, "_downloads_snapshot", return_value={}),
                   mock.patch.object(core.cdp, "submit_form_fields", return_value=None),
                   mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False))]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        try:
            core.act("click", ref="e3")
        except Exception:  # noqa: BLE001 - only the looks matter here
            pass
        self.assertGreaterEqual(len(looked), 2)
        self.assertIsNone(looked[0])         # the look before the action grabs its own
        self.assertIs(looked[1], settled)    # the look after it is the settled frame: no second grab

    def test_observe_given_a_frame_grabs_nothing(self):
        grabs = []

        class Stop(Exception):
            pass
        with mock.patch.object(session, "ensure"), \
                mock.patch.object(screen, "grab", side_effect=lambda *a, **k: grabs.append(1)), \
                mock.patch.object(screen, "windows", side_effect=Stop):
            with self.assertRaises(Stop):
                core.observe(frame=object())
            self.assertEqual(grabs, [])
            with self.assertRaises(Stop):
                core.observe()
            self.assertEqual(grabs, [1])


class PageFoldersAreLetGo(unittest.TestCase):
    def test_old_folders_are_swept_and_used_ones_dropped(self):
        with tempfile.TemporaryDirectory() as tmp:
            old = Path(tmp) / (office.PAGES_PREFIX + "old")
            new = Path(tmp) / (office.PAGES_PREFIX + "new")
            other = Path(tmp) / "something-else"
            for folder in (old, new, other):
                folder.mkdir()
                (folder / "page-1.png").write_bytes(b"x")
            past = time.time() - office.PAGES_KEEP_SECONDS - 60
            os.utime(old, (past, past))
            os.utime(other, (past, past))
            self.assertEqual(office.sweep_page_folders(root=tmp), 1)
            self.assertFalse(old.exists())
            self.assertTrue(new.exists() and other.exists())
            office.drop_pages([str(new / "page-1.png")])
            self.assertFalse(new.exists())
            office.drop_pages([str(other / "page-1.png")])  # (not a page-picture folder: kept)
            self.assertTrue(other.exists())


class HandledCommandFilesGo(unittest.TestCase):
    def test_after_their_keep(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(config, "COMMAND_DIR", Path(tmp)):
            old, new, waiting = (Path(tmp) / "a.txt.done", Path(tmp) / "b.txt.done", Path(tmp) / "c.txt")
            for p in (old, new, waiting):
                p.write_text("[withheld]", encoding="utf-8")
            past = time.time() - watchers.DONE_KEEP_SECONDS - 60
            os.utime(old, (past, past))
            os.utime(waiting, (past, past))
            watchers._last_sweep["at"] = 0.0
            self.assertEqual(watchers.sweep_handled(), 1)
            self.assertFalse(old.exists())
            self.assertTrue(new.exists() and waiting.exists())  # a request still to handle is never touched
            self.assertEqual(watchers.sweep_handled(), 0)       # (at most once in `every` seconds)


class CachedTokensAreCounted(unittest.TestCase):
    def test_both_shapes_the_proxy_passes_on(self):
        from decurix_agent import llm_client
        self.assertEqual(llm_client.cached_tokens({"prompt_tokens": 900, "prompt_tokens_details": {"cached_tokens": 640}}),
                         640)
        self.assertEqual(llm_client.cached_tokens({"cache_read_input_tokens": 12}), 12)
        self.assertIsNone(llm_client.cached_tokens({"prompt_tokens": 9}))  # unknown, not 0

    def test_a_chat_adds_them_to_the_task(self):
        from decurix_agent import llm_client
        answer = {"choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}],
                  "usage": {"prompt_tokens": 900, "completion_tokens": 20, "prompt_tokens_details": {"cached_tokens": 640}}}

        class Ok:
            def read(self):
                import json
                return json.dumps(answer).encode()

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False
        usage.reset()
        self.addCleanup(usage.reset)
        with mock.patch.object(llm_client.urllib.request, "urlopen", lambda request, timeout=None: Ok()), \
                mock.patch.object(llm_client, "available", lambda: True):
            llm_client.chat([{"role": "user", "content": "x"}], purpose="py.verify")
        self.assertEqual(usage.snapshot().get("cached_input_tokens"), 640)
        self.assertEqual(usage.snapshot().get("input_tokens"), 900)


class TheEmbedWarm(unittest.TestCase):
    def warmed(self, hosted, health):
        posts = []

        def urlopen(request, timeout=None):
            posts.append(request.full_url)
            return mock.MagicMock()
        get = (lambda url: health) if not isinstance(health, Exception) else mock.Mock(side_effect=health)
        with mock.patch.object(config, "HOSTED_BRAIN", hosted, create=True), \
                mock.patch.object(thebot_harness, "_get_json", get), \
                mock.patch.object(thebot_harness.urllib.request, "urlopen", urlopen):
            thebot_harness.warm_after_ready()
        return [p for p in posts if p.endswith("/api/embed")]

    def test_a_local_brain_embedding_through_ollama_is_warmed_as_before(self):
        self.assertEqual(len(self.warmed(False, {"ok": True, "embeddings": "ollama"})), 1)

    def test_a_hosted_brain_or_another_embedder_is_not(self):
        self.assertEqual(self.warmed(True, {"embeddings": "ollama"}), [])
        self.assertEqual(self.warmed(False, {"embeddings": "off"}), [])
        self.assertEqual(self.warmed(False, {"ok": True}), [])          # a soul that does not say
        self.assertEqual(self.warmed(False, OSError("no soul")), [])


if __name__ == "__main__":
    unittest.main()

