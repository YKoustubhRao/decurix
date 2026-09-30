"""
A point click whose look no longer matches because the run typed something
else: the first result of another search. The replay clicks the same kind of
thing in the same place in order (visual.make_kin / find_kin), and only then.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import sys
import tempfile
import types
import unittest
from pathlib import Path

try:
    from decurix_agent import config  # noqa: F401
except OSError:
    import decurix_agent

    stub = types.ModuleType("decurix_agent.config")
    stub.DESKTOP_ID = "test-box"
    stub.DESKTOP_NAME = "test-box"
    stub.CONTROL_PLANE_URL = "http://127.0.0.1:9"
    stub.AGENT_HARNESS = "ollama"
    sys.modules["decurix_agent.config"] = stub
    decurix_agent.config = stub

from decurix_agent.computer import recording, replay, visual

sys.path.insert(0, str(Path(__file__).parent))
from test_visual import Frame, _noise  # noqa: E402

WIDTH, HEIGHT = 640, 480
LINK = (26, 13, 171)
BODY = (60, 64, 67)
COLUMN = 80
SESSION = "demo-20260923T204006-c28033"


def _text(rgb, x, y, chars, colour, seed, tall=14):
    """A line of text-like glyphs: strokes of `colour` on the page, different words for a different seed."""

    values = _noise(seed)
    for c in range(chars):
        shape = next(values)
        for i in range(8):
            if (shape >> i) & 1 == 0 and i not in (0, 7):
                continue
            for j in range(tall):
                if i in (0, 7) and (shape >> (8 + j % 4)) & 1:
                    continue
                offset = ((y + j) * WIDTH + x + c * 10 + i) * 3
                rgb[offset:offset + 3] = bytes(colour)


def results_page(titles, seed=1, top=120, extra=None):
    """
    A search results page: a search box, then for each title (chars, seed) a
    link-coloured title at COLUMN with two lines of grey text under it.
    """

    rgb = bytearray(b"\xff" * (WIDTH * HEIGHT * 3))
    _text(rgb, COLUMN, 30, 18, BODY, seed)  # what was searched for, in the box
    y = top
    for chars, title_seed in titles:
        _text(rgb, COLUMN, y, chars, LINK, title_seed, tall=16)
        _text(rgb, COLUMN, y + 26, 40, BODY, title_seed + 100, tall=10)
        _text(rgb, COLUMN, y + 42, 32, BODY, title_seed + 200, tall=10)
        y += 90
    if extra:
        extra(rgb)
    return Frame(WIDTH, HEIGHT, bytes(rgb))


RECORDED = results_page([(9, 11), (12, 12), (7, 13)], seed=1, top=170)
FIRST = (COLUMN + 30, 178)
SECOND = (COLUMN + 30, 268)


class KinTests(unittest.TestCase):

    def test_a_click_on_a_link_is_described_as_which_link_it_was(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, FIRST)

        self.assertIsNotNone(kin)
        self.assertEqual((kin["ordinal"], kin["count"]), (1, 3))
        self.assertEqual(kin["left"], COLUMN)
        self.assertEqual(visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, SECOND)["ordinal"], 2)

    def test_the_first_result_of_another_search_is_the_first_link_there(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, FIRST)
        other = results_page([(4, 21), (10, 22)], seed=2, top=110)

        found = visual.find_kin(kin, other)

        self.assertEqual(found["ordinal"], 1)
        self.assertEqual(found["box"][1], 110)
        self.assertEqual(found["point"], (COLUMN + 30, 118))

    def test_a_shorter_title_is_clicked_on_itself_not_past_its_end(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, (COLUMN + 80, 178))
        other = results_page([(3, 21)], seed=2, top=110)

        x, _ = visual.find_kin(kin, other)["point"]
        self.assertTrue(COLUMN <= x < COLUMN + 30, x)

    def test_the_second_link_stays_the_second(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, SECOND)
        other = results_page([(4, 21), (10, 22), (6, 23)], seed=2, top=100)

        self.assertEqual(visual.find_kin(kin, other)["box"][1], 190)

    def test_fewer_links_than_it_was_counted_among_finds_nothing(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, SECOND)
        self.assertIsNone(visual.find_kin(kin, results_page([(4, 21)], seed=2)))

    def test_black_or_grey_text_is_not_described(self):
        self.assertIsNone(visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, (COLUMN + 30, 200)))
        self.assertIsNone(visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, (COLUMN + 30, 36)))

    def test_a_click_on_empty_page_is_not_described(self):
        self.assertIsNone(visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, (500, 400)))

    def test_a_link_in_another_column_or_size_is_not_kin(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, FIRST)

        def elsewhere(rgb):
            _text(rgb, COLUMN + 60, 100, 8, LINK, 31, tall=16)  # another column
            _text(rgb, COLUMN, 140, 8, LINK, 32, tall=8)  # far smaller text

        page = results_page([(4, 21)], seed=2, top=200, extra=elsewhere)
        self.assertEqual(visual.find_kin(kin, page)["box"][1], 200)

    def test_a_coloured_picture_in_the_column_is_not_a_link(self):
        # Found live: John Cena's portrait (a blue background) at the results' column was clicked as "the first link".
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, FIRST)

        def photo(rgb):
            # A picture ending at the column, mostly skin and shadow, with a patch of blue backdrop the size of a title.
            for y in range(40, 160):
                for x in range(COLUMN - 60, COLUMN + 5):
                    shade = (x * 7 + y * 3) % 40
                    blue = 100 <= y < 116 and x >= COLUMN - 1
                    colour = (30 + shade, 20 + shade, 150) if blue else (200 - shade, 150 - shade, 120)
                    rgb[(y * WIDTH + x) * 3:(y * WIDTH + x) * 3 + 3] = bytes(colour)

        self.assertIsNone(visual.find_kin(kin, results_page([], seed=2, extra=photo)))
        self.assertEqual(visual.find_kin(kin, results_page([(4, 21)], seed=2, top=200, extra=photo))["box"][1], 200)

    def test_a_screen_of_another_size_finds_nothing(self):
        kin = visual.make_kin(RECORDED.rgb, WIDTH, HEIGHT, FIRST)
        self.assertIsNone(visual.find_kin(kin, Frame(320, 240, bytes(320 * 240 * 3))))


class Screen:
    """core, as a replay of a coordinate click sees it: a screen and a log of what was done."""

    def __init__(self, frame):
        self.frame = frame
        self.acted = []

    def observe(self, remember=True):
        return {"elements": [], "windows": [{"title": "Search"}], "active_title": "Search",
                "size": [WIDTH, HEIGHT], "frame": self.frame}

    def active_group(self, observation):
        return "g"

    def act(self, kind, **kwargs):
        self.acted.append((kind, kwargs))
        return {"status": "done", "text": "ok", "observation": self.observe()}


class ReplayTests(unittest.TestCase):
    """Type a search, press Enter, click the first result - recorded once, run for another search."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        recording.ROOT = Path(self.tmp.name)
        replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = 0.3, 0, 0

        recording.append(SESSION, {"kind": "type", "actor": "human", "text": "koustubh is great", "window": "Search",
                                   "outcome": "done"})
        recording.append(SESSION, {"kind": "key", "actor": "human", "keys": ["Return"], "window": "Search",
                                   "outcome": "done"})
        recording.append(SESSION, {"kind": "click", "actor": "human", "point": list(FIRST), "window": "Search",
                                   "outcome": "done"}, before=visual.encode_png(WIDTH, HEIGHT, RECORDED.rgb))
        recording.update(SESSION, title='google "koustubh is great"')
        self.meta, self.events = recording.load(SESSION)

    def tearDown(self):
        recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved
        self.tmp.cleanup()

    def run_on(self, frame, params=None, program=None, lines=None):
        fake = Screen(frame)
        replay.core = fake
        program = program or replay.compile_program(self.meta, self.events)
        return fake, replay.run(program, params=params, report=(lines.append if lines is not None else lambda _l: None))

    def clicks(self, fake):
        return [kwargs.get("point") for kind, kwargs in fake.acted if kind == "click"]

    def test_another_search_clicks_its_first_result(self):
        lines = []
        fake, result = self.run_on(results_page([(4, 21), (10, 22)], seed=2, top=110),
                                   params={"text": "who is rama"}, lines=lines)

        self.assertEqual(self.clicks(fake), [(COLUMN + 30, 118)], result["text"])
        self.assertTrue(any("1st line of text in its colour and size" in line for line in lines), lines)

    def test_the_same_search_on_a_changed_page_still_halts(self):
        # Nothing typed differs, so a look that fails means the page is not what was recorded.
        fake, result = self.run_on(results_page([(4, 21), (10, 22)], seed=2, top=110))

        self.assertEqual(self.clicks(fake), [])
        self.assertIn("does not click there", result["text"])

    def test_no_result_of_that_kind_still_halts(self):
        fake, result = self.run_on(results_page([], seed=2), params={"text": "who is rama"})

        self.assertEqual(self.clicks(fake), [])
        self.assertIn("does not click there", result["text"])

    def test_a_consequential_step_is_never_clicked_by_kind(self):
        program = replay.compile_program(self.meta, self.events)
        program["steps"][2]["consequential"] = True

        fake, _ = self.run_on(results_page([(4, 21)], seed=2, top=110), params={"text": "who is rama"},
                              program=program)
        self.assertEqual(self.clicks(fake), [])

    def test_a_skill_whose_recording_is_still_on_disk_finds_the_kind_from_its_frame(self):
        # A skill keeps its steps, not the frames; the kind is read from the recording it names.
        program = replay.compile_program(self.meta, self.events)
        steps = [dict(s, session=SESSION) for s in program["steps"]]
        skill_program = {"session": "skill-jukilo", "steps": steps,
                         "params": [dict(program["params"][0], default=None, example="koustubh is great")]}

        fake, result = self.run_on(results_page([(4, 21)], seed=2, top=110), params={"text": "who is rama"},
                                   program=skill_program)
        self.assertEqual(self.clicks(fake), [(COLUMN + 30, 118)], result["text"])


if __name__ == "__main__":
    unittest.main()
