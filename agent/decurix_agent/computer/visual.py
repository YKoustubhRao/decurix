import base64
import struct
import zlib


# ============================================================
# Visual anchors: the "template" rung of openadapt-flow's ladder
#
# openadapt-flow finds a step's target by structure first, then by a picture of
# it (template matching), then by its text, and only then by coordinates. A
# step recorded without an element - a click on a canvas, on LibreOffice's
# document, in an app that exposes nothing - had only the last of those: it
# clicked where it was recorded, whatever was there now.
#
# A look is a small picture of what was around such a click when it was
# recorded, kept in the step (a skill outlives the recordings it came from, so
# it cannot point at a frame on disk). Before the click, the replay checks the
# screen still shows it - at the recorded spot, or, failing that, nearby - and
# never clicks on a hope:
#
#   found        the picture is where it was recorded, or within SEARCH pixels
#                of it and nowhere else: click there (the offset is reported)
#   missing      it is nowhere near: halt, the way an element that is gone does
#   ambiguous    it is in more than one place: halt, a replay does not guess
#   unverifiable the picture says nothing (a flat patch of colour), or the
#                screen is not the size it was recorded on: nothing can be
#                promised either way, so the step runs as it always did
#
# No numpy and no imaging library: a desktop's agent has only the standard
# library. What keeps it fast enough is that the common case is one comparison
# at the recorded spot, and the search - coarse on a 4x smaller picture, then
# refined at full size - only happens when that fails.
# ============================================================

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"

LOOK_WIDTH = 144
LOOK_HEIGHT = 48

# How far from where it was recorded a control may have moved and still be
# taken for the same one (a window dragged, a page scrolled a little).
SEARCH = 96

# What is compared is structure, not tone. A patch is turned into a gradient
# map (how sharply each pixel differs from its neighbours) and judged in CELL x
# CELL blocks. Measured on real Chromium pixels, a mean brightness difference
# cannot tell "Save" from "Cancel" in the same style - the words are a small
# share of the patch, and it scored them 7-11 against a threshold that a hover
# highlight (a whole button a shade darker) also had to fit under. A shade
# changes no edge; a different word breaks every cell it touches.
CELL = 8
STRIDE = CELL // 2

# A block of the picture is compared only if either side has something in it:
# its mean gradient, per pixel.
ENERGY = 6.0

# How wide a strip at the picture's edge is looked at to see whether what is in
# it runs off the side.
EDGE = 3

# An edge strip narrower than this is too little to judge on.
MIN_AREA = 16

# Both sides are blurred a touch (a 2 x 2 average) before their gradients are
# taken, so that a picture resampled by a fraction of a pixel - text
# re-antialiased where a page has settled after scrolling - still matches.
SMOOTH = True

# A block differs when its gradients differ by more than this per pixel and by
# more than this share of the stronger side's (a bright edge tolerates more).
CELL_DIFF = 10.0
CELL_SHARE = 0.5

# The share of compared blocks that may differ for it to be the same control,
# where it was recorded: a caret that blinked, a hover, are a block or two.
MAX_MISMATCH = 0.02

# Anywhere else it must be all but identical. The further from where it was
# recorded, the less a near-match proves: on real pixels the row above
# `Invoice 1042 Paid` read `Invoice 1041 Paid` - one digit - and was found for
# it. That one digit is 0.018 of its blocks in Chromium 112's rendering and
# 0.012 in Chrome 154's; what is rightly accepted there scored 0.006 at most
# (Chromium, a row reflowed onto a fractional pixel) and 0.0 on Chrome. The
# limit is the middle of the narrower gap, 0.006..0.012, measured with
# bench/recordings/tune.py on a corpus built in each browser. A false halt is
# put to a person; a click on the wrong row is not undone.
MAX_MISMATCH_MOVED = 0.009

# A patch with fewer blocks that have anything in them cannot be recognised
# (a blank margin matches everywhere). Blocks overlap, so one small feature
# is several of them.
MIN_CELLS = 8

# How far a picture is slid across itself to ask whether it is one place or
# any of several (parallel lines, a ruled table, a repeated texture): if it
# still looks the same slid this far, it recognises nothing.
SELF_SHIFTS = ((8, 0), (16, 0), (24, 0), (0, 8), (0, 16))

# A second place that mismatches by less than this more than the best one
# makes the answer ambiguous.
AMBIGUOUS_MARGIN = 0.06

