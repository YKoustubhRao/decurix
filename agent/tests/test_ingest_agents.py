"""
Ingest of what other agents did: BrowserGym / AgentLab trajectories and OpenHands
event streams. The fixtures are real - BrowserGym 0.14.3 solving MiniWoB++ tasks
in Chromium (bench/agents/gen_trajectories.py) - so what these tests assert is
what the real trees and real event shapes do, not what was expected of them.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import tempfile
import unittest
from pathlib import Path

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import FakeScreen, field

from decurix_agent.computer import ingest, ingest_agents as agents, lint, recording, replay

FIXTURES = Path(__file__).parent / "fixtures" / "agents"


def fixture(name):
    return json.loads((FIXTURES / name).read_text())


LOGIN_TREE = fixture("login-user-3.browsergym.json")["steps"][0]["axtree_txt"]


class Tree(unittest.TestCase):
    """BrowserGym's accessibility text, in the grammar of its own formatter."""

    def test_a_real_tree_reads_back_as_nodes_with_their_depth_bid_role_and_name(self):
        nodes = agents.parse_axtree_text(LOGIN_TREE)
        by_bid = {n["bid"]: n for n in nodes if n["bid"]}

        self.assertEqual(nodes[0]["role"], "RootWebArea")
        self.assertEqual((by_bid["16"]["role"], by_bid["16"]["name"], by_bid["16"]["depth"]), ("textbox", "", 2))
        self.assertEqual(by_bid["20"]["name"], "Login")

    def test_a_value_follows_the_name_after_a_space_and_the_other_properties_after_commas(self):
        # A first version took properties to start at a comma: every filled-in field
        # and every dropdown fell out of the tree.
        nodes = agents.parse_axtree_text(
            "\t[13] combobox '' value='Arielle', hasPopup='menu', expanded=False\n"
            "\t[16] textbox '' value=\"it's\", focused, required\n"
            "\t[17] generic, atomic\n"
            "\t\t[14] option 'Arielle', selected=True")

        self.assertEqual([n["role"] for n in nodes], ["combobox", "textbox", "generic", "option"])
        self.assertEqual(nodes[0]["props"]["value"], "Arielle")
        self.assertEqual(nodes[1]["props"], {"value": "it's", "focused": True, "required": True})
        self.assertEqual((nodes[2]["name"], nodes[2]["props"]), ("", {"atomic": True}))
        self.assertEqual(nodes[3]["depth"], 2)

    def test_unnamed_controls_are_told_apart_by_their_place_and_called_by_their_label(self):
        nodes = agents.parse_axtree_text(LOGIN_TREE)

        first, second = agents.identify(nodes, "16"), agents.identify(nodes, "19")
        self.assertEqual((first["nth"], first["hint"]), (1, "Username"))
        self.assertEqual((second["nth"], second["hint"]), (2, "Password"))

    def test_a_named_control_has_no_place_or_hint_and_a_label_is_never_a_neighbours_value(self):
        # The Login button once got `••••` - the value of the field before it - as its hint.
        button = agents.identify(agents.parse_axtree_text(LOGIN_TREE), "20")
        self.assertEqual(button, {"role": "button", "name": "Login", "source": "cdp"})

    def test_a_bid_the_tree_does_not_have_is_not_guessed(self):
        self.assertIsNone(agents.identify(agents.parse_axtree_text(LOGIN_TREE), "99"))
        self.assertIsNone(agents.identify(None, "16"))

    def test_openhands_keeps_the_tree_as_a_chrome_object_and_reads_the_same(self):
        events = fixture("login-user-3.openhands.json")
        ax = next(e for e in events if e.get("observation") == "browse")["extras"]["axtree_object"]
        from_object = agents.nodes_from_object(ax)

        first = agents.identify(from_object, "16")
        self.assertEqual((first["role"], first["name"], first["nth"], first["hint"]), ("textbox", "", 1, "Username"))


