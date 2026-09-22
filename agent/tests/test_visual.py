import base64
import unittest

from decurix_agent.computer import visual

WIDTH, HEIGHT = 400, 300


class Frame:
    """The part of screen.Frame a look needs: a size and packed RGB rows."""

    def __init__(self, width, height, rgb):
        self.width = width
        self.height = height
        self.rgb = rgb


def _noise(seed):
    state = seed
    while True:
        state = (state * 1103515245 + 12345) & 0x7FFFFFFF
        yield state >> 16


def canvas(seed=1, flat=False):
    """A quiet background - a window's own colour, with the faint texture of a real one."""

    rgb = bytearray(WIDTH * HEIGHT * 3)
    values = _noise(seed)

    for i in range(WIDTH * HEIGHT):
        v = 120 if flat else 118 + next(values) % 3
        rgb[i * 3:i * 3 + 3] = bytes((v, v, v))

    return rgb


def draw_button(rgb, x, y, w=84, h=34, seed=7, fill=(60, 110, 200)):
    """
    A control with edges and text-like strokes: plenty to recognise it by. A
    different `seed` is different text in the same style.
    """

    values = _noise(seed)

    for j in range(h):
        for i in range(w):
            edge = i in (0, w - 1) or j in (0, h - 1)
            stroke = 6 <= j < h - 6 and (i // 3 + next(values) % 3) % 2 == 0 and 8 <= i < w - 8
            colour = (20, 20, 20) if edge else ((235, 235, 235) if stroke else fill)
            offset = ((y + j) * WIDTH + x + i) * 3
            rgb[offset:offset + 3] = bytes(colour)


def frame_with(*buttons, seed=1):
    rgb = canvas(seed)
    for x, y in buttons:
        draw_button(rgb, x, y)
    return Frame(WIDTH, HEIGHT, bytes(rgb))


def png_of(frame):
    return visual.encode_png(frame.width, frame.height, frame.rgb)


RECORDED = (150, 120)
CLICK = (RECORDED[0] + 42, RECORDED[1] + 17)


class PngTests(unittest.TestCase):

    def test_a_png_this_module_writes_reads_back_the_same(self):
        frame = frame_with(RECORDED)
        width, height, rgb = visual.decode_png(png_of(frame))
        self.assertEqual((width, height), (WIDTH, HEIGHT))
        self.assertEqual(rgb, frame.rgb)

    def test_every_scanline_filter_is_undone(self):
        # A 5x4 RGB image written with each of the five filters in turn.
        import struct
        import zlib

        w, h = 5, 4
        pixels = [[(x * 40 + y * 7, y * 60 + x, (x * y * 13) % 256) for x in range(w)] for y in range(h)]
        rows = [bytes(c for px in row for c in px) for row in pixels]

        def filtered(kind, line, prev):
            out = bytearray()
            for i, byte in enumerate(line):
                a = line[i - 3] if i >= 3 else 0
                b = prev[i]
                c = prev[i - 3] if i >= 3 else 0
                if kind == 0:
                    predictor = 0
                elif kind == 1:
                    predictor = a
                elif kind == 2:
                    predictor = b
                elif kind == 3:
                    predictor = (a + b) >> 1
                else:
                    p = a + b - c
                    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                    predictor = a if pa <= pb and pa <= pc else b if pb <= pc else c
                out.append((byte - predictor) & 255)
            return bytes([kind]) + bytes(out)

        raw, prev = b"", bytes(w * 3)
        for y, line in enumerate(rows):
            raw += filtered(y % 5, line, prev)
            prev = line

        def chunk(tag, body):
            return struct.pack(">I", len(body)) + tag + body + struct.pack(">I", zlib.crc32(tag + body) & 0xFFFFFFFF)

        data = (visual.PNG_SIGNATURE + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))

        self.assertEqual(visual.decode_png(data), (w, h, b"".join(rows)))

    def test_what_it_cannot_read_is_refused_not_guessed_at(self):
        with self.assertRaises(ValueError):
            visual.decode_png(b"not a png at all")