# The search first looks at the gradient's energy pooled into COARSE x COARSE
# cells, at more than one phase (a control rarely sits on the grid: a version
# that block-averaged the picture and took its gradient scored a control 2 px
# off the grid worse than places that were nothing like it, and missed it). It
# only nominates places - on real pixels the right one scored 0-3 and the
# nearest wrong one 12 - whether one is the control is decided block by block.
COARSE = 4
PHASES = (0, COARSE // 2)
COARSE_CAP = 8.0

# Then each nominated place is aligned at full size, on the gradients.
ALIGN_CAP = 28.0

# How many separate places the search looks at closely.
MAX_PLACES = 8

FOUND = "found"
MISSING = "missing"
AMBIGUOUS = "ambiguous"
UNVERIFIABLE = "unverifiable"


# ============================================================
# PNG, from the standard library
# ============================================================

def encode_png(width, height, rgb):
    """Packed 8-bit RGB rows as a PNG."""

    stride = width * 3
    raw = b"".join(b"\x00" + bytes(rgb[y * stride:(y + 1) * stride]) for y in range(height))
    return png_of_rows(width, height, raw, 6)


def png_of_rows(width, height, raw, level):
    """A PNG of 8-bit RGB rows that already carry their filter byte, compressed at zlib `level`."""

    def chunk(tag, body):
        return struct.pack(">I", len(body)) + tag + body + struct.pack(">I", zlib.crc32(tag + body) & 0xFFFFFFFF)

    return (
        PNG_SIGNATURE
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw, level))
        + chunk(b"IEND", b"")
    )


def decode_png(data):
    """
    (width, height, packed RGB) of a non-interlaced 8-bit PNG (grey, RGB or
    RGBA). Raises ValueError for anything else - every recording Decurix has
    written is one of these.
    """

    if data[:8] != PNG_SIGNATURE:
        raise ValueError("not a PNG")

    pos, idat, header = 8, [], None

    while pos + 8 <= len(data):
        length, tag = struct.unpack(">I4s", data[pos:pos + 8])
        body = data[pos + 8:pos + 8 + length]
        pos += 12 + length

        if tag == b"IHDR":
            header = struct.unpack(">IIBBBBB", body)
        elif tag == b"IDAT":
            idat.append(body)
        elif tag == b"IEND":
            break

    if header is None or not idat:
        raise ValueError("a PNG with no image in it")

    width, height, depth, colour, _, _, interlace = header
    channels = {0: 1, 2: 3, 6: 4}.get(colour)

    if depth != 8 or channels is None or interlace:
        raise ValueError("a PNG this reader does not handle")

    raw = zlib.decompress(b"".join(idat))
    stride = width * channels

    if len(raw) < height * (stride + 1):
        raise ValueError("a PNG cut short")

    out = bytearray(width * height * 3)
    previous = bytearray(stride)

    for y in range(height):
        base = y * (stride + 1)
        kind = raw[base]
        line = bytearray(raw[base + 1:base + 1 + stride])

        if kind == 1:
            for i in range(channels, stride):
                line[i] = (line[i] + line[i - channels]) & 255
        elif kind == 2:
            for i in range(stride):
                line[i] = (line[i] + previous[i]) & 255
        elif kind == 3:
            for i in range(stride):
                left = line[i - channels] if i >= channels else 0
                line[i] = (line[i] + ((left + previous[i]) >> 1)) & 255
        elif kind == 4:
            for i in range(stride):
                a = line[i - channels] if i >= channels else 0
                b = previous[i]
                c = previous[i - channels] if i >= channels else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                line[i] = (line[i] + (a if pa <= pb and pa <= pc else b if pb <= pc else c)) & 255
        elif kind != 0:
            raise ValueError("a PNG with an unknown filter")

        row = out[y * width * 3:(y + 1) * width * 3]
        if channels == 3:
            row[:] = line
        elif channels == 4:
            row[0::3], row[1::3], row[2::3] = line[0::4], line[1::4], line[2::4]
        else:
            row[0::3] = row[1::3] = row[2::3] = line

        out[y * width * 3:(y + 1) * width * 3] = row
        previous = line

    return width, height, bytes(out)


# ============================================================
# Pixels
# ============================================================

