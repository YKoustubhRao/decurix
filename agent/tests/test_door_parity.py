"""
The two doors to the screen - the `gui` command and the MCP server - refuse the same actions and keep the same
evidence (computer/door.py, review 2026-11-03 D-23).
"""

import unittest
from types import SimpleNamespace
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import cli, core, door, mcp_server, shown
from decurix_agent.computer.errors import ComputerError

OBS = {"elements": [], "frame": None, "windows": [], "groups": [], "size": [800, 600], "active_title": "x"}
DONE = {"status": "done", "text": "ok", "observation": OBS, "element": None, "change": None}

# One input of each class a door refuses, as (kind, act keywords).
REFUSED = {
    "a guessed ref": ("click", {"ref": "e99"}),
    "a blind walk of keys": ("key", {"keys": ["Tab"] * 40}),
    "a backslash sequence typing cannot type": ("type", {"text": "one\\f two"}),
}


def mcp(kind, kwargs):
    arguments = dict(kwargs, action=kind)
    with mock.patch.object(core, "act", return_value=DONE) as act, mock.patch.object(mcp_server.el, "render", return_value=""), \
            mock.patch.object(mcp_server, "_image", return_value={}):
        try:
            mcp_server.call_tool("act", arguments)
        except ComputerError:
            return act, True
    return act, False


def gui(kind, kwargs):
    with mock.patch.object(core, "act", return_value=DONE) as act:
        try:
            door.act(kind, **kwargs)
        except ComputerError:
            return act, True
    return act, False


class BothDoorsRefuseTheSame(unittest.TestCase):
    def setUp(self):
        p = mock.patch.object(shown, "unseen", side_effect=lambda ref: ref == "e99")
        p.start()
        self.addCleanup(p.stop)

    def test_each_refused_input_is_refused_by_both_doors_and_nothing_is_done(self):
        for what, (kind, kwargs) in REFUSED.items():
            for name, through in (("mcp", mcp), ("gui", gui)):
                with self.subTest(input=what, door=name):
                    act, refused = through(kind, kwargs)
                    self.assertTrue(refused)
                    act.assert_not_called()

    def test_the_gui_command_uses_the_same_refusals(self):
        self.assertIs(cli.door, door)

    def test_a_press_on_a_named_control_is_made_by_its_ref_through_mcp(self):
        act, _ = mcp("click", {"ref": "e5"})
        self.assertTrue(act.call_args.kwargs.get("named"))
        act, _ = mcp("type", {"text": "hello"})
        self.assertTrue(act.call_args.kwargs.get("keys_from_task"))


class BothDoorsKeepEvidence(unittest.TestCase):
    def test_an_mcp_look_and_check_are_kept_as_evidence(self):
        with mock.patch.object(core, "observe", return_value=OBS), mock.patch.object(core, "report_observation"), \
                mock.patch.object(mcp_server.el, "render", return_value="e1 button Save"), \
                mock.patch.object(mcp_server, "_image", return_value={}), \
                mock.patch("decurix_agent.computer.evidence.note_look") as look, \
                mock.patch("decurix_agent.computer.evidence.note") as note, \
                mock.patch.object(core, "verify", return_value={"ok": True, "text": "PASS: found", "observation": OBS}):
            mcp_server.call_tool("observe", {})
            mcp_server.call_tool("verify", {"name": "Save"})
        look.assert_called_once()
        self.assertIn("PASS: found", note.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
