"""
The `gui` command refuses through computer/door.py (review 2026-11-03 D-23): a guessed ref, a blind walk of keys, a
sequence typing cannot type. The MCP server, the other door, went with the Claude Code harness (2026-11-07).
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import cli, core, door, shown
from decurix_agent.computer.errors import ComputerError

OBS = {"elements": [], "frame": None, "windows": [], "groups": [], "size": [800, 600], "active_title": "x"}
DONE = {"status": "done", "text": "ok", "observation": OBS, "element": None, "change": None}

# One input of each class the door refuses, as (kind, act keywords).
REFUSED = {
    "a guessed ref": ("click", {"ref": "e99"}),
    "a blind walk of keys": ("key", {"keys": ["Tab"] * 40}),
    "a backslash sequence typing cannot type": ("type", {"text": "one\\f two"}),
}


GUI_ARGS = {"click": lambda k: ["click", k["ref"]], "key": lambda k: ["key"] + list(k["keys"]),
            "type": lambda k: ["type", "e5", k["text"]]}


def gui(kind, kwargs):
    """The input through the gui command itself (cli.run), as an engine types it."""
    with mock.patch.object(core, "act", return_value=DONE) as act, mock.patch.object(cli, "_print_act", create=True), \
            mock.patch.object(core, "_load_state", return_value={}):
        try:
            cli.run(GUI_ARGS[kind](kwargs))
        except ComputerError:
            return act, True
    return act, False


class TheGuiDoorRefuses(unittest.TestCase):
    def setUp(self):
        p = mock.patch.object(shown, "unseen", side_effect=lambda ref: ref == "e99")
        p.start()
        self.addCleanup(p.stop)

    def test_each_refused_input_is_refused_and_nothing_is_done(self):
        for what, (kind, kwargs) in REFUSED.items():
            with self.subTest(input=what):
                act, refused = gui(kind, kwargs)
                self.assertTrue(refused)
                act.assert_not_called()

    def test_the_gui_command_uses_the_doors_refusals(self):
        self.assertIs(cli.door, door)


if __name__ == "__main__":
    unittest.main()
