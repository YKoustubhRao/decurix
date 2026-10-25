"""Tests for brainiac_worker.py that need only the standard library.

Run: python -m unittest discover -s workers/python
"""

import array
import base64
import importlib.util
import io
import json
import math
import os
import struct
import subprocess
import sys
import tempfile
import unittest
import wave
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "brainiac_worker.py")

spec = importlib.util.spec_from_file_location("brainiac_worker", SCRIPT)
bw = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bw)

HAVE_NUMPY = importlib.util.find_spec("numpy") is not None


def run_lines(requests):
    """Replies of an in-process serve() to a list of request objects or raw strings."""
    raw = "\n".join(r if isinstance(r, str) else json.dumps(r) for r in requests) + "\n"
    out = io.StringIO()
    bw.serve(io.BytesIO(raw.encode("utf-8")), out)
    return [json.loads(line) for line in out.getvalue().splitlines()]


def make_wav(rate=16000, channels=1, width=2, seconds=0.5, freq=440.0):
    """A sine-tone WAV file as bytes."""
    n = int(rate * seconds)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(width)
        w.setframerate(rate)
        frames = bytearray()
        for i in range(n):
            v = 0.5 * math.sin(2 * math.pi * freq * i / rate)
            for _ in range(channels):
                if width == 1:
                    frames += bytes([int(128 + v * 127)])
                elif width == 2:
                    frames += struct.pack("<h", int(v * 32767))
                elif width == 3:
                    frames += int(v * 8388607).to_bytes(3, "little", signed=True)
                else:
                    frames += struct.pack("<i", int(v * 2147483647))
        w.writeframes(bytes(frames))
    return buf.getvalue()


def make_float_wav(rate=16000, seconds=0.25):
    """A 32-bit IEEE-float WAV (format 3), which the wave module refuses."""
    n = int(rate * seconds)
    samples = array.array("f", (0.25 * math.sin(2 * math.pi * 300 * i / rate) for i in range(n)))
    payload = samples.tobytes()
    fmt = struct.pack("<HHIIHH", 3, 1, rate, rate * 4, 4, 32)
    body = b"WAVE" + b"fmt " + struct.pack("<I", len(fmt)) + fmt + b"data" + struct.pack("<I", len(payload)) + payload
    return b"RIFF" + struct.pack("<I", len(body)) + body


class ProtocolTest(unittest.TestCase):
    def test_hello_echoes_id_and_lists_capabilities(self):
        (reply,) = run_lines([{"id": 7, "op": "hello"}])
        self.assertEqual(reply["id"], 7)
        self.assertTrue(reply["ok"])
        self.assertEqual(reply["worker"], "brainiac-python")
        self.assertEqual(reply["protocol"], 1)
        self.assertEqual(set(reply["have"]), {"rapidocr", "sherpa_onnx", "numpy", "tesseract", "ffmpeg"})

    def test_bad_requests_are_answered_and_the_loop_goes_on(self):
        replies = run_lines(["not json", "[1, 2]", {"id": 1, "op": "nope"}, {"id": 2, "op": "ocr"},
                             {"id": 3, "op": "sleep", "seconds": 0}, {"id": 4, "op": "hello"}])
        self.assertEqual(len(replies), 6)
        self.assertFalse(replies[0]["ok"])
        self.assertIsNone(replies[0]["id"])
        self.assertIn("not JSON", replies[0]["error"])
        self.assertIn("JSON object", replies[1]["error"])
        self.assertEqual((replies[2]["id"], replies[2]["ok"]), (1, False))
        self.assertIn("unknown op", replies[2]["error"])
        self.assertIn("image_b64 is missing", replies[3]["error"])
        self.assertIn("unknown op", replies[4]["error"], "sleep must be refused outside tests")
        self.assertTrue(replies[5]["ok"])

    def test_library_failure_becomes_an_error_reply(self):
        with mock.patch.dict(bw.OPS, {"hello": lambda req: 1 / 0}), mock.patch.object(bw, "log"):
            (reply,) = run_lines([{"id": 9, "op": "hello"}])
        self.assertFalse(reply["ok"])
        self.assertIn("ZeroDivisionError", reply["error"])

    def test_ocr_with_a_stand_in_engine_orders_lines(self):
        lines = [
            {"text": "Total 12,345.00", "box": [300, 52, 160, 20], "conf": 0.9},
            {"text": "Invoice 4417", "box": [10, 10, 120, 20], "conf": 0.98},
            {"text": "Date", "box": [10, 50, 50, 22], "conf": 0.95},
        ]
        image = base64.b64encode(b"fake-png").decode()
        with mock.patch.object(bw, "_rapidocr_api", return_value=3), \
                mock.patch.object(bw, "ocr_rapid", return_value=(lines, 640, 480)):
            (reply,) = run_lines([{"id": 1, "op": "ocr", "image_b64": image}])
        self.assertTrue(reply["ok"], reply)
        self.assertEqual(reply["engine"], "rapidocr")
        self.assertEqual((reply["width"], reply["height"]), (640, 480))
        self.assertEqual([l["text"] for l in reply["lines"]], ["Invoice 4417", "Date", "Total 12,345.00"])

    def test_ocr_without_any_engine_says_so(self):
        image = base64.b64encode(b"x").decode()
        with mock.patch.object(bw, "have", return_value={"rapidocr": False, "tesseract": False}):
            (reply,) = run_lines([{"id": 1, "op": "ocr", "image_b64": image}])
        self.assertFalse(reply["ok"])
        self.assertIn("no OCR engine", reply["error"])

    def test_asr_without_sherpa_says_so(self):
        audio = base64.b64encode(make_wav()).decode()
        with mock.patch.object(bw, "_importable", return_value=False):
            (reply,) = run_lines([{"id": 1, "op": "asr", "audio_b64": audio, "model": {"kind": "whisper"}}])
        self.assertFalse(reply["ok"])
        self.assertIn("sherpa-onnx", reply["error"])

    def test_unload_clears_engines(self):
        bw._rapid, bw._recognizer, bw._recognizer_key = object(), object(), "k"
        (reply,) = run_lines([{"id": 1, "op": "unload"}])
        self.assertTrue(reply["ok"])
        self.assertIsNone(bw._rapid)
        self.assertIsNone(bw._recognizer)


