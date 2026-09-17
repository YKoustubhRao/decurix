import unittest
from unittest.mock import MagicMock, patch

from flowera_agent.computer import screen, core, cdp
from flowera_agent.computer.screen import Frame, changed_fraction


class TestScreenOptimizations(unittest.TestCase):

    def test_frame_sample_caching(self):
        rgb = bytes([10, 20, 30] * (100 * 100))
        frame = Frame(100, 100, rgb)
        self.assertIsNone(frame._sample)

        s1 = frame.sample()
        self.assertIsNotNone(frame._sample)
        self.assertIs(s1, frame._sample)

        s2 = frame.sample()
        self.assertIs(s1, s2)

    def test_frame_png_caching(self):
        rgb = bytes([10, 20, 30] * (50 * 50))
        frame = Frame(50, 50, rgb)
        self.assertEqual(len(frame._png_cache), 0)

        png1 = frame.png(scale=1)
        self.assertIn(1, frame._png_cache)
        self.assertIs(png1, frame._png_cache[1])

        png2 = frame.png(scale=1)
        self.assertIs(png1, png2)

        png_thumb = frame.png(scale=2)
        self.assertIn(2, frame._png_cache)
        self.assertIsNot(png1, png_thumb)

    def test_changed_fraction_accuracy(self):
        rgb1 = bytes([10, 20, 30] * (100 * 100))
        rgb2 = bytes([10, 20, 30] * (100 * 100))
        rgb3 = bytes([200, 200, 200] * (100 * 100))

        f1 = Frame(100, 100, rgb1)
        f2 = Frame(100, 100, rgb2)
        f3 = Frame(100, 100, rgb3)

        self.assertEqual(changed_fraction(f1, f2), 0.0)
        self.assertEqual(changed_fraction(f1, f3), 1.0)
        self.assertEqual(changed_fraction(None, f1), 1.0)
        self.assertEqual(changed_fraction(f1, None), 1.0)


import tempfile
from pathlib import Path


class TestCoreObserveFrameDeduplication(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved_state_dir = core.STATE_DIR
        self.saved_state_file = core.STATE_FILE
        core.STATE_DIR = Path(self.tmp.name)
        core.STATE_FILE = core.STATE_DIR / "observation.json"

    def tearDown(self):
        core.STATE_DIR = self.saved_state_dir
        core.STATE_FILE = self.saved_state_file
        self.tmp.cleanup()

    @patch("flowera_agent.computer.atspi.observe", return_value=([], []))
    @patch("flowera_agent.computer.screen.grab")
    @patch("flowera_agent.computer.screen.windows")
    @patch("flowera_agent.computer.session.ensure")
    @patch("flowera_agent.computer.cdp.available")
    def test_observe_reuses_provided_frame(self, mock_cdp_avail, mock_ensure, mock_windows, mock_grab, mock_atspi):
        mock_cdp_avail.return_value = False
        mock_windows.return_value = []
        custom_frame = Frame(10, 10, bytes(300))

        obs = core.observe(remember=False, frame=custom_frame)

        self.assertFalse(mock_grab.called)
        self.assertIs(obs["frame"], custom_frame)

    @patch("flowera_agent.computer.atspi.observe", return_value=([], []))
    @patch("flowera_agent.computer.screen.grab")
    @patch("flowera_agent.computer.screen.windows")
    @patch("flowera_agent.computer.session.ensure")
    @patch("flowera_agent.computer.cdp.available")
    def test_observe_grabs_when_frame_is_none(self, mock_cdp_avail, mock_ensure, mock_windows, mock_grab, mock_atspi):
        mock_cdp_avail.return_value = False
        mock_windows.return_value = []
        grabbed_frame = Frame(10, 10, bytes(300))
        mock_grab.return_value = grabbed_frame

        obs = core.observe(remember=False, frame=None)

        self.assertTrue(mock_grab.called)
        self.assertIs(obs["frame"], grabbed_frame)


class TestCdpWaitReady(unittest.TestCase):

    @patch("flowera_agent.computer.cdp.active_page")
    def test_wait_ready_returns_title_without_navigate(self, mock_active):
        mock_page = MagicMock()
        mock_page.evaluate.side_effect = lambda expr: {
            "document.readyState": "complete",
            "document.title": "Flowera Dashboard",
        }.get(expr, "")
        mock_active.return_value = mock_page

        title = cdp.wait_ready(timeout=1, default_title="Fallback")

        self.assertEqual(title, "Flowera Dashboard")
        self.assertFalse(any(call[0] == "Page.navigate" for call in mock_page.call.call_args_list))
        self.assertTrue(mock_page.close.called)


if __name__ == "__main__":
    unittest.main()
