"""Tests for computer/panelwatch.py: when the worker's panel is laid out again after the screen changes size."""

import pathlib
import unittest

from decurix_agent.computer import panelwatch

ROOT = pathlib.Path(__file__).resolve().parents[1]


class WatchDecides(unittest.TestCase):
    def test_the_first_size_is_the_one_the_panel_started_with(self):
        w = panelwatch.Watch()
        self.assertFalse(w.look((1280, 720)))
        self.assertFalse(w.look((1280, 720)))

    def test_a_new_size_restarts_the_panel_once_it_has_held_for_two_looks(self):
        w = panelwatch.Watch()
        w.look((1280, 720))
        self.assertFalse(w.look((1536, 793)))
        self.assertTrue(w.look((1536, 793)))
        self.assertFalse(w.look((1536, 793)), "laid out once for a size, not on every look")

    def test_a_window_being_dragged_waits_for_the_size_to_settle(self):
        w = panelwatch.Watch()
        w.look((1280, 720))
        for size in [(1300, 730), (1320, 740), (1340, 750)]:
            self.assertFalse(w.look(size))
        self.assertTrue(w.look((1340, 750)))

    def test_going_back_to_the_laid_out_size_restarts_nothing(self):
        w = panelwatch.Watch()
        w.look((1280, 720))
        w.look((1536, 793))
        self.assertFalse(w.look((1280, 720)))
        self.assertFalse(w.look((1280, 720)))

    def test_a_look_with_no_display_changes_nothing(self):
        w = panelwatch.Watch()
        self.assertFalse(w.look(None))
        w.look((1280, 720))
        w.look((1536, 793))
        self.assertFalse(w.look(None))
        self.assertTrue(w.look((1536, 793)))


class StartedByTheWorkerSession(unittest.TestCase):
    def test_the_worker_session_starts_it_after_the_panel(self):
        script = (ROOT / "decurix-worker-session.sh").read_text(encoding="utf-8")
        panel = script.index('xfce4-panel "Thunar --daemon"')
        watch = script.index("decurix_agent.computer.panelwatch")
        self.assertLess(panel, watch)

    def test_it_starts_only_the_panel_by_absolute_path(self):
        source = (ROOT / "decurix_agent" / "computer" / "panelwatch.py").read_text(encoding="utf-8")
        self.assertEqual(panelwatch.PANEL, "/usr/bin/xfce4-panel")
        self.assertEqual(source.count("subprocess.run("), 1)
        for start in ("os.system", "os.exec", "os.spawn", "Popen(", "shell=True"):
            self.assertNotIn(start, source)


if __name__ == "__main__":
    unittest.main()
