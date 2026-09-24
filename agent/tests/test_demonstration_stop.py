"""
The demonstration recorder at End: input a person made before pressing End is
recorded even when reading it has fallen behind. Found end to end on a 1-vCPU
desktop: a full-screen capture per keystroke put the recorder seconds behind a
person typing a search, End stopped reading at once, and " breeding season"
and the Enter after it were never recorded.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import queue
import sys
import threading
import types
import unittest
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401

from decurix_agent.computer import demonstration


class FakeInput:
    """An X connection with events already waiting on it."""

    def __init__(self, count):
        self.waiting = list(range(count))

    def pending_events(self):
        return len(self.waiting)

    def next_event(self):
        return self.waiting.pop(0)

    def screen(self):
        return mock.Mock()


def recorder(count):
    rec = demonstration.Recorder.__new__(demonstration.Recorder)
    rec.raw = queue.Queue()
    rec.stop_requested = threading.Event()
    rec.stopping = threading.Event()
    rec.paused = False
    rec.agent_devices = set()
    rec.raw_types = ("raw",)
    rec.input = FakeInput(count)
    return rec


def fake_xlib():
    """The names the recorder's input thread uses from python-xlib, for a machine without it."""

    X = types.SimpleNamespace(MappingNotify=34, LockMask=2)
    ge = types.SimpleNamespace(GenericEventCode=35)
    xinput = types.SimpleNamespace(RawKeyPress=13, RawKeyRelease=14, RawButtonPress=15, RawButtonRelease=16)
    XK = types.SimpleNamespace(keysym_to_string=lambda keysym: None)
    xlib = types.ModuleType("Xlib")
    xlib.X, xlib.XK = X, XK
    ext = types.ModuleType("Xlib.ext")
    ext.ge, ext.xinput = ge, xinput
    xlib.ext = ext
    return {"Xlib": xlib, "Xlib.X": X, "Xlib.XK": XK, "Xlib.ext": ext, "Xlib.ext.ge": ge, "Xlib.ext.xinput": xinput}


XLIB = fake_xlib()


def fake_event(n):
    return mock.Mock(type=35, evtype="raw", data=mock.Mock(sourceid=2), n=n)


class Stopping(unittest.TestCase):

    def setUp(self):
        patches = [
            # Readable while something is waiting, as a real socket is.
            mock.patch.object(demonstration.select, "select", lambda r, w, x, t: (r if r[0].pending_events() else [], [], [])),
            mock.patch.dict(sys.modules, XLIB),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_end_reads_everything_already_made_before_stopping(self):
        rec = recorder(30)
        rec.input.next_event = lambda: fake_event(rec.input.waiting.pop(0))
        rec._raw = lambda evtype, data, root: {"type": "key_down", "n": 1}
        rec.stop_requested.set()

        rec._read_input()

        self.assertTrue(rec.stopping.is_set())
        self.assertEqual(rec.raw.qsize(), 30, "every event waiting when End came was read, not dropped")

    def test_a_person_still_typing_cannot_hold_end_open_forever(self):
        rec = recorder(0)
        rec.input.pending_events = lambda: 1
        rec.input.next_event = lambda: fake_event(0)
        rec._raw = lambda evtype, data, root: None
        rec.stop_requested.set()

        with mock.patch.object(demonstration, "DRAIN_SECONDS", 0.2):
            thread = threading.Thread(target=rec._read_input, daemon=True)
            thread.start()
            thread.join(3)

        self.assertFalse(thread.is_alive())
        self.assertTrue(rec.stopping.is_set())

    def test_the_last_events_queued_are_flushed_not_left_behind(self):
        rec = recorder(0)
        rec.pause_requested = rec.resume_requested = False
        flushed = []
        rec._flush = lambda buffer: flushed.extend(buffer)
        rec.stopping.set()
        for n in range(5):
            rec.raw.put({"type": "key_down", "n": n})

        rec._process()

        self.assertEqual([e["n"] for e in flushed], [0, 1, 2, 3, 4])


class KeyFrames(unittest.TestCase):

    def test_only_the_first_key_of_a_burst_captures_the_screen(self):
        rec = demonstration.Recorder.__new__(demonstration.Recorder)
        rec._shift, rec._caps, rec._keys_held, rec._last_key = set(), False, set(), 0.0
        rec.input = mock.Mock()
        rec.input.keycode_to_keysym = lambda keycode, index: ord("a") if index == 0 else 0
        root = mock.Mock()
        root.query_pointer.return_value = mock.Mock(mask=0)
        grabs = []
        rec._grab = lambda: grabs.append(1) or object()

        xinput = XLIB["Xlib.ext.xinput"]
        clock = iter([100.0, 100.05, 100.2, 100.25, 100.4, 100.45, 105.0, 105.05])
        with mock.patch.object(demonstration.time, "time", lambda: next(clock)), mock.patch.dict(sys.modules, XLIB):
            events = [rec._raw(xinput.RawKeyPress if i % 2 == 0 else xinput.RawKeyRelease, mock.Mock(detail=38), root)
                      for i in range(8)]

        self.assertEqual(len(grabs), 2, "the burst's first key, and the first key after a pause")
        self.assertIn("frame", events[0])
        self.assertNotIn("frame", events[2])
        self.assertIn("frame", events[6])


if __name__ == "__main__":
    unittest.main()
