"""The screen side's window list in broker mode (decurix_agent/computer/windowlist.py; docs/gate-boundary.md, "The
panel's window list").

What runs anywhere: the module can start no program (read from its code: what it imports and calls), which windows
get a button, where the list goes between the worker's bars, the buttons' layout and which one a click hits, the
icon picked from a window's _NET_WM_ICON (bounded, whatever the window's client wrote), titles cleaned, and the
drawing (with PIL). The X side is checked live (docs/gate-boundary.md).

    cd agent && PYTHONPATH=. python3 -m unittest tests.test_windowlist
"""

import ast
import unittest
from pathlib import Path

from decurix_agent.computer import windowlist as wl

SOURCE = Path(wl.__file__).read_text(encoding="utf-8")

try:
    import PIL  # noqa: F401
    HAVE_PIL = True
except ImportError:
    HAVE_PIL = False


class ItStartsNothing(unittest.TestCase):
    """Run as the screen user, a way to start a program here would be a way round the gate (a terminal started as
    decurixscreen types into anything). So the module holds none."""

    def setUp(self):
        self.tree = ast.parse(SOURCE)

    def test_it_imports_only_what_draws_and_speaks_x(self):
        allowed = {"os", "select", "struct", "sys", "unicodedata", "Xlib", "PIL"}
        for node in ast.walk(self.tree):
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom):
                names = [node.module or ""]
            else:
                continue
            for name in names:
                self.assertIn(name.split(".")[0], allowed, f"windowlist imports {name}")

    def test_it_calls_no_way_to_start_a_process(self):
        banned_os = {"system", "popen", "fork", "forkpty", "posix_spawn", "posix_spawnp", "startfile"}
        for node in ast.walk(self.tree):
            if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name) and node.value.id == "os":
                self.assertFalse(node.attr in banned_os or node.attr.startswith(("exec", "spawn")),
                                 f"windowlist uses os.{node.attr}")
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
                self.assertNotIn(node.func.id, {"exec", "eval", "compile", "__import__", "open"},
                                 f"windowlist calls {node.func.id}()")
        code = "\n".join(line for line in SOURCE.splitlines() if not line.lstrip().startswith("#"))
        body = code.split('"""', 2)[2]      # past the module's docstring, which names what it does not do
        for word in ("subprocess", "xdg-open", "exo-open", "Gio", "gi.repository", "Gtk", "ctypes", "importlib"):
            self.assertNotIn(word, body)

    def test_a_click_only_ever_sends_the_window_manager_two_messages(self):
        sent = [n for n in ast.walk(self.tree) if isinstance(n, ast.Attribute) and n.attr == "send_event"]
        self.assertEqual(len(sent), 1)
        click = next(n for n in ast.walk(self.tree) if isinstance(n, ast.FunctionDef) and n.name == "click")
        text = ast.get_source_segment(SOURCE, click)
        self.assertIn('self.atom["WM_CHANGE_STATE"]', text)
        self.assertIn('self.atom["_NET_ACTIVE_WINDOW"]', text)
        self.assertIn("self.root.send_event", text)
        # Only the left button, and no key events at all.
        self.assertIn("event.detail == 1", SOURCE)
        self.assertNotIn("KeyPress", SOURCE)
        self.assertIn("flags=Xutil.InputHint, input=0", SOURCE)


ATOMS = {name: i for i, name in enumerate(wl.ATOMS, start=100)}


class WhichWindowsAreListed(unittest.TestCase):
    def test_normal_windows_and_lone_dialogs(self):
        normal, dialog = ATOMS["_NET_WM_WINDOW_TYPE_NORMAL"], ATOMS["_NET_WM_WINDOW_TYPE_DIALOG"]
        self.assertTrue(wl.listed([normal], [], False, ATOMS))
        self.assertTrue(wl.listed([], [], False, ATOMS))
        self.assertTrue(wl.listed([dialog], [], False, ATOMS))
        self.assertFalse(wl.listed([dialog], [], True, ATOMS))
        self.assertFalse(wl.listed([], [], True, ATOMS))
        self.assertFalse(wl.listed([ATOMS["_NET_WM_WINDOW_TYPE_DOCK"]], [], False, ATOMS))
        self.assertFalse(wl.listed([normal], [ATOMS["_NET_WM_STATE_SKIP_TASKBAR"]], False, ATOMS))
        # The first type it knows decides.
        self.assertFalse(wl.listed([ATOMS["_NET_WM_WINDOW_TYPE_DOCK"], normal], [], False, ATOMS))


