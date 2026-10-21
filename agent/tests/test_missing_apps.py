import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import identity
from decurix_agent.computer import core, launchers
from decurix_agent.computer.core import ComputerError


# The control plane writes the apps a person asked a desktop to have and it does not (internal/desktop/missingapps.go).
# Found live: a desktop created without the Word asked for said nothing, and the worker wrote the letter elsewhere.
MISSING = [
    {"id": "win_word", "name": "Microsoft Word", "reason": "Not added yet: the check did not answer.", "pending": True},
    {"id": "win_paint", "name": "Paint", "reason": "This is a Microsoft Store app."},
]


class MissingApps(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.file = Path(self.tmp.name) / "missing-apps.json"
        self.file.write_text(json.dumps(MISSING), encoding="utf-8")
        patcher = patch.object(launchers, "MISSING_FILE", self.file)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_request_names_a_missing_app_by_the_words_people_use(self):
        self.assertEqual(launchers.missing_named("Word")["id"], "win_word")
        self.assertEqual(launchers.missing_named("microsoft word")["id"], "win_word")
        self.assertEqual(launchers.missing_named("Paint")["id"], "win_paint")
        # Part of a word is not a name, and an app nobody asked for is not missing.
        self.assertIsNone(launchers.missing_named("Wor"))
        self.assertIsNone(launchers.missing_named("WordPad"))
        self.assertIsNone(launchers.missing_named("Calc"))

    def test_pending_and_refused_apps_are_said_differently(self):
        pending, refused = (launchers.missing_sentence(a) for a in MISSING)
        self.assertIn("isn't on this desktop yet", pending)
        self.assertIn("Paint isn't on this desktop (This is a Microsoft Store app).", refused)

    def test_no_file_or_a_bad_one_means_nothing_is_missing(self):
        self.file.write_text("{not json", encoding="utf-8")
        self.assertEqual(launchers.missing(), [])
        self.file.unlink()
        self.assertEqual(launchers.missing(), [])
        self.assertIsNone(launchers.missing_named("Word"))

    def test_the_agent_is_told_which_apps_it_does_not_have(self):
        with patch.object(identity, "_desktop_apps", return_value=[]):
            text = identity.get_identity_context()
        self.assertIn("does NOT have", text)
        self.assertIn("Microsoft Word isn't on this desktop yet", text)
        self.assertIn("not do the work in another app", text)

    def test_nothing_is_said_when_nothing_is_missing(self):
        self.file.write_text("[]", encoding="utf-8")
        with patch.object(identity, "_desktop_apps", return_value=[]):
            self.assertNotIn("does NOT have", identity.get_identity_context())

    def test_opening_a_missing_app_is_refused_instead_of_a_stand_in(self):
        with patch.object(core.screen, "windows", return_value=[]), \
                patch.object(core, "_task_windows"), \
                patch.object(launchers, "entries", return_value=[]), \
                patch.object(core.session, "launch") as launch:
            with self.assertRaises(ComputerError) as caught:
                core._open("Word", False)
        launch.assert_not_called()
        self.assertIn("Microsoft Word isn't on this desktop yet", str(caught.exception))

    def test_an_app_that_is_on_the_desktop_opens_even_if_once_missing(self):
        # Added by the background retry after the file was read: its launcher wins.
        entry = {"name": "Microsoft Word (Host)", "exec": "/home/headless/.local/bin/word-host", "path": "x", "wm_class": ""}
        with patch.object(launchers, "entries", return_value=[entry]):
            self.assertIsNotNone(launchers.by_name("Microsoft Word"))
            self.assertIsNotNone(launchers.missing_named("Microsoft Word"))  # the file may lag; by_name is asked first


if __name__ == "__main__":
    unittest.main()