class Actions(unittest.TestCase):

    def test_actions_are_read_never_run(self):
        self.assertEqual(agents.parse_actions("fill('14', 'x')\nclick('15')"),
                         [("fill", ["14", "x"], {}), ("click", ["15"], {})])
        self.assertEqual(agents.parse_actions("```python\nclick('5', button='right')\n```"),
                         [("click", ["5"], {"button": "right"})])

        for hostile in ("__import__('os').system('true')", "click(open('/etc/passwd').read())",
                        "x = 1", "click(a + b)", "obj.click('1')", "click('1'); import os"):
            with self.assertRaises(ingest.IngestError, msg=hostile):
                agents.parse_actions(hostile)

    def test_a_key_combination_is_pressed_by_the_names_a_replay_uses(self):
        self.assertEqual(agents._keys("Enter"), "Return")
        self.assertEqual(agents._keys("Control+a"), "ctrl+a")
        self.assertEqual(agents._keys("ControlOrMeta+Shift+ArrowDown"), "ctrl+shift+Down")


class BrowserGym(unittest.TestCase):

    def setUp(self):
        self.parsed = agents.from_browsergym(fixture("login-user-3.browsergym.json"))
        self.events = self.parsed["events"]

    def test_it_is_recognised_and_titled_by_its_goal(self):
        self.assertEqual(ingest.detect(fixture("login-user-3.browsergym.json")), ingest.BROWSERGYM)
        self.assertIn("Enter the username", self.parsed["title"])

    def test_each_action_becomes_a_step_on_the_control_its_number_meant(self):
        self.assertEqual([e["kind"] for e in self.events], ["type", "type", "click"])

        first, second, button = [e["element"] for e in self.events]
        self.assertEqual((first["role"], first["name"], first["nth"], first["hint"]), ("textbox", "", 1, "Username"))
        self.assertEqual((second["nth"], second["hint"]), (2, "Password"))
        self.assertEqual((button["role"], button["name"]), ("button", "Login"))

    def test_steps_read_the_way_recorded_ones_do_and_name_an_unnamed_field_by_its_label(self):
        lines = [e["summary"] for e in self.events]

        self.assertEqual(lines[0], 'type \'kenda\' into textbox "Username (#1)"'.replace("'", '"'))
        self.assertIn("Password (#2)", lines[1])
        self.assertEqual(lines[2], 'click button "Login"')

    def test_a_password_is_withheld_because_the_tree_shows_its_value_as_bullets(self):
        # Nothing in the tree calls the field a password - it is a plain textbox - but
        # once typed into, BrowserGym reports its value as bullets.
        username, password = self.events[0], self.events[1]

        self.assertEqual(username["text"], "kenda")
        self.assertIsNone(password["text"])
        self.assertEqual(password["withheld"], {"text": "password field"})
        self.assertTrue(password["element"]["secret"])
        self.assertNotIn("Ttlh", json.dumps(self.events))

    def test_the_masking_is_used_even_when_no_label_says_password(self):
        tree = "RootWebArea 'x'\n\t[5] textbox ''\n\t[6] button 'Go'"
        after = "RootWebArea 'x'\n\t[5] textbox '' value='•••'\n\t[6] button 'Go'"
        parsed = agents.from_browsergym({"steps": [{"action": "fill('5', 'hunter2')", "axtree_txt": tree}],
                                         "final": {"axtree_txt": after}})

        self.assertEqual(parsed["events"][0]["withheld"], {"text": "password field"})
        self.assertNotIn("hunter2", json.dumps(parsed))

    def test_what_was_typed_is_expected_back_in_its_field(self):
        self.assertEqual(self.events[0]["expect"], {"values": [{"role": "textbox", "name": "", "value": "kenda"}]})

    def test_a_dropdown_choice_names_its_option(self):
        parsed = agents.from_browsergym(fixture("choose-list-3.browsergym.json"))
        select = parsed["events"][0]

        self.assertEqual((select["kind"], select["option"], select["element"]["role"]), ("select", "Arielle", "combobox"))
        self.assertIn("'Arielle'", select["summary"])

    def test_every_checkbox_of_a_choice_is_its_own_step_by_its_own_name(self):
        parsed = agents.from_browsergym(fixture("click-checkboxes-1.browsergym.json"))
        names = [e["element"]["name"] for e in parsed["events"] if e["element"].get("role") == "checkbox"]

        self.assertEqual(len(names), 4)
        self.assertTrue(all(names))

    def test_a_step_whose_number_is_not_in_its_tree_is_kept_with_no_target_and_says_why(self):
        parsed = agents.from_browsergym({"steps": [{"action": "click('99')", "axtree_txt": LOGIN_TREE}]})
        event = parsed["events"][0]

        self.assertEqual(event["element"], {})
        self.assertIn("no control numbered 99", event["imported"]["why"])

    def test_a_trajectory_with_no_trees_at_all_says_so(self):
        parsed = agents.from_browsergym({"steps": [{"action": "click('5')"}]})

        self.assertEqual(parsed["events"][0]["element"], {})
        self.assertTrue(any("no control can be found again" in w for w in parsed["warnings"]))

    def test_what_a_replay_cannot_do_is_listed_with_why(self):
        parsed = agents.from_browsergym({"steps": [
            {"action": "hover('5')", "axtree_txt": LOGIN_TREE},
            {"action": "click('20', modifiers=['Shift'])", "axtree_txt": LOGIN_TREE},
            {"action": "drag_and_drop('16', '19')", "axtree_txt": LOGIN_TREE},
            {"action": "noop()"},
            {"action": "this is not an action"},
        ]})

        self.assertEqual(parsed["events"], [])
        self.assertEqual({s["type"] for s in parsed["skipped"]}, {"hover", "click", "drag_and_drop", "action"})
        self.assertEqual(parsed["ignored"], 1)

    def test_focus_and_check_are_clicks_and_say_so(self):
        parsed = agents.from_browsergym({"steps": [{"action": "focus('16')", "axtree_txt": LOGIN_TREE},
                                                   {"action": "check('18')", "axtree_txt": LOGIN_TREE}]})

        self.assertEqual([e["kind"] for e in parsed["events"]], ["click", "click"])
        self.assertTrue(any("clicking it" in w for w in parsed["warnings"]))

    def test_a_click_on_a_position_is_a_blind_click_lint_says_so(self):
        parsed = agents.from_browsergym({"steps": [{"action": "mouse_click(120, 45)"}]})

        self.assertEqual(parsed["events"][0]["point"], [120, 45])


