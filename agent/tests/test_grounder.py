"""
Finding a target by looking at the screen (computer/grounder.py): the point
comes from the grounding model, what is clicked and whether it may be is
decided as for any other click.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_grounder
"""

import json
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from decurix_agent.computer import approval, audit, cli, core, grounder
from decurix_agent.computer.errors import ComputerError
from decurix_agent.computer.screen import Frame


def element(role, name, box, ref):
    return {"role": role, "name": name, "value": "", "states": [], "box": box, "ref": ref,
            "path": f"site x > {role} \"{name}\"", "source": "cdp", "group": "g", "window_title": "Mail - Chromium"}


FRAME = Frame(1280, 720, bytes(1280 * 720 * 3))


def look(*elements):
    return {"elements": list(elements), "frame": FRAME, "windows": [], "groups": [{"id": "g", "title": "Mail"}],
            "active_title": "Mail - Chromium", "size": [1280, 720]}


class ClickingWhatTheModelFound(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = approval.STATE_DIR, approval.STATE_FILE
        approval.STATE_DIR = Path(self.tmp.name)
        approval.STATE_FILE = approval.STATE_DIR / "approval.json"

    def tearDown(self):
        approval.STATE_DIR, approval.STATE_FILE = self.saved
        self.tmp.cleanup()

    def _act(self, screen, at, **target):
        with patch.object(core, "observe", return_value=screen), \
             patch.object(grounder, "available", return_value=True), \
             patch.object(grounder, "locate", return_value=at) as locate, \
             patch.object(core, "_perform", return_value="cdp") as perform, \
             patch.object(core, "_wait_for_settle"), patch.object(audit, "record"):
            result = core.act("click", **target)
        return result, perform, locate

    def test_the_element_under_the_point_is_clicked_and_named(self):
        bell = element("button", "Notifications", [1180, 96, 30, 30], "e4")
        result, perform, locate = self._act(look(bell), (1195, 110), see="the bell icon at the top right")
        self.assertEqual(locate.call_args.args[1], "the bell icon at the top right")
        self.assertIs(perform.call_args.args[1], bell)
        self.assertIn("found \"the bell icon at the top right\" at 1195,110", result["text"])
        self.assertIn('button "Notifications"', result["text"])

    def test_a_name_written_loosely_still_finds_its_element(self):
        box = element("checkbox", "Replace all in:", [100, 380, 120, 20], "e33")
        result, perform, locate = self._act(look(box, element("button", "Replace", [400, 420, 60, 24], "e36")),
                                            (1, 1), see="checkbox Replace all in")
        locate.assert_not_called()
        self.assertIs(perform.call_args.args[1], box)

    def test_a_description_that_is_an_elements_name_needs_no_model(self):
        cell = element("cell", "B6", [200, 300, 80, 20], "e40")
        for words in ("cell B6", "the cell B6", "B6"):
            with self.subTest(words=words):
                result, perform, locate = self._act(look(cell, element("cell", "B5", [200, 280, 80, 20], "e39")),
                                                    (1, 1), see=words)
                locate.assert_not_called()
                self.assertIs(perform.call_args.args[1], cell)
                self.assertNotIn("by looking at the screen", result["text"])


    def test_a_send_button_found_by_eye_still_stops_for_approval(self):
        # The description does not decide what the click is: the control under the point does.
        send = element("button", "Send", [600, 400, 80, 30], "e9")
        result, perform, _ = self._act(look(send), (640, 415), see="the blue arrow icon")
        self.assertEqual(result["status"], "needs_approval")
        perform.assert_not_called()

    def test_a_name_not_in_the_list_is_looked_for(self):
        other = element("link", "Inbox", [10, 100, 60, 20], "e1")
        result, perform, locate = self._act(look(other), (900, 300), name="gear icon")
        self.assertEqual(locate.call_args.args[1], "gear icon")
        self.assertEqual(perform.call_args.args[2], (900, 300))
        self.assertIn("nothing in the element list is there", result["text"])

    def test_two_matches_are_not_settled_by_a_picture(self):
        a = element("button", "Save", [10, 10, 40, 20], "e1")
        b = element("button", "Save", [10, 60, 40, 20], "e2")
        with self.assertRaises(ComputerError) as ctx:
            self._act(look(a, b), (30, 20), name="Save")
        self.assertIn("2 elements match", str(ctx.exception))

    def test_without_a_grounding_model_the_old_error_stands(self):
        with patch.object(core, "observe", return_value=look()), \
             patch.object(grounder, "available", return_value=False), \
             patch.object(grounder, "locate") as locate:
            with self.assertRaises(ComputerError) as ctx:
                core.act("click", name="gear icon")
        self.assertIn("nothing on the screen matches", str(ctx.exception))
        locate.assert_not_called()

    def test_a_ref_that_is_gone_is_not_looked_for(self):
        with patch.object(core, "observe", return_value=look()), \
             patch.object(grounder, "available", return_value=True), \
             patch.object(grounder, "locate") as locate:
            with self.assertRaises(ComputerError):
                core.act("click", ref="e7")
        locate.assert_not_called()


class TheControlPlanesAnswer(unittest.TestCase):

    def _serve(self, status, answer):
        seen = {}

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                self._reply(200, {"configured": True, "model": "qwen3-vl:2b"})

            def do_POST(self):
                seen.update(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
                self._reply(status, answer)

            def _reply(self, code, body):
                data = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        patcher = patch.object(grounder, "BASE_URL", f"http://127.0.0.1:{server.server_port}/api/grounder")
        patcher.start()
        self.addCleanup(patcher.stop)
        patch.object(grounder, "_status", None).start()
        self.addCleanup(patch.stopall)
        return seen

    def test_a_point_comes_back_in_screen_pixels(self):
        seen = self._serve(200, {"x": 640, "y": 360, "model": "qwen3-vl:2b", "seconds": 1.5})
        self.assertEqual(grounder.locate(FRAME, "  the  Save button "), (640, 360))
        self.assertEqual(seen["description"], "the Save button")
        self.assertTrue(seen["image"])

    def test_no_position_says_so(self):
        self._serve(422, {"error": "no position"})
        with self.assertRaises(ComputerError) as ctx:
            grounder.locate(FRAME, "a unicorn")
        self.assertIn("could not find 'a unicorn'", str(ctx.exception))

    def test_a_point_off_the_screen_is_refused(self):
        self._serve(200, {"x": 5000, "y": 10})
        with self.assertRaises(ComputerError):
            grounder.locate(FRAME, "x")


class TheCommand(unittest.TestCase):

    def test_click_see(self):
        with patch.object(cli.core, "act", return_value={"status": "done", "text": "clicked"}) as act:
            self.assertEqual(cli.run(["click", "--see", "the bell icon"]), cli.EXIT_OK)
        act.assert_called_once_with("click", see="the bell icon")

    def test_type_see(self):
        with patch.object(cli.core, "act", return_value={"status": "done", "text": "typed"}) as act:
            cli.run(["type", "--see", "the Notes box", "hello"])
        self.assertEqual(act.call_args.kwargs["see"], "the Notes box")
        self.assertEqual(act.call_args.kwargs["text"], "hello")

    def test_a_target_and_see_together_are_refused(self):
        with self.assertRaises(ComputerError):
            cli.run(["click", "e4", "--see", "the bell icon"])


class AReplayStepPlacedByEye(unittest.TestCase):
    """The replay ladder's last rung before a person: what the person said a point step does, found by the
    grounding model - taken only when it lands on a control."""

    def _ground(self, step, at, elements):
        from decurix_agent.computer import escalation
        ladder = escalation.Ladder(ask_model=None)
        with patch.object(grounder, "available", return_value=True),              patch.object(grounder, "locate", return_value=at) as locate:
            point = ladder.ground_point(step, FRAME, None, elements)
        return point, ladder, locate

    def step(self, **changes):
        step = {"n": 4, "kind": "click", "point": [352, 544], "intent": "opens the first search result"}
        step.update(changes)
        return step

    def test_a_control_found_for_the_persons_words_is_clicked(self):
        result = element("link", "Rama - Wikipedia", [330, 480, 200, 20], "e3")
        point, ladder, locate = self._ground(self.step(), (400, 490), [result])
        self.assertEqual(point, (400, 490))
        self.assertEqual(locate.call_args.args[1], "opens the first search result")
        self.assertEqual(ladder.grounded[0]["how"], "grounder")

    def test_a_point_on_no_control_is_not_clicked(self):
        point, ladder, _ = self._ground(self.step(), (400, 490), [])
        self.assertIsNone(point)
        self.assertFalse(ladder.grounded)
        self.assertIn("not clicking a guess", ladder.trail[-1])

    def test_nothing_is_looked_for_without_the_persons_words_or_for_a_step_that_could_do_harm(self):
        for step in (self.step(intent=""), self.step(consequential=True)):
            with self.subTest(step=step):
                point, _, locate = self._ground(step, (400, 490), [element("link", "x", [330, 480, 200, 20], "e3")])
                self.assertIsNone(point)
                locate.assert_not_called()


if __name__ == "__main__":
    unittest.main()
