"""
Every gui action's result says what its effect was, as data - confirmed, unverifiable or suspected_noop - and what that
leaves the engine to do: done, look again before any retry, or escalate (computer/outcome.py; technique from Hermes'
`_classify_action_result`). After an action that was not confirmed, the same input on the same target is refused until
the engine looks again; typing of which not one character landed is set directly where the field allows it, and read
back. Only the screen is mocked: core.act runs as it does for `gui`.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_action_outcome.py
"""

import contextlib
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import core, outcome
from decurix_agent.computer.errors import ActedError, ComputerError

TASK = "t0123456789abcdef"


def field(value="", role="textbox", name="Customer", source="cdp", states=(), path=None, ref="e5", group="g1",
          **extra):
    """An element as a look lists it."""

    element = {"ref": ref, "role": role, "name": name, "value": value, "states": list(states), "box": [10, 10, 100, 20],
               "path": path or f'site x > {role} "{name}"', "source": source, "group": group,
               "window_title": "Form - Chromium", "handle": {"backend_id": 5} if source == "cdp" else
               {"indices": [0, 0, 1], "raw_role": "entry", "own_name": name, "single_line": True, "editable": True}}
    element.update(extra)
    return element


class Screen:
    """A screen whose controls the test changes; every look lists them as they are then."""

    def __init__(self, *elements, url="http://x.test/form"):
        self.elements = [dict(e) for e in elements]
        self.url = url
        self.looks = 0

    def look(self, *args, **kwargs):
        self.looks += 1
        return {"elements": [dict(e, states=list(e.get("states") or [])) for e in self.elements], "page_url": self.url,
                "active_title": "Form - Chromium", "frame": None, "size": [1280, 720], "windows": [],
                "popups": [], "notes": [], "gone": {}, "behind": False,
                "groups": [{"id": "g1", "window_title": "Form - Chromium", "source": "cdp", "url": self.url}]}

    def set(self, path, **values):
        for e in self.elements:
            if e["path"] == path:
                e.update(values)


ALLOWED = types.SimpleNamespace(allowed=True, request=None, approved=False, reason="")


