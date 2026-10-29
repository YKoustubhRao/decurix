import base64
import json
import os
import struct
import tempfile
import unittest
import zlib
from pathlib import Path
from unittest.mock import patch

from decurix_agent import practice
from decurix_agent.computer import recording, skills


def _png(seed):
    """A tiny valid PNG whose pixels differ by seed."""
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)
    raw = b"\x00" + bytes([seed % 256, 0, 0])
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


class PracticeSourceTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, skills.ROOT
        recording.ROOT = Path(self.tmp.name) / "recordings"
        skills.ROOT = Path(self.tmp.name) / "skills"

    def tearDown(self):
        recording.ROOT, skills.ROOT = self.saved
        self.tmp.cleanup()

    def _skill(self):
        skill = {"schema": skills.SCHEMA, "name": "enter-order", "title": "Enter an order", "status": skills.READY,
                 "goal": "Enter the order from the sheet", "params": [],
                 "steps": [
                     {"n": 1, "kind": "click", "url": "http://forms.example.test/orders/new", "window": "Orders - Chromium",
                      "summary": "click New", "target": {"role": "button", "name": "New"}},
                     {"n": 2, "kind": "type", "url": "http://forms.example.test/orders/new", "text": "[withheld]",
                      "withheld": True, "summary": "type into Card", "target": {"role": "textbox", "name": "Card"}},
                     {"n": 3, "kind": "click", "url": "http://forms.example.test/orders/new", "consequential": True, "summary": "click Submit",
                      "target": {"role": "button", "name": "Submit"}},
                 ]}
        skills.ROOT.mkdir(parents=True)
        raw = json.dumps(skill, ensure_ascii=False, indent=1).encode("utf-8") + b"\n"
        (skills.ROOT / "enter-order.json").write_bytes(raw)
        return raw

    def test_skill_source_is_exact_and_keeps_withheld_values_withheld(self):
        raw = self._skill()
        out = practice.skill_source("enter-order")
        files = {k: base64.b64decode(v) for k, v in out["files"].items()}
        self.assertEqual(files[".decurix/skills/enter-order.json"], raw)
        self.assertTrue(all(k.startswith(".decurix/") for k in files))
        joined = b"".join(files.values())
        self.assertIn(b"[withheld]", files[".decurix/skills/enter-order.json"])
        self.assertNotIn(b"4111", joined)
        facts = out["facts"]
        self.assertEqual(facts["hosts"], ["forms.example.test"])
        self.assertEqual(facts["schemes"], ["http"])
        self.assertTrue(facts["consequential"])
        self.assertFalse(facts["manual"])
        self.assertEqual(facts["steps"], 3)
        self.assertEqual(out["kind"], "skill")

    def test_bad_names_are_not_found(self):
        for bad in ("../x", "X", ""):
            with self.assertRaises(FileNotFoundError):
                practice.skill_source(bad)
        with self.assertRaises(FileNotFoundError):
            practice.recording_source("../../etc")
        with self.assertRaises(FileNotFoundError):
            practice.recording_source("demo-20261028T000000-nothere")

    def test_recording_source_carries_exact_bytes_and_chosen_frames(self):
        session = "demo-20261028T000000-abc123"
        windows = ["Sheet - Office", "Sheet - Office", "Mail - Office", "Mail - Office"]
        for i, window in enumerate(windows):
            recording.append(session, {"kind": "click", "actor": "human", "window": window, "outcome": "done",
                                       "target": {"role": "button", "name": f"b{i}"}},
                             before=_png(i), after=_png(100 + i), defaults={"title": "Copy totals"})
        out = practice.recording_source(session)
        files = {k: base64.b64decode(v) for k, v in out["files"].items()}
        base = f".decurix/recordings/{session}"
        self.assertEqual(files[f"{base}/meta.json"], (recording.ROOT / session / "meta.json").read_bytes())
        self.assertEqual(files[f"{base}/events.jsonl"], (recording.ROOT / session / "events.jsonl").read_bytes())
        frames = [k for k in files if "/frames/" in k]
        # first screen, the window change, and the end
        self.assertEqual(len(frames), 3)
        self.assertEqual(out["facts"]["hosts"], [])
        listed = practice.sources()["sources"]
        self.assertIn(("recording", session), [(s["kind"], s["name"]) for s in listed])

    def test_sources_lists_skills_and_demonstrations_only(self):
        self._skill()
        recording.append("task-t1", {"kind": "click", "actor": "agent"}, defaults={"title": "a task"})
        names = [(s["kind"], s["name"]) for s in practice.sources()["sources"]]
        self.assertIn(("skill", "enter-order"), names)
        self.assertNotIn(("recording", "task-t1"), names)
        self.assertNotIn("files", practice.sources()["sources"][0])

    def test_facts_flag_by_hand_steps_and_secure_sites(self):
        facts = practice.facts([{"kind": "manual"}, {"kind": "click", "url": "https://a.test/x"}, {"kind": "click", "url": "http://b.test/"}])
        self.assertTrue(facts["manual"])
        self.assertEqual(facts["hosts"], ["a.test", "b.test"])
        self.assertEqual(facts["schemes"], ["https", "http"])

    def test_oversized_sources_drop_frames_then_refuse(self):
        files = {"a/frames/1.png": b"x" * 10, "a/meta.json": b"y" * 10}
        with patch.object(practice, "MAX_SOURCE_BYTES", 5):
            with self.assertRaises(ValueError):
                practice._bounded(dict(files))
        with patch.object(practice, "MAX_SOURCE_BYTES", 5):
            self.assertEqual(practice._bounded({"a/frames/1.png": b"x" * 10, "a/meta.json": b"y"}), {"a/meta.json": b"y"})


class PracticeModeTest(unittest.TestCase):
    def test_practice_mode_reads_the_flag(self):
        with patch.dict(os.environ, {"DECURIX_PRACTICE": "1"}):
            self.assertTrue(practice.active())
        with patch.dict(os.environ, {"DECURIX_PRACTICE": ""}):
            self.assertFalse(practice.active())

    def test_practice_mode_changes_no_prompt_text(self):
        # The worker is not told it is practising: nothing but practice.py reads the flag.
        root = Path(practice.__file__).resolve().parent
        readers = []
        for path in root.rglob("*.py"):
            if path.name == "practice.py" or "__pycache__" in path.parts:
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            if "DECURIX_PRACTICE" in text or "practice.active" in text:
                readers.append(str(path.relative_to(root)))
        self.assertEqual(readers, [])


if __name__ == "__main__":
    unittest.main()
