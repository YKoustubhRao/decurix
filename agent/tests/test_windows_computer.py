"""
The computer-use layer of a native Windows worker (computer/win.py and what
hands over to it), tested on any system: the helper program is stood in for by
a script speaking its line protocol, and UI Automation by the lists it returns.
What only a real private desktop can show is checked there, live.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import cliargs
from decurix_agent.computer import browser, cdp, core, demonstration, processing, screen, win
from decurix_agent.computer.errors import ComputerError

# A helper that answers like decurix-ui.exe: a JSON line per request, a
# picture's bytes after the line that announces them, and - told to - an exit
# in the middle of a request, the way a crashed helper looks.
FAKE_HELPER = textwrap.dedent("""
    import json, os, sys
    out = sys.stdout.buffer
    for raw in sys.stdin.buffer:
        request = json.loads(raw)
        op = request["op"]
        if op == "die":
            marker = request["marker"]
            if not os.path.exists(marker):
                open(marker, "w").close()
                sys.exit(1)
            reply = {"ok": True, "second": True}
        elif op == "grab":
            data = bytes(range(12))
            out.write((json.dumps({"ok": True, "width": 2, "height": 2, "bytes": len(data)}) + "\\n").encode())
            out.write(data)
            out.flush()
            continue
        elif op == "fail":
            reply = {"ok": False, "error": "InvalidOperationException: the window is gone"}
        else:
            reply = {"ok": True, "echo": request}
        out.write((json.dumps(reply) + "\\n").encode())
        out.flush()