class Act(unittest.TestCase):
    """core.act on a mocked screen, with a task running and its own state folder."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.task = TASK
        for p in (mock.patch("decurix_agent.config.DECURIX_DIR", self.dir),
                  mock.patch.object(core, "FRONT_FILE", self.dir / "front.json"),
                  mock.patch.object(core, "PENDING_SAVE_FILE", self.dir / "pending_save.json"),
                  mock.patch("decurix_agent.task_context.current", side_effect=lambda: self.task),
                  mock.patch.object(core, "_task_now", side_effect=lambda: self.task)):
            p.start()
            self.addCleanup(p.stop)
        self.recorded = []

    def act(self, screen, kind, perform=None, aimed=None, **kwargs):
        def performed(kind_, element, *args, **kw):
            if perform:
                perform(kind_, element, *args, **kw)
            return "cdp" if (element or {}).get("source") == "cdp" else "xdotool"

        patches = [
            mock.patch.object(core, "observe", screen.look),
            mock.patch.object(core.audit, "record", lambda entry, **kw: self.recorded.append(dict(entry))),
            mock.patch.object(core.approval, "check", return_value=ALLOWED),
            mock.patch.object(core, "_perform", side_effect=performed),
            mock.patch.object(core, "_wait_for_settle", return_value=None),
            mock.patch.object(core, "_downloads_snapshot", return_value=None),
            mock.patch.object(core, "_note_front", lambda *a, **k: None),
            mock.patch.object(core, "_adopt_front", lambda *a, **k: None),
            mock.patch.object(core, "_keys_home", return_value=None),
            mock.patch.object(core, "_nothing_in_front", return_value=False),
            mock.patch.object(core, "_left_from_before", return_value=""),
            mock.patch.object(core, "_aimed_elsewhere", return_value=aimed),
            mock.patch.object(core, "_overwritten_note", return_value=""),
            mock.patch.object(core, "RETYPE_PAUSE", 0),
            mock.patch.object(core.cdp, "available", return_value=False),
        ]
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            return core.act(kind, **kwargs)


class Classify(unittest.TestCase):
    """The effect from what the two looks show, by code only."""

    def look(self, *elements, pixels=None):
        return {"elements": list(elements)}

    def change(self, **parts):
        base = {"pixels": None, "active_before": "a", "active_after": "a", "windows_opened": [], "windows_closed": [],
                "url_before": "u", "url_after": "u", "appeared": [], "disappeared": [], "changed": [], "focused": None}
        base.update(parts)
        return base

    def test_a_field_that_reads_back_the_text_is_confirmed(self):
        before, after = field(""), field("Acme Traders")
        got = outcome.classify("type", before, self.look(before), self.look(after), self.change(), text="Acme Traders")
        self.assertEqual((got["effect"], got["decision"]), (outcome.CONFIRMED, outcome.DONE))

    def test_a_number_the_field_formats_is_the_same_number(self):
        before, after = field(""), field("1,200.00")
        got = outcome.classify("type", before, self.look(before), self.look(after), self.change(), text="1200")
        self.assertEqual(got["effect"], outcome.CONFIRMED)

    def test_a_field_holding_what_it_held_took_none_of_it(self):
        before = field("old")
        got = outcome.classify("type", before, self.look(before), self.look(dict(before)), self.change(), text="new")
        self.assertEqual((got["effect"], got["decision"]), (outcome.SUSPECTED_NOOP, outcome.ESCALATE))
        self.assertIn("none of the 3 characters", got["why"])

    def test_a_field_that_changed_to_something_else_is_unverifiable(self):
        before, after = field(""), field("Ac")
        got = outcome.classify("type", before, self.look(before), self.look(after), self.change(), text="Acme")
        self.assertEqual((got["effect"], got["decision"]), (outcome.UNVERIFIABLE, outcome.LOOK_AGAIN))

    def test_a_secret_field_is_never_read_back(self):
        before = field("", role="textbox", name="Password", handle={"backend_id": 5, "input_type": "password"})
        after = dict(before, value="[withheld]")
        got = outcome.classify("type", before, self.look(before), self.look(after), self.change(), text="x")
        self.assertEqual((got["effect"], got["decision"]), (outcome.UNVERIFIABLE, outcome.LOOK_AGAIN))
        self.assertNotIn("x", got["why"].replace("text", "").replace("secret", "").replace("never", ""))

    def test_enter_that_emptied_the_field_proves_nothing_either_way(self):
        before = field("")
        got = outcome.classify("type", before, self.look(before), self.look(dict(before)),
                               self.change(url_after="u2"), text="query\n")
        self.assertEqual(got["effect"], outcome.UNVERIFIABLE)

    def test_a_box_whose_tick_changed_is_confirmed_and_one_that_did_not_is_escalated(self):
        box = field(role="checkbox", name="Agree", states=["unchecked"])
        ticked = dict(box, states=["checked"])
        got = outcome.classify("click", box, self.look(box), self.look(ticked), self.change())
        self.assertEqual(got["effect"], outcome.CONFIRMED)
        got = outcome.classify("click", box, self.look(box), self.look(dict(box)), self.change())
        self.assertEqual((got["effect"], got["decision"]), (outcome.SUSPECTED_NOOP, outcome.ESCALATE))

    def test_a_press_is_judged_by_the_screens_controls_not_its_picture(self):
        button = field(role="button", name="Next")
        got = outcome.classify("click", button, self.look(button), self.look(button),
                               self.change(appeared=[field(role="heading", name="Step 2")]))
        self.assertEqual(got["effect"], outcome.CONFIRMED)
        got = outcome.classify("click", button, self.look(button), self.look(button), self.change(pixels=0.05))
        self.assertEqual((got["effect"], got["decision"]), (outcome.UNVERIFIABLE, outcome.LOOK_AGAIN))
        got = outcome.classify("click", button, self.look(button), self.look(button), self.change(pixels=0.0))
        self.assertEqual((got["effect"], got["decision"]), (outcome.SUSPECTED_NOOP, outcome.LOOK_AGAIN))

    def test_a_scroll_that_moved_the_controls_is_confirmed(self):
        a = field(role="button", name="Save")
        moved = dict(a, box=[10, -200, 100, 20])
        got = outcome.classify("scroll", None, self.look(a), self.look(moved), self.change())
        self.assertEqual(got["effect"], outcome.CONFIRMED)
        got = outcome.classify("scroll", None, self.look(a), self.look(dict(a)), self.change())
        self.assertEqual(got["effect"], outcome.SUSPECTED_NOOP)

    def test_an_app_still_being_read_is_unverifiable(self):
        got = outcome.classify("click", None, {"elements": []}, {"elements": [], "behind": True}, self.change())
        self.assertEqual(got["effect"], outcome.UNVERIFIABLE)

    def test_the_line_is_machine_readable(self):
        line = outcome.line({"effect": "confirmed", "decision": "done", "why": "it reads back"})
        self.assertEqual(line, "Outcome: effect=confirmed; decision=done - it reads back.")


class EveryActionSaysItsEffect(Act):

    def test_a_typed_field_that_holds_the_text_is_done(self):
        screen = Screen(field(""))
        result = self.act(screen, "type", ref="e5", text="Acme",
                          perform=lambda *a, **k: screen.set(screen.elements[0]["path"], value="Acme"))
        self.assertEqual((result["effect"], result["decision"]), ("confirmed", "done"))
        self.assertIn("Outcome: effect=confirmed; decision=done", result["text"])
        self.assertEqual(self.recorded[-1]["effect"], "confirmed")
        self.assertNotIn("Acme", result["text"].split("Outcome:")[1])


class NoBlindRepeat(Act):

    def test_the_same_input_after_an_unconfirmed_one_waits_for_a_look(self):
        button = field(role="button", name="Next", value="")
        screen = Screen(button)
        first = self.act(screen, "click", ref="e5", look_first=True)
        self.assertEqual(first["decision"], outcome.LOOK_AGAIN)
        with self.assertRaises(ComputerError) as caught:
            self.act(screen, "click", ref="e5", look_first=True)
        self.assertIn("Look again first", str(caught.exception))
        self.assertEqual(self.recorded[-1].get("guard"), "look_first")
        # Another input, or a replay's step, is not held.
        self.act(screen, "double_click", ref="e5", look_first=True)
        self.act(screen, "click", ref="e5")
        # After a look it may be done again.
        outcome.looked()
        self.act(screen, "click", ref="e5", look_first=True)

    def test_typing_other_text_into_the_same_field_is_not_a_repeat(self):
        screen = Screen(field("old", source="atspi", path='gedit window "Find" > textbox "Search for"', name="Search for"))
        self.act(screen, "type", ref="e5", text="new", look_first=True)  # none landed: suspected_noop
        with self.assertRaises(ComputerError):
            self.act(screen, "type", ref="e5", text="new", look_first=True)
        self.act(screen, "type", ref="e5", text="other", look_first=True)

    def test_a_confirmed_action_ends_its_targets_mark(self):
        box = field(role="checkbox", name="Agree", states=["unchecked"])
        screen = Screen(box)
        self.act(screen, "click", ref="e5", look_first=True)  # did not tick: marked
        screen.set(box["path"], states=["checked"])
        outcome.remember({"effect": outcome.CONFIRMED}, "click", outcome.target_key(dict(box)),
                         outcome.input_key("click"))
        self.act(screen, "click", ref="e5", look_first=True)

    def test_another_task_starts_with_no_marks(self):
        screen = Screen(field(role="button", name="Next"))
        self.act(screen, "click", ref="e5", look_first=True)
        self.task = "tfedcba9876543210"
        self.act(screen, "click", ref="e5", look_first=True)

    def test_nothing_typed_is_kept_in_the_marks(self):
        screen = Screen(field("old", source="atspi", path='gedit window "Find" > textbox "Search for"', name="Search for"))
        self.act(screen, "type", ref="e5", text="s3cret-Value", look_first=True)
        kept = (self.dir / "computer" / "unconfirmed.json").read_text(encoding="utf-8")
        self.assertNotIn("s3cret-Value", kept)


class SetDirectlyWhenNoKeyLanded(Act):

    def test_a_page_field_that_drops_every_key_is_set_and_read_back(self):
        screen = Screen(field(""))

        def set_value(element, text):
            screen.set(element["path"], value=text)
            return text

        with mock.patch.object(core.cdp, "set_value", side_effect=set_value) as setter:
            result = self.act(screen, "type", ref="e5", text="Acme")
        setter.assert_called_once()
        self.assertEqual(result["effect"], "confirmed")
        self.assertIn("set directly through the page", result["text"])
        self.assertEqual(self.recorded[-1]["backend"], "cdp:set-value")

    def test_one_that_cannot_be_set_still_fails(self):
        screen = Screen(field(""))
        with mock.patch.object(core.cdp, "set_value", return_value=None):
            with self.assertRaises(ActedError):
                self.act(screen, "type", ref="e5", text="Acme")

    def test_a_field_that_took_part_of_it_is_not_set(self):
        screen = Screen(field(""))
        with mock.patch.object(core.cdp, "set_value") as setter:
            with self.assertRaises(ActedError):
                self.act(screen, "type", ref="e5", text="Acme",
                         perform=lambda *a, **k: screen.set(screen.elements[0]["path"], value="Ac"))
        setter.assert_not_called()

    def test_an_app_field_is_set_through_the_accessibility_interface(self):
        app = field("", source="atspi", path='gedit window "Find" > textbox "Search for"', name="Search for")
        screen = Screen(app)

        def set_text(element, text):
            screen.set(element["path"], value=text)
            return "set-text"

        with mock.patch.object(core.atspi, "set_text", side_effect=set_text) as setter:
            result = self.act(screen, "type", ref="e5", text="colour")
        setter.assert_called_once()
        self.assertEqual(result["effect"], "confirmed")
        self.assertIn("accessibility interface", result["text"])
        self.assertEqual(self.recorded[-1]["backend"], "atspi:set-text")

    def test_not_when_the_keys_went_to_another_field(self):
        app = field("", source="atspi", path='gedit window "Find" > textbox "Search for"', name="Search for")
        other = field("", source="atspi", path='gedit window "Find" > textbox "Replace with"', name="Replace with",
                      ref="e6")
        screen = Screen(app, other)
        with mock.patch.object(core.atspi, "set_text") as setter:
            result = self.act(screen, "type", ref="e5", text="colour",
                              perform=lambda *a, **k: screen.set(other["path"], value="colour"))
        setter.assert_not_called()
        self.assertEqual((result["effect"], result["decision"]), (outcome.SUSPECTED_NOOP, outcome.ESCALATE))

    def test_never_a_secret_field(self):
        secret = field("", source="atspi", path='app window "Sign in" > textbox "Password"', name="Password",
                       role="password text")
        screen = Screen(secret)
        with mock.patch.object(core.atspi, "set_text") as setter:
            self.act(screen, "type", ref="e5", text="pw")
        setter.assert_not_called()


if __name__ == "__main__":
    unittest.main()