class MakeLookTests(unittest.TestCase):

    def test_a_click_on_a_control_gets_a_look_of_its_surroundings(self):
        look = visual.make_look(png_of(frame_with(RECORDED)), CLICK, [WIDTH, HEIGHT])

        self.assertIsNotNone(look)
        self.assertEqual((look["w"], look["h"]), (visual.LOOK_WIDTH, visual.LOOK_HEIGHT))
        self.assertEqual(look["screen"], [WIDTH, HEIGHT])
        # The click is inside the patch, and the patch is a PNG.
        ax, ay = look["at"]
        self.assertTrue(0 <= ax < look["w"] and 0 <= ay < look["h"])
        self.assertEqual(look["origin"][0] + ax, CLICK[0])
        self.assertTrue(base64.b64decode(look["png"]).startswith(visual.PNG_SIGNATURE))

    def test_a_patch_with_nothing_in_it_is_not_kept(self):
        # A blank margin matches everywhere - a look of it would prove nothing.
        flat = Frame(WIDTH, HEIGHT, bytes(canvas(flat=True)))
        self.assertIsNone(visual.make_look(png_of(flat), (200, 150), [WIDTH, HEIGHT]))

    def test_a_click_at_the_screen_edge_is_kept_inside_it(self):
        look = visual.make_look(png_of(frame_with((0, 0))), (3, 3), [WIDTH, HEIGHT])
        self.assertEqual(look["origin"], [0, 0])
        self.assertEqual(look["at"], [3, 3])

    def test_a_frame_of_another_size_than_recorded_is_not_used(self):
        self.assertIsNone(visual.make_look(png_of(frame_with(RECORDED)), CLICK, [1280, 720]))

    def test_an_unreadable_frame_is_not_used(self):
        self.assertIsNone(visual.make_look(b"garbage", CLICK, None))


class FindTests(unittest.TestCase):

    def setUp(self):
        self.look = visual.make_look(png_of(frame_with(RECORDED)), CLICK, [WIDTH, HEIGHT])

    def test_the_control_where_it_was_recorded_is_found_there(self):
        result = visual.find(self.look, frame_with(RECORDED))
        self.assertEqual(result["status"], visual.FOUND)
        self.assertEqual(result["offset"], (0, 0))
        self.assertEqual(result["point"], CLICK)

    def test_the_same_page_a_little_different_is_still_the_control(self):
        # Another noise seed: the same control on a background that differs the
        # way a hover state or a re-rendered margin does.
        result = visual.find(self.look, frame_with(RECORDED, seed=99))
        self.assertEqual(result["status"], visual.FOUND)
        self.assertEqual(result["offset"], (0, 0))

    def test_a_control_that_moved_is_found_where_it_is_now(self):
        moved = (RECORDED[0] + 31, RECORDED[1] - 22)
        result = visual.find(self.look, frame_with(moved))

        self.assertEqual(result["status"], visual.FOUND)
        self.assertEqual(result["offset"], (31, -22))
        self.assertEqual(result["point"], (CLICK[0] + 31, CLICK[1] - 22))

    def test_a_screen_without_the_control_is_missing_not_clicked_anyway(self):
        result = visual.find(self.look, frame_with())
        self.assertEqual(result["status"], visual.MISSING)
        self.assertIsNone(result["point"])

    def test_a_control_too_far_away_is_missing(self):
        far = (RECORDED[0] + visual.SEARCH + 60, RECORDED[1])
        self.assertEqual(visual.find(self.look, frame_with(far))["status"], visual.MISSING)

    def test_two_places_it_could_be_are_never_guessed_between(self):
        result = visual.find(self.look, frame_with((RECORDED[0] - 60, RECORDED[1] + 10), (RECORDED[0] + 70, RECORDED[1] - 10)))
        self.assertEqual(result["status"], visual.AMBIGUOUS)
        self.assertIsNone(result["point"])
        self.assertGreaterEqual(result["matches"], 2)

    def test_the_recorded_spot_wins_over_a_lookalike_elsewhere(self):
        # As recorded is as recorded: a twin further along does not make it ambiguous.
        result = visual.find(self.look, frame_with(RECORDED, (RECORDED[0] + 80, RECORDED[1] + 40)))
        self.assertEqual(result["status"], visual.FOUND)
        self.assertEqual(result["offset"], (0, 0))

    def test_a_screen_of_another_size_says_nothing(self):
        small = Frame(200, 150, bytes(200 * 150 * 3))
        self.assertEqual(visual.find(self.look, small)["status"], visual.UNVERIFIABLE)

    def test_no_look_or_no_frame_says_nothing(self):
        self.assertEqual(visual.find(None, frame_with(RECORDED))["status"], visual.UNVERIFIABLE)
        self.assertEqual(visual.find(self.look, None)["status"], visual.UNVERIFIABLE)

    def test_a_damaged_look_says_nothing_rather_than_raising(self):
        self.assertEqual(visual.find({"png": "!!!", "w": 1, "h": 1}, frame_with(RECORDED))["status"], visual.UNVERIFIABLE)