""")


class TheHelperProtocol(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        script = Path(self.dir.name) / "helper.py"
        script.write_text(FAKE_HELPER, encoding="utf-8")

        def start():
            return subprocess.Popen([sys.executable, str(script)], stdin=subprocess.PIPE, stdout=subprocess.PIPE)

        self.patch = patch.object(win, "_start", side_effect=start)
        self.started = self.patch.start()
        win._process = None

    def tearDown(self):
        self.patch.stop()
        if win._process is not None:
            win._process.kill()
            win._process.wait()
            win._process.stdin.close()
            win._process.stdout.close()
            win._process = None
        self.dir.cleanup()

    def test_a_request_is_one_line_and_its_reply_another(self):
        reply = win.call("click", x=5, y=7)
        self.assertEqual(reply["echo"], {"op": "click", "x": 5, "y": 7})
        # One helper serves every request.
        win.call("ping")
        self.assertEqual(self.started.call_count, 1)

    def test_a_picture_comes_as_bytes_after_its_line(self):
        reply = win.call("grab")
        self.assertEqual(bytes(reply["data"]), bytes(range(12)))
        # The stream is still in step for the next request.
        self.assertEqual(win.call("ping")["echo"], {"op": "ping"})

    def test_the_helpers_error_is_raised_in_plain_words(self):
        with self.assertRaises(ComputerError) as ctx:
            win.call("fail")
        self.assertEqual(str(ctx.exception), "the window is gone")

    def test_a_helper_that_died_is_replaced_once(self):
        marker = str(Path(self.dir.name) / "died")
        self.assertTrue(win.call("die", marker=marker)["second"])
        self.assertEqual(self.started.call_count, 2)


def node(role, name="", **more):
    base = {"role": role, "name": name, "box": [10, 20, 80, 24], "rid": [1, 2], "patterns": [], "states": []}
    base.update(more)
    return base


class ReadingWindowsControls(unittest.TestCase):
    def observe(self, windows, texts=None):
        def call(op, **fields):
            if op == "elements":
                return {"ok": True, "windows": windows}
            if op == "text":
                return {"ok": True, "text": (texts or {}).get(tuple(fields["rid"]), "")}
            raise AssertionError(f"unexpected {op}")

        with patch.object(win, "call", side_effect=call):
            return win.observe()

    def test_controls_become_elements_with_the_roles_every_backend_uses(self):
        found, groups = self.observe([{
            "hwnd": 100, "pid": 7, "app": "charmap", "title": "Character Map",
            "nodes": [
                node("Button", "Select", patterns=["invoke"], framework="Win32"),
                node("CheckBox", "Advanced view", patterns=["toggle"], states=["checked"]),
                node("Edit", "", label="Characters to copy :", patterns=["value"]),
                node("Text", "Font :"),
                # Structure nobody acts on, and a control with no name to ask for it by.
                node("Pane", "Workspace"),
                node("Button", ""),
            ],
        }])

        self.assertEqual([(e["role"], e["name"]) for e in found], [
            ("button", "Select"), ("checkbox", "Advanced view"),
            ("textbox", "Characters to copy"), ("text", "Font :"),
        ])
        self.assertEqual(groups[0]["window_title"], "Character Map")
        self.assertEqual(found[0]["source"], "uia")
        self.assertEqual(found[0]["path"], 'charmap window "Character Map" > button "Select"')
        self.assertEqual(found[1]["states"], ["checked"])

    def test_a_password_fields_value_is_never_read(self):
        found, _ = self.observe([{"hwnd": 1, "pid": 1, "app": "app", "title": "Sign in", "nodes": [
            node("Edit", "Password", password=True, value="hunter2", patterns=["value"]),
        ]}])
        self.assertEqual(found[0]["value"], "")
        # What privacy.py reads to withhold what is typed into it.
        self.assertEqual(found[0]["handle"]["raw_role"], "password text")

    def test_an_open_menu_is_listed_as_part_of_the_window_it_belongs_to(self):
        found, groups = self.observe([
            {"hwnd": 1, "pid": 9, "app": "notepad", "title": "Untitled - Notepad",
             "nodes": [node("MenuItem", "File", patterns=["expand"])]},
            {"hwnd": 2, "pid": 9, "app": "notepad", "title": "", "popup": True,
             "nodes": [node("MenuItem", "Save", patterns=["invoke"])]},
        ])
        self.assertEqual(len(groups), 1)
        self.assertEqual({e["group"] for e in found}, {groups[0]["id"]})
        self.assertEqual(found[1]["window_title"], "Untitled - Notepad")

    def test_an_unsaved_mark_does_not_change_a_controls_path(self):
        saved, _ = self.observe([{"hwnd": 1, "pid": 9, "app": "notepad", "title": "notes.txt - Notepad",
                                  "nodes": [node("Document", "Text editor", patterns=["text"])]}])
        edited, _ = self.observe([{"hwnd": 1, "pid": 9, "app": "notepad", "title": "*notes.txt - Notepad",
                                   "nodes": [node("Document", "Text editor", patterns=["text"])]}])
        self.assertEqual(saved[0]["path"], edited[0]["path"])

    def test_a_documents_text_is_read_for_its_value(self):
        found, _ = self.observe(
            [{"hwnd": 1, "pid": 9, "app": "notepad", "title": "Untitled - Notepad",
              "nodes": [node("Document", "Text editor", patterns=["text"], rid=[4])]}],
            texts={(4,): "hello world\r\n"})
        self.assertEqual(found[0]["value"], "hello world")

    def test_a_browsers_window_is_called_chromium_whatever_it_is(self):
        with patch.object(win, "call", return_value={"ok": True, "windows": [
            {"id": 5, "title": "Example Domain - Personal - Microsoft\u200b Edge", "pid": 3, "box": [0, 0, 800, 600],
             "active": True}]}):
            listed = win.windows()
        self.assertEqual(listed[0]["title"], "Example Domain - Chromium")
        self.assertEqual(listed[0]["raw_title"], "Example Domain - Personal - Microsoft\u200b Edge")
        self.assertEqual(listed[0]["box"], (0, 0, 800, 600))


class WorkingWindowsControls(unittest.TestCase):
    """
    Measured on a private desktop: UI Automation's Invoke, Toggle and SetValue
    on a classic (Win32, WinForms) control report success and do nothing, or
    change what is shown without telling the app. Those are clicked and typed
    into; a modern control's own pattern is used.
    """

    def element(self, framework, patterns=("invoke", "value")):
        return {"role": "button", "name": "OK", "handle": {"hwnd": 1, "rid": [3], "framework": framework,
                                                           "patterns": list(patterns)}}

    def test_a_classic_control_is_left_to_a_click_and_to_typing(self):
        for framework in ("Win32", "WinForm", ""):
            with patch.object(win, "call") as call:
                self.assertIsNone(win.default_action(self.element(framework)))
                self.assertFalse(win.set_text(self.element(framework), "x"))
                self.assertFalse(win.grab_focus(self.element(framework)))
                call.assert_not_called()

    def test_a_modern_control_is_worked_through_its_own_pattern(self):
        with patch.object(win, "call", return_value={"ok": True, "did": "invoke", "set": True}) as call:
            self.assertEqual(win.default_action(self.element("XAML")), "invoke")
            self.assertTrue(win.set_text(self.element("XAML"), "hello"))
        call.assert_any_call("invoke", hwnd=1, rid=[3])
        call.assert_any_call("set_value", text="hello", hwnd=1, rid=[3])

    def test_a_field_without_a_value_pattern_is_typed_into(self):
        with patch.object(win, "call") as call:
            self.assertFalse(win.set_text(self.element("XAML", patterns=("invoke",)), "hello"))
            call.assert_not_called()

    def test_a_private_desktop_keeps_the_computers_own_size(self):
        with patch.object(win, "call", return_value={"ok": True, "screen": [1920, 1080]}):
            self.assertTrue(win.set_size(1920, 1080))
            self.assertFalse(win.set_size(1280, 720))


class OpeningThingsOnAWorkersScreen(unittest.TestCase):
    def test_a_page_file_is_for_the_browser(self):
        with tempfile.TemporaryDirectory() as folder:
            page = Path(folder) / "form.html"
            page.write_text("<p>hi</p>")
            kind, address = win.open_argv(str(page))
        self.assertEqual(kind, "url")
        self.assertTrue(address.startswith("file:") and address.endswith("form.html"))

    def test_a_folder_is_refused_with_what_to_do_instead(self):
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaises(ComputerError) as ctx:
                win.open_argv(folder)
        self.assertIn("Get-ChildItem", str(ctx.exception))

    def test_a_file_opens_in_the_program_windows_names_for_it(self):
        with tempfile.TemporaryDirectory() as folder:
            sheet = Path(folder) / "book.xlsx"
            sheet.write_bytes(b"")
            with patch.object(win, "associated_program", return_value=r"C:\Office\excel.exe"):
                argv = win.open_argv(str(sheet))
            self.assertEqual(argv, [r"C:\Office\excel.exe", str(sheet.resolve())])
            # A kind only a Store app opens has no program to start here.
            with patch.object(win, "associated_program", return_value=None):
                with self.assertRaises(ComputerError):
                    win.open_argv(str(sheet))

    def test_an_app_is_found_by_the_name_a_person_gives_it(self):
        with patch.object(win, "find_program", side_effect=lambda name: rf"C:\Windows\{name}") as find:
            self.assertEqual(win.open_argv("Character Map"), [r"C:\Windows\charmap.exe"])
            # A program named with its own arguments keeps them.
            self.assertEqual(win.open_argv("winword /safe"), [r"C:\Windows\winword", "/safe"])
        find.assert_any_call("charmap.exe")

    def test_what_is_not_installed_says_what_can_be_opened(self):
        with patch.object(win, "find_program", return_value=None):
            with self.assertRaises(ComputerError) as ctx:
                win.open_argv("photoshop")
        self.assertIn("notepad", str(ctx.exception))


class ArgumentsPassedByPowerShell(unittest.TestCase):
    def test_arguments_given_directly_win(self):
        with patch.dict(os.environ, {cliargs.VARIABLE: "observe"}):
            self.assertEqual(cliargs.from_environment(["click", "e1"]), ["click", "e1"])

    def test_the_variable_carries_each_argument_whole(self):
        passed = cliargs.SEPARATOR.join(["click", 'button "Save"', "it's here"])
        with patch.dict(os.environ, {cliargs.VARIABLE: passed}):
            self.assertEqual(cliargs.from_environment(), ["click", 'button "Save"', "it's here"])

    def test_an_empty_variable_is_no_arguments(self):
        with patch.dict(os.environ, {cliargs.VARIABLE: ""}), patch.object(sys, "argv", ["gui", "stale"]):
            self.assertEqual(cliargs.from_environment(), [])

    def test_without_the_variable_it_is_the_command_line(self):
        env = {k: v for k, v in os.environ.items() if k != cliargs.VARIABLE}
        with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["gui", "observe"]):
            self.assertEqual(cliargs.from_environment(), ["observe"])


class EdgeIsTheBrowserOnWindows(unittest.TestCase):
    def test_its_titles_read_like_every_recordings(self):
        for raw, want in [
            ("Example Domain - Microsoft Edge", "Example Domain - Chromium"),
            ("Example Domain - Microsoft\u200b Edge", "Example Domain - Chromium"),
            ("Example Domain - Personal - Microsoft\u200b Edge", "Example Domain - Chromium"),
            ("Orders - Profile 2 - Microsoft Edge", "Orders - Chromium"),
            ("Example Domain - Google Chrome", "Example Domain - Chromium"),
        ]:
            self.assertEqual(browser.canonical_title(raw), want, raw)
            self.assertTrue(browser.is_browser_title(raw), raw)

    def test_a_page_about_work_keeps_its_title(self):
        # "Work" is an Edge profile's name only directly before the product.
        self.assertEqual(browser.canonical_title("Work - Tasks - Microsoft Edge"), "Work - Tasks - Chromium")

    def test_another_programs_window_is_left_alone(self):
        self.assertEqual(browser.canonical_title("Untitled - Notepad"), "Untitled - Notepad")
        self.assertFalse(browser.is_browser_title("Untitled - Notepad"))


class TheBrowserPickedForTheWorkerIsTheOneItRuns(unittest.TestCase):
    """Found live: Chrome was copied to a worker and used, and the agent opened its pages in Edge."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir, True)
        env = patch.dict(os.environ, {"DECURIX_DIR": str(self.dir), "DECURIX_CDP_PORT": "9333"})
        env.start()
        self.addCleanup(env.stop)
        # Which browser is run is what is tested here - not the copying of the
        # profile of whoever runs the tests (it copied their real one, each test).
        if os.name == "nt":
            from decurix_agent.computer import profile_seed
            seed = patch.object(profile_seed, "seed", lambda *args, **kwargs: "")
            seed.start()
            self.addCleanup(seed.stop)

    def test_the_browser_opened_for_itself_is_given_no_page(self):
        # It was given about:blank: a person who pressed the worker's Chrome
        # icon got an empty white tab instead of the browser's own first page.
        self.pick(kind="chrome")
        with self.installed("chrome"):
            argv = browser.windows_argv("")

        self.assertTrue(argv[-1].startswith("--"), argv)
        self.assertNotIn("about:blank", argv)
        self.assertNotIn("", argv)

    def pick(self, **choice):
        (self.dir / browser.CHOICE_FILE).write_text(json.dumps(choice), encoding="utf-8")

    def installed(self, *kinds):
        return patch.object(browser, "_installed", lambda kind: f"C:\\{kind}.exe" if kind in kinds else "")

    def test_the_picked_browser_is_run_with_a_profile_of_its_own(self):
        self.pick(kind="chrome")
        with self.installed("msedge", "chrome"):
            argv = browser.windows_argv("https://example.com")
        self.assertEqual(argv[0], "C:\\chrome.exe")
        self.assertIn("--remote-debugging-port=9333", argv)
        self.assertIn(f"--user-data-dir={self.dir / 'browser-chrome'}", argv)
        with self.installed("msedge", "chrome"):
            self.assertEqual(browser.windows_browser_name(), "Google Chrome")

    def test_the_program_the_shortcut_names_is_the_one_run(self):
        exe = self.dir / "chrome.exe"
        exe.write_bytes(b"")
        self.pick(kind="chrome", exe=str(exe))
        with self.installed("msedge"):
            self.assertEqual(browser.windows_browser(), ("chrome", str(exe)))

    def test_a_picked_browser_that_is_gone_is_not_swapped_for_another(self):
        self.pick(kind="brave")
        with self.installed("msedge", "chrome"):
            with self.assertRaises(ComputerError) as ctx:
                browser.windows_browser()
            self.assertEqual(browser.windows_browser_name(), "Brave")
        self.assertIn("Brave", str(ctx.exception))

    def test_with_none_picked_it_is_whichever_the_computer_has(self):
        with self.installed("msedge", "chrome"):
            self.assertEqual(browser.windows_browser(), ("msedge", "C:\\msedge.exe"))
            self.assertIn(f"--user-data-dir={self.dir / 'browser'}", browser.windows_argv("about:blank"))
        with self.installed("chrome"):
            self.assertEqual(browser.windows_browser(), ("chrome", "C:\\chrome.exe"))

    def test_the_profile_the_workers_browser_already_has_is_the_one_used(self):
        # Found live: a person signed in to a site in the worker's Chrome,
        # and the agent then opened the site in a profile of its own.
        kept = self.dir / "kept"
        kept.mkdir()
        self.pick(kind="chrome", profile_directory="Profile 2", user_data_dir=str(kept))
        with self.installed("msedge", "chrome"):
            argv = browser.windows_argv("https://example.com")
        self.assertEqual(argv[0], "C:\\chrome.exe")
        self.assertIn(f"--user-data-dir={kept}", argv)
        self.assertIn("--profile-directory=Profile 2", argv)
        self.assertEqual(argv[-1], "https://example.com")

    def test_a_kept_profile_that_is_gone_is_not_used(self):
        self.pick(kind="chrome", user_data_dir=str(self.dir / "gone"))
        with self.installed("chrome"):
            argv = browser.windows_argv("about:blank")
        self.assertIn(f"--user-data-dir={self.dir / 'browser-chrome'}", argv)
        self.assertFalse([a for a in argv if a.startswith("--profile-directory")])

    def test_a_choice_that_is_not_a_browser_it_knows_is_no_choice(self):
        self.pick(kind="netscape")
        self.assertIsNone(browser.chosen())


