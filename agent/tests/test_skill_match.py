import os
import tempfile
import unittest
from pathlib import Path

from decurix_agent import skill_task
from decurix_agent.computer import skills


def _skill(name, goal, params, status=skills.READY, title=None):
    return {"schema": skills.SCHEMA, "name": name, "title": title or goal, "goal": goal, "status": status,
            "params": params, "steps": []}


def _param(name, example, varies=True):
    return {"name": name, "label": name, "example": example, "varies": varies}


class TestNamingASkill(unittest.TestCase):
    def test_a_name_with_spaces_is_read_whole(self):
        self.assertEqual(skill_task.parse_learn('/learn demo-1 name="Google a phrase" demo-2'),
                         (["demo-1", "demo-2"], "Google a phrase"))
        self.assertEqual(skill_task.parse_learn("/learn demo-1 name='Say \"hi\"'"), (["demo-1"], 'Say "hi"'))

    def test_the_request_reads_back_the_same(self):
        for name in ["Google a phrase", 'Say "hi"', "", None]:
            content = skill_task.learn_request(["demo-1"], name)
            self.assertEqual(skill_task.parse_learn(content), (["demo-1"], name or None))
        with self.assertRaises(ValueError):
            skill_task.learn_request(["demo-1"], """both ' and " """)

    def test_a_given_name_never_overwrites_another_skill(self):
        tmp = tempfile.TemporaryDirectory()
        saved = skills.ROOT
        skills.ROOT = Path(tmp.name)
        try:
            skills.save(_skill("google-a-phrase", "x", []))
            self.assertEqual(skills.unique_name("Google a phrase"), "google-a-phrase-2")
        finally:
            skills.ROOT = saved
            tmp.cleanup()


