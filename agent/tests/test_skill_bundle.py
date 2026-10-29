"""
Sharing a skill between desktops (skill_bundle.py) and reading the organisation's knowledge (org_knowledge.py).

    cd agent/tests && PYTHONPATH=.. python -m unittest test_skill_bundle
"""

import io
import json
import tempfile
import unittest
import urllib.parse
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401 - first: stubs decurix_agent.config on a machine with no desktop

from decurix_agent import org_knowledge, skill_bundle
from decurix_agent.computer import skills
from decurix_agent.computer.errors import ComputerError


def skill(name="file-report", **extra):
    """A small ready skill as learning.py saves one."""

    shown = {
        "schema": skills.SCHEMA,
        "name": name,
        "title": "File a report",
        "status": skills.READY,
        "version": 1,
        "steps": [
            {"n": 1, "kind": "click", "summary": "click Reports"},
            {"n": 2, "kind": "type", "summary": "type the period", "param": "period", "samples": ["March"]},
            {"n": 3, "kind": "manual", "summary": "type your password yourself", "secret": True,
             "reason": "type your password yourself - it is never recorded"},
        ],
        "params": [{"name": "period", "label": "Period", "default": "March", "varies": True, "step": 2}],
    }
    shown.update(extra)
    return shown


class _Skills(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT
        skills.ROOT = Path(self.tmp.name) / "skills"

    def tearDown(self):
        skills.ROOT = self.saved
        self.tmp.cleanup()


class ExportTest(_Skills):
    def test_export_carries_the_whole_skill(self):
        skills.save(skill(runs=4))

        bundle = skill_bundle.export("file-report")

        self.assertEqual(bundle["format"], skill_bundle.FORMAT)
        self.assertEqual(bundle["skill"], skills.load("file-report"))
        self.assertIsInstance(bundle["exported_at"], int)
        json.dumps(bundle)  # it must travel as JSON

    def test_export_of_a_missing_skill(self):
        with self.assertRaises(FileNotFoundError):
            skill_bundle.export("not-here")

    def test_export_refuses_a_typed_secret(self):
        bad = skill()
        bad["steps"][2] = {"n": 3, "kind": "type", "summary": "type the password", "secret": True}
        skills.save(bad)

        with self.assertRaises(ComputerError):
            skill_bundle.export("file-report")


class InstallTest(_Skills):
    def bundle(self, **extra):
        return {"format": skill_bundle.FORMAT, "skill": skill(**extra), "exported_at": 1}

    def test_install_saves_keeps_status_and_notes_where_it_came_from(self):
        shown = skill_bundle.install(self.bundle(runs=9, experience={"steps": {}}, status=skills.DRAFT), version=3,
                                     sha="ab" * 32)

        self.assertEqual(shown["name"], "file-report")
        self.assertEqual(shown["status"], skills.DRAFT)
        saved = skills.load("file-report")
        self.assertEqual(saved["status"], skills.DRAFT)
        self.assertNotIn("runs", saved)
        self.assertNotIn("experience", saved)
        self.assertEqual(saved["installed_from"]["version"], 3)
        self.assertEqual(saved["installed_from"]["sha"], "ab" * 32)
        self.assertEqual(saved["installed_from"]["skill_version"], 1)

    def test_bad_bundles_are_refused(self):
        cases = {
            "not a dict": "x",
            "wrong format": {"format": "other", "skill": skill()},
            "no skill": {"format": skill_bundle.FORMAT},
            "bad name": {"format": skill_bundle.FORMAT, "skill": skill(name="../etc")},
            "no steps": {"format": skill_bundle.FORMAT, "skill": skill(steps=None)},
        }
        for label, bundle in cases.items():
            with self.subTest(label), self.assertRaises(ValueError):
                skill_bundle.install(bundle)

        with self.assertRaises(ValueError):
            skill_bundle.install(self.bundle(), version=0)
        with self.assertRaises(ValueError):
            skill_bundle.install(self.bundle(), version=True)

    def test_secrets_are_refused(self):
        typed = skill()
        typed["steps"][2] = {"n": 3, "kind": "type", "summary": "type the password", "secret": True, "text": "x"}
        kept = skill()
        kept["steps"][2]["samples"] = ["hunter2"]
        param = skill()
        param["params"].append({"name": "pw", "label": "Password", "secret": True, "default": "hunter2"})
        spelled = skill()
        spelled["steps"][1]["samples"] = ["password: Lift$2026"]

        for label, bad in {"typed": typed, "kept value": kept, "param": param, "spelled out": spelled}.items():
            with self.subTest(label), self.assertRaises(ComputerError):
                skill_bundle.install({"format": skill_bundle.FORMAT, "skill": bad})
            self.assertFalse((skills.ROOT / "file-report.json").exists())

    def test_a_skill_taught_here_is_not_overwritten(self):
        skills.save(skill(title="Taught here"))

        with self.assertRaises(ComputerError):
            skill_bundle.install(self.bundle())
        self.assertEqual(skills.load("file-report")["title"], "Taught here")

        skill_bundle.install(self.bundle(), replace=True)
        self.assertEqual(skills.load("file-report")["title"], "File a report")

    def test_an_installed_skill_updates_unless_changed_here(self):
        skill_bundle.install(self.bundle(), version=1)

        # Only used here: a newer version goes in.
        used = skills.load("file-report")
        used["runs"] = 5
        skills.save(used)
        skill_bundle.install(self.bundle(title="Newer"), version=2)
        self.assertEqual(skills.load("file-report")["title"], "Newer")

        # Corrected here: the next install is refused.
        corrected = skills.load("file-report")
        corrected["corrections"] = [{"text": "use the second sheet"}]
        skills.save(corrected)
        with self.assertRaises(ComputerError):
            skill_bundle.install(self.bundle(title="Newest"), version=3)

        # A run that learned from the screen bumps the version: refused too.
        relearned = skills.load("file-report")
        relearned.pop("corrections")
        relearned["version"] = 2
        skills.save(relearned)
        with self.assertRaises(ComputerError):
            skill_bundle.install(self.bundle(title="Newest"), version=3)

    def test_round_trip_between_desktops(self):
        skills.save(skill(runs=2))
        bundle = json.loads(json.dumps(skill_bundle.export("file-report")))
        skills.delete("file-report")

        skill_bundle.install(bundle, version=1, sha="c" * 64)

        saved = skills.load("file-report")
        self.assertEqual(saved["steps"], skill()["steps"])
        self.assertNotIn("runs", saved)


class _Answer(io.BytesIO):
    def __init__(self, body, status=200):
        super().__init__(body if isinstance(body, bytes) else json.dumps(body).encode("utf-8"))
        self.status = status

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class OrgKnowledgeTest(unittest.TestCase):
    def test_fetch_returns_the_lines_for_this_worker(self):
        seen = {}

        def fake(url, timeout=None):
            seen["url"], seen["timeout"] = url, timeout
            return _Answer({"lines": ["Rule (org): Never pay twice.", "  Fact (role clerk):\nclose   monthly ", 7, ""]})

        with mock.patch("urllib.request.urlopen", fake):
            lines = org_knowledge.fetch("w-1", task_id="t 9")

        self.assertEqual(lines, ["Rule (org): Never pay twice.", "Fact (role clerk): close monthly"])
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(seen["url"]).query)
        self.assertEqual(query, {"worker": ["w-1"], "task": ["t 9"]})
        self.assertIn("/api/knowledge?", seen["url"])
        self.assertEqual(seen["timeout"], org_knowledge.TIMEOUT_SECONDS)

    def test_fetch_never_raises(self):
        def unreachable(url, timeout=None):
            raise OSError("connection refused")

        answers = {
            "unreachable": unreachable,
            "not json": lambda url, timeout=None: _Answer(b"<html>"),
            "not a dict": lambda url, timeout=None: _Answer([1, 2]),
            "no lines": lambda url, timeout=None: _Answer({"items": []}),
            "server error": lambda url, timeout=None: _Answer({"lines": ["x"]}, status=500),
            "too large": lambda url, timeout=None: _Answer(b"x" * (org_knowledge.MAX_ANSWER_BYTES + 2)),
        }
        for label, fake in answers.items():
            with self.subTest(label), mock.patch("urllib.request.urlopen", fake):
                self.assertEqual(org_knowledge.fetch("w-1"), [])
                self.assertEqual(org_knowledge.as_text("w-1"), "")

    def test_cached_reuses_an_answer_but_not_a_failure(self):
        org_knowledge._cache.clear()
        calls = []

        def fake(url, timeout=None):
            calls.append(url)
            if len(calls) == 1:
                raise OSError("down")
            return _Answer({"lines": [f"Rule (org): call {len(calls)}"]})

        with mock.patch("urllib.request.urlopen", fake):
            self.assertEqual(org_knowledge.cached("w-1"), [])
            self.assertEqual(org_knowledge.cached("w-1"), ["Rule (org): call 2"])
            self.assertEqual(org_knowledge.cached("w-1"), ["Rule (org): call 2"])
            self.assertEqual(len(calls), 2)
            self.assertEqual(org_knowledge.cached("w-1", max_age=0), ["Rule (org): call 3"])
        org_knowledge._cache.clear()

    def test_fetch_bounds_the_lines(self):
        many = {"lines": [f"Rule (org): {n}" for n in range(org_knowledge.MAX_LINES + 50)]}
        with mock.patch("urllib.request.urlopen", lambda url, timeout=None: _Answer(many)):
            self.assertEqual(len(org_knowledge.fetch("w-1")), org_knowledge.MAX_LINES)


if __name__ == "__main__":
    unittest.main()