class ProcessTest(unittest.TestCase):
    def test_subprocess_stdout_carries_only_replies(self):
        """Prints from Python and from native code (fd 1) must land on stderr."""
        wrapper = (
            "import importlib.util, os, sys\n"
            f"spec = importlib.util.spec_from_file_location('w', {SCRIPT!r})\n"
            "w = importlib.util.module_from_spec(spec); spec.loader.exec_module(w)\n"
            "orig = w.OPS['hello']\n"
            "def noisy(req):\n"
            "    print('python noise')\n"
            "    os.write(1, b'native noise\\n')\n"
            "    return orig(req)\n"
            "w.OPS['hello'] = noisy\n"
            "w.main()\n"
        )
        with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as f:
            f.write(wrapper)
            path = f.name
        try:
            done = subprocess.run([sys.executable, "-I", "-u", path],
                                  input=b'{"id": 1, "op": "hello"}\n{"id": 2, "op": "hello"}\n',
                                  capture_output=True, timeout=60)
        finally:
            os.unlink(path)
        self.assertEqual(done.returncode, 0, done.stderr)
        lines = done.stdout.decode().splitlines()
        self.assertEqual([json.loads(l)["id"] for l in lines], [1, 2])
        self.assertIn(b"python noise", done.stderr)
        self.assertIn(b"native noise", done.stderr)

    def test_real_script_answers_hello_and_exits_on_eof(self):
        done = subprocess.run([sys.executable, "-I", "-u", SCRIPT], input=b'{"id": 5, "op": "hello"}\n',
                              capture_output=True, timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        (line,) = done.stdout.decode().splitlines()
        self.assertEqual(json.loads(line)["id"], 5)


class GeometryTest(unittest.TestCase):
    def test_polygon_box_is_axis_aligned_ints(self):
        self.assertEqual(bw.polygon_box([[10.2, 5.9], [50.1, 6.0], [50.0, 20.4], [10.0, 20.0]]), [10, 5, 41, 16])

    def test_order_lines_keeps_tilted_rows_together(self):
        lines = [
            {"text": "c", "box": [200, 14, 40, 20]},  # same row as a, slightly lower
            {"text": "d", "box": [0, 60, 40, 20]},
            {"text": "a", "box": [0, 10, 40, 20]},
            {"text": "b", "box": [100, 12, 40, 20]},
        ]
        self.assertEqual([l["text"] for l in bw.order_lines(lines)], ["a", "b", "c", "d"])

    def test_order_lines_of_nothing(self):
        self.assertEqual(bw.order_lines([]), [])

    def test_image_size_reads_headers(self):
        png = b"\x89PNG\r\n\x1a\n" + struct.pack(">I", 13) + b"IHDR" + struct.pack(">II", 640, 480) + b"\x08\x02\x00\x00\x00"
        self.assertEqual(bw.image_size(png), (640, 480))
        gif = b"GIF89a" + struct.pack("<HH", 33, 44) + b"\x00" * 8
        self.assertEqual(bw.image_size(gif), (33, 44))
        jpeg = b"\xff\xd8" + b"\xff\xe0" + struct.pack(">H", 16) + b"\x00" * 14 + \
            b"\xff\xc0" + struct.pack(">HBHH", 17, 8, 300, 200) + b"\x00" * 10
        self.assertEqual(bw.image_size(jpeg), (200, 300))
        self.assertEqual(bw.image_size(b"nope"), (0, 0))

    def test_tesseract_tsv_groups_words_into_lines(self):
        tsv = "\n".join([
            "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext",
            "5\t1\t1\t1\t1\t1\t10\t10\t50\t20\t90\tInvoice",
            "5\t1\t1\t1\t1\t2\t70\t11\t40\t20\t80\t4417",
            "5\t1\t1\t1\t2\t1\t10\t40\t60\t20\t-1\t",
            "5\t1\t1\t1\t2\t2\t10\t40\t60\t20\t70\tTotal",
        ])
        lines = bw.parse_tesseract_tsv(tsv)
        self.assertEqual([l["text"] for l in lines], ["Invoice 4417", "Total"])
        self.assertEqual(lines[0]["box"], [10, 10, 100, 21])
        self.assertAlmostEqual(lines[0]["conf"], 0.85)


class AudioTest(unittest.TestCase):
    def test_16k_mono_wav_decodes_unchanged(self):
        samples, rate = bw.decode_wav(make_wav(rate=16000))
        self.assertEqual(rate, 16000)
        self.assertEqual(len(samples), 8000)
        self.assertLessEqual(max(abs(s) for s in samples), 0.51)

    def test_stereo_is_mixed_to_mono(self):
        samples, rate = bw.decode_wav(make_wav(rate=16000, channels=2, seconds=0.1))
        self.assertEqual((rate, len(samples)), (16000, 1600))

    def test_every_pcm_width(self):
        for width in (1, 2, 3, 4):
            samples, rate = bw.decode_wav(make_wav(rate=8000, width=width, seconds=0.1))
            self.assertEqual((rate, len(samples)), (8000, 800), width)
            self.assertGreater(max(samples), 0.4, width)
            self.assertLess(min(samples), -0.4, width)

    def test_float_wav(self):
        samples, rate = bw.decode_wav(make_float_wav())
        self.assertEqual((rate, len(samples)), (16000, 4000))
        self.assertAlmostEqual(max(samples), 0.25, places=2)

    def test_not_a_wav(self):
        with self.assertRaises(bw.WorkerError):
            bw.decode_wav(b"hello there")

    def test_8k_is_resampled_to_16k(self):
        audio = bw.decode_audio(make_wav(rate=8000, seconds=0.5), "wav")
        self.assertEqual(len(audio), 8000)

    def test_resample_without_numpy(self):
        samples = array.array("f", [0.0, 1.0, 0.0, -1.0] * 100)
        with mock.patch.dict(sys.modules, {"numpy": None}):
            out = bw.resample(samples, 8000, 16000)
        self.assertEqual(len(out), 800)
        self.assertAlmostEqual(out[1], 0.5, places=5)

    @unittest.skipUnless(HAVE_NUMPY, "numpy not installed")
    def test_resample_with_numpy_matches_length(self):
        samples = array.array("f", [0.1] * 44100)
        self.assertEqual(len(bw.resample(samples, 44100, 16000)), 16000)

    @unittest.skipUnless(HAVE_NUMPY, "numpy not installed")
    def test_fixed_chunks_are_at_most_30_seconds(self):
        samples = array.array("f", bytes(4 * 16000 * 65))
        chunks = bw.fixed_chunks(samples)
        self.assertEqual([round(start) for start, _ in chunks], [0, 30, 60])
        self.assertEqual(len(chunks[-1][1]), 16000 * 5)

    def test_other_formats_need_ffmpeg(self):
        with mock.patch.object(bw.shutil, "which", return_value=None):
            with self.assertRaises(bw.WorkerError) as ctx:
                bw.decode_audio(b"ID3....", "mp3")
        self.assertIn("ffmpeg", str(ctx.exception))

    def test_recognizer_needs_its_files(self):
        with mock.patch.dict(sys.modules, {"sherpa_onnx": mock.MagicMock()}):
            with self.assertRaises(bw.WorkerError) as ctx:
                bw.build_recognizer({"kind": "whisper", "files": {"encoder": "/x"}})
            self.assertIn("decoder", str(ctx.exception))
            with self.assertRaises(bw.WorkerError):
                bw.build_recognizer({"kind": "martian"})

    def test_filter_kwargs_drops_unknown_arguments(self):
        def ctor(encoder, tokens, num_threads=1):
            return encoder, tokens, num_threads
        self.assertEqual(bw._filter_kwargs(ctor, {"encoder": 1, "tokens": 2, "language": "en"}),
                         {"encoder": 1, "tokens": 2})


if __name__ == "__main__":
    unittest.main()
