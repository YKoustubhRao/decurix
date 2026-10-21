import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import playbooks


class QualificationSource(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for p in (patch.object(playbooks, "ROOT", self.root / "playbooks"),
                  patch.object(playbooks, "TOOLS", self.root / "tools")):
            p.start()
            self.addCleanup(p.stop)
        playbooks.TOOLS.mkdir()

    def test_exports_exact_source_including_changes_after_the_preview_limit(self):
        target = playbooks.TOOLS / "reconcile.py"
        script = "# padding\n" * 2500 + "print('first')\n"
        target.write_bytes(script.encode("utf-8"))
        playbooks.save({"name": "reconcile", "commands": [], "tool": {"path": str(target)}})
        self.assertEqual(playbooks.qualification_source("reconcile"),
                         {"script": script, "language": "python"})
        target.write_bytes((script + "print('changed')\n").encode("utf-8"))
        self.assertNotEqual(playbooks.qualification_source("reconcile")["script"], script)

    def test_rejects_paths_outside_tools_and_never_truncates_oversized_scripts(self):
        target = self.root / "outside.py"
        target.write_text("print('outside')", encoding="utf-8")
        playbooks.save({"name": "reconcile", "commands": [], "tool": {"path": str(target)}})
        with self.assertRaises(ValueError):
            playbooks.qualification_source("reconcile")
        target = playbooks.TOOLS / "large.py"
        target.write_bytes(b"x" * (128 * 1024 + 1))
        playbooks.save({"name": "reconcile", "commands": [], "tool": {"path": str(target)}})
        with self.assertRaises(ValueError):
            playbooks.qualification_source("reconcile")
        with self.assertRaises(ValueError):
            playbooks.qualification_source("../secret")


if __name__ == "__main__":
    unittest.main()
