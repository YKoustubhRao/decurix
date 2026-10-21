import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import config
from decurix_agent.computer import cli, core, shown


class ShownRefs(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for patcher in (mock.patch.object(config, "DECURIX_DIR", Path(self.dir.name)),
                        mock.patch("decurix_agent.task_context.current", return_value="t0123456789abcdef")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_nothing_is_refused_before_anything_was_shown(self):
        self.assertFalse(shown.unseen("e15909"))

    def test_a_ref_that_was_printed_is_known_and_a_guessed_one_is_not(self):
        shown.note('35 element(s) appeared: button "PivotTable" (e15895), menuitem "Shapes" (e15900) (+29 more)')
        self.assertFalse(shown.unseen("e15900"))
        self.assertTrue(shown.unseen("e15909"))

    def test_another_task_starts_afresh(self):
        shown.note('button "OK" (e1)')
        with mock.patch("decurix_agent.task_context.current", return_value="tfedcba9876543210"):
            self.assertFalse(shown.unseen("e2"))

    def test_gui_refuses_a_guessed_ref_and_does_nothing(self):
        shown.note('button "OK" (e1)')
        with mock.patch.object(core, "act") as act:
            code = cli.main(["click", "e15909"])
        self.assertEqual(code, cli.EXIT_ERROR)
        act.assert_not_called()

    def test_what_gui_prints_is_noted(self):
        with mock.patch.object(core, "act", return_value={"status": "done", "text": 'Clicked tab "Insert" (e7). '
                                                          '2 element(s) appeared: button "Recommended Charts" (e8)'}):
            cli.main(["click", "e7"])
        self.assertFalse(shown.unseen("e8"))
        self.assertTrue(shown.unseen("e9"))


if __name__ == "__main__":
    unittest.main()


class MovedRef(ShownRefs):
    def test_a_gone_ref_is_named_with_its_new_ref(self):
        shown.note('e1 tab "Data"\ne2 button "OK"')
        look = {"elements": [{"ref": "e9", "role": "tab", "name": "Data"}]}
        with mock.patch.object(core, "observe", return_value=look):
            note = cli._moved_note("e1 is no longer on the screen (the window or page changed).")
        self.assertIn('tab "Data", which is now e9', note)

    def test_a_ref_named_in_a_result_is_remembered_too(self):
        shown.note('Clicked tab "Insert" (e7).')
        self.assertEqual(shown.was("e7"), ("tab", "Insert"))

    def test_two_alike_are_not_guessed_between(self):
        shown.note('e1 button "Save"')
        look = {"elements": [{"ref": "e8", "role": "button", "name": "Save"},
                             {"ref": "e9", "role": "button", "name": "Save"}]}
        with mock.patch.object(core, "observe", return_value=look):
            note = cli._moved_note("e1 is no longer on the screen")
        self.assertNotIn("now", note)
