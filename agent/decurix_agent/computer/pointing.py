"""
A ruler on the screenshot a vision model is shown, so it can say where
something is.

A model that looks at a screenshot does not see the screen's own pixels: the
provider shrinks a large picture before the model gets it (a 1920x1080 screen
arrives as about 1365x768), and a position the model reads off the picture is
in that shrunk picture's terms. Found live: asked to press a box at (100, 248)
the model clicked (68, 171) - seven times - each landing on nothing. No fixed
correction holds: the shrink differs from provider to provider.

So the picture carries its own coordinates. Dotted lines every GRID pixels,
each labelled at the top and left edges with its real screen position: wherever
the picture is shown and however it was resized, the numbers in it are the
numbers `gui click x,y` takes. Only for what has no ref - an element in the
list is still clicked by its ref, which needs no position at all.

Pure Python on the frame's own bytes: a desktop image has no imaging library.
"""

from decurix_agent.computer.screen import Frame

# The distance between ruler lines, in screen pixels.
GRID = 100

# Digits as 3x5 dots, drawn DOT x DOT screen pixels each.
DIGITS = {
    "0": ("###", "# #", "# #", "# #", "###"),
    "1": (" # ", "## ", " # ", " # ", "###"),
    "2": ("###", "  #", "###", "#  ", "###"),
    "3": ("###", "  #", "###", "  #", "###"),
    "4": ("# #", "# #", "###", "  #", "  #"),
    "5": ("###", "#  ", "###", "  #", "###"),
    "6": ("###", "#  ", "###", "# #", "###"),
    "7": ("###", "  #", "  #", "  #", "  #"),
    "8": ("###", "# #", "###", "# #", "###"),
    "9": ("###", "# #", "###", "  #", "###"),
}
DOT = 3

LINE = (255, 0, 200)        # nothing on an ordinary screen is this colour
LABEL_INK = (255, 255, 255)
LABEL_PAPER = (120, 0, 95)


def _label_size(text):
    return len(text) * 4 * DOT + DOT, 7 * DOT


def ruled(frame):
    """A copy of `frame` with the ruler drawn on it."""

    width, height = frame.width, frame.height
    pixels = bytearray(frame.rgb)
    stride = width * 3

    def put(x, y, colour):
        if 0 <= x < width and 0 <= y < height:
            at = y * stride + x * 3
            pixels[at:at + 3] = bytes(colour)

    def box(x, y, w, h, colour):
        x0, x1 = max(0, x), min(width, x + w)
        if x1 <= x0:
            return
        row = bytes(colour) * (x1 - x0)
        for yy in range(max(0, y), min(height, y + h)):
            at = yy * stride + x0 * 3
            pixels[at:at + len(row)] = row

    def label(text, x, y):
        w, h = _label_size(text)
        box(x, y, w, h, LABEL_PAPER)
        for i, ch in enumerate(text):
            for r, line in enumerate(DIGITS[ch]):
                for c, mark in enumerate(line):
                    if mark == "#":
                        box(x + DOT + i * 4 * DOT + c * DOT, y + DOT + r * DOT, DOT, DOT, LABEL_INK)

    # Dotted lines: enough to follow across the screen, not enough to hide what is under them.
    for x in range(GRID, width, GRID):
        for y in range(0, height, 6):
            put(x, y, LINE)
            put(x, y + 1, LINE)
    for y in range(GRID, height, GRID):
        for x in range(0, width, 6):
            put(x, y, LINE)
            put(x + 1, y, LINE)

    # Each line's position, at the top edge and the left edge - and again half
    # way across and down, so a spot far from the edges has a number near it.
    for x in range(GRID, width, GRID):
        text = str(x)
        w, _ = _label_size(text)
        for y in (0, height // 2):
            label(text, x - w // 2, y)
    for y in range(GRID, height, GRID):
        text = str(y)
        _, h = _label_size(text)
        for x in (0, width // 2):
            label(text, x, y - h // 2)

    return Frame(width, height, bytes(pixels))
