"""
A click recorded as a coordinate, replayed: the recording keeps a picture of
what was around it, and the replay clicks only where that picture still is.

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
from test_visual import CLICK, HEIGHT, RECORDED, WIDTH, Frame, frame_with, png_of  # noqa: E402

SESSION = "demo-20260921T101500-abc123"


class CanvasScreen:
    """core, as a replay of a coordinate click sees it: a screen and a log of clicks."""

    def __init__(self, frame):
        self.frame = frame
        self.acted = []

    def observe(self, remember=True):
        return {"elements": [], "windows": [{"title": "Canvas"}], "active_title": "Canvas",
                "size": [WIDTH, HEIGHT], "frame": self.frame}

    def active_group(self, observation):
        return "g"

    def act(self, kind, **kwargs):
        self.acted.append((kind, kwargs))
        return {"status": "done", "text": "ok", "observation": self.observe()}


class Anchored(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        recording.ROOT = Path(self.tmp.name)
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

        # A demonstration whose one action is a click on something that exposed no element.
        recording.append(SESSION, {"kind": "click", "actor": "human", "point": list(CLICK), "window": "Canvas",
                                   "outcome": "done"}, before=png_of(frame_with(RECORDED)))
        recording.update(SESSION, title="Press the canvas button")
        self.meta, self.events = recording.load(SESSION)

    def tearDown(self):
        recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved
        self.tmp.cleanup()

    def run_on(self, frame):
        fake = CanvasScreen(frame)
        replay.core = fake
        program = replay.compile_program(self.meta, self.events)
        return fake, replay.run(program, report=lambda line: None)

    def test_a_coordinate_click_keeps_a_look_of_what_was_around_it(self):
        step = replay.compile_program(self.meta, self.events)["steps"][0]

        self.assertNotIn("target", step)
        self.assertEqual(step["point"], list(CLICK))
        self.assertEqual(step["look"]["screen"], [WIDTH, HEIGHT])

    def test_a_press_read_after_the_screen_changed_gets_no_look(self):
        # Its frame shows what the press made, not what was pressed (demonstration.LATE_NOTE).
        events = [dict(self.events[0], late=True)]
        self.assertNotIn("look", replay.compile_program(self.meta, events)["steps"][0])

    def test_a_click_that_had_an_element_needs_no_look(self):
        events = [dict(self.events[0], element={"role": "button", "name": "OK", "path": "w > OK"})]
        self.assertNotIn("look", replay.compile_program(self.meta, events)["steps"][0])

    def test_a_recording_with_no_frames_still_compiles_and_clicks_as_recorded(self):
        meta, events = dict(self.meta), [{k: v for k, v in self.events[0].items() if k not in ("before", "after")}]
        program = replay.compile_program(meta, events)

        self.assertNotIn("look", program["steps"][0])

        fake = CanvasScreen(frame_with(RECORDED))
        replay.core = fake
        result = replay.run(program, report=lambda line: None)
        self.assertEqual(fake.acted[0][1]["point"], tuple(CLICK), result["text"])

    def test_the_control_where_it_was_is_clicked_where_it_was(self):
        fake, result = self.run_on(frame_with(RECORDED))
        self.assertEqual(fake.acted[0][1]["point"], tuple(CLICK), result["text"])

    def test_a_control_that_moved_is_clicked_where_it_is_now(self):
        fake, _ = self.run_on(frame_with((RECORDED[0] + 30, RECORDED[1] + 25)))
        self.assertEqual(fake.acted[0][1]["point"], (CLICK[0] + 30, CLICK[1] + 25))

    def test_a_control_that_moved_says_so_in_the_runs_own_report(self):
        replay.core = CanvasScreen(frame_with((RECORDED[0] + 30, RECORDED[1] + 25)))
        lines = []
        replay.run(replay.compile_program(self.meta, self.events), report=lines.append)

        self.assertTrue(any("has moved +30,+25 px" in line for line in lines), lines)

    def test_a_control_that_is_gone_is_not_clicked_at_all(self):
        fake, result = self.run_on(frame_with())

        self.assertEqual(fake.acted, [])
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("does not click there", result["text"])

    def test_two_lookalikes_halt_rather_than_being_guessed_between(self):
        fake, result = self.run_on(frame_with((RECORDED[0] - 60, RECORDED[1] + 10), (RECORDED[0] + 70, RECORDED[1] - 10)))

        self.assertEqual(fake.acted, [])
        self.assertIn("does not guess which", result["text"])

    def test_a_screen_of_another_size_still_halts_as_it_always_did(self):
        replay.core = fake = CanvasScreen(Frame(200, 150, bytes(200 * 150 * 3)))
        observation = fake.observe()
        observation["size"] = [200, 150]
        program = replay.compile_program(self.meta, self.events)
        program["steps"][0]["screen"] = [WIDTH, HEIGHT]

        _, point, why = replay.resolve(program["steps"][0], observation)
        self.assertIsNone(point)
        self.assertIn("different size", why)

    def test_a_look_survives_being_saved_in_a_skill(self):
        import copy
        import json

        step = copy.deepcopy(replay.compile_program(self.meta, self.events)["steps"][0])
        again = json.loads(json.dumps(step))

        self.assertEqual(visual.find(again["look"], frame_with(RECORDED))["status"], visual.FOUND)


if __name__ == "__main__":
    unittest.main()