def draw_row(rgb, x, y, glyphs, w=140, h=32):
    """
    A bordered row of text: sparse dark glyphs on white, the way a table cell
    is. Each glyph is a 9 x 11 pattern from its own number, so changing one
    number changes one character.
    """

    for j in range(h):
        for i in range(w):
            edge = i in (0, w - 1) or j in (0, h - 1)
            offset = ((y + j) * WIDTH + x + i) * 3
            rgb[offset:offset + 3] = bytes((190, 200, 215) if edge else (255, 255, 255))

    for k, glyph in enumerate(glyphs):
        values = _noise(glyph)
        for j in range(11):
            for i in range(7):
                if next(values) % 3 == 0:
                    offset = ((y + 10 + j) * WIDTH + x + 8 + k * 10 + i) * 3
                    rgb[offset:offset + 3] = bytes((30, 30, 30))


def row_frame(*rows, seed=1):
    rgb = canvas(seed)
    for x, y, glyphs in rows:
        draw_row(rgb, x, y, glyphs)
    return Frame(WIDTH, HEIGHT, bytes(rgb))


def frame_of(*draws, seed=1):
    """A frame with each of `draws` - (x, y, keywords for draw_button) - painted on the quiet background."""

    rgb = canvas(seed)
    for x, y, keywords in draws:
        draw_button(rgb, x, y, **keywords)
    return Frame(WIDTH, HEIGHT, bytes(rgb))


