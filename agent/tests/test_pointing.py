import unittest

from decurix_agent.computer import cli, core, pointing
from decurix_agent.computer.screen import Frame


def pixel(frame, x, y):
    at = (y * frame.width + x) * 3
    return tuple(frame.rgb[at:at + 3])


class TestRuler(unittest.TestCase):
    """The ruler a vision model reads positions off (pointing.py)."""

    def setUp(self):
        self.width, self.height = 640, 360
        self.plain = Frame(self.width, self.height, bytes([240, 240, 240]) * (self.width * self.height))
        self.ruled = pointing.ruled(self.plain)

    def test_the_picture_keeps_its_size_and_the_original_is_untouched(self):
        self.assertEqual((self.ruled.width, self.ruled.height), (self.width, self.height))
        self.assertEqual(len(self.ruled.rgb), len(self.plain.rgb))
        self.assertEqual(pixel(self.plain, 100, 51), (240, 240, 240))

    def test_a_line_is_drawn_at_every_hundred_and_nowhere_else(self):
        # Away from the labels (which sit at the edges and half way across).
        self.assertEqual(pixel(self.ruled, 100, 54), pointing.LINE)
        self.assertEqual(pixel(self.ruled, 300, 60), pointing.LINE)
        self.assertEqual(pixel(self.ruled, 54, 100), pointing.LINE)
        self.assertEqual(pixel(self.ruled, 150, 150), (240, 240, 240))
        self.assertEqual(pixel(self.ruled, 250, 250), (240, 240, 240))

    def test_each_line_is_numbered_at_the_edge(self):
        # The label for x=100 sits at the top, centred on its line; "100" is 3 digits wide.
        width, height = pointing._label_size("100")
        inked = {pixel(self.ruled, x, y) for x in range(100 - width // 2, 100 + width // 2) for y in range(height)}
        self.assertIn(pointing.LABEL_PAPER, inked)
        self.assertIn(pointing.LABEL_INK, inked)

    def test_it_is_still_a_frame_that_encodes(self):
        self.assertTrue(self.ruled.png().startswith(b"\x89PNG"))


class TestTypingAPath(unittest.TestCase):
    r"""`gui type` reads \n and \t as keys - but not inside a Windows path."""

    def test_a_windows_path_keeps_its_backslashes(self):
        # Found by bench/native: C:\...\workers\t1\home\... was refused as "a Tab in the text".
        for path in (r"C:\Users\ada\workers\t1\home\new\typed.txt", r"\\server\share\table\n1.txt"):
            self.assertEqual(cli._unescape(path), path)

    def test_escapes_outside_a_path_still_work(self):
        self.assertEqual(cli._unescape(r"line one\nline two"), "line one\nline two")
        self.assertEqual(cli._unescape(r"Admin\tsecret"), "Admin\tsecret")
        self.assertEqual(cli._unescape("plain"), "plain")


class TestHumanCheck(unittest.TestCase):
    """A prove-you-are-a-person page is the person's to pass, and observe says so."""

    def test_a_captcha_page_is_recognised(self):
        self.assertTrue(core._human_check([{"name": "Our systems have detected unusual traffic from your computer network."}], None))
        self.assertTrue(core._human_check([{"name": "I'm not a robot"}], None))
        self.assertTrue(core._human_check([], {"url": "https://www.google.com/sorry/index?continue=x", "title": "x"}))

    def test_an_ordinary_page_is_not(self):
        self.assertFalse(core._human_check([{"name": "Sign in"}, {"name": "Search"}], {"url": "https://example.com", "title": "Home"}))
        # A page that only mentions the technology by its product name.
        self.assertFalse(core._human_check([{"name": "Protected by reCAPTCHA Enterprise terms"}], None))


if __name__ == "__main__":
    unittest.main()
