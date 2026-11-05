#!/usr/bin/env python3
"""Brainiac's perception worker: OCR and speech-to-text over JSON lines.

The Go engine starts this as a child process and talks to it on stdin/stdout,
one request per line and one reply per line, each reply echoing the request's
"id". Logs go to stderr only. Killing the process is how its memory is given
back, so every model it holds lives in this process and nowhere else.

Only ONNX Runtime-based libraries are used (RapidOCR, sherpa-onnx), never
torch: on an 8 GB machine a torch import alone costs as much memory as a small
language model. Each library is imported inside the op that needs it, so a
missing one disables that op and nothing else.

Ops:
  hello   what this worker can do, checked without loading any model
  ocr     an image's text lines, each with an axis-aligned box and confidence
          ("upright": false lets the angle classifier turn upside-down lines)
  asr     an audio clip's transcript, cut into speech segments by a VAD
  unload  drop every cached engine and collect garbage
"""

import array
import base64
import gc
import importlib.util
import io
import json
import math
import os
import platform
import shutil
import struct
import subprocess
import sys
import time
import traceback
import wave

PROTOCOL = 1
TARGET_RATE = 16000
CHUNK_SECONDS = 30
TESSERACT_TIMEOUT = 120
FFMPEG_TIMEOUT = 300


class WorkerError(Exception):
    """A request this worker cannot do; its message is shown to the caller."""


def log(message):
    """Write one log line to stderr."""
    print(message, file=sys.stderr, flush=True)


# ---------------------------------------------------------------- capability


def _importable(name):
    """Whether a module can be imported, found without importing it."""
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _rapidocr_api():
    """Which RapidOCR is installed: 3 (package rapidocr, needs onnxruntime), 1 (rapidocr_onnxruntime), or 0."""
    if _importable("rapidocr") and _importable("onnxruntime"):
        return 3
    if _importable("rapidocr_onnxruntime"):
        return 1
    return 0


def have():
    """What this worker can use right now."""
    return {
        "rapidocr": _rapidocr_api() != 0,
        "sherpa_onnx": _importable("sherpa_onnx"),
        "numpy": _importable("numpy"),
        "tesseract": shutil.which("tesseract") is not None,
        "ffmpeg": shutil.which("ffmpeg") is not None,
    }


def op_hello(_req):
    """The worker's identity and capabilities."""
    return {
        "worker": "brainiac-python",
        "protocol": PROTOCOL,
        "python": platform.python_version(),
        "have": have(),
    }


# ---------------------------------------------------------------------- input


def _b64(req, key):
    """The bytes of a base64 field, or a WorkerError saying what is wrong."""
    value = req.get(key)
    if not isinstance(value, str) or not value:
        raise WorkerError(f"{key} is missing")
    if value.startswith("data:") and "," in value:
        value = value.split(",", 1)[1]
    try:
        return base64.b64decode(value, validate=False)
    except (ValueError, TypeError) as exc:
        raise WorkerError(f"{key} is not base64: {exc}") from exc


# ------------------------------------------------------------------------ OCR