def _luma_rows(rgb, width, x0, y0, x1, y1):
    """The luma (0-255) of a rectangle, as a list of rows."""

    rows = []

    for y in range(y0, y1):
        line = rgb[(y * width + x0) * 3:(y * width + x1) * 3]
        rows.append([(30 * r + 59 * g + 11 * b) // 100 for r, g, b in zip(line[0::3], line[1::3], line[2::3])])

    return rows


def _gradient(rows):
    """How sharply each pixel differs from the next one across and down: (h-1) rows of (w-1)."""

    out = []

    for y in range(len(rows) - 1):
        row, below = rows[y], rows[y + 1]
        out.append([abs(row[x + 1] - row[x]) + abs(below[x] - row[x]) for x in range(len(row) - 1)])

    return out


def _structure(luma):
    """A picture's gradient map, from its luma, blurred first when SMOOTH."""

    if SMOOTH:
        luma = [[(a + b + c + d) // 4 for a, b, c, d in zip(row, row[1:], below, below[1:])]
                for row, below in zip(luma, luma[1:])]

    return _gradient(luma)


def _cells(height, width):
    """
    The (row range, column range) of each CELL x CELL block of a map, taken
    every STRIDE pixels so the blocks overlap - and the strips at the right and
    bottom edges included.

    Both matter, and both were found on real pixels. Blocks that tile the map
    split a glyph-sized change across neighbours, each holding too little of
    it to count, so a whole letter could go unseen; and a window that is not a
    whole number of blocks wide left its last few columns uncompared, where a
    label's last letter lived.
    """

    return [(range(y0, min(y0 + CELL, height)), (x0, min(x0 + CELL, width)))
            for y0 in range(0, height, STRIDE) for x0 in range(0, width, STRIDE)
            if height - y0 >= STRIDE and width - x0 >= STRIDE]


def _energy_cells(gradient):
    """How many blocks of a gradient map have something in them."""

    count = 0

    for rows, (x0, x1) in _cells(len(gradient), len(gradient[0]) if gradient else 0):
        area = len(rows) * (x1 - x0)
        if area >= MIN_AREA and sum(sum(gradient[y][x0:x1]) for y in rows) / area >= ENERGY:
            count += 1

    return count


def _mismatch(template, gradient, ox, oy):
    """
    The share of blocks, of those either side has anything in, whose structure
    differs between a template's gradient map and the part of `gradient` at
    (ox, oy). 0 is the same picture; a different word is well over MAX_MISMATCH.
    """

    considered = differing = 0

    for rows, (x0, x1) in _cells(len(template), len(template[0])):
        t_sum = g_sum = d_sum = m_sum = 0

        for y in rows:
            wanted = template[y][x0:x1]
            got = gradient[oy + y][ox + x0:ox + x1]
            t_sum += sum(wanted)
            g_sum += sum(got)
            d_sum += sum(abs(a - b) for a, b in zip(wanted, got))
            m_sum += sum(max(a, b) for a, b in zip(wanted, got))

        area = len(rows) * (x1 - x0)
        if area < MIN_AREA or (t_sum / area < ENERGY and g_sum / area < ENERGY):
            continue

        considered += 1
        if d_sum / area > CELL_DIFF and d_sum > CELL_SHARE * m_sum:
            differing += 1

    return differing / considered if considered else 1.0


def _runs_off(gradient):
    """Whether structure reaches the picture's left or right edge, cut by the window."""

    height, width = len(gradient), len(gradient[0])

    for columns in (range(EDGE), range(width - EDGE, width)):
        if sum(gradient[y][x] for y in range(height) for x in columns) / (height * EDGE) >= ENERGY:
            return True

    return False


def _distinct(gradient):
    """
    Whether a picture is recognisable: slid across itself it never looks the
    same. Two border lines have plenty of structure and identify nothing - the
    same lines are under every row of a menu, and a look of them scored a
    different row as a perfect match.
    """

    height, width = len(gradient), len(gradient[0])

    for dx, dy in SELF_SHIFTS:
        if width - dx < CELL or height - dy < CELL:
            continue

        cropped = [row[:width - dx] for row in gradient[:height - dy]]
        if _mismatch(cropped, gradient, dx, dy) <= MAX_MISMATCH:
            return False

    return True


def _difference(template, region, ox, oy, cap):
    """
    Mean absolute difference between `template` and the `region` at (ox, oy),
    or None as soon as it is certain to exceed `cap`.
    """

    count = len(template) * len(template[0])
    budget = cap * count
    total = 0

    for ty, wanted in enumerate(template):
        got = region[oy + ty][ox:ox + len(wanted)]
        total += sum(abs(a - b) for a, b in zip(wanted, got))
        if total > budget:
            return None

    return total / count


def _pool(gradient, k, px=0, py=0):
    """The mean of each k x k block of a gradient map, the grid starting at (px, py)."""

    height = (len(gradient) - py) // k
    width = (len(gradient[0]) - px) // k
    area = k * k
    out = []

    for cy in range(height):
        band = gradient[py + cy * k:py + (cy + 1) * k]
        out.append([sum(sum(row[px + cx * k:px + (cx + 1) * k]) for row in band) // area for cx in range(width)])

    return out


# ============================================================
# Making a look, and finding it again
# ============================================================

def make_look(png, point, screen=None):
    """
    A look at a click's surroundings, from the frame it was recorded on (PNG
    bytes): {"png", "w", "h", "at", "origin", "screen", "detail"}. None when the
    frame cannot be read, is not the size the recording says, or the patch
    has too little in it to recognise - nothing, or only what repeats.
    """

    try:
        width, height, rgb = decode_png(png)
    except (ValueError, zlib.error, struct.error):
        return None

    if screen and list(screen) != [width, height]:
        return None
    if width < LOOK_WIDTH or height < LOOK_HEIGHT or not point:
        return None

    px, py = int(point[0]), int(point[1])
    if not (0 <= px < width and 0 <= py < height):
        return None

    x0 = min(max(px - LOOK_WIDTH // 2, 0), width - LOOK_WIDTH)
    y0 = min(max(py - LOOK_HEIGHT // 2, 0), height - LOOK_HEIGHT)

    patch = bytearray()
    for y in range(y0, y0 + LOOK_HEIGHT):
        patch += rgb[(y * width + x0) * 3:(y * width + x0 + LOOK_WIDTH) * 3]

    gradient = _structure(_luma_rows(patch, LOOK_WIDTH, 0, 0, LOOK_WIDTH, LOOK_HEIGHT))
    cells = _energy_cells(gradient)
    if cells < MIN_CELLS or not _distinct(gradient):
        return None

    # Whether what is in the picture runs off its left or right edge (a label
    # longer than the window, a wide button): a control that differs only
    # beyond it would look the same, and whoever reads a step's checks should be
    # told.
    partial = _runs_off(gradient)

    return {
        "png": base64.b64encode(encode_png(LOOK_WIDTH, LOOK_HEIGHT, patch)).decode("ascii"),
        "w": LOOK_WIDTH,
        "h": LOOK_HEIGHT,
        "at": [px - x0, py - y0],
        "origin": [x0, y0],
        "screen": [width, height],
        "detail": cells,
        "partial": partial,
    }


def _line_digits(width, height, rgb, at):
    """The digit runs of the line of text at `at` in a picture, by OCR; None when it cannot be read."""

    from decurix_agent.computer import identity, ocr

    if not ocr.available():
        return None
    try:
        line = ocr.line_at(ocr.read(width, height, bytes(rgb)), at)
    except Exception:  # noqa: BLE001 - not read is not known
        return None
    return identity.digit_runs(line["text"]) if line else []


def digits_hold(look, frame, point):
    """
    Whether the place a look was found at reads the same numbers as the look itself: True (the same digit runs, or the
    look shows none), False (other numbers - another record that only looks alike), None (it cannot be told: no OCR,
    or nothing readable). Found in review (2026-11-03, D-15): a row one digit off ("Invoice 1041" for "Invoice 1042")
    scores under MAX_MISMATCH, so a look alone took it. A look with digits is a candidate until a text read confirms
    them; no threshold is tuned for it.
    """

    try:
        width, height, rgb = decode_png(base64.b64decode(look["png"]))
        recorded = _line_digits(width, height, rgb, look["at"])
        if recorded is None:
            return None
        if not recorded:
            return True
        x0, y0 = int(point[0]) - look["at"][0], int(point[1]) - look["at"][1]
        if x0 < 0 or y0 < 0 or x0 + width > frame.width or y0 + height > frame.height:
            return None
        patch = bytearray()
        for y in range(y0, y0 + height):
            patch += bytes(frame.rgb[(y * frame.width + x0) * 3:(y * frame.width + x0 + width) * 3])
        now = _line_digits(width, height, patch, look["at"])
    except Exception:  # noqa: BLE001 - not read is not known
        return None
    if now is None or not now:
        return None
    return now == recorded


def _template(look):
    """(luma rows, gradient map) of a look's picture."""

    width, height, rgb = decode_png(base64.b64decode(look["png"]))
    luma = _luma_rows(rgb, width, 0, 0, width, height)
    return luma, _structure(luma)


def _result(status, point=None, offset=None, score=None, matches=0, closest=None):
    """`closest` is the smallest share of blocks that differed at any place looked at - how near a miss was."""

    return {"status": status, "point": point, "offset": offset, "score": score, "matches": matches,
            "closest": closest if closest is not None else score}


def find(look, frame):
    """
    Where a look is on `frame` (a screen.Frame): {"status", "point", "offset",
    "score", "matches"}. `point` is where the recorded click belongs now and
    `score` the share of its blocks that differ (0 is identical).
    """

    if not look or frame is None:
        return _result(UNVERIFIABLE)

    try:
        _, template = _template(look)
        tw, th = look["w"], look["h"]
        ox, oy = look["origin"]
        ax, ay = look["at"]
        recorded = list(look.get("screen") or [])
    except (ValueError, zlib.error, struct.error, KeyError, TypeError):
        return _result(UNVERIFIABLE)

    width, height = frame.width, frame.height
    if recorded != [width, height]:
        return _result(UNVERIFIABLE)

    # The healthy case: it is where it was recorded.
    window = _structure(_luma_rows(frame.rgb, width, ox, oy, ox + tw, oy + th))
    here = _mismatch(template, window, 0, 0)
    if here <= MAX_MISMATCH:
        return _result(FOUND, (ox + ax, oy + ay), (0, 0), round(here, 3), 1)

    rx0, ry0 = max(0, ox - SEARCH), max(0, oy - SEARCH)
    rx1, ry1 = min(width, ox + tw + SEARCH), min(height, oy + th + SEARCH)
    region = _structure(_luma_rows(frame.rgb, width, rx0, ry0, rx1, ry1))

    coarse_template = _pool(template, COARSE)
    if not coarse_template or not coarse_template[0]:
        return _result(MISSING, closest=round(here, 3))

    ctw, cth = len(coarse_template[0]), len(coarse_template)
    candidates = []

    for py in PHASES:
        for px in PHASES:
            pooled = _pool(region, COARSE, px, py)

            for cy in range(len(pooled) - cth + 1):
                for cx in range(len(pooled[0]) - ctw + 1):
                    score = _difference(coarse_template, pooled, cx, cy, COARSE_CAP)
                    if score is not None:
                        candidates.append((score, cx * COARSE + px, cy * COARSE + py))

    # One candidate per place: the best of each cluster of neighbours.
    candidates.sort()
    places = []
    for candidate in candidates:
        if all(abs(candidate[1] - p[1]) >= tw // 2 or abs(candidate[2] - p[2]) >= th // 2 for p in places):
            places.append(candidate)
        if len(places) >= MAX_PLACES:
            break

    judged = []
    for _, cx, cy in places:
        # Align at full size on the gradients, then judge that alignment block by block.
        best = None
        for dy in range(-COARSE // 2 - 1, COARSE // 2 + 2):
            for dx in range(-COARSE // 2 - 1, COARSE // 2 + 2):
                x, y = cx + dx, cy + dy
                if x < 0 or y < 0 or x + len(template[0]) > len(region[0]) or y + len(template) > len(region):
                    continue
                score = _difference(template, region, x, y, best[0] if best else ALIGN_CAP)
                if score is not None and (best is None or score < best[0]):
                    best = (score, x, y)

        if best is not None:
            judged.append((_mismatch(template, region, best[1], best[2]), best[1], best[2]))

    closest = min([here] + [j[0] for j in judged])
    same = sorted(j for j in judged if j[0] <= MAX_MISMATCH_MOVED)
    if not same:
        return _result(MISSING, closest=round(closest, 3))

    share, x, y = same[0]
    rivals = [j for j in same[1:] if j[0] <= share + AMBIGUOUS_MARGIN]

    if rivals:
        return _result(AMBIGUOUS, score=round(share, 3), matches=1 + len(rivals))

    fx, fy = rx0 + x, ry0 + y
    return _result(FOUND, (fx + ax, fy + ay), (fx - ox, fy - oy), round(share, 3), 1)


# ============================================================
# Kin: the same kind of thing, when what it says has changed
#
# A look fails, correctly, when what was clicked now reads differently. That
# is exactly what a search does: recorded as `koustubh is great`, the first
# result's title was "Kaustubha" at 352,544; searched for `who is rama`, the
# first result is "Rama", 58 px higher. The person meant "the first result",
# and nothing in the pixels of "Kaustubha" says so.
#
# What the frame does say is what *kind* of thing it was: a line of text in a
# link's colour, this tall, starting at this column - and which of those it
# was, counted from the top of the screen. A kin is that description:
#
#   {"ink": [r, g, b], "left": x, "top": y, "height": h, "ordinal": k,
#    "count": n, "at": [dx, dy]}
#
# and find_kin() finds the k-th such line on the screen now. Only coloured
# ink (a link, a highlighted label) is described: body text is black or grey
# everywhere, and "the third black line" means nothing. The replay only asks
# for a kin when a value it typed has changed (replay.resolve), so a look that
# fails for any other reason still halts exactly as before.
# ============================================================

# The ink must be a colour, not black, grey or white: max - min channel.
KIN_MIN_CHROMA = 60
# A pixel of the ink: at least this coloured, within this hue (degrees) and brightness of the ink. JPEG and
# anti-aliasing smear thin text, so these are loose; what they must not let in is black or grey text.
KIN_PIXEL_CHROMA = 40
KIN_HUE = 25
KIN_LUMA = 70
# A pixel this far from the background around the click is ink, not the background.
KIN_CONTRAST = 110
# Fewer pixels of the ink than this around the click: there is no text of that colour there.
KIN_MIN_INK = 12
# How far a kin's left edge may be from the recorded one: the same column.
KIN_COLUMN = 6
# A kin's height as a share of the recorded one's: descenders ("gyp") and a bold title change it; another text size does not fit.
KIN_HEIGHT = (0.7, 1.45)
# Gaps inside one line of text: between letters and words across, between an accent and its letter down.
KIN_GAP_X = 12
KIN_GAP_Y = 2
# Shorter than this is an icon or a rule, not a line of text.
KIN_MIN_HEIGHT = 8
# How far from the text a click may land and still be a click on it.
KIN_REACH = 8
# The widest a line of text is followed across.
KIN_SPAN = 700
# A line of text is at least this many times as wide as it is tall (a few letters): a sliver of a coloured photo is not.
KIN_MIN_ASPECT = 1.5
# Text stands on the page: this share of the pixels just above and just below it must be one plain colour, not the ink.
# Found live: a blue photo on a search results page (John Cena's portrait) was taken for "the first link", and clicked.
KIN_PLAIN_GROUND = 0.7


def _pixel(rgb, width, x, y):
    i = (y * width + x) * 3
    return rgb[i], rgb[i + 1], rgb[i + 2]


def _near(colour, ink, limit):
    return (colour[0] - ink[0]) ** 2 + (colour[1] - ink[1]) ** 2 + (colour[2] - ink[2]) ** 2 <= limit * limit


def _hue(colour):
    r, g, b = colour
    hi, lo = max(colour), min(colour)
    if hi == lo:
        return 0.0
    d = hi - lo
    if hi == r:
        return (60.0 * (g - b) / d) % 360
    if hi == g:
        return 60.0 * (b - r) / d + 120
    return 60.0 * (r - g) / d + 240


def _luma(colour):
    return (30 * colour[0] + 59 * colour[1] + 11 * colour[2]) // 100


def _inker(ink):
    """
    Whether a pixel is the ink: the same hue, still clearly coloured, and about
    as dark. Hue rather than distance, because anti-aliasing blends a thin
    letter with the background (a lighter blue is still blue) while dark teal
    text on a pale highlight is close to a link's blue in RGB and is not one.
    """

    hue, luma = _hue(ink), _luma(ink)

    def is_ink(r, g, b):
        hi, lo = max(r, g, b), min(r, g, b)
        if hi - lo < KIN_PIXEL_CHROMA or abs(_luma((r, g, b)) - luma) > KIN_LUMA:
            return False
        d = abs(_hue((r, g, b)) - hue)
        return min(d, 360 - d) <= KIN_HUE

    return is_ink


def _ink_colour(rgb, width, height, px, py):
    """The colour of the text around a click, when it is a colour (a link): [r, g, b], or None."""

    x0, x1 = max(0, px - 40), min(width, px + 41)
    y0, y1 = max(0, py - 14), min(height, py + 15)

    counts = {}
    pixels = []
    for y in range(y0, y1):
        for x in range(x0, x1):
            c = _pixel(rgb, width, x, y)
            pixels.append(c)
            key = (c[0] >> 3, c[1] >> 3, c[2] >> 3)
            counts[key] = counts.get(key, 0) + 1
    if not pixels:
        return None

    bk = max(counts, key=counts.get)
    background = (bk[0] * 8 + 4, bk[1] * 8 + 4, bk[2] * 8 + 4)

    # The letters' cores: coloured, and the furthest from the background. Their edges are a blend of the two.
    ink = [c for c in pixels if not _near(c, background, KIN_CONTRAST) and max(c) - min(c) >= KIN_MIN_CHROMA]
    if len(ink) < KIN_MIN_INK:
        return None
    ink.sort(key=lambda c: -((c[0] - background[0]) ** 2 + (c[1] - background[1]) ** 2 + (c[2] - background[2]) ** 2))
    core = ink[:max(KIN_MIN_INK, len(ink) // 3)]

    # One colour, not two side by side: most of the ink must be that colour's hue.
    colour = [sorted(c[i] for c in core)[len(core) // 2] for i in range(3)]
    is_ink = _inker(colour)
    if sum(1 for c in ink if is_ink(*c)) < 0.6 * len(ink):
        return None
    return colour


def _band(rgb, width, x0, x1, y, is_ink):
    """Whether any pixel of row y between x0 and x1 is the ink."""

    row = y * width
    for x in range(max(0, x0), min(width, x1)):
        i = (row + x) * 3
        if is_ink(rgb[i], rgb[i + 1], rgb[i + 2]):
            return True
    return False


def _item_at(rgb, width, height, px, py, ink):
    """The box (left, top, right, bottom) of the line of text in `ink` a click at px,py landed on, or None."""

    is_ink = _inker(ink)
    rows = [y for y in range(max(0, py - KIN_REACH), min(height, py + KIN_REACH + 1))
            if _band(rgb, width, px - KIN_REACH, px + KIN_REACH + 1, y, is_ink)]
    if not rows:
        return None

    seed = min(rows, key=lambda y: abs(y - py))
    reach_x0, reach_x1 = px - 40, px + 41

    def last_row(ys):
        edge, gap = seed, 0
        for y in ys:
            if _band(rgb, width, reach_x0, reach_x1, y, is_ink):
                edge, gap = y, 0
            else:
                gap += 1
                if gap > KIN_GAP_Y:
                    break
        return edge

    top = last_row(range(seed - 1, max(-1, seed - 60), -1))
    bottom = last_row(range(seed + 1, min(height, seed + 60)))

    def column(x):
        for y in range(top, bottom + 1):
            if is_ink(*_pixel(rgb, width, x, y)):
                return True
        return False

    left = right = None
    gap = 0
    for x in range(px + KIN_REACH, max(-1, px - KIN_SPAN), -1):
        if x >= width:
            continue
        if column(x):
            left, gap = x, 0
            if right is None:
                right = x
        elif left is not None:
            gap += 1
            if gap > KIN_GAP_X:
                break
    if left is None:
        return None
    gap = 0
    for x in range(right + 1, min(width, px + KIN_SPAN)):
        if column(x):
            right, gap = x, 0
        else:
            gap += 1
            if gap > KIN_GAP_X:
                break

    if not (left - KIN_REACH <= px <= right + KIN_REACH):
        return None
    return left, top, right, bottom


def _looks_like_text(rgb, width, height, box, is_ink):
    """Whether a run of ink is a line of text: wide enough, and on a plain ground rather than inside a picture."""

    left, top, right, bottom = box
    tall = bottom - top + 1
    if right - left + 1 < KIN_MIN_ASPECT * tall:
        return False
    counts = {}
    total = 0
    for y in (top - 2, bottom + 2):
        if not 0 <= y < height:
            continue
        for x in range(max(0, left), min(width, right + 1)):
            c = _pixel(rgb, width, x, y)
            key = (c[0] >> 3, c[1] >> 3, c[2] >> 3)
            counts[key] = counts.get(key, 0) + 1
            total += 1
    if not total:
        return False
    key = max(counts, key=counts.get)
    if counts[key] < KIN_PLAIN_GROUND * total:
        return False
    return not is_ink(key[0] * 8 + 4, key[1] * 8 + 4, key[2] * 8 + 4)


def _text_items(rgb, width, height, left, ink):
    """_column_items that are lines of text (_looks_like_text), each with its whole box: [(item, box)]."""

    is_ink = _inker(ink)
    out = []
    for item in _column_items(rgb, width, height, left, ink):
        x0, top, _, bottom = item
        box = _item_at(rgb, width, height, x0 + 1, (top + bottom) // 2, ink) or item
        if _looks_like_text(rgb, width, height, box, is_ink):
            out.append((item, box))
    return out


def _column_items(rgb, width, height, left, ink):
    """
    Every line of text in `ink` that starts at column `left` (within
    KIN_COLUMN), top to bottom: [(left, top, right_in_strip, bottom)].
    """

    is_ink = _inker(ink)
    x0, x1 = max(0, left - KIN_COLUMN - KIN_GAP_X), min(width, left + KIN_COLUMN + 40)
    items = []
    top = last = None
    for y in range(height + KIN_GAP_Y + 1):
        hit = y < height and _band(rgb, width, x0, x1, y, is_ink)
        if hit:
            if top is None:
                top = y
            last = y
        elif top is not None and y - last > KIN_GAP_Y:
            items.append((top, last))
            top = None

    out = []
    for t, b in items:
        xs = [x for x in range(x0, x1) for y in range(t, b + 1) if is_ink(*_pixel(rgb, width, x, y))]
        if xs and abs(min(xs) - left) <= KIN_COLUMN:
            out.append((min(xs), t, max(xs), b))
    return out


def make_kin(rgb, width, height, point):
    """What kind of thing a click at `point` was on a frame, as a kin (see above), or None."""

    if not point:
        return None
    px, py = int(point[0]), int(point[1])
    if not (0 <= px < width and 0 <= py < height):
        return None

    ink = _ink_colour(rgb, width, height, px, py)
    if ink is None:
        return None
    item = _item_at(rgb, width, height, px, py, ink)
    if item is None:
        return None
    left, top, right, bottom = item
    tall = bottom - top + 1
    if tall < KIN_MIN_HEIGHT or not (left <= px <= right):
        return None

    if not _looks_like_text(rgb, width, height, item, _inker(ink)):
        return None

    kin = [i for i, _ in _text_items(rgb, width, height, left, ink) if _same_height(i, tall)]
    ordinal = next((n for n, i in enumerate(kin, 1) if abs(i[1] - top) <= KIN_GAP_Y + 1), None)
    if ordinal is None:
        return None

    return {"ink": ink, "left": left, "top": top, "height": tall, "width": right - left + 1, "ordinal": ordinal,
            "count": len(kin), "at": [px - left, py - top], "screen": [width, height]}


def _same_height(item, tall):
    h = item[3] - item[1] + 1
    return KIN_HEIGHT[0] * tall <= h <= KIN_HEIGHT[1] * tall


def find_kin(kin, frame):
    """
    Where the thing of the kind a kin describes, in the same place in order,
    is on `frame` now: {"point", "box", "ordinal", "count"}, or None.
    """

    try:
        ink, left, tall, ordinal = list(kin["ink"]), int(kin["left"]), int(kin["height"]), int(kin["ordinal"])
        ax, ay = kin["at"]
    except (KeyError, TypeError, ValueError):
        return None
    if frame is None or list(kin.get("screen") or []) != [frame.width, frame.height] or ordinal < 1:
        return None

    rgb, width, height = frame.rgb, frame.width, frame.height
    items = [(i, box) for i, box in _text_items(rgb, width, height, left, ink) if _same_height(i, tall)]
    if len(items) < ordinal:
        return None

    bl, bt, br, bb = items[ordinal - 1][1]
    # Where on the line it was clicked, kept inside the line: a shorter title is clicked on its middle.
    x = bl + ax if bl + ax <= br else (bl + br) // 2
    y = min(max(bt + ay, bt), bb)
    return {"point": (x, y), "box": [bl, bt, br, bb], "ordinal": ordinal, "count": len(items)}
