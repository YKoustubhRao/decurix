import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent.computer import launchers, learning, replay

HOST_CHROME = """[Desktop Entry]
Version=1.0
Type=Application
Name=Koustubh - Chrome (Host)
Exec=/home/headless/.local/bin/koustubh-chrome-host %U
Icon=chrome
"""

CHROMIUM = """[Desktop Entry]
Type=Application
Name=Chromium Web Browser
Exec=chromium-browser %U
StartupWMClass=Chromium-browser
"""

# The steps of the skill that stopped twice, live ("mutra", recorded on the
# googl desktop): a nudge of the icon, the double click that opened host
# Chrome, then a click in its window.
NUDGE = {"n": 1, "kind": "drag", "point": [68, 275], "end": [72, 259], "window": "", "expect": [], "screen": [1536, 729]}
OPEN = {"n": 2, "kind": "double_click", "point": [72, 259], "window": "", "screen": [1536, 729],
        "expect": [{"type": "url", "value": "http://decurix:8080/host-gui/view"},
                   {"type": "window", "value": "Koustubh - Chrome"}],
        "look": {"w": 144, "h": 48, "png": ""}}
IN_CHROME = {"n": 3, "kind": "click", "point": [535, 361], "window": "Koustubh - Chrome", "screen": [1536, 729], "expect": []}