class WhereAPageIsOnTheScreen(unittest.TestCase):
    METRICS = {"dpr": 1.25, "sx": 100, "sy": 50, "ow": 816, "oh": 700, "iw": 800, "ih": 600}

    def test_a_linux_desktop_adds_the_browsers_bars(self):
        with patch.object(cdp.os, "name", "posix"):
            self.assertEqual(cdp._viewport(dict(self.METRICS, dpr=1)), (116, 150, 1))

    def test_windows_asks_the_browsers_own_content_window(self):
        with patch.object(cdp.os, "name", "nt"), patch.object(win, "browser_viewport", return_value=(277, 238)):
            self.assertEqual(cdp._viewport(self.METRICS), (277, 238, 1.25))

    def test_windows_without_an_answer_scales_the_arithmetic(self):
        with patch.object(cdp.os, "name", "nt"), patch.object(win, "browser_viewport", return_value=None):
            x, y, dpr = cdp._viewport(self.METRICS)
        self.assertEqual((x, y, dpr), ((100 + 8) * 1.25, (50 + 100 - 8) * 1.25, 1.25))


class TypingNeedsAFieldOnWindowsToo(unittest.TestCase):
    """Found live: `gui type e40 "Hello World"` with e40 a toolbar button of Notepad typed nothing."""

    def screen(self):
        def element(role, name, ref, states=()):
            return {"role": role, "name": name, "source": "uia", "ref": ref, "states": list(states),
                    "path": f'notepad window "Untitled - Notepad" > {role} "{name}"', "box": [0, 0, 50, 20]}

        return {"frame": None, "windows": [], "elements": [
            element("document", "Text editor", "e28"),
            element("button", "Strikethrough (Ctrl+Shift+X)", "e40"),
            element("textbox", "Find", "e41", states=["disabled"]),
        ]}

    def test_a_button_is_refused_and_the_fields_are_named(self):
        with patch.object(core, "observe", return_value=self.screen()), patch.object(core, "_perform") as perform:
            with self.assertRaises(ComputerError) as ctx:
                core.act("type", ref="e40", text="Hello World")
            perform.assert_not_called()
        message = str(ctx.exception)
        self.assertIn("not a field", message)
        self.assertIn('document "Text editor" (e28)', message)
        self.assertNotIn("e41", message)


