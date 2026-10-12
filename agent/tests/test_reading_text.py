"""
An engine can read the text it edits, sees what an edit changed, and cannot
replace a whole document by typing into it - three things the GUI benchmark
found missing (mousepad-replace: one of three words visible, the change cut
off at 40 characters, and `gui type` on the editor replaced the file).

    PYTHONPATH=agent python3 -m unittest agent.tests.test_reading_text
"""

import io
import unittest
from contextlib import redirect_stdout
from types import SimpleNamespace
from unittest.mock import patch

from decurix_agent.computer import approval, audit, cli, core
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError

NOTES = "The colour of the logo is blue.\nPick a colour for the header.\nNo other colour changes.\n"


def field(value, role="textbox", name="", ref="e13", **extra):
    base = {"role": role, "name": name, "value": value, "states": ["focused"], "box": [0, 40, 800, 500], "ref": ref,
            "path": f"win > {role}", "source": "atspi", "group": "g", "window_title": "notes.txt - Mousepad",
            "handle": {}}
    base.update(extra)
    return base


def look(*elements):
    return {"elements": list(elements), "frame": None, "windows": [], "groups": [{"id": "g", "title": "Mousepad"}],
            "active_title": "notes.txt - Mousepad", "size": [1280, 720]}


def gui(*argv):
    out = io.StringIO()
    with redirect_stdout(out):
        code = cli.run(list(argv))
    return code, out.getvalue()


class ReadingAField(unittest.TestCase):

    def test_the_whole_text_with_line_numbers(self):
        with patch.object(core, "observe", return_value=look(field(NOTES))):
            code, out = gui("read", "e13")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertIn("  3| No other colour changes.", out)
        self.assertEqual(out.count("colour"), 3)

    def test_no_target_reads_the_field_with_the_keyboard(self):
        other = field("x", ref="e2", states=[])
        with patch.object(core, "observe", return_value=look(other, field(NOTES))):
            _, out = gui("read")
        self.assertIn("Pick a colour", out)

    def test_a_password_is_never_shown(self):
        secret = field("hunter2", role="password text", name="Password")
        with patch.object(core, "observe", return_value=look(secret)):
            _, out = gui("read", "e13")
        self.assertNotIn("hunter2", out)
        self.assertIn("secret", out)

    def test_observe_says_there_is_more_and_how_to_read_it(self):
        line = el.element_line(field(NOTES))
        self.assertIn("3 lines", line)
        self.assertIn("gui read e13 shows all", line)
        self.assertNotIn("gui read", el.element_line(field("short")))


class WhatAnEditChanged(unittest.TestCase):

    def test_a_change_far_into_the_text_is_shown(self):
        after = NOTES.replace("No other colour", "No other color")
        text = core.text_change(NOTES, after)
        self.assertIn("changed in 1 place", text)
        self.assertIn("line 3", text)
        self.assertIn("No other color changes", text)

    def test_each_change_is_listed(self):
        text = core.text_change(NOTES, NOTES.replace("colour", "color"))
        self.assertIn("changed in 3 places", text)
        for n in (1, 2, 3):
            self.assertIn(f"line {n}:", text)

    def test_a_short_value_reads_as_before(self):
        self.assertEqual(core.text_change("Bob", "Bob Lee"), '"Bob" -> "Bob Lee"')


class TypingIntoADocument(unittest.TestCase):

    def _type(self, element, **kw):
        with patch.object(core, "observe", return_value=look(element)), \
             patch.object(core, "_perform", return_value="value") as perform, \
             patch.object(core, "_wait_for_settle"), patch.object(audit, "record"), \
             patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            core.act("type", ref="e13", text="color", **kw)
        return perform

    def test_replacing_a_whole_document_is_refused(self):
        with self.assertRaises(ComputerError) as ctx:
            self._type(field(NOTES))
        self.assertIn("3 lines", str(ctx.exception))
        self.assertIn("--replace-all", str(ctx.exception))
        self.assertIn("nothing was typed", str(ctx.exception))

    def test_unless_it_is_asked_for_outright_or_added_to(self):
        self.assertTrue(self._type(field(NOTES), replace_all=True).called)
        self.assertTrue(self._type(field(NOTES), append=True).called)

    def test_a_one_line_field_is_replaced_as_always(self):
        self.assertTrue(self._type(field("old name", name="Name")).called)

    def test_the_command_passes_the_flag(self):
        with patch.object(cli.core, "act", return_value={"status": "done", "text": "typed"}) as act:
            gui("type", "e13", "--replace-all", "new text")
        self.assertTrue(act.call_args.kwargs["replace_all"])


class TheCheckReadsTheTextItself(unittest.TestCase):
    """The final check is given the fields' own text, not only a picture of it."""

    def test_the_fields_text_whole_and_no_secret(self):
        from decurix_agent.computer import evidence
        secret = field("hunter2", role="password text", name="Password", ref="e2", states=[])
        with patch.object(core, "observe", return_value=look(secret, field(NOTES))):
            text = evidence.live_text()
        self.assertIn("No other colour changes.", text)
        self.assertIn("notes.txt - Mousepad", text)
        self.assertNotIn("hunter2", text)

    def test_a_screen_with_no_text_field_says_nothing(self):
        from decurix_agent.computer import evidence
        with patch.object(core, "observe", return_value=look()):
            self.assertEqual(evidence.live_text(), "")

    def test_the_judge_is_shown_it(self):
        from decurix_agent import llm_client, verify
        seen = {}

        def chat(messages, **kw):
            seen["text"] = messages[1]["content"][0]["text"]
            return {"content": '{"verdict": "not_done", "confidence": 0.9, "seen": "", "why": "one left"}'}, {}

        with patch.object(verify, "available", return_value=True), patch.object(llm_client, "chat", side_effect=chat):
            verify.judge("replace every colour with color", claim="all replaced", screen_text="textbox holds: colour")
        self.assertIn("read from the apps themselves", seen["text"])
        self.assertIn("textbox holds: colour", seen["text"])


if __name__ == "__main__":
    unittest.main()