class Desktop(unittest.TestCase):
    """A desktop with two launchers and a fake window manager."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        place = Path(self.tmp.name)
        (place / "koustubh-chrome-host.desktop").write_text(HOST_CHROME)
        (place / "chromium-browser.desktop").write_text(CHROMIUM)
        self.windows = []
        self.launched = []
        self.activated = []
        self.lines = []

        def launch(argv):
            self.launched.append(argv)
            self.windows.append({"id": 7, "title": "Koustubh - Chrome", "active": True})

        self.patches = [
            patch.object(launchers, "PLACES", [place]),
            patch.object(launchers.screen, "windows", side_effect=lambda: list(self.windows)),
            patch.object(launchers.session, "launch", side_effect=launch),
            patch.object(launchers.xinput, "activate", side_effect=self.activated.append),
            patch.object(launchers.time, "sleep"),
            patch.object(replay.time, "sleep"),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        self.tmp.cleanup()


class TestFindingTheLauncher(Desktop):
    def test_a_window_is_matched_to_the_launcher_that_opens_it(self):
        self.assertEqual(launchers.for_window("Koustubh - Chrome")["name"], "Koustubh - Chrome (Host)")
        self.assertEqual(launchers.for_window("New Tab - Chromium-browser")["name"], "Chromium Web Browser")
        self.assertIsNone(launchers.for_window("Untitled 1 - Mousepad"))

    def test_the_browsers_own_icon_opens_a_browser_window_whatever_it_is_called(self):
        # Since the desktops run Chrome the icon is "Google Chrome", while a
        # page's window keeps the canonical "... - Chromium" title.
        place = Path(self.tmp.name)
        (place / "chromium-browser.desktop").unlink()
        (place / "decurix-desktop-browser.desktop").write_text(
            "[Desktop Entry]\nType=Application\nName=Google Chrome\nExec=chromium-browser %U\n")
        (place / "chatgpt-host.desktop").write_text(
            "[Desktop Entry]\nType=Application\nName=ChatGPT\nExec=chromium-browser --app=https://chatgpt.com\n")
        self.assertEqual(launchers.for_window("Google - Chromium")["name"], "Google Chrome")
        self.assertEqual(launchers.for_window("Koustubh - Chrome")["name"], "Koustubh - Chrome (Host)")

    def test_two_launchers_that_fit_equally_are_not_guessed_between(self):
        (Path(self.tmp.name) / "other.desktop").write_text(HOST_CHROME.replace("koustubh-chrome-host", "other-chrome"))
        self.assertIsNone(launchers.for_window("Koustubh - Chrome"))

    def test_an_app_is_found_by_the_name_a_person_would_say(self):
        # Found live: an engine asked to open an app by its name was refused, and double-clicked around its icon instead.
        (Path(self.tmp.name) / "books-host.desktop").write_text(
            "[Desktop Entry]\nType=Application\nName=Books (India Compliance) (Host)\nExec=/home/headless/.local/bin/books-host %U\n")
        self.assertEqual(launchers.by_name("books")["name"], "Books (India Compliance) (Host)")
        self.assertEqual(launchers.by_name("Books (India Compliance) (Host)")["name"], "Books (India Compliance) (Host)")
        self.assertEqual(launchers.by_name("Chromium Web Browser")["name"], "Chromium Web Browser")
        self.assertEqual(launchers.by_name("chromium")["name"], "Chromium Web Browser")
        self.assertIsNone(launchers.by_name("chrom"))
        self.assertIsNone(launchers.by_name("Mousepad"))
        self.assertIsNone(launchers.by_name(""))

    def test_a_name_two_apps_share_is_not_guessed_between(self):
        place = Path(self.tmp.name)
        (place / "a.desktop").write_text("[Desktop Entry]\nType=Application\nName=Books (Host)\nExec=/bin/a\n")
        (place / "b.desktop").write_text("[Desktop Entry]\nType=Application\nName=Books Viewer\nExec=/bin/b\n")
        self.assertEqual(launchers.by_name("books")["exec"], "/bin/a")
        (place / "c.desktop").write_text("[Desktop Entry]\nType=Application\nName=Books (Test)\nExec=/bin/c\n")
        self.assertIsNone(launchers.by_name("books"))

    def test_the_command_loses_its_field_codes(self):
        self.assertEqual(launchers.argv(launchers.for_window("Koustubh - Chrome")),
                         ["/home/headless/.local/bin/koustubh-chrome-host"])


class TestReplayingTheDesktop(Desktop):
    def test_the_icon_nudge_is_nothing_to_do(self):
        with patch.object(replay.core, "act") as act:
            result = replay._run_step(NUDGE, {}, report=self.lines.append)
        act.assert_not_called()
        self.assertEqual(result["status"], "done")
        self.assertIn("nudged an icon", self.lines[0])

    def test_double_clicking_an_icon_opens_its_launcher_not_its_picture(self):
        # Live, the picture was mostly a hover tooltip, and the icon was drawn
        # selected: it never matched.
        with patch.object(replay.core, "act") as act, patch.object(replay.core, "observe") as observe:
            result = replay._run_step(OPEN, {}, report=self.lines.append)
        act.assert_not_called()
        observe.assert_not_called()
        self.assertEqual(result["status"], "done")
        self.assertEqual(self.launched, [["/home/headless/.local/bin/koustubh-chrome-host"]])
        self.assertEqual(self.activated, [7])
        self.assertIn("from its launcher", self.lines[0])

    def test_next_steps_window_opens_desktop_icon_when_result_check_is_missing(self):
        # A learned skill can lose a window check when demonstrations differ.
        step = dict(OPEN, n=1, expect=[])
        next_step = dict(IN_CHROME, n=2)
        with patch.object(replay.core, "act") as act, patch.object(replay.core, "observe") as observe:
            result = replay._run_step(step, {}, report=self.lines.append, next_step=next_step)
        act.assert_not_called()
        observe.assert_not_called()
        self.assertEqual(self.launched, [["/home/headless/.local/bin/koustubh-chrome-host"]])
        self.assertEqual(result["raised"]["window"], "Koustubh - Chrome")

    def test_run_uses_following_steps_window_for_desktop_icon(self):
        first = dict(OPEN, n=1, expect=[], summary="double click 72,262")
        second = dict(IN_CHROME, n=2, expect=[], summary="click 535,361")
        observation = {"size": [1536, 729], "active_title": "Koustubh - Chrome", "frame": object()}
        with patch.object(replay.core, "observe", return_value=observation), \
             patch.object(replay.core, "act", return_value={"status": "done", "observation": observation}) as act, \
             patch.object(replay, "_fit_screen"):
            result = replay.run({"steps": [first, second], "params": [], "title": "kol"}, report=self.lines.append)
        self.assertIn(result["outcome"], replay.SUCCESS)
        self.assertEqual(self.launched, [["/home/headless/.local/bin/koustubh-chrome-host"]])
        act.assert_called_once()
        self.assertEqual(act.call_args.kwargs["point"], (535, 361))

    def test_an_unrelated_following_step_does_not_choose_a_launcher(self):
        step = dict(OPEN, n=1, expect=[])
        self.assertIsNone(replay._open_desktop_icon(step, dict(IN_CHROME, n=3)))
        self.assertEqual(self.launched, [])

    def test_an_app_already_open_is_raised_not_opened_twice(self):
        self.windows.append({"id": 9, "title": "Koustubh - Chrome", "active": False})
        replay._run_step(OPEN, {}, report=self.lines.append)
        self.assertEqual((self.launched, self.activated), ([], [9]))

    def test_a_window_that_never_appears_halts_and_says_so(self):
        with patch.object(launchers.session, "launch"), patch.object(launchers, "LAUNCH_SECONDS", 0):
            with self.assertRaises(replay._Halt) as halt:
                replay._run_step(OPEN, {}, report=self.lines.append)
        self.assertIn("Koustubh - Chrome", halt.exception.reason)

    def test_a_click_with_no_launcher_for_its_window_is_replayed_as_recorded(self):
        step = dict(OPEN, expect=[{"type": "window", "value": "Untitled 1 - Mousepad"}])
        self.assertIsNone(replay._open_desktop_icon(step))

    def test_a_step_whose_window_is_not_open_opens_it_first(self):
        # Live: `skip` on the double click, then the next step stopped because
        # "Koustubh - Chrome" was not in front.
        observations = iter([
            {"size": [1536, 729], "active_title": ""},
            {"size": [1536, 729], "active_title": "Koustubh - Chrome"},
        ])
        with patch.object(replay.core, "observe", side_effect=lambda: next(observations)):
            element, point, _, _ = replay._find(IN_CHROME, report=self.lines.append)
        self.assertEqual((element, point), (None, [535, 361]))
        self.assertEqual(self.launched, [["/home/headless/.local/bin/koustubh-chrome-host"]])
        self.assertIn("Step 3: opened", self.lines[0])


class TestLearningLeavesTheNudgeOut(unittest.TestCase):
    def test_a_desktop_nudge_is_tidied_away(self):
        program = {"steps": [dict(NUDGE), dict(OPEN), dict(IN_CHROME)], "params": []}
        notes = learning._tidy(program, [])
        self.assertEqual([s["kind"] for s in program["steps"]], ["double_click", "click"])
        self.assertEqual([s["n"] for s in program["steps"]], [1, 2])
        self.assertIn("nudged an icon", notes[0])

    def test_a_real_drag_on_the_desktop_stays(self):
        drag = dict(NUDGE, end=[400, 300])
        program = {"steps": [drag, dict(OPEN)], "params": []}
        learning._tidy(program, [])
        self.assertEqual(program["steps"][0]["kind"], "drag")


class TestAnAppFoundOpenAlready(Desktop):
    """Live: Chrome was open on Wikipedia, and the start page's search box was nowhere."""

    def _run(self):
        steps = [dict(OPEN, n=1, summary="double click 72,259"),
                 dict(IN_CHROME, n=2, summary="click 535,361", look={"w": 144, "h": 48, "png": ""})]
        observation = {"size": [1536, 729], "active_title": "Koustubh - Chrome", "frame": object()}
        with patch.object(replay.core, "observe", return_value=observation), \
                patch.object(replay.visual, "find", return_value={"status": replay.visual.MISSING}), \
                patch.object(replay, "FIND_SECONDS", 0), patch.object(replay, "_fit_screen"), \
                patch.object(replay, "_await_checks", side_effect=lambda checks, observation, values, seconds=None: ([], observation)):
            return replay.run({"steps": steps, "params": [], "title": "mutra"}, report=self.lines.append)

    def test_the_halt_says_the_app_was_open_already(self):
        self.windows.append({"id": 9, "title": "Koustubh - Chrome", "active": False})
        result = self._run()
        self.assertEqual(result["outcome"], replay.HALTED)
        self.assertEqual(result["reopen"], {"window": "Koustubh - Chrome", "step": 1, "already": True})
        self.assertEqual(self.launched, [])

    def test_an_app_its_launcher_opened_is_offered_a_restart_too(self):
        # A host app keeps running between uses: its launcher reconnects to it
        # on the page it was left on.
        result = self._run()
        self.assertEqual(result["reopen"], {"window": "Koustubh - Chrome", "step": 1, "already": False})

    def test_a_step_in_another_window_is_not_offered_a_restart(self):
        self.windows.append({"id": 9, "title": "Koustubh - Chrome", "active": False})
        steps = [dict(OPEN, n=1, summary="open"),
                 dict(IN_CHROME, n=2, summary="click", window="Untitled 1 - Mousepad", look={"w": 1, "h": 1, "png": ""})]
        observation = {"size": [1536, 729], "active_title": "Untitled 1 - Mousepad", "frame": object()}
        with patch.object(replay.core, "observe", return_value=observation), \
                patch.object(replay.visual, "find", return_value={"status": replay.visual.MISSING}), \
                patch.object(replay, "FIND_SECONDS", 0), patch.object(replay, "_fit_screen"), \
                patch.object(replay, "_await_checks", side_effect=lambda checks, observation, values, seconds=None: ([], observation)):
            result = replay.run({"steps": steps, "params": [], "title": "mutra"}, report=self.lines.append)
        self.assertIsNone(result["reopen"])


