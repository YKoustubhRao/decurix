"""
Detours: steps a recording took for something a replay does not meet (a
CAPTCHA, a pop-up), left out when the screen says the task is already past
them; and a point click placed by the engine reading the screen's text.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import sys
import types
import unittest
from pathlib import Path
from unittest import mock

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

from decurix_agent.computer import detour, escalation, ocr, replay

FIXTURE = json.loads((Path(__file__).parent / "fixtures" / "detour" / "captcha-search.json").read_text())

W, H = 320, 160


def canvas(*blocks):
    """A white frame with striped (edge-rich) content in each (x, y, w, h) block."""

    rgb = bytearray(b"\xff" * (W * H * 3))
    for bx, by, bw, bh in blocks:
        for y in range(by, by + bh):
            for x in range(bx, bx + bw):
                if (x // 2) % 2 == 0:
                    i = (y * W + x) * 3
                    rgb[i:i + 3] = b"\x10\x10\x10"
    return bytes(rgb)


# A page of results (rows down the left column), and the same site showing a
# robot check (one box near the top) instead.
RESULTS = canvas((0, 0, W, 16), (40, 40, 200, 12), (40, 70, 200, 12), (40, 100, 200, 12), (40, 130, 200, 12))
CHECK = canvas((0, 0, W, 16), (40, 40, 110, 40))


class Recorded(detour.Screen):
    """A recorded screen from the fixture: its content cells and its text, as measured on the real frame."""

    def __init__(self, name):
        frame = FIXTURE["frames"][name]
        self.size = tuple(frame["size"])
        self.cells = {tuple(c) for c in frame["cells"]}
        self._lines = frame["lines"]
        self._rgb = b""


def fixture_screen_of(step):
    name = f"before-step-{step['n']}"
    return Recorded(name) if name in FIXTURE["frames"] else None


STEPS = FIXTURE["steps"]
TYPED = ["rama pethicus", "jimi hendrix greatest"]


class RealRecording(unittest.TestCase):
    """The recording this was built for: a CAPTCHA while recording, none when replayed."""

    def test_the_captcha_steps_are_left_out_by_layout_alone(self):
        found = detour.find(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of, typed=TYPED, text=False)
        self.assertIsNotNone(found)
        self.assertEqual(STEPS[found["to"]]["n"], 10, "carries on at the scroll on the results page")
        self.assertGreaterEqual(found["margin"], detour.SURE_MARGIN)

    def test_the_text_on_the_screens_agrees(self):
        with mock.patch.object(ocr, "available", return_value=True):
            found = detour.find(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of, typed=TYPED)
        self.assertEqual(STEPS[found["to"]]["n"], 10)
        self.assertGreaterEqual(found["margin"], detour.SURE_MARGIN)

    def test_nothing_is_left_out_while_the_captcha_is_on_the_screen(self):
        for now in ("before-step-5", "before-step-6"):
            index = int(now.rsplit("-", 1)[1]) - 1
            with self.subTest(now=now), mock.patch.object(ocr, "available", return_value=True):
                self.assertIsNone(detour.find(STEPS, index, Recorded(now), fixture_screen_of, typed=TYPED))

    def test_a_results_page_at_the_results_step_is_where_it_should_be(self):
        with mock.patch.object(ocr, "available", return_value=True):
            self.assertIsNone(detour.find(STEPS, 9, Recorded("replay-after-search"), fixture_screen_of, typed=TYPED))


class Rules(unittest.TestCase):

    def steps(self, **changes):
        steps = [{"n": n, "kind": "click", "point": [10, 10], "phase": 1, "summary": f"click {n}"} for n in (1, 2, 3)]
        steps[0].update(changes)
        return steps

    def screens(self):
        return {1: detour.Screen(W, H, CHECK), 2: detour.Screen(W, H, CHECK), 3: detour.Screen(W, H, RESULTS)}

    def test_layout_sees_where_the_content_is(self):
        self.assertEqual(detour.layout(W, H, canvas()), set())
        self.assertGreater(detour.layout_score(detour.layout(W, H, RESULTS), detour.layout(W, H, RESULTS)), 0.99)
        # Both have the site's header bar; find() leaves out what every screen shares.
        self.assertLess(detour.layout_score(detour.layout(W, H, RESULTS), detour.layout(W, H, CHECK)), 0.8)

    def test_steps_that_dealt_with_what_is_not_here_are_left_out(self):
        screens = self.screens()
        found = detour.find(self.steps(), 0, detour.Screen(W, H, RESULTS), lambda s: screens[s["n"]], text=False)
        self.assertEqual(found["to"], 2)

    def test_what_could_matter_is_never_a_detour(self):
        screens = self.screens()
        now = detour.Screen(W, H, RESULTS)
        for changes in ({"consequential": True}, {"param": "name", "kind": "type"}, {"by_hand": True}):
            with self.subTest(changes=changes):
                self.assertIsNone(detour.find(self.steps(**changes), 0, now, lambda s: screens[s["n"]], text=False))

    def test_a_detour_does_not_reach_past_a_step_that_types(self):
        screens = self.screens()
        steps = self.steps()
        steps[1].update(kind="type", param="name")
        found = detour.find(steps, 0, detour.Screen(W, H, RESULTS), lambda s: screens[s["n"]], text=False)
        self.assertIsNone(found, "the step that types is not skipped over, and its screen is not what is here")

    def test_another_part_of_the_task_is_out_of_reach(self):
        screens = self.screens()
        steps = self.steps()
        steps[2]["phase"] = 2
        steps[1]["phase"] = 2
        self.assertIsNone(detour.find(steps, 0, detour.Screen(W, H, RESULTS), lambda s: screens[s["n"]], text=False))


class Engine(unittest.TestCase):
    """The ladder's two new questions to the desktop's engine."""

    def setUp(self):
        self.asked = []

    def ladder(self, reply):
        def ask(system, prompt):
            self.asked.append((system, prompt))
            return reply
        ladder = escalation.Ladder(ask_model=ask if reply is not None else None, goal="search")
        ladder.typed = [("rama pethicus", "jimi hendrix greatest")]
        return ladder

    def test_the_engine_can_refuse_a_detour_the_screens_propose(self):
        ladder = self.ladder('{"skip": false, "confidence": 0.9, "reason": "still on the check"}')
        with mock.patch.object(ocr, "available", return_value=True):
            self.assertIsNone(ladder.detour(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of))
        self.assertEqual(len(self.asked), 1)
        self.assertIn("I'm not a robot", self.asked[0][1])
        self.assertIn("jimi hendrix greatest", self.asked[0][1])

    def test_the_engine_carries_a_close_one(self):
        ladder = self.ladder('{"skip": true, "confidence": 0.8, "reason": "robot check not shown"}')
        with mock.patch.object(ocr, "available", return_value=True), \
                mock.patch.object(detour, "SURE_MARGIN", 0.99):
            self.assertEqual(ladder.detour(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of), 9)
        self.assertEqual(ladder.detours[0]["how"], "the engine agreed")
        self.assertIn("left out steps 5-9", ladder.summary())

    def test_without_an_engine_only_a_clear_one_is_taken(self):
        with mock.patch.object(ocr, "available", return_value=False):
            self.assertEqual(self.ladder(None).detour(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of), 9)
            with mock.patch.object(detour, "SURE_MARGIN", 0.99):
                self.assertIsNone(self.ladder(None).detour(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of))

    def test_an_unsure_engine_leaves_it_to_the_screens(self):
        ladder = self.ladder("I think so?")
        with mock.patch.object(ocr, "available", return_value=True):
            self.assertEqual(ladder.detour(STEPS, 4, Recorded("replay-after-search"), fixture_screen_of), 9)
        self.assertEqual(ladder.detours[0]["how"], "the screens say so")

    # -- a point click placed by the text on the screen ------------------

    RECORDED_LINES = [
        {"text": "Britannica", "box": [354, 486, 60, 12]},
        {"text": "Ramapithecus | Early Human Ancestor, Fossil Primate Genus", "box": [322, 521, 475, 18]},
        {"text": "30 Jul 2026 - Ramapithecus, fossil primate", "box": [322, 545, 480, 12]},
    ]
    NOW_LINES = [
        {"text": "Videos", "box": [322, 242, 50, 14]},
        {"text": "Best songs of Jimi Hendrix {COOL MUSIC}", "box": [455, 277, 243, 14]},
        {"text": "YouTube - Cool Music", "box": [455, 307, 110, 12]},
    ]

    def point_step(self, **changes):
        step = {"n": 11, "kind": "click", "point": [459, 531], "summary": "click 459,531"}
        step.update(changes)
        return step

    def ground(self, ladder, step):
        frame = types.SimpleNamespace(width=W, height=H, rgb=b"now")
        with mock.patch.object(ocr, "available", return_value=True), \
                mock.patch.object(ocr, "read", return_value=self.RECORDED_LINES), \
                mock.patch.object(ocr, "read_frame", return_value=self.NOW_LINES):
            return ladder.ground_point(step, frame, (W, H, b"recorded"))

    def test_the_engine_picks_the_line_that_plays_the_same_part(self):
        ladder = self.ladder('{"choice": 2, "confidence": 0.85, "reason": "first result title"}')
        point = self.ground(ladder, self.point_step())
        self.assertEqual(point, (455 + 60, 277 + 7))
        self.assertIn("> Ramapithecus | Early Human Ancestor", self.asked[0][1])
        self.assertIn("2. Best songs of Jimi Hendrix", self.asked[0][1])
        self.assertEqual(ladder.grounded[0]["now"], "Best songs of Jimi Hendrix {COOL MUSIC}")

    def test_an_unsure_or_impossible_answer_places_nothing(self):
        for reply in ('{"choice": 0, "confidence": 0.9}', '{"choice": 2, "confidence": 0.4}',
                      '{"choice": 9, "confidence": 0.99}', "the second one"):
            with self.subTest(reply=reply):
                self.assertIsNone(self.ground(self.ladder(reply), self.point_step()))

    def test_a_step_that_could_do_harm_is_never_placed_by_the_engine(self):
        ladder = self.ladder('{"choice": 2, "confidence": 0.99}')
        self.assertIsNone(self.ground(ladder, self.point_step(consequential=True)))
        self.assertEqual(self.asked, [])


class Screen:
    """core, as a replay sees a pixel-only screen: a frame, no elements, and a log of what was done."""

    def __init__(self, frame):
        self.frame = types.SimpleNamespace(width=W, height=H, rgb=frame)
        self.acted = []

    def observe(self, remember=True):
        return {"elements": [], "windows": [{"title": "Site"}], "active_title": "Site", "size": [W, H], "frame": self.frame}

    def active_group(self, observation):
        return "g"

    def act(self, kind, **kwargs):
        self.acted.append((kind, kwargs))
        return {"status": "done", "text": "ok", "observation": self.observe()}


class Run(unittest.TestCase):

    def setUp(self):
        self.saved = replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS, replay._recorded_frame
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0
        frames = {0: CHECK, 1: CHECK, 2: RESULTS}
        replay._recorded_frame = lambda step: (W, H, frames[step["event"]]) if step.get("event") in frames else None

    def tearDown(self):
        replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS, replay._recorded_frame = self.saved

    def program(self):
        steps = [
            {"n": 1, "event": 0, "kind": "click", "target": {"role": "checkbox", "name": "I'm not a robot"}},
            {"n": 2, "event": 1, "kind": "click", "target": {"role": "button", "name": "Verify"}},
            {"n": 3, "event": 2, "kind": "click", "point": [60, 45]},
        ]
        for step in steps:
            step.update(phase=1, session="demo", summary=f"step {step['n']}", expect=[], window="Site")
        return {"session": "demo", "title": "Search", "params": [], "steps": steps}

    def test_a_run_goes_on_past_a_check_it_never_met(self):
        fake = Screen(RESULTS)
        replay.core = fake
        lines = []
        with mock.patch.object(ocr, "available", return_value=False):
            result = replay.run(self.program(), report=lines.append, ladder=escalation.Ladder())

        self.assertEqual(result["outcome"], replay.COMPLETED_UNVERIFIED, result["text"])
        self.assertEqual([kwargs.get("point") for _, kwargs in fake.acted], [(60, 45)], "only the step after the check ran")
        self.assertEqual(result["detoured"], 2)
        self.assertIn("not needed this time", result["text"])
        self.assertTrue(any(line.startswith("Left out steps 1-2") for line in lines), lines)

    def test_a_plain_replay_still_halts(self):
        replay.core = Screen(RESULTS)
        result = replay.run(self.program(), report=lambda line: None)
        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(result["step"], 1)

    def test_the_check_on_the_screen_is_not_stepped_over(self):
        replay.core = Screen(CHECK)
        with mock.patch.object(ocr, "available", return_value=False):
            result = replay.run(self.program(), report=lambda line: None, ladder=escalation.Ladder())
        self.assertEqual(result["step"], 1)
        self.assertIn(result["outcome"], (replay.HALTED_BEFORE_EFFECT, replay.HALTED))


if __name__ == "__main__":
    unittest.main()