class WhoAWindowsWorkerIsToldItIs(unittest.TestCase):
    """Found live: a native worker's prompt said it was an Ubuntu desktop in a Docker container."""

    def test_a_native_worker_is_told_about_the_computer_it_is_on(self):
        from decurix_agent import config, identity

        with patch.object(config, "NATIVE_WINDOWS", True), \
                patch.object(browser, "windows_browser_name", return_value="Google Chrome"), \
                patch.dict(os.environ, {"USERNAME": "ana", "COMPUTERNAME": "OFFICE-PC"}):
            facts = identity.get_identity_context("TheBot (soul)")

        self.assertRegex(facts, r"Windows \S+ computer OFFICE-PC")
        self.assertIn("Windows user: ana", facts)
        self.assertIn("PowerShell", facts)
        self.assertIn("The browser is Google Chrome.", facts)
        for wrong in ("Ubuntu", "Docker", "VNC", "headless", "Chromium installed"):
            self.assertNotIn(wrong, facts)

    def test_a_linux_desktop_is_told_what_it_always_was(self):
        from decurix_agent import config, identity

        with patch.object(config, "NATIVE_WINDOWS", False):
            facts = identity.get_identity_context()
        self.assertIn("Ubuntu XFCE desktop in a Docker container", facts)


class FakeCamera:
    """
    win.Camera without a helper: every picture is the same small frame, and
    what is under a point is read off `windows` - the helper's own answers
    ("at", "front"), topmost first. With `windows` None the screen cannot be
    asked, and the recorder goes by its last look at the screen.
    """

    taken = 0
    windows = None

    def grab(self):
        FakeCamera.taken += 1
        return screen.Frame(4, 4, bytearray(48))

    def ask(self, op, **fields):
        if FakeCamera.windows is None:
            raise ComputerError("no helper here")
        if op == "at":
            for window in FakeCamera.windows:
                x0, y0, w, h = window["box"]
                if x0 <= fields["x"] < x0 + w and y0 <= fields["y"] < y0 + h:
                    return {"ok": True, "window": window}
            return {"ok": True}
        if op == "front":
            return {"ok": True, "window": next((w for w in FakeCamera.windows if not w.get("popup")), None)}
        raise ComputerError(f"unexpected {op}")

    def close(self):
        pass


