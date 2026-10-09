"""
What a person's demonstration on a native Windows worker keeps, and how a replay
puts it back: each press named by what was under it at the moment it was made,
a drop-down's choosing as one select step, the program each step was in, and a
replay that opens that program when it is not there - with this run's values,
never the recorded ones. Found live on a worker (see demonstration.py, replay.py).
"""

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from decurix_agent import guidance
from decurix_agent.computer import cdp, demonstration, launchers, mimic, replay
from decurix_agent.computer.errors import ComputerError


def recorder():
    with patch.object(demonstration.recording, "meta", return_value={}):
        return demonstration.Recorder("demo-test")


class WhatAnEntrySays(unittest.TestCase):

    def test_typing_into_a_field_that_was_empty_replaces_it_on_replay(self):
        # Measured: a look taken part way through the typing read "Acme Lt",
        # and the step was kept as one that adds to whatever the field holds.
        field = {"role": "textbox", "name": "Customer", "value": "", "states": [], "source": "uia"}
        located = {"window": "Orders", "app": {"name": "orders", "exe": "C:\\o.exe"}, "url": "", "element": field, "where": ""}
        entry = recorder()._entry({"kind": "type", "text": "Acme Ltd", "t": 0, "t_end": 1}, field, None, None, located)
        self.assertFalse(entry["append"])
        self.assertEqual(entry["window"], "Orders")
        self.assertEqual(entry["app"]["name"], "orders")

    def test_typing_after_what_a_field_held_adds_to_it(self):
        field = {"role": "textbox", "name": "Note", "value": "Dear", "states": [], "source": "uia"}
        located = {"window": "Mail", "app": None, "url": "", "element": field, "where": ""}
        entry = recorder()._entry({"kind": "type", "text": " Sir", "t": 0, "t_end": 1}, field, None, None, located)
        self.assertTrue(entry["append"])

    def test_a_value_the_settled_screen_shows_is_kept_as_a_check(self):
        field = {"role": "textbox", "name": "Customer", "group": "cdp:T", "window_title": "New order"}
        after = {"elements": [dict(field, value="Acme", window_title="Order saved")]}
        held = demonstration._holds({"kind": "type", "text": "Acme", "append": False}, field, after)
        self.assertEqual(held, {"role": "textbox", "name": "Customer", "value": "Acme"})
        self.assertIsNone(demonstration._holds({"kind": "type", "text": "Acme", "append": True}, field, after))
        self.assertIsNone(demonstration._holds({"kind": "type", "text": "Other", "append": False}, field, after))

    def test_controls_of_a_covered_window_are_not_what_a_press_was_on(self):
        windows = [{"title": "Orders", "box": (0, 0, 800, 600)}, {"title": "Notepad", "box": (0, 0, 400, 400)}]
        behind = {"window_title": "Orders", "role": "textbox"}
        front = {"window_title": "Notepad", "role": "button"}
        menu = {"window_title": "", "role": "menuitem"}
        self.assertEqual(demonstration._unoccluded([behind, front, menu], windows, 50, 50), [front, menu])
        self.assertEqual(demonstration._unoccluded([behind, front], windows, 700, 500), [behind, front])


class PageHitTest(unittest.TestCase):
    """cdp._describe: the element a DOM node is, as a look at the page lists it."""

    class Page:
        target = {"id": "T"}

        def __init__(self, nodes):
            self.nodes = nodes

        def call(self, method, params=None):
            if method == "Accessibility.getPartialAXTree":
                return {"nodes": self.nodes}
            if method == "DOM.describeNode":
                return {"node": {"nodeName": "SELECT", "attributes": ["type", "x"]}}
            if method == "DOM.getContentQuads":
                return {"quads": [[10, 20, 110, 20, 110, 50, 10, 50]]}
            raise ComputerError(method)

    METRICS = {"url": "http://shop.test/order", "title": "Order", "scx": 0, "scy": 0}

    def test_a_node_inside_a_controls_own_shadow_tree_is_that_control(self):
        # Measured: a press on a drop-down's arrow hit a node that names no
        # parent; the drop-down is found as the node that lists it as a child.
        nodes = [
            {"nodeId": "0", "backendDOMNodeId": 217, "ignored": True, "role": {"value": "none"}},
            {"nodeId": "192", "backendDOMNodeId": 192, "role": {"value": "combobox"}, "name": {"value": "Country"},
             "value": {"value": "Choose"}, "childIds": ["0"], "parentId": "201"},
        ]
        with patch.object(cdp, "_viewport", return_value=(0, 100, 1.0)):
            found = cdp._describe(self.Page(nodes), self.METRICS, 217, {})
        self.assertEqual((found["role"], found["name"], found["value"]), ("combobox", "Country", "Choose"))
        self.assertEqual(found["path"], 'site shop.test > combobox "Country"')
        self.assertEqual(found["handle"]["tag"], "select")
        self.assertEqual(found["box"], [10, 120, 100, 30])

    def test_a_block_that_only_holds_text_is_that_text(self):
        nodes = [
            {"nodeId": "251", "backendDOMNodeId": 251, "role": {"value": "generic"}, "name": {"value": ""},
             "childIds": ["211"], "parentId": "248"},
            {"nodeId": "211", "backendDOMNodeId": 211, "role": {"value": "StaticText"}, "name": {"value": "Urgent"},
             "parentId": "251"},
        ]
        with patch.object(cdp, "_viewport", return_value=(0, 0, 1.0)):
            found = cdp._describe(self.Page(nodes), self.METRICS, 251, {})
        self.assertEqual((found["role"], found["name"]), ("text", "Urgent"))
        listed = {"role": "text", "name": "Urgent", "path": "site shop.test > text \"Urgent\"", "source": "cdp",
                  "handle": {"backend_id": 211, "target_id": "T"}}
        with patch.object(cdp, "_viewport", return_value=(0, 0, 1.0)):
            self.assertEqual(cdp._describe(self.Page(nodes), self.METRICS, 251, {211: listed})["path"], listed["path"])


