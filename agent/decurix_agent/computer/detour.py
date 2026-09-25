from decurix_agent.computer import ocr


# ============================================================
# Detours: steps a recording took for something that is not there this time
#
# Workflows are similar, not the same. Found live: a person recorded a Google
# search, Google put up a CAPTCHA, and the person clicked "I'm not a robot",
# three pictures of traffic lights and Verify before scrolling to the result
# they wanted. The replay got no CAPTCHA - it went straight to the results -
# and halted at "I'm not a robot", because nothing like it was on the screen.
# The same shape is every cookie banner, "what's new" pop-up, session-expired
# sign-in prompt and update nag a person dealt with while recording.
#
# The evidence is the recording's own screens. At the step that cannot find
# its target, compare what is on the screen now with what the recording saw
# before that step, and before each of the next few: when a later step's
# screen is clearly what is here now and this step's is not, the steps in
# between dealt with something this run never met. Nothing is guessed about
# *what* they were; the screens say where the task is.
#
# Two measures, both relative (only which recorded screen is closest counts,
# never a similarity on its own):
#   layout  where the screen has content: 16 px cells with edges, compared as
#           the overlap of the two sets with a one-cell tolerance, leaving out
#           cells busy in every frame (window frame, taskbar). A pixel-by-pixel
#           comparison ranks the wrong way: the CAPTCHA page is mostly white
#           and scored closer to a white results page than another results
#           page did (0.85 vs 0.77); by content it is 0.43 vs 0.69.
#   text    the words on each screen (ocr.py), leaving out the words typed
#           into the task - the results of another search share the page's
#           words, not the search's - and the words every screen has.
# ============================================================

CELL = 16
BUSY = 40

# How many steps ahead a detour may end, and how much closer the later
# screen must be than this step's own before the steps between are left out
# without a model's say (SURE_MARGIN), or offered to one (MIN_MARGIN).
MAX_AHEAD = 8
MIN_MARGIN = 0.08
SURE_MARGIN = 0.18

# Only steps like these can be a detour: nothing typed that the task is
# about, nothing that opens something, nothing a person must do or approve.
SKIPPABLE = {"click", "double_click", "right_click", "key", "scroll", "drag"}


def layout(width, height, rgb):
    """The cells of a frame that have content: a set of (row, column)."""

    busy = set()
    for r in range(height // CELL):
        y = r * CELL + CELL // 2
        for c in range(width // CELL):
            energy, prev = 0, None
            for x in range(c * CELL, c * CELL + CELL, 2):
                i = (y * width + x) * 3
                luma = (rgb[i] * 3 + rgb[i + 1] * 6 + rgb[i + 2]) // 10
                if prev is not None:
                    energy += abs(luma - prev)
                prev = luma
            x, prev = c * CELL + CELL // 2, None
            for yy in range(r * CELL, r * CELL + CELL, 2):
                i = (yy * width + x) * 3
                luma = (rgb[i] * 3 + rgb[i + 1] * 6 + rgb[i + 2]) // 10
                if prev is not None:
                    energy += abs(luma - prev)
                prev = luma
            if energy > BUSY:
                busy.add((r, c))
    return busy


def _grow(cells):
    return {(r + dr, c + dc) for r, c in cells for dr in (-1, 0, 1) for dc in (-1, 0, 1)}


def layout_score(a, b):
    """How much of two content maps land on each other, 0..1, with one cell of slack."""

    if not a and not b:
        return 1.0
    return (len(a & _grow(b)) + len(b & _grow(a))) / (len(a) + len(b))


def text_score(a, b):
    if not a and not b:
        return None
    return len(a & b) / len(a | b)


def skippable(step):
    return (step.get("kind") in SKIPPABLE and not step.get("consequential") and not step.get("param")
            and not step.get("by_hand"))


class Screen:
    """One frame, measured once; its text is read only when asked (it costs about a second)."""

    def __init__(self, width, height, rgb):
        self.size = (width, height)
        self._rgb = rgb
        self.cells = layout(width, height, rgb)
        self._lines = None

    @property
    def lines(self):
        if self._lines is None:
            self._lines = ocr.read(self.size[0], self.size[1], self._rgb)
        return self._lines

    @property
    def words(self):
        return ocr.words(self.lines)


def _scores(now, recorded, typed, text):
    """How close `now` is to each recorded screen (None for one missing or of another size)."""

    shown = [s for s in recorded if s is not None and s.size == now.size]
    if not shown:
        return [None] * len(recorded)

    common = set.intersection(now.cells, *[s.cells for s in shown])
    have_text = text and bool(now.words) and all(s.words for s in shown)
    noise = {w.lower() for value in typed for w in ocr.WORD_RE.findall(str(value or ""))}
    shared = set.intersection(now.words, *[s.words for s in shown]) if have_text else set()

    scores = []
    for s in recorded:
        if s is None or s.size != now.size:
            scores.append(None)
            continue
        score = layout_score(now.cells - common, s.cells - common)
        if have_text:
            words = text_score(now.words - noise - shared, s.words - noise - shared)
            if words is not None:
                score = (score + words) / 2
        scores.append(round(score, 3))
    return scores


def find(steps, index, now, screen_of, typed=(), text=True):
    """
    Whether the steps from steps[index] on were a detour this run does not
    need: {"to": index of the step to go on from, "margin", "scores", "here",
    "there"} or None. `screen_of(step)` is the recorded Screen before that
    step (or None); `now` is the live one. The layout ranks every candidate;
    the text (when `text` and OCR can read it) is then read for the three
    screens that matter and must agree.
    """

    step = steps[index]
    if not skippable(step):
        return None

    candidates = [index]
    for j in range(index + 1, min(len(steps), index + 1 + MAX_AHEAD)):
        later = steps[j]
        if later.get("phase") != step.get("phase") or later.get("kind") in ("manual", "open"):
            break
        candidates.append(j)
        if not skippable(later):
            break

    if len(candidates) < 2:
        return None

    screens = [screen_of(steps[j]) for j in candidates]
    if screens[0] is None:
        return None

    scores = _scores(now, screens, typed, text=False)
    here = scores[0]
    if here is None:
        return None

    best = max((k for k in range(1, len(candidates)) if scores[k] is not None), key=lambda k: scores[k], default=None)
    if best is None or scores[best] - here < MIN_MARGIN:
        return None

    margin = scores[best] - here
    if text and ocr.available():
        pair = _scores(now, [screens[0], screens[best]], typed, text=True)
        if None not in pair:
            margin = min(margin, pair[1] - pair[0])
            if margin < MIN_MARGIN:
                return None

    return {"to": candidates[best], "margin": round(margin, 3), "scores": scores,
            "here": screens[0], "there": screens[best]}
