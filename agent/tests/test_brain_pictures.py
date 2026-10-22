import io
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest import mock

from decurix_agent.computer import cli, screen
from decurix_agent.tools import office


class FakeFrame:
    def __init__(self):
        self.scales = []

    def png(self, scale=1):
        self.scales.append(scale)
        return b"PNG-%d" % scale


class GuiLeavesTheScreenForAVisionEngine(unittest.TestCase):
    def test_a_screen_command_leaves_the_screen_where_asked(self):
        frame = FakeFrame()
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "screen.png")
            env = {"DECURIX_GUI_SCREENSHOT": path, "DECURIX_GUI_SCREENSHOT_SCALE": "2"}
            with mock.patch.dict(os.environ, env), mock.patch.object(screen, "grab", return_value=frame):
                cli.save_screenshot(["click", "e12"])
                with open(path, "rb") as f:
                    self.assertEqual(f.read(), b"PNG-2")

                os.remove(path)
                cli.save_screenshot(["skills", "invoice"])  # not a screen command
                self.assertFalse(os.path.exists(path))

    def test_nothing_is_saved_unless_asked(self):
        with mock.patch.dict(os.environ, {"DECURIX_GUI_SCREENSHOT": ""}), \
                mock.patch.object(screen, "grab", side_effect=AssertionError("grabbed")):
            cli.save_screenshot(["click", "e12"])

    def test_a_failed_grab_never_fails_the_command(self):
        with tempfile.TemporaryDirectory() as d, \
                mock.patch.dict(os.environ, {"DECURIX_GUI_SCREENSHOT": os.path.join(d, "s.png")}), \
                mock.patch.object(screen, "grab", side_effect=OSError("no display")):
            cli.save_screenshot(["observe"])


class OfficeReadsPicturesTheWayTheModelCan(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        # Resolved, as office names it: Windows' temp folder has a short name too.
        self.picture = os.path.join(os.path.realpath(self.dir.name), "invoice.png")
        with open(self.picture, "wb") as f:
            # (A PNG's own signature: office tells a file named as a picture that is not one - GST suite T8.)
            f.write(b"\x89PNG\r\n\x1a\n not really")

    def tearDown(self):
        self.dir.cleanup()

    def read(self, mode):
        out = io.StringIO()
        with mock.patch.dict(os.environ, {"DECURIX_OFFICE_IMAGES": mode}), \
                mock.patch.object(office, "_ocr_file", return_value="Invoice INV-7 total 1,200.00"), \
                redirect_stdout(out):
            code = office.main(["read", self.picture])
        self.assertEqual(code, 0)
        return out.getvalue()

    def test_a_model_that_sees_is_given_the_picture(self):
        text = self.read("attach")
        self.assertIn(f"[page image: {self.picture}]", text)
        self.assertNotIn("INV-7", text)

    def test_a_text_model_is_given_what_ocr_reads(self):
        text = self.read("ocr")
        self.assertIn("read by OCR", text)
        self.assertIn("INV-7", text)
        self.assertNotIn("[page image:", text)


if __name__ == "__main__":
    unittest.main()
