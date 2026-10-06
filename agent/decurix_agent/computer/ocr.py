import hashlib
import os
import re
import shutil
import subprocess
import tempfile

from decurix_agent.logging_utils import log


# ============================================================
# Reading the text of a screen that has no accessibility tree
#
# A host app shown on a canvas, a remote desktop, a game: the screen is only
# pixels, and a replay that halts there has nothing to tell a model about what
# it sees. Tesseract reads it, when the image has it (the desktop Dockerfiles
# install tesseract-ocr); without it `available()` is False and every caller
# carries on as it did before - OCR is a way to be smarter, never a way to act.
#
# What was measured on real Chromium pixels (a Google results page, 1528x729):
# tesseract on the frame as it is reads blue link titles on white as noise
# ("t songs o" for "Best songs of Jimi Hendrix"), and its sparse mode is
# worse. Thresholded to black on white and scaled 2x it reads every title -
# "Ramapithecus | Early Human Ancestor, Fossil Primate Genus" - in ~1.3 s on
# a 6-core host. So: luma < INK is ink, everything else paper, SCALE 2x.
# Light text on a dark bar is lost that way; a page's content is what matters.
# ============================================================

INK = 170
SCALE = 2
TIMEOUT_SECONDS = 30
MIN_CONFIDENCE = 30

_cache = {}
_CACHE_LIMIT = 16

WORD_RE = re.compile(r"[A-Za-z][A-Za-z'\-]{2,}")


def available():
    return shutil.which("tesseract") is not None


def _pgm(width, height, rgb):
    """The frame as a binary PGM, ink black on white paper, scaled SCALE times."""

    grey = bytearray(width * height)
    for i in range(width * height):
        j = 3 * i
        grey[i] = 0 if (rgb[j] * 299 + rgb[j + 1] * 587 + rgb[j + 2] * 114) // 1000 < INK else 255

    out = bytearray()
    for y in range(height):
        row = grey[y * width:(y + 1) * width]
        wide = bytearray(width * SCALE)
        for k in range(SCALE):
            wide[k::SCALE] = row
        out += bytes(wide) * SCALE

    return b"P5 %d %d 255\n" % (width * SCALE, height * SCALE) + bytes(out)


def _parse_tsv(tsv):
    """Tesseract's TSV as lines: [{"text", "box": [x, y, w, h], "words"}], in screen pixels."""

    lines = {}
    for row in tsv.splitlines()[1:]:
        cols = row.split("\t")
        if len(cols) < 12 or cols[0] != "5":
            continue
        text = cols[11].strip()
        try:
            conf = float(cols[10])
        except ValueError:
            continue
        if not text or conf < MIN_CONFIDENCE:
            continue

        key = (cols[2], cols[3], cols[4])  # block, paragraph, line
        x, y, w, h = (int(c) // SCALE for c in cols[6:10])
        line = lines.setdefault(key, {"words": [], "x0": x, "y0": y, "x1": x + w, "y1": y + h})
        line["words"].append(text)
        line["x0"], line["y0"] = min(line["x0"], x), min(line["y0"], y)
        line["x1"], line["y1"] = max(line["x1"], x + w), max(line["y1"], y + h)

    out = []
    for line in lines.values():
        text = " ".join(line["words"])
        if len(WORD_RE.findall(text)) == 0:
            continue
        out.append({"text": text, "box": [line["x0"], line["y0"], line["x1"] - line["x0"], line["y1"] - line["y0"]]})

    out.sort(key=lambda l: (l["box"][1], l["box"][0]))
    return out


def read(width, height, rgb):
    """
    The lines of text on a frame, top to bottom: [{"text", "box"}]. Empty when
    tesseract is not installed or fails - never an exception.
    """

    if not available():
        return []

    key = hashlib.blake2b(rgb, digest_size=12).hexdigest()
    if key in _cache:
        return _cache[key]

    try:
        done = subprocess.run(["tesseract", "stdin", "stdout", "--psm", "3", "tsv"],
                              input=_pgm(width, height, rgb), capture_output=True, timeout=TIMEOUT_SECONDS)
        lines = _parse_tsv(done.stdout.decode("utf-8", "replace")) if done.returncode == 0 else []
    except (OSError, subprocess.SubprocessError) as exc:
        log(f"ocr: tesseract failed: {exc}")
        lines = []

    if len(_cache) >= _CACHE_LIMIT:
        _cache.pop(next(iter(_cache)))
    _cache[key] = lines
    return lines


def read_frame(frame):
    """read() for a screen.Frame (or anything with width, height and rgb)."""

    if frame is None:
        return []
    return read(frame.width, frame.height, bytes(frame.rgb))


def words(lines):
    """The words of some lines, lower-cased, for comparing two screens."""

    return {w.lower() for line in lines for w in WORD_RE.findall(line["text"])}


def line_at(lines, point, reach=14):
    """The line under a point, or the nearest within `reach` px; None when nothing is there."""

    if not point:
        return None
    px, py = point
    best, best_d = None, None
    for line in lines:
        x, y, w, h = line["box"]
        dx = max(x - px, 0, px - (x + w))
        dy = max(y - py, 0, py - (y + h))
        d = max(dx, dy)
        if d <= reach and (best_d is None or d < best_d):
            best, best_d = line, d
    return best


def text(lines, limit=900):
    """Lines joined for a prompt, cut at `limit` characters."""

    out = "\n".join(l["text"] for l in lines)
    return out if len(out) <= limit else out[:limit].rsplit("\n", 1)[0] + "\n..."