class TestRestartReply(unittest.TestCase):
    def setUp(self):
        from decurix_agent import skill_task
        self.skill_task = skill_task
        self.run = {"expanded": {"steps": [dict(OPEN, n=1, id="s1"), dict(IN_CHROME, n=2, id="s2")]},
                    "asked_ids": [], "start": 1}
        self.paused = {"run": self.run, "step": 2, "ran": False, "resume_from": 2, "why": replay.LOOK_MISSING,
                       "unclear": 0, "options": [], "trail": [],
                       "reopen": {"window": "Koustubh - Chrome", "step": 1}}

    def test_restart_closes_the_app_and_runs_from_the_step_that_opens_it(self):
        with patch.object(self.skill_task.launchers, "close", return_value=True) as close, \
                patch.object(self.skill_task, "_execute") as execute, patch.object(self.skill_task.progress, "emit"):
            self.skill_task._stuck_reply(self.paused, "restart")
        close.assert_called_once_with("Koustubh - Chrome", replay.title_matches)
        execute.assert_called_once()
        self.assertEqual(self.run["start"], 1)

    def test_restart_is_only_read_when_it_was_offered(self):
        self.paused["reopen"] = None
        with patch.object(self.skill_task.launchers, "close") as close, \
                patch.object(self.skill_task, "_ask_stuck") as ask:
            self.skill_task._stuck_reply(self.paused, "restart")
        close.assert_not_called()
        ask.assert_called_once()

    def test_the_question_offers_it_in_plain_words(self):
        with patch.object(self.skill_task, "_pause") as pause:
            self.skill_task._ask_stuck(self.run, {"step": 2, "resume_from": 2, "text": "Stopped at step 2.",
                                                  "reopen": {"window": "Koustubh - Chrome", "step": 1, "already": True}})
        text = pause.call_args[0][2]
        self.assertIn("was already open", text)
        self.assertIn("`restart`", text)
        self.assertEqual(pause.call_args[1]["reopen"]["window"], "Koustubh - Chrome")