class OpenHands(unittest.TestCase):

    def test_it_is_recognised_by_its_events(self):
        self.assertEqual(ingest.detect(fixture("login-user-3.openhands.json")), ingest.OPENHANDS)
        self.assertEqual(ingest.detect({"history": fixture("login-user-3.openhands.json")}), ingest.OPENHANDS)
        self.assertIsNone(ingest.detect([{"nothing": "here"}]))

    def test_it_makes_the_same_steps_the_same_run_makes_in_browsergym(self):
        theirs = agents.from_openhands(fixture("login-user-3.openhands.json"))["events"]
        ours = agents.from_browsergym(fixture("login-user-3.browsergym.json"))["events"]

        self.assertEqual([e["kind"] for e in theirs], ["open"] + [e["kind"] for e in ours])
        self.assertEqual([e["element"] for e in theirs[1:]], [e["element"] for e in ours])

    def test_the_first_user_message_titles_it_and_the_page_it_browsed_to_is_an_open_step(self):
        parsed = agents.from_openhands(fixture("login-user-3.openhands.json"))

        self.assertIn("Enter the username", parsed["title"])
        self.assertEqual(parsed["events"][0]["kind"], "open")
        self.assertEqual(parsed["events"][0]["expect"], {"url": "file:///miniwob/login-user.html"})

    def test_a_password_is_withheld_from_the_observation_that_follows_it(self):
        parsed = agents.from_openhands(fixture("login-user-3.openhands.json"))
        password = next(e for e in parsed["events"] if e["kind"] == "type" and e["element"].get("nth") == 2)

        self.assertIsNone(password["text"])
        # The tree after it shows the field's value as bullets - kept on the node itself, not
        # among its properties - which is the same evidence BrowserGym's text gives.
        self.assertEqual(password["withheld"], {"text": "password field"})
        self.assertNotIn("Ttlh", json.dumps(parsed))

    def test_a_credential_a_task_spells_out_is_not_kept_in_the_title(self):
        # MiniWoB's own goal reads: the password "Ttlh". A title is shown to everyone
        # who opens the page and exported with the recording.
        parsed = agents.from_openhands(fixture("login-user-3.openhands.json"))
        self.assertNotIn("Ttlh", parsed["title"])
        self.assertIn("[withheld]", parsed["title"])
        self.assertIn('username "kenda"', parsed["title"])

    def test_a_saved_trajectory_has_lost_its_trees_and_the_import_says_so_instead_of_guessing(self):
        # OpenHands deletes `axtree_object` when it saves a trajectory.
        saved = agents.from_openhands(fixture("login-user-3.openhands-saved.json"))
        clicks = [e for e in saved["events"] if e["kind"] != "open"]

        self.assertTrue(all(e["element"] == {} for e in clicks))
        self.assertTrue(all("no accessibility tree" in e["imported"]["why"] for e in clicks))
        self.assertTrue(any("removes the accessibility tree" in w for w in saved["warnings"]))

    def test_actions_that_are_not_on_a_screen_are_listed_and_not_taken(self):
        events = fixture("login-user-3.openhands.json") + [
            {"id": 90, "source": "agent", "action": "run", "args": {"command": "ls"}},
            {"id": 91, "source": "agent", "action": "edit", "args": {"path": "x"}},
        ]
        parsed = agents.from_openhands(events)

        self.assertEqual({s["type"] for s in parsed["skipped"]}, {"run", "edit"})

    def test_several_actions_in_one_string_are_several_steps(self):
        events = fixture("login-user-3.openhands.json")
        for e in events:
            if e.get("action") == "browse_interactive" and e["args"]["browser_actions"].startswith("fill('16'"):
                e["args"]["browser_actions"] = "fill('16', 'kenda')\nfill('19', 'Ttlh')"

        parsed = agents.from_openhands(events)
        self.assertGreaterEqual(len([e for e in parsed["events"] if e["kind"] == "type"]), 2)