class WhatRealPixelsTaught(unittest.TestCase):
    """
    Each of these failed on real Chromium pixels (bench/recordings/tune.py)
    before it was fixed, and passed on the drawings the first version was tuned
    on. They are the drawings that would have caught it.
    """

    def setUp(self):
        self.look = visual.make_look(png_of(frame_with(RECORDED)), CLICK, [WIDTH, HEIGHT])

    def test_a_control_in_the_same_style_with_other_text_is_not_it(self):
        # "Cancel" where "Save" was: same fill, same size, same edges. A mean
        # brightness difference scored this 11 against a limit of 14.
        other = frame_of((RECORDED[0], RECORDED[1], {"seed": 19}))
        self.assertEqual(visual.find(self.look, other)["status"], visual.MISSING)

    def test_a_change_of_tone_alone_is_still_the_control(self):
        # A hover highlight: the whole button a shade darker, every edge where it was.
        hovered = frame_of((RECORDED[0], RECORDED[1], {"fill": (30, 78, 140)}))
        result = visual.find(self.look, hovered)

        self.assertEqual((result["status"], result["offset"]), (visual.FOUND, (0, 0)))

    def test_a_difference_in_the_last_columns_of_the_window_is_seen(self):
        # The window is not a whole number of blocks wide; the strip left over used
        # to go uncompared, and a trailing letter of a label lived in it.
        ox, oy = self.look["origin"]
        rgb = bytearray(frame_with(RECORDED).rgb)
        for y in range(oy + 6, oy + 40):
            for x in range(ox + self.look["w"] - 7, ox + self.look["w"] - 1):
                offset = (y * WIDTH + x) * 3
                rgb[offset:offset + 3] = bytes((250, 250, 250)) if (y // 3) % 2 else bytes((5, 5, 5))

        self.assertEqual(visual.find(self.look, Frame(WIDTH, HEIGHT, bytes(rgb)))["status"], visual.MISSING)

    def test_a_picture_that_repeats_recognises_nothing(self):
        # The border lines of one row of a menu are under every row of it: a look
        # of them scored a different row as a perfect match.
        rgb = bytearray(canvas())
        for y in (100, 140):
            for x in range(0, WIDTH):
                offset = (y * WIDTH + x) * 3
                rgb[offset:offset + 3] = bytes((180, 190, 205))
        lines = Frame(WIDTH, HEIGHT, bytes(rgb))

        self.assertIsNone(visual.make_look(png_of(lines), (200, 120), [WIDTH, HEIGHT]))

    def test_a_jump_needs_the_same_picture_and_a_row_one_character_off_is_not_it(self):
        # `Invoice 1041 Paid` above `Invoice 1042 Paid`. Found one row up, it would
        # have been clicked for the row that was meant - on real pixels it was.
        text = [11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22]
        recorded = (150, 120)
        click = (recorded[0] + 70, recorded[1] + 16)
        look = visual.make_look(png_of(row_frame((recorded[0], recorded[1], text))), click, [WIDTH, HEIGHT])
        self.assertIsNotNone(look)

        below = (recorded[0], recorded[1] + 60)
        one_off = text[:5] + [99] + text[6:]

        near = visual.find(look, row_frame((below[0], below[1], one_off)))
        self.assertEqual(near["status"], visual.MISSING)
        self.assertGreater(near["closest"], visual.MAX_MISMATCH_MOVED)

        same = visual.find(look, row_frame((below[0], below[1], text)))
        self.assertEqual((same["status"], same["offset"]), (visual.FOUND, (0, 60)))

    def test_a_look_says_when_what_it_shows_runs_off_its_edge(self):
        wide = frame_of((RECORDED[0] - 100, RECORDED[1], {"w": 300}))
        self.assertTrue(visual.make_look(png_of(wide), (RECORDED[0] + 40, RECORDED[1] + 17), [WIDTH, HEIGHT])["partial"])
        self.assertFalse(self.look["partial"])

    def test_how_near_a_miss_was_is_reported(self):
        result = visual.find(self.look, frame_of((RECORDED[0], RECORDED[1], {"seed": 19})))
        self.assertIsNotNone(result["closest"])
        self.assertGreater(result["closest"], visual.MAX_MISMATCH)


class WithTheRealFrame(unittest.TestCase):
    """The agent's own screen.Frame - what a live replay hands find() - not the stand-in above."""

    def real(self, frame):
        from decurix_agent.computer import screen

        return screen.Frame(frame.width, frame.height, bytearray(frame.rgb))

    def test_a_look_is_made_from_the_pngs_the_recorder_writes_and_found_on_a_live_frame(self):
        recorded = self.real(frame_with(RECORDED))
        look = visual.make_look(recorded.png(), CLICK, [WIDTH, HEIGHT])

        self.assertIsNotNone(look)
        self.assertEqual(visual.find(look, self.real(frame_with(RECORDED)))["status"], visual.FOUND)

        moved = visual.find(look, self.real(frame_with((RECORDED[0] + 12, RECORDED[1] + 9))))
        self.assertEqual((moved["status"], moved["offset"]), (visual.FOUND, (12, 9)))


if __name__ == "__main__":
    unittest.main()