def uia_window(title, app, box, nodes, popup=False, hwnd=1, pid=10):
    """A window as the helper's "at" describes one: its controls as UI Automation nodes."""

    return {"hwnd": hwnd, "title": title, "pid": pid, "class": "#32770", "popup": popup, "app": app,
            "exe": rf"C:\Apps\{app}.exe", "browser": False, "box": box, "nodes": nodes}


def uia_node(role, name, box, **more):
    return dict({"role": role, "name": name, "box": box, "containers": [], "patterns": [], "states": [],
                 "rid": [len(name), box[0]], "framework": "Win32"}, **more)


class WhatAPersonDidInTheViewer(unittest.TestCase):
    """feed(): the viewer's messages as the raw events the X recorder produces."""

    def setUp(self):
        with patch.object(demonstration.recording, "meta", return_value={}):
            self.recorder = demonstration.Recorder("demo-test")
        self.recorder.camera = FakeCamera()

    def fed(self, *events):
        for event in events:
            self.recorder.feed(event)
        raw = []
        while not self.recorder.raw.empty():
            raw.append(self.recorder.raw.get())
        return raw

    def test_a_click_is_a_press_and_a_release_and_the_press_pictures_the_screen(self):
        raw = self.fed({"kind": "down", "x": 120, "y": 80, "right": False, "mods": ""},
                       {"kind": "up", "x": 120, "y": 80, "right": False, "mods": ""})
        self.assertEqual([(e["type"], e["button"], e["x"], e["y"]) for e in raw],
                         [("press", 1, 120, 80), ("release", 1, 120, 80)])
        self.assertIsNotNone(raw[0]["frame"])
        self.assertNotIn("frame", raw[1])
        self.assertEqual([a["kind"] for a in processing.merge(raw)], ["click"])

    def test_the_right_button_is_button_three(self):
        raw = self.fed({"kind": "down", "x": 1, "y": 2, "right": True})
        self.assertEqual(raw[0]["button"], 3)

    def test_typed_characters_merge_into_what_was_typed(self):
        raw = self.fed(*[{"kind": "type", "text": c} for c in "Acme Co"])
        actions = processing.merge(raw)
        self.assertEqual([(a["kind"], a["text"]) for a in actions], [("type", "Acme Co")])
        # Only the first key of the burst pictured the screen.
        self.assertEqual(sum(1 for e in raw if e.get("frame") is not None), 1)

    def test_a_named_key_keeps_the_name_every_recording_uses(self):
        raw = self.fed({"kind": "key", "vk": 13, "mods": ""})
        self.assertEqual([(e["type"], e["key"]) for e in raw], [("key_down", "Return"), ("key_up", "Return")])

    def test_a_shortcut_holds_its_modifiers_around_the_key(self):
        raw = self.fed({"kind": "key", "vk": 83, "mods": "c"})
        self.assertEqual([(e["type"], e["key"]) for e in raw], [
            ("key_down", "Control_L"), ("key_down", "s"), ("key_up", "s"), ("key_up", "Control_L")])
        actions = processing.merge(raw)
        self.assertEqual([(a["kind"], a["keys"]) for a in actions], [("key", ["ctrl+s"])])

    def test_a_copy_is_the_shortcut_it_was(self):
        raw = self.fed({"kind": "copy", "vk": 67})
        self.assertEqual([a["keys"] for a in processing.merge(raw)], [["ctrl+c"]])

    def test_pasted_lines_are_text_and_the_keys_between_them(self):
        raw = self.fed({"kind": "paste", "text": "a\r\nb"})
        self.assertEqual([e["key"] for e in raw if e["type"] == "key_down"], ["a", "Return", "b"])

    def test_the_wheel_is_the_scroll_buttons(self):
        raw = self.fed({"kind": "wheel", "x": 5, "y": 6, "delta": -1})
        self.assertEqual([(e["type"], e["button"]) for e in raw], [("press", 5), ("release", 5)])
        self.assertEqual([(a["kind"], a["direction"]) for a in processing.merge(raw)], [("scroll", "down")])

    def test_nothing_is_taken_while_paused(self):
        self.recorder.paused = True
        self.assertEqual(self.fed({"kind": "type", "text": "secret"}), [])

    def test_a_key_nothing_is_known_about_is_left_out(self):
        self.assertEqual(self.fed({"kind": "key", "vk": 255, "mods": ""}), [])