class ADropDownsChoiceIsAValue(unittest.TestCase):

    EVENTS = [
        {"i": 0, "kind": "type", "outcome": "done", "text": "Initech", "actor": "human",
         "element": {"role": "textbox", "name": "Customer name", "path": "site s > textbox \"Customer name\""},
         "expect": {"values": [{"role": "textbox", "name": "Customer name", "value": "Initech"}]}},
        {"i": 1, "kind": "select", "outcome": "done", "option": "Japan", "actor": "human", "url": "http://s/order",
         "window": "New order - Chromium", "app": {"name": "chrome", "exe": "C:\\chrome.exe"},
         "element": {"role": "combobox", "name": "Country", "path": "site s > combobox \"Country\""},
         "expect": {"values": [{"role": "combobox", "name": "Country", "value": "Japan"}]}},
    ]

    def program(self):
        return replay.compile_program({"id": "demo-x", "title": "enter an order"}, self.EVENTS)

    def test_the_option_chosen_is_a_parameter_checked_after_the_step(self):
        program = self.program()
        country = next(p for p in program["params"] if p["name"] == "country")
        self.assertEqual((country["default"], country.get("choice")), ("Japan", True))
        step = program["steps"][1]
        self.assertEqual(step["summary"], 'select {country} in combobox "Country"')
        self.assertIn({"type": "value", "role": "combobox", "name": "Country", "param": "country"}, step["expect"])
        self.assertEqual((step["app"]["name"], step["url"]), ("chrome", "http://s/order"))

    def test_a_run_chooses_this_runs_option(self):
        step = self.program()["steps"][1]
        with patch.object(replay, "_find", return_value=({"path": "p"}, None, {}, None)), \
                patch.object(replay.core, "act", return_value={"ok": True}) as act:
            replay._run_step(step, {"country": "Germany"})
        self.assertEqual(act.call_args.kwargs["option"], "Germany")

    def test_a_request_with_another_option_is_the_same_task(self):
        places = mimic._places_from_events(self.EVENTS)
        found = mimic.align('enter an order for "Initech" in Japan', 'enter an order for "Globex" in Germany', places)
        self.assertIsNotNone(found)
        self.assertTrue(all(span["placed"] for span in found["spans"]))


class AnEnginesReplayUsesThisRunsValues(unittest.TestCase):
    """Found live: handed a request for "Globex", the engine replayed the recording as it was - for Initech."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.patches = [patch.object(guidance, "RUN_FILE", Path(self.tmp.name) / "guided-run.json"),
                        patch.object(guidance.task_context, "current", return_value="t0000000000000001")]
        for p in self.patches:
            p.start()

    def tearDown(self):
        guidance.end()
        for p in reversed(self.patches):
            p.stop()
        self.tmp.cleanup()

    def test_values_of_the_run_fill_what_the_engine_left_out(self):
        run = {"one_off": True, "mimic": {"id": "demo-1"}}
        guidance.begin(skill="enter-order", task="x", text="", values={"customer_name": "Globex", "country": "Germany"}, run=run)
        params = [{"name": "customer_name"}, {"name": "country"}]
        self.assertEqual(guidance.with_run_values({"country": "India"}, params, recording="demo-1"),
                         {"country": "India", "customer_name": "Globex"})
        self.assertEqual(guidance.with_run_values({}, params, skill="enter-order"),
                         {"customer_name": "Globex", "country": "Germany"})
        # Another recording, or another task: nothing of this run's.
        self.assertEqual(guidance.with_run_values({}, params, recording="demo-2"), {})
        with patch.object(guidance.task_context, "current", return_value="t0000000000000002"):
            self.assertEqual(guidance.with_run_values({}, params, recording="demo-1"), {})
        guidance.end()
        self.assertEqual(guidance.with_run_values({}, params, recording="demo-1"), {})


if __name__ == "__main__":
    unittest.main()