class Saving(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT
        recording.ROOT = Path(self.tmp.name)

    def tearDown(self):
        recording.ROOT = self.saved
        self.tmp.cleanup()

    def test_an_agents_trajectory_is_saved_as_what_an_agent_did(self):
        result = ingest.import_recording(fixture("login-user-3.browsergym.json"))
        meta, events = recording.load(result["session"])

        self.assertNotIn("Ttlh", json.dumps(meta))

        self.assertEqual((result["format"], result["imported"], meta["actors"]), ("browsergym", 3, ["agent"]))
        self.assertEqual(meta["source"]["format"], "browsergym")
        self.assertEqual(len(events), 3)
        self.assertNotIn("Ttlh", (recording.ROOT / result["session"] / "events.jsonl").read_text())

    def test_warnings_reach_the_person_who_imported_it(self):
        result = ingest.import_recording(fixture("login-user-3.openhands-saved.json"))
        self.assertTrue(result["warnings"])

    def test_the_kinds_can_be_told_apart_and_an_unknown_one_is_refused(self):
        self.assertEqual(ingest.detect({"steps": [{"type": "click"}]}), ingest.CHROME_RECORDER)
        with self.assertRaises(ingest.IngestError):
            ingest.import_recording({"nothing": "a recorder wrote"})


class Replaying(unittest.TestCase):
    """The point: what an agent did is compiled, graded, and replayed with no model."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS
        recording.ROOT = Path(self.tmp.name)
        replay.FIND_SECONDS = replay.EXPECT_SECONDS = replay.POLL_SECONDS = 0

    def tearDown(self):
        recording.ROOT, replay.core, replay.FIND_SECONDS, replay.EXPECT_SECONDS, replay.POLL_SECONDS = self.saved
        self.tmp.cleanup()

    def program(self, name="login-user-3.browsergym.json"):
        session = ingest.import_recording(fixture(name))["session"]
        meta, events = recording.load(session)
        return replay.compile_program(meta, events)

    def test_unnamed_fields_become_parameters_called_by_their_labels(self):
        program = self.program()
        params = {p["name"]: p for p in program["params"]}

        self.assertEqual(set(params), {"username", "password"})
        self.assertFalse(params["username"]["secret"])
        self.assertTrue(params["password"]["secret"])
        self.assertIsNone(params["password"]["default"])

    def test_a_target_carries_its_place_and_a_missing_secret_stops_the_run_before_it_starts(self):
        program = self.program()

        self.assertEqual(program["steps"][1]["target"]["nth"], 2)

        replay.core = fake = FakeScreen([field("textbox", "", path="a"), field("textbox", "", path="b"),
                                         field("button", "Login")])
        result = replay.run(program, report=lambda line: None)

        self.assertEqual(result["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(result["missing"], ["password"])
        self.assertEqual(fake.acted, [])

    def test_the_second_of_two_unnamed_boxes_is_the_one_typed_into(self):
        program = self.program()
        replay.core = fake = FakeScreen([field("textbox", "", path="first"), field("textbox", "", path="second"),
                                         field("button", "Login", path="go")])

        replay.run(program, params={"password": "S3cret!"}, report=lambda line: None)

        typed = [(kwargs["path"], kwargs["text"]) for kind, kwargs in fake.acted if kind == "type"]
        self.assertEqual(typed, [("first", "kenda"), ("second", "S3cret!")])

    def test_two_unnamed_boxes_with_no_place_are_still_not_guessed_between(self):
        _, point, why = replay.resolve({"target": {"role": "textbox", "name": ""}},
                                       {"elements": [field("textbox", "", path="a"), field("textbox", "", path="b")]})
        self.assertIn("does not guess", why)

    def test_a_place_that_is_not_there_is_not_on_the_screen(self):
        found, _, why = replay.resolve({"target": {"role": "textbox", "name": "", "nth": 3}},
                                       {"elements": [field("textbox", "", path="a"), field("textbox", "", path="b")]})
        self.assertIsNone(found)
        self.assertIn("(#3) is not on the screen", why)

    def test_lint_says_a_target_found_by_its_place_is_only_as_good_as_the_pages_order(self):
        verdict = lint.lint(self.program())

        self.assertIn("positional_target", [f["code"] for f in verdict["findings"]])
        self.assertEqual(verdict["grade"], lint.REHEARSE)

    def test_what_was_stripped_from_a_saved_trajectory_makes_it_risky_before_anything_runs(self):
        verdict = lint.lint(self.program("login-user-3.openhands-saved.json"))

        self.assertEqual(verdict["grade"], lint.RISKY)
        self.assertIn("unidentified_control", [f["code"] for f in verdict["findings"]])

    def test_a_step_that_meant_a_control_nothing_identifies_stops_the_run_before_anything_acts(self):
        # Found by the live run: a typing step with no target types into whatever has
        # focus, and the stripped trajectory's `fill` steps did exactly that before its
        # first click halted. A step that named a control it cannot identify is not that step.
        program = self.program("login-user-3.openhands-saved.json")
        program["steps"] = [s for s in program["steps"] if s["kind"] != "open"]

        replay.core = fake = FakeScreen([field("textbox", "", path="a"), field("textbox", "", path="b"),
                                         field("button", "Login")])
        result = replay.run(program, report=lambda line: None)

        self.assertEqual(fake.acted, [])
        self.assertEqual((result["outcome"], result["step"]), (replay.HALTED_BEFORE_EFFECT, program["steps"][0]["n"]))
        self.assertIn("cannot identify", result["text"])

    def test_typing_into_whatever_has_focus_is_still_a_step_when_that_is_what_was_meant(self):
        parsed = agents.from_browsergym({"steps": [{"action": "keyboard_type('hello')"}]})
        meta = {"id": "demo-20260921T100000-aaaaaa", "kind": "demonstration", "started_at": 1789380000}
        step = replay.compile_program(meta, [dict(e, i=0) for e in parsed["events"]])["steps"][0]

        self.assertEqual(step["kind"], "type")
        self.assertNotIn("unresolved", step)


if __name__ == "__main__":
    unittest.main()