class RecordingADemonstrationOnAWindowsWorker(unittest.TestCase):
    """start() -> what the person did -> stop(), with the recorder as a thread of the agent."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        field = {"role": "textbox", "name": "Customer", "value": "", "states": ["focused"], "box": [100, 60, 200, 30],
                 "source": "uia", "group": "uia:app:Orders", "window_title": "Orders", "path": 'app window "Orders" > textbox "Customer"',
                 "handle": {"raw_role": "Edit"}}
        frame = screen.Frame(4, 4, bytearray(48))
        look = {"elements": [field], "frame": frame, "active_title": "Orders", "size": [1920, 1080], "windows": [], "groups": []}

        self.patches = [
            patch.object(demonstration, "WINDOWS", True),
            patch.object(demonstration.recording, "ROOT", root),
            patch.object(demonstration, "ACTIVE_FILE", root / "active.json"),
            patch.object(demonstration, "READY_FILE", root / "active.ready"),
            patch.object(demonstration, "QUIET_SECONDS", 0.1),
            patch.object(demonstration.session, "ensure"),
            patch.object(win, "Camera", FakeCamera),
            patch.object(demonstration.screen, "grab", return_value=frame),
            patch.object(demonstration.core, "observe", return_value=look),
            patch.object(demonstration.core, "_wait_for_settle", return_value=frame),
            patch.object(demonstration.core, "compare", return_value={}),
            patch.object(demonstration.core, "summarize", return_value="nothing changed"),
            patch.object(demonstration.core, "expectations", return_value={}),
            patch.object(demonstration, "_report_to_chat"),
            patch.object(demonstration.progress, "flush"),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        if demonstration._thread is not None:
            demonstration._recorder.stop_requested.set()
            demonstration._thread.join(timeout=10)
        demonstration._recorder = demonstration._thread = None
        for p in reversed(self.patches):
            p.stop()
        self.tmp.cleanup()

    def test_what_was_fed_is_recorded_as_the_persons_actions(self):
        meta = demonstration.start(title="Enter a customer")
        session_id = meta["id"]
        self.assertFalse(demonstration.status()["paused"])

        self.assertTrue(demonstration.feed({"kind": "down", "x": 150, "y": 70}))
        demonstration.feed({"kind": "up", "x": 150, "y": 70})
        for char in "Acme":
            demonstration.feed({"kind": "type", "text": char})

        # A private step: nothing typed during it is kept.
        time.sleep(0.6)
        demonstration.pause("signing in")
        demonstration.feed({"kind": "type", "text": "hunter2"})
        demonstration.resume()

        ended = demonstration.stop()
        self.assertEqual(ended["status"], "complete")
        self.assertIsNone(demonstration.status())
        self.assertFalse(demonstration.feed({"kind": "type", "text": "late"}))

        _, events = demonstration.recording.load(session_id)
        kinds = [(e["kind"], e.get("actor")) for e in events]
        self.assertEqual(kinds, [("click", "human"), ("type", "human"), ("manual", "human")])
        self.assertEqual(events[0]["element"]["name"], "Customer")
        self.assertEqual(events[1]["text"], "Acme")
        self.assertNotIn("hunter2", json.dumps(events))

    def test_what_a_person_has_opened_is_a_step_in_order_with_what_they_did(self):
        # The worker's screen has no Start menu: the viewer's Open box is how
        # an app gets on it, and a replay has to open it again.
        with patch.object(demonstration.core, "_open", return_value='Opened "Untitled - Notepad".') as opened, \
                patch.object(win, "open_argv", return_value=[r"C:\Windows\notepad.exe"]):
            meta = demonstration.start(title="Write a note")
            demonstration.feed({"kind": "type", "text": "x"})
            said = demonstration.open_as_person("notepad")
            demonstration.feed({"kind": "type", "text": "y"})
            demonstration.stop()

        self.assertEqual(said, 'Opened "Untitled - Notepad".')
        opened.assert_called_once_with("notepad", False)
        _, events = demonstration.recording.load(meta["id"])
        self.assertEqual([(e["kind"], e.get("text") or e.get("target")) for e in events],
                         [("type", "x"), ("open", "notepad"), ("type", "y")])
        self.assertEqual(events[1]["actor"], "human")
        self.assertEqual(events[1]["expect"], {"active_window": "Orders"})

    def test_what_could_not_be_opened_is_said_and_not_recorded(self):
        with patch.object(demonstration.core, "_open", side_effect=ComputerError("don't know how to open 'photoshop'")), \
                patch.object(win, "open_argv", return_value=[r"C:\Apps\photoshop.exe"]):
            meta = demonstration.start(title="Edit a photo")
            with self.assertRaises(ComputerError) as ctx:
                demonstration.open_as_person("photoshop")
            demonstration.stop()
        self.assertIn("photoshop", str(ctx.exception))
        self.assertEqual(demonstration.recording.load(meta["id"])[1], [])

    def test_with_nothing_being_recorded_it_just_opens(self):
        with patch.object(demonstration.core, "_open", return_value="Opened.") as opened:
            self.assertEqual(demonstration.open_as_person("https://example.com"), "Opened.")
        opened.assert_called_once_with("https://example.com", False)

    def test_a_program_is_given_no_arguments_from_the_viewer(self):
        with patch.object(win, "open_argv", return_value=[r"C:\Windows\System32\cmd.exe", "/c", "del", "x"]), \
                patch.object(demonstration.core, "_open") as opened:
            with self.assertRaises(ComputerError) as ctx:
                demonstration.open_as_person("cmd /c del x")
            opened.assert_not_called()
        self.assertIn("name the program only", str(ctx.exception))

    def test_a_second_demonstration_is_refused_while_one_is_being_recorded(self):
        demonstration.start(title="one")
        with self.assertRaises(ComputerError):
            demonstration.start(title="two")
        demonstration.stop()


if __name__ == "__main__":
    unittest.main()