class WhereTheListGoes(unittest.TestCase):
    def test_between_the_worker_bars_of_the_broker_layout(self):
        bars = [(0, 724, 180, 44), (1160, 724, 200, 44)]
        self.assertEqual(wl.place(1360, 768, bars), (180, 724, 980, 44))

    def test_a_bar_across_the_middle_leaves_the_reserves(self):
        self.assertEqual(wl.place(1360, 768, [(0, 724, 1360, 44)]),
                         (wl.LEFT_RESERVE, 724, 1360 - wl.LEFT_RESERVE - wl.RIGHT_RESERVE, 44))

    def test_no_bars_the_whole_edge_and_bars_elsewhere_do_not_count(self):
        self.assertEqual(wl.place(1280, 720, []), (0, 720 - wl.HEIGHT_DEFAULT, 1280, wl.HEIGHT_DEFAULT))
        self.assertEqual(wl.place(1280, 720, [(0, 0, 300, 30)]), (0, 720 - wl.HEIGHT_DEFAULT, 1280, wl.HEIGHT_DEFAULT))

    def test_a_height_out_of_bounds_is_clamped(self):
        self.assertEqual(wl.place(1000, 700, [(0, 200, 100, 500)])[3], wl.HEIGHT_MAX)
        self.assertEqual(wl.place(1000, 700, [(0, 695, 100, 5)])[3], wl.HEIGHT_MIN)

    def test_bars_that_overlap_never_give_a_negative_width(self):
        x, _, w, _ = wl.place(1000, 700, [(0, 656, 600, 44), (400, 656, 600, 44)])
        self.assertGreaterEqual(w, 0)
        self.assertLessEqual(x + w, 1000)


class TheButtons(unittest.TestCase):
    def test_layout_narrows_to_fit_and_leaves_out_what_does_not(self):
        self.assertEqual(wl.layout(2, 1000), [(0, wl.BUTTON_MAX), (wl.BUTTON_MAX, wl.BUTTON_MAX)])
        boxes = wl.layout(10, 500)
        self.assertEqual(len(boxes), 10)
        self.assertEqual(boxes[1], (50, 50))
        self.assertEqual(len(wl.layout(100, 440)), 440 // wl.BUTTON_MIN)
        self.assertEqual(wl.layout(3, 10), [])
        self.assertEqual(wl.layout(0, 1000), [])

    def test_a_click_hits_its_button(self):
        entries = [{"id": 1}, {"id": 2}]
        self.assertEqual(wl.button_at(entries, 1000, 10)["id"], 1)
        self.assertEqual(wl.button_at(entries, 1000, wl.BUTTON_MAX + 1)["id"], 2)
        self.assertIsNone(wl.button_at(entries, 1000, 2 * wl.BUTTON_MAX + 5))


class TheIcon(unittest.TestCase):
    @staticmethod
    def icon(side, value):
        return [side, side] + [value] * (side * side)

    def test_the_smallest_at_least_the_size_wanted_else_the_largest(self):
        data = self.icon(16, 1) + self.icon(48, 2) + self.icon(32, 3)
        w, h, pixels = wl.pick_icon(data, want=22)
        self.assertEqual((w, h, set(pixels)), (32, 32, {3}))
        w, _, _ = wl.pick_icon(self.icon(16, 1) + self.icon(8, 2), want=22)
        self.assertEqual(w, 16)

    def test_what_the_client_wrote_wrong_ends_the_walk(self):
        self.assertIsNone(wl.pick_icon([]))
        self.assertIsNone(wl.pick_icon([5000, 5000, 1, 2, 3]))
        self.assertIsNone(wl.pick_icon([4, 4, 1, 2]))                      # shorter than it says
        w, _, _ = wl.pick_icon(self.icon(16, 1) + [100000, 1, 7])
        self.assertEqual(w, 16)


class TheTitle(unittest.TestCase):
    def test_text_only_and_bounded(self):
        self.assertEqual(wl.clean_title("a\x1b[31mb\tc\u202ed".encode()), "a [31mb c d")
        self.assertEqual(wl.clean_title("x" * 5000), "x" * wl.MAX_TITLE_CHARS)
        self.assertEqual(wl.clean_title(None), "")


@unittest.skipUnless(HAVE_PIL, "PIL is not installed here")
class TheDrawing(unittest.TestCase):
    def test_it_draws_bgrx_of_the_list_size(self):
        font = wl.load_font()
        entries = [{"title": "Mousepad", "icon": (2, 2, [0xFFFF0000] * 4), "active": True, "hidden": False},
                   {"title": "", "icon": None, "active": False, "hidden": True},
                   {"title": "a" * 300, "icon": (1, 1, [1, 2]), "active": False, "hidden": False}]
        data = wl.render(entries, 700, 44, font)
        self.assertEqual(len(data), 700 * 44 * 4)
        # The background, in B, G, R, X order.
        r, g, b = wl.BACKGROUND
        self.assertEqual(data[-4:-1], bytes([b, g, r]))

    def test_fit_cuts_with_an_ellipsis(self):
        font = wl.load_font()
        self.assertEqual(wl.fit("short", font, 1000), "short")
        cut = wl.fit("a long title of a window " * 5, font, 80)
        self.assertTrue(cut.endswith("\u2026"))
        self.assertLessEqual(font.getlength(cut), 80)
        self.assertEqual(wl.fit("anything", font, 0), "")


if __name__ == "__main__":
    unittest.main()