class TestFitScreen(unittest.TestCase):
    """A coordinate replay puts the screen back to the size it was recorded at."""

    def setUp(self):
        from unittest.mock import patch
        from decurix_agent.computer import replay, screen
        self.replay, self.screen = replay, screen
        self.sizes = []
        self.lines = []
        self.patches = [
            patch.object(screen, "size", side_effect=lambda: self.current),
            patch.object(screen, "set_size", side_effect=lambda w, h: self.sizes.append((w, h)) or True),
            patch.object(replay.time, "sleep"),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()

    def test_a_click_recorded_on_another_size_resizes_the_screen(self):
        self.current = (1280, 720)
        self.replay._fit_screen([{"point": [347, 86], "screen": [1527, 727]}, {"kind": "type"}], self.lines.append)
        self.assertEqual(self.sizes, [(1527, 727)])
        self.assertIn("1527x727", self.lines[0])

    def test_nothing_changes_when_it_already_fits_or_nothing_is_a_point(self):
        self.current = (1527, 727)
        self.replay._fit_screen([{"point": [1, 1], "screen": [1527, 727]}], self.lines.append)
        self.current = (1280, 720)
        self.replay._fit_screen([{"target": {"role": "button"}, "screen": [1527, 727]}], self.lines.append)
        self.assertEqual((self.sizes, self.lines), ([], []))

    def test_clicks_recorded_at_two_sizes_are_left_to_halt(self):
        self.current = (1280, 720)
        self.replay._fit_screen([{"point": [1, 1], "screen": [1527, 727]}, {"point": [1, 1], "screen": [800, 600]}],
                                self.lines.append)
        self.assertEqual(self.sizes, [])

    def test_a_screen_resized_again_during_the_run_is_fitted_back(self):
        # A viewer that reconnects mid-run resizes the screen to its browser.
        from unittest.mock import patch
        step = {"n": 1, "kind": "click", "point": [69, 276], "screen": [1536, 864]}
        observations = iter([{"size": [1536, 729]}, {"size": [1536, 864]}])
        with patch.object(self.replay.core, "observe", side_effect=lambda: next(observations)):
            element, point, _, _ = self.replay._find(step)
        self.assertEqual((element, point, self.sizes), (None, [69, 276], [(1536, 864)]))


@unittest.skipIf(os.name == "nt", "xrandr is a Linux desktop's; a Windows worker's screen keeps the computer's size")
class TestSetSize(unittest.TestCase):
    """xrandr is asked for a mode of exactly the recorded size, under a name it will take."""

    def test_a_name_xvnc_keeps_hidden_is_not_the_end_of_it(self):
        # Xvnc keeps "1536x864" from an earlier remote resize but xrandr no
        # longer lists it: --newmode is refused and --addmode cannot find it.
        import subprocess
        from unittest.mock import patch
        from decurix_agent.computer import screen

        state = {"size": (1536, 729)}
        calls = []

        def run(args, **kwargs):
            calls.append(args[1:])
            out = "VNC-0 connected 1536x729+0+0\n" if len(args) == 1 else ""
            if args[1:2] == ["--output"] and args[-1].startswith("decurix-"):
                state["size"] = (1536, 864)
            return subprocess.CompletedProcess(args, 0 if len(args) == 1 else 1, out, "")

        with patch.object(screen, "size", side_effect=lambda: state["size"]), \
                patch.object(screen.session, "ensure"), patch.object(subprocess, "run", side_effect=run):
            self.assertTrue(screen.set_size(1536, 864))
        self.assertIn(["--output", "VNC-0", "--mode", "1536x864"], calls)
        self.assertIn(["--output", "VNC-0", "--mode", "decurix-1536x864"], calls)


class TestClickAnyway(unittest.TestCase):
    """A person can let a point click go without its picture - for one run, never in the saved skill."""

    def test_the_run_goes_without_the_picture_and_the_skill_keeps_it(self):
        from unittest.mock import patch
        from decurix_agent.computer import replay

        step = {"id": "s1", "n": 1, "kind": "click", "point": [347, 86], "screen": [1527, 727], "look": {"png": "x"}}
        skill = _skill("g", "google x", [])
        skill["steps"] = [step]
        run = {"skill": skill, "values": {}, "start": 1, "checked": True, "patches": [], "asked": 0, "stuck": 0,
               "ladder": skill_task._ladder(skill), "asked_ids": []}
        seen = []

        def fake_run(program, **kwargs):
            seen.append(program["steps"][0])
            return {"outcome": replay.HALTED, "text": replay.LOOK_MISSING, "step": 1, "resume_from": 1,
                    "options": [], "trail": []}

        with patch.object(replay, "run", side_effect=fake_run), \
             patch.object(skill_task, "ask_for_query") as asked, \
             patch.object(skill_task, "write_final_reply"):
            skill_task._execute(run)
            self.assertIn("look", seen[0])
            self.assertIn("`click anyway`", asked.call_args[0][0])

            skill_task._stuck_reply(skill_task.state.paused_skill, "click anyway")

        self.assertNotIn("look", seen[1])
        self.assertIn("look", skill["steps"][0])
        skill_task.state.paused_skill = None

    def test_the_words(self):
        for text in ["click anyway", "Click it anyway.", "click there", "anyway"]:
            self.assertTrue(skill_task.ANYWAY_RE.match(text), text)
        for text in ["click Save", "don't click anyway please", "use anyway"]:
            self.assertFalse(skill_task.ANYWAY_RE.match(text), text)


class TestRenamingKeepsTheTask(unittest.TestCase):
    def test_a_renamed_recording_keeps_what_it_was_first_called(self):
        from decurix_agent.computer import recording
        tmp = tempfile.TemporaryDirectory()
        saved = recording.ROOT
        recording.ROOT = Path(tmp.name)
        try:
            recording.ensure("demo-20260923T152346-abcdef", title='google "koustubh is great"')
            recording.update("demo-20260923T152346-abcdef", title="Google a phrase")
            meta = recording.update("demo-20260923T152346-abcdef", title="Search Google")
            self.assertEqual((meta["title"], meta["task"]), ("Search Google", 'google "koustubh is great"'))
        finally:
            recording.ROOT = saved
            tmp.cleanup()


class TestWindowChecksFollowTheValue(unittest.TestCase):
    """A search's result is a window titled with what was typed; a run with another value must look for that."""

    def test_the_title_check_takes_the_runs_value(self):
        from decurix_agent.computer import learning, replay
        skill = {"steps": [{"expect": [
            {"type": "active_window", "value": "koustubh is great - Google Search - Chromium"},
            {"type": "window", "value": "Koustubh is great - Google Search"},
            {"type": "url", "value": "https://www.google.com/search"},
        ]}]}
        learning._follow_param(skill, {"name": "typed_text", "example": "koustubh is great"})

        checks = skill["steps"][0]["expect"]
        self.assertEqual(checks[0]["value"], "{typed_text} - Google Search - Chromium")
        self.assertEqual(checks[1]["value"], "{typed_text} - Google Search")
        self.assertEqual(checks[2]["value"], "https://www.google.com/search")

        seen = {"active_title": "sanmayee is great - Google Search - Chromium",
                "windows": [{"title": "sanmayee is great - Google Search - Chromium"}],
                "page_url": "https://www.google.com/search?q=sanmayee+is+great"}
        self.assertEqual(replay.unmet(checks, seen, {"typed_text": "sanmayee is great"}), [])
        self.assertEqual(len(replay.unmet(checks, seen, {"typed_text": "koustubh is great"})), 2)