class TestAStepAfterTheTypedTextChanged(unittest.TestCase):
    def test_the_person_is_asked_to_click_it_and_carry_on(self):
        from decurix_agent import skill_task
        run = {"expanded": {"steps": [dict(IN_CHROME, n=1, id="s6", look={"w": 1, "h": 1, "png": ""})]}, "asked_ids": []}
        with patch.object(skill_task, "_pause") as pause:
            skill_task._ask_stuck(run, {"step": 1, "resume_from": 1, "text": "Stopped: " + replay.LOOK_MISSING,
                                        "after_change": True, "reopen": None})
        text = pause.call_args[0][2]
        self.assertIn("click it yourself", text)
        self.assertIn("`done`", text)
        self.assertNotIn("`restart`", text)
        # "done" carries on past the step.
        self.assertTrue(skill_task.CARRY_ON_RE.match("done"))


class TestRestartIsOnlyOfferedBeforeAnythingWorkedThere(Desktop):
    def test_a_stop_after_steps_worked_in_the_window_offers_no_restart(self):
        self.windows.append({"id": 9, "title": "Koustubh - Chrome", "active": True})
        steps = [dict(OPEN, n=1, summary="open"),
                 dict(IN_CHROME, n=2, summary="click the box"),
                 dict(IN_CHROME, n=3, summary="click the result", look={"w": 1, "h": 1, "png": ""})]
        observation = {"size": [1536, 729], "active_title": "Koustubh - Chrome", "frame": object()}
        with patch.object(replay.core, "observe", return_value=observation), \
                patch.object(replay.core, "act", return_value={"status": "done", "observation": observation}), \
                patch.object(replay.visual, "find", return_value={"status": replay.visual.MISSING}), \
                patch.object(replay, "FIND_SECONDS", 0), patch.object(replay, "_fit_screen"), \
                patch.object(replay, "_await_checks", side_effect=lambda checks, observation, values, seconds=None: ([], observation)):
            result = replay.run({"steps": steps, "params": [], "title": "mutra"}, report=self.lines.append)
        self.assertEqual((result["outcome"], result["step"]), (replay.HALTED, 3))
        self.assertIsNone(result["reopen"])


if __name__ == "__main__":
    unittest.main()