def image_size(data):
    """(width, height) of a PNG, JPEG, GIF or WebP from its header, or (0, 0)."""
    if data[:8] == b"\x89PNG\r\n\x1a\n" and len(data) >= 24:
        return struct.unpack(">II", data[16:24])
    if data[:6] in (b"GIF87a", b"GIF89a") and len(data) >= 10:
        return struct.unpack("<HH", data[6:10])
    if data[:4] == b"RIFF" and data[8:12] == b"WEBP" and len(data) >= 30:
        chunk = data[12:16]
        if chunk == b"VP8 ":
            w, h = struct.unpack("<HH", data[26:30])
            return w & 0x3FFF, h & 0x3FFF
        if chunk == b"VP8L":
            b = data[21:25]
            w = 1 + (((b[1] & 0x3F) << 8) | b[0])
            h = 1 + (((b[3] & 0x0F) << 10) | (b[2] << 2) | ((b[1] & 0xC0) >> 6))
            return w, h
        if chunk == b"VP8X":
            w = 1 + int.from_bytes(data[24:27], "little")
            h = 1 + int.from_bytes(data[27:30], "little")
            return w, h
    if data[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(data):
            if data[i] != 0xFF:
                i += 1
                continue
            marker = data[i + 1]
            if marker in (0xD8, 0x01) or 0xD0 <= marker <= 0xD7:
                i += 2
                continue
            length = struct.unpack(">H", data[i + 2:i + 4])[0]
            if 0xC0 <= marker <= 0xCF and marker not in (0xC4, 0xC8, 0xCC):
                h, w = struct.unpack(">HH", data[i + 5:i + 9])
                return w, h
            i += 2 + length
    return 0, 0


def polygon_box(points):
    """The axis-aligned [x, y, w, h] (ints) around a polygon of [x, y] points."""
    xs = [float(p[0]) for p in points]
    ys = [float(p[1]) for p in points]
    x0, y0 = math.floor(min(xs)), math.floor(min(ys))
    x1, y1 = math.ceil(max(xs)), math.ceil(max(ys))
    return [int(x0), int(y0), int(max(0, x1 - x0)), int(max(0, y1 - y0))]


def order_lines(lines):
    """Lines in reading order: rows top to bottom, each row left to right.

    Two lines share a row when they overlap vertically by at least half of
    the shorter one's height - a slightly tilted scan keeps its rows."""
    rows = []
    for line in sorted(lines, key=lambda l: (l["box"][1] + l["box"][3] / 2.0, l["box"][0])):
        _, y, _, h = line["box"]
        placed = False
        for row in rows:
            top, bottom = row["top"], row["bottom"]
            overlap = min(bottom, y + h) - max(top, y)
            if overlap >= 0.5 * max(1, min(h, bottom - top)):
                row["lines"].append(line)
                row["top"], row["bottom"] = min(top, y), max(bottom, y + h)
                placed = True
                break
        if not placed:
            rows.append({"top": y, "bottom": y + h, "lines": [line]})
    rows.sort(key=lambda r: r["top"])
    out = []
    for row in rows:
        out.extend(sorted(row["lines"], key=lambda l: l["box"][0]))
    return out


_rapid = None
_rapid_api = 0

# The models each RapidOCR ships inside its wheel (used unless told otherwise),
# named so a reading can say which model made it.
RAPID_MODELS = {3: "PP-OCRv6-small", 1: "PP-OCRv4-mobile"}


def _rapid_engine():
    """The RapidOCR engine, created once (it loads three small ONNX models).

    RapidOCR 3 (package rapidocr) is preferred: it ships PP-OCRv6 and has wheels
    for every current Python. rapidocr_onnxruntime (PP-OCRv4) stops at 3.12."""
    global _rapid, _rapid_api
    if _rapid is None:
        api = _rapidocr_api()
        if api == 3:
            from rapidocr import RapidOCR  # noqa: imported here on purpose

            _rapid = RapidOCR(params={"Global.log_level": "warning"})
        elif api == 1:
            from rapidocr_onnxruntime import RapidOCR  # noqa: imported here on purpose

            _rapid = RapidOCR()
        else:
            raise WorkerError("RapidOCR is not installed")
        _rapid_api = api
    return _rapid


def _rapid_items(result):
    """(polygon, text, score) of each line a RapidOCR call returned, for either API."""
    if result is None:
        return []
    if isinstance(result, tuple) and len(result) == 2 and not hasattr(result, "txts"):
        result = result[0]  # rapidocr_onnxruntime returns (lines, elapse)
    if hasattr(result, "txts"):
        boxes = result.boxes if result.boxes is not None else []
        return list(zip(boxes, result.txts or (), result.scores or ()))
    return [(item[0], item[1], item[2]) for item in result or []]


def ocr_rapid(data, upright=True):
    """Lines read by RapidOCR (PP-OCR det + cls + rec on ONNX Runtime), and the image size.

    upright skips the 0/180-degree angle classifier. On screens and rendered
    pages text is never upside down, and the classifier then only adds
    mistakes: measured on a rendered invoice, it turned "SGST 9%" into
    "%6 1S9S" by flipping a line that was already the right way up."""
    import numpy as np
    import cv2

    img = cv2.imdecode(np.frombuffer(data, dtype=np.uint8), cv2.IMREAD_COLOR)
    if img is None:
        raise WorkerError("the image could not be decoded (send PNG, JPEG or WebP)")
    height, width = img.shape[:2]
    lines = []
    engine = _rapid_engine()
    try:
        result = engine(img, use_cls=not upright)
    except TypeError:  # an older RapidOCR without per-call switches
        result = engine(img)
    for points, text, score in _rapid_items(result):
        text = str(text).strip()
        if not text:
            continue
        lines.append({"text": text, "box": polygon_box(points), "conf": round(float(score), 4)})
    return lines, int(width), int(height)


def parse_tesseract_tsv(tsv):
    """Word rows of tesseract's TSV grouped into lines with a mean confidence."""
    groups = {}
    order = []
    for row in tsv.splitlines()[1:]:
        cols = row.split("\t")
        if len(cols) < 12 or cols[0] != "5":
            continue
        text = cols[11].strip()
        try:
            conf = float(cols[10])
            left, top, w, h = (int(c) for c in cols[6:10])
        except ValueError:
            continue
        if not text or conf < 0:
            continue
        key = (cols[1], cols[2], cols[3], cols[4])
        if key not in groups:
            groups[key] = {"words": [], "confs": [], "x0": left, "y0": top, "x1": left + w, "y1": top + h}
            order.append(key)
        g = groups[key]
        g["words"].append(text)
        g["confs"].append(conf)
        g["x0"], g["y0"] = min(g["x0"], left), min(g["y0"], top)
        g["x1"], g["y1"] = max(g["x1"], left + w), max(g["y1"], top + h)
    lines = []
    for key in order:
        g = groups[key]
        lines.append({
            "text": " ".join(g["words"]),
            "box": [g["x0"], g["y0"], g["x1"] - g["x0"], g["y1"] - g["y0"]],
            "conf": round(sum(g["confs"]) / len(g["confs"]) / 100.0, 4),
        })
    return lines


def ocr_tesseract(data):
    """Lines read by the tesseract command."""
    exe = shutil.which("tesseract")
    if not exe:
        raise WorkerError("tesseract is not installed")
    try:
        done = subprocess.run([exe, "stdin", "stdout", "--psm", "3", "tsv"], input=data,
                              capture_output=True, timeout=TESSERACT_TIMEOUT)
    except subprocess.TimeoutExpired as exc:
        raise WorkerError("tesseract took too long") from exc
    if done.returncode != 0:
        raise WorkerError("tesseract failed: " + done.stderr.decode("utf-8", "replace").strip()[-300:])
    width, height = image_size(data)
    return parse_tesseract_tsv(done.stdout.decode("utf-8", "replace")), width, height


# MAX_PIXELS is the largest picture decoded: a small file can declare a huge
# size and take gigabytes to decode. Brainiac sets the same limit for OpenCV.
MAX_PIXELS = 50_000_000


def op_ocr(req):
    """The text lines of one image, in reading order."""
    data = _b64(req, "image_b64")
    width, height = image_size(data)
    if width * height > MAX_PIXELS:
        raise WorkerError(f"the picture is {width}x{height}, more than {MAX_PIXELS // 1_000_000} million pixels; not decoded")
    engine = (req.get("engine") or "auto").lower()
    caps = have()
    if engine == "auto":
        if caps["rapidocr"]:
            engine = "rapidocr"
        elif caps["tesseract"]:
            engine = "tesseract"
        else:
            raise WorkerError("no OCR engine is installed (rapidocr_onnxruntime or tesseract)")
    started = time.monotonic()
    if engine == "rapidocr":
        if not caps["rapidocr"]:
            raise WorkerError("RapidOCR is not installed (pip install rapidocr onnxruntime)")
        lines, width, height = ocr_rapid(data, upright=req.get("upright", True) is not False)
        model = RAPID_MODELS.get(_rapid_api, "")
    elif engine == "tesseract":
        lines, width, height = ocr_tesseract(data)
        model = "tesseract"
    else:
        raise WorkerError(f"unknown OCR engine {engine!r}")
    return {
        "engine": engine,
        "model": model,
        "width": width,
        "height": height,
        "lines": order_lines(lines),
        "ms": int((time.monotonic() - started) * 1000),
    }


# ---------------------------------------------------------------------- audio


def _riff_chunks(data):
    """(id, payload) of each chunk of a RIFF/WAVE file."""
    if data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise WorkerError("not a WAV file")
    i = 12
    while i + 8 <= len(data):
        cid = data[i:i + 4]
        size = struct.unpack("<I", data[i + 4:i + 8])[0]
        yield cid, data[i + 8:i + 8 + size]
        i += 8 + size + (size & 1)


def _decode_float_wav(data):
    """Samples and rate of a 32/64-bit IEEE-float WAV (stdlib wave refuses these)."""
    fmt = payload = None
    for cid, chunk in _riff_chunks(data):
        if cid == b"fmt ":
            fmt = chunk
        elif cid == b"data":
            payload = chunk
    if fmt is None or payload is None:
        raise WorkerError("the WAV file has no fmt or data chunk")
    tag, channels, rate = struct.unpack("<HHI", fmt[:8])
    bits = struct.unpack("<H", fmt[14:16])[0]
    if tag == 0xFFFE and len(fmt) >= 26:
        tag = struct.unpack("<H", fmt[24:26])[0]
    if tag != 3 or bits not in (32, 64):
        raise WorkerError(f"unsupported WAV encoding (format {tag}, {bits} bits)")
    values = array.array("f" if bits == 32 else "d")
    usable = len(payload) - len(payload) % values.itemsize
    values.frombytes(payload[:usable])
    if sys.byteorder != "little":
        values.byteswap()
    return _mix(values, channels), rate


def _mix(values, channels):
    """Interleaved samples averaged down to one channel, as array('f')."""
    if channels <= 1:
        return array.array("f", values)
    frames = len(values) // channels
    out = array.array("f", bytes(4 * frames))
    for i in range(frames):
        base = i * channels
        out[i] = sum(values[base:base + channels]) / channels
    return out


def decode_wav(data):
    """(mono float samples in [-1, 1], sample rate) of a WAV file."""
    try:
        reader = wave.open(io.BytesIO(data), "rb")
    except (wave.Error, EOFError) as exc:
        if "unknown format: 3" in str(exc) or "unknown format: 65534" in str(exc):
            return _decode_float_wav(data)
        raise WorkerError(f"not a readable WAV file: {exc}") from exc
    with reader:
        channels = reader.getnchannels()
        width = reader.getsampwidth()
        rate = reader.getframerate()
        raw = reader.readframes(reader.getnframes())
    if width == 1:
        values = array.array("f", ((b - 128) / 128.0 for b in raw))
    elif width == 2:
        ints = array.array("h")
        ints.frombytes(raw[:len(raw) - len(raw) % 2])
        if sys.byteorder != "little":
            ints.byteswap()
        values = array.array("f", (v / 32768.0 for v in ints))
    elif width == 3:
        count = len(raw) // 3
        values = array.array("f", bytes(4 * count))
        for i in range(count):
            v = int.from_bytes(raw[3 * i:3 * i + 3], "little", signed=True)
            values[i] = v / 8388608.0
    elif width == 4:
        ints = array.array("i")
        ints.frombytes(raw[:len(raw) - len(raw) % 4])
        if sys.byteorder != "little":
            ints.byteswap()
        values = array.array("f", (v / 2147483648.0 for v in ints))
    else:
        raise WorkerError(f"unsupported WAV sample width {width}")
    return _mix(values, channels), rate


def resample(samples, src_rate, dst_rate=TARGET_RATE):
    """Samples at dst_rate by linear interpolation (numpy when installed)."""
    if src_rate == dst_rate or len(samples) == 0:
        return samples
    out_n = max(1, int(round(len(samples) * dst_rate / float(src_rate))))
    step = src_rate / float(dst_rate)
    try:
        import numpy as np
    except ImportError:
        np = None
    if np is not None:
        x = np.asarray(samples, dtype=np.float32)
        positions = np.arange(out_n, dtype=np.float64) * step
        res = np.interp(positions, np.arange(len(x), dtype=np.float64), x).astype(np.float32)
        return array.array("f", res.tobytes())
    out = array.array("f", bytes(4 * out_n))
    last = len(samples) - 1
    for i in range(out_n):
        pos = i * step
        j = int(pos)
        if j >= last:
            out[i] = samples[last]
        else:
            frac = pos - j
            out[i] = samples[j] * (1.0 - frac) + samples[j + 1] * frac
    return out


def decode_ffmpeg(data):
    """Mono 16 kHz samples of any format ffmpeg can read."""
    exe = shutil.which("ffmpeg")
    if not exe:
        raise WorkerError("this audio format needs ffmpeg, which is not installed (send WAV instead)")
    try:
        done = subprocess.run([exe, "-hide_banner", "-loglevel", "error", "-i", "pipe:0",
                               "-f", "s16le", "-ac", "1", "-ar", str(TARGET_RATE), "pipe:1"],
                              input=data, capture_output=True, timeout=FFMPEG_TIMEOUT)
    except subprocess.TimeoutExpired as exc:
        raise WorkerError("ffmpeg took too long") from exc
    if done.returncode != 0:
        raise WorkerError("ffmpeg could not read the audio: " + done.stderr.decode("utf-8", "replace").strip()[-300:])
    ints = array.array("h")
    raw = done.stdout
    ints.frombytes(raw[:len(raw) - len(raw) % 2])
    if sys.byteorder != "little":
        ints.byteswap()
    return array.array("f", (v / 32768.0 for v in ints))


def decode_audio(data, fmt):
    """Mono float samples at 16 kHz of an audio file."""
    fmt = (fmt or "wav").lower().lstrip(".")
    if fmt == "wav" or data[:4] == b"RIFF":
        try:
            samples, rate = decode_wav(data)
        except WorkerError:
            if shutil.which("ffmpeg"):
                return decode_ffmpeg(data)
            raise
        return resample(samples, rate)
    return decode_ffmpeg(data)


# ------------------------------------------------------------------------ ASR

_recognizer = None
_recognizer_key = None
# The files each model kind needs, by the name sherpa-onnx's constructor gives
# them (checked against sherpa-onnx 2.0.0's OfflineRecognizer.from_* methods).
_RECOGNIZER_FILES = {
    "transducer": ("encoder", "decoder", "joiner", "tokens"),
    "whisper": ("encoder", "decoder", "tokens"),
    "moonshine": ("preprocessor", "encoder", "uncached_decoder", "cached_decoder", "tokens"),
    "moonshine_v2": ("encoder", "decoder", "tokens"),
    "sense_voice": ("model", "tokens"),
    "nemo_ctc": ("model", "tokens"),
    "nemo_canary": ("encoder", "decoder", "tokens"),
    "omnilingual_ctc": ("model", "tokens"),
    "qwen3_asr": ("conv_frontend", "encoder", "decoder", "tokenizer"),
}
# Files that are directories, not single files.
_RECOGNIZER_DIRS = {("qwen3_asr", "tokenizer")}


def _filter_kwargs(fn, kwargs):
    """Only the keyword arguments fn accepts (sherpa-onnx adds and renames them across versions)."""
    import inspect

    try:
        params = inspect.signature(fn).parameters
    except (TypeError, ValueError):
        return kwargs
    if any(p.kind == p.VAR_KEYWORD for p in params.values()):
        return kwargs
    return {k: v for k, v in kwargs.items() if k in params}


def build_recognizer(model):
    """A sherpa-onnx OfflineRecognizer for a model description."""
    import sherpa_onnx

    kind = model.get("kind") or ""
    files = model.get("files") or {}
    needed = _RECOGNIZER_FILES.get(kind)
    if needed is None:
        raise WorkerError(f"unknown ASR model kind {kind!r}")
    missing = [role for role in needed if not files.get(role)]
    if missing:
        raise WorkerError(f"the {kind} model is missing files: {', '.join(missing)}")
    for role in needed:
        exists = os.path.isdir if (kind, role) in _RECOGNIZER_DIRS else os.path.isfile
        if not exists(files[role]):
            raise WorkerError(f"model file not found: {files[role]}")
    threads = int(model.get("threads") or max(1, min(4, os.cpu_count() or 1)))
    language = model.get("language") or "auto"
    # Extra constructor arguments (max_new_tokens, hotwords, ...), passed
    # through as given; ones this sherpa-onnx does not take are dropped.
    kwargs = dict(model.get("options") or {})
    kwargs.update({role: files[role] for role in needed})
    kwargs["num_threads"] = threads
    R = sherpa_onnx.OfflineRecognizer
    if kind == "transducer":
        # sherpa-onnx's own default is "transducer" (icefall zipformer); the
        # catalog's transducers are NeMo Parakeet TDT.
        kwargs["model_type"] = model.get("model_type") or "nemo_transducer"
        fn = R.from_transducer
    elif kind == "whisper":
        kwargs["language"] = "" if language == "auto" else language
        kwargs["task"] = "transcribe"
        fn = R.from_whisper
    elif kind == "moonshine":
        fn = R.from_moonshine
    elif kind == "moonshine_v2":
        fn = R.from_moonshine_v2
    elif kind == "sense_voice":
        kwargs["language"] = language
        kwargs["use_itn"] = True
        fn = R.from_sense_voice
    elif kind == "nemo_canary":
        kwargs["src_lang"] = kwargs["tgt_lang"] = "en" if language == "auto" else language
        fn = R.from_nemo_canary
    elif kind == "omnilingual_ctc":
        fn = R.from_omnilingual_asr_ctc
    elif kind == "qwen3_asr":
        fn = R.from_qwen3_asr
    else:
        fn = R.from_nemo_ctc
    return fn(**_filter_kwargs(fn, kwargs))


def _get_recognizer(model):
    """The cached recognizer for this model, building (and replacing) it when it differs."""
    global _recognizer, _recognizer_key
    key = json.dumps({k: model.get(k) for k in ("kind", "files", "language", "threads", "model_type", "options")},
                     sort_keys=True)
    if _recognizer is None or key != _recognizer_key:
        _recognizer = None
        gc.collect()
        _recognizer = build_recognizer(model)
        _recognizer_key = key
    return _recognizer


def vad_segments(samples, vad_model):
    """(start_seconds, float32 samples) of each speech segment the silero VAD finds."""
    import numpy as np
    import sherpa_onnx

    if not os.path.isfile(vad_model):
        raise WorkerError(f"VAD model not found: {vad_model}")
    config = sherpa_onnx.VadModelConfig()
    config.silero_vad.model = vad_model
    config.sample_rate = TARGET_RATE
    window = int(getattr(config.silero_vad, "window_size", 512) or 512)
    vad = sherpa_onnx.VoiceActivityDetector(config, buffer_size_in_seconds=60)
    x = np.asarray(samples, dtype=np.float32)
    out = []

    def drain():
        while not vad.empty():
            seg = vad.front
            out.append((seg.start / float(TARGET_RATE), np.asarray(seg.samples, dtype=np.float32)))
            vad.pop()

    for i in range(0, len(x), window):
        vad.accept_waveform(x[i:i + window])
        drain()
    vad.flush()
    drain()
    return out


def fixed_chunks(samples):
    """(start_seconds, samples) pieces of at most CHUNK_SECONDS each."""
    import numpy as np

    x = np.asarray(samples, dtype=np.float32)
    step = CHUNK_SECONDS * TARGET_RATE
    return [(i / float(TARGET_RATE), x[i:i + step]) for i in range(0, len(x), step)] or [(0.0, x)]


def op_asr(req):
    """The transcript of one audio clip, with segment times."""
    if not _importable("sherpa_onnx") or not _importable("numpy"):
        raise WorkerError("speech-to-text needs sherpa-onnx and numpy, which are not installed")
    data = _b64(req, "audio_b64")
    model = req.get("model") or {}
    if not isinstance(model, dict):
        raise WorkerError("model must be an object")
    started = time.monotonic()
    samples = decode_audio(data, req.get("format"))
    duration = len(samples) / float(TARGET_RATE)
    recognizer = _get_recognizer(model)
    vad_model = (req.get("vad") or {}).get("model") if isinstance(req.get("vad"), dict) else None
    pieces = vad_segments(samples, vad_model) if vad_model else fixed_chunks(samples)
    segments = []
    language = ""
    for start, chunk in pieces:
        if len(chunk) == 0:
            continue
        stream = recognizer.create_stream()
        stream.accept_waveform(TARGET_RATE, chunk)
        recognizer.decode_stream(stream)
        result = stream.result
        text = (result.text or "").strip()
        language = language or (getattr(result, "lang", "") or "")
        if text:
            segments.append({
                "start": round(start, 3),
                "end": round(start + len(chunk) / float(TARGET_RATE), 3),
                "text": text,
            })
    wanted = model.get("language") or ""
    if not language and wanted and wanted != "auto":
        language = wanted
    language = language.strip("<|>") if language else ""
    return {
        "text": " ".join(s["text"] for s in segments),
        "language": language,
        "duration": round(duration, 3),
        "segments": segments,
        "ms": int((time.monotonic() - started) * 1000),
    }


# ----------------------------------------------------------------- dispatch


def op_unload(_req):
    """Drop every cached engine so its memory can be returned."""
    global _rapid, _rapid_api, _recognizer, _recognizer_key
    _rapid = None
    _rapid_api = 0
    _recognizer = None
    _recognizer_key = None
    gc.collect()
    return {}


def op_sleep(req):
    """Sleep (tests only: lets a test make a call outlive its deadline)."""
    if os.environ.get("BRAINIAC_WORKER_TEST") != "1":
        raise WorkerError("unknown op 'sleep'")
    time.sleep(float(req.get("seconds") or 0))
    return {}


OPS = {
    "hello": op_hello,
    "ocr": op_ocr,
    "asr": op_asr,
    "unload": op_unload,
    "sleep": op_sleep,
}


def handle(req):
    """The reply to one decoded request (without its id)."""
    if not isinstance(req, dict):
        raise WorkerError("a request must be a JSON object")
    op = req.get("op")
    fn = OPS.get(op)
    if fn is None:
        raise WorkerError(f"unknown op {op!r}")
    return fn(req)


def reply_for(raw):
    """The reply object for one raw request line."""
    try:
        req = json.loads(raw)
    except (ValueError, UnicodeDecodeError) as exc:
        return {"id": None, "ok": False, "error": f"not JSON: {exc}"}
    rid = req.get("id") if isinstance(req, dict) else None
    try:
        out = handle(req)
    except WorkerError as exc:
        return {"id": rid, "ok": False, "error": str(exc)}
    except Exception as exc:  # a bug or a library failure: report it, keep serving
        log(traceback.format_exc())
        return {"id": rid, "ok": False, "error": f"{type(exc).__name__}: {exc}"}
    reply = {"id": rid, "ok": True}
    reply.update(out)
    return reply


def serve(inp, out):
    """Answer requests from the binary stream inp on the text stream out until EOF."""
    for raw in inp:
        raw = raw.strip()
        if not raw:
            continue
        out.write(json.dumps(reply_for(raw), ensure_ascii=False) + "\n")
        out.flush()


def main():
    """Serve stdin/stdout, with every other write to stdout sent to stderr.

    Native libraries may print to file descriptor 1; a stray line there would
    corrupt the protocol, so fd 1 is pointed at stderr and replies go to a
    private copy of the original stdout."""
    reply_fd = os.dup(1)
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    replies = os.fdopen(reply_fd, "w", encoding="utf-8", newline="\n")
    try:
        serve(sys.stdin.buffer, replies)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
