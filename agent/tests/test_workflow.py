"""
The workflow view (UI-Mate's subtasks with completion criteria) and the SKILL.md
export (Understudy's intent / routes / GUI-hints layers, in the Agent Skills format).

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import importlib.util
import json
import os
import re
import tempfile
import threading
import unittest
from unittest import mock
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import event, field
from test_skills import demo

from decurix_agent import http_api
from decurix_agent.computer import learning, recording, replay, skillmd, skills, workflow


def two_part_program():
    """A recording that moves from one place to another: two phases."""

    meta = {"id": "demo-20260921T090000-aaaaaa", "title": "Pay an invoice", "started_at": 1789380000}
    events = [
        event(0, "click", field("link", "Invoices"), window="Shop - Chromium",
              expect={"appeared": [{"role": "heading", "name": "Invoices"}]}),
        event(1, "click", field("link", "Open the invoice"), window="Shop - Chromium"),
        event(2, "click", field("menuitem", "Export"), window="Accounts - LibreOffice Calc",
              expect={"appeared": [{"role": "dialog", "name": "Export"}]}),
        event(3, "click", field("button", "Pay now"), window="Accounts - LibreOffice Calc", approved=True,
              expect={"appeared": [{"role": "alert", "name": "Paid"}]}),
    ]
    return replay.compile_program(meta, events)


class Workflow(unittest.TestCase):

    def test_a_run_of_steps_in_one_place_is_a_subtask_with_what_it_should_leave(self):
        subtasks = workflow.build(two_part_program())

        self.assertEqual([s["place"] for s in subtasks], ["Shop - Chromium", "Accounts - LibreOffice Calc"])
        self.assertEqual((subtasks[0]["first"], subtasks[0]["last"]), (1, 2))
        # A part is finished by what the screen shows, taken from the last thing its steps expected.
        self.assertEqual(subtasks[1]["done_when"], ['alert "Paid"'])

    def test_a_part_that_left_nothing_to_check_says_so(self):
        program = two_part_program()
        for step in program["steps"][:2]:
            step["expect"] = []

        self.assertEqual(workflow.build(program)[0]["done_when"], [])
        self.assertIn("nothing here can be confirmed", workflow.render(program))

    def test_guidance_says_what_to_do_and_never_where_on_the_screen(self):
        program = two_part_program()
        program["steps"][1] = dict(program["steps"][1], target=None, point=[640, 360], summary="click 640,360")
        program["steps"][1].pop("target")

        text = workflow.render(program)

        self.assertNotIn("640", text)
        self.assertIn("the control shown in the recording", text)

    def test_the_current_part_is_shown_in_full_and_the_others_as_goals(self):
        text = workflow.render(two_part_program(), current=2)
        lines = text.splitlines()

        self.assertIn("[x] 1.", next(l for l in lines if l.startswith("[")))
        self.assertTrue(any(l.startswith("[>] 2.") for l in lines))
        # The steps of the part in hand, and only that part's.
        self.assertIn('click menuitem "Export"', text)
        self.assertNotIn('click link "Invoices"\n', text.split("[>] 2.")[1])

    def test_it_carries_the_rule_that_the_live_screen_wins(self):
        self.assertIn("the screen in front of you is what is true", workflow.render(two_part_program()))

    def test_approval_steps_and_secret_values_are_named(self):
        program = two_part_program()
        program["params"] = [{"name": "password", "secret": True}, {"name": "customer", "secret": False,
                                                                     "default": None, "example": "Acme"}]
        text = workflow.render(program)

        # Part 2 is not the current one, so its steps are not listed - but its boundary is stated.
        self.assertIn("[1 step(s) stop for a person's approval]", text)
        self.assertIn("stops for a person's approval", workflow.render(program, current=2))
        self.assertIn("secret - never in a recording, must be given", text)
        self.assertIn("changes every run; recorded as 'Acme'", text)

    def test_a_manual_step_is_named_as_a_persons_and_never_cuts_a_goal_mid_word(self):
        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")])
        goal = workflow.build(skills.program(skill))[0]["goal"]

        self.assertIn("a step a person does by hand", goal)
        self.assertNotIn("(type your", goal)

    def test_a_program_with_no_steps_has_no_workflow(self):
        self.assertEqual(workflow.build({"steps": []}), [])
        self.assertEqual(workflow.render({"steps": []}), "")

    def test_an_engine_told_about_a_recording_is_told_its_parts(self):
        meta = {"id": "demo-20260921T090000-aaaaaa", "title": "Pay an invoice", "started_at": 1789380000}
        program = two_part_program()
        text = replay.render_text(meta, program)

        self.assertIn("Parts: 1. Shop - Chromium (steps 1-2)", text)
        self.assertIn("2. Accounts - LibreOffice Calc (steps 3-4), done when", text)


def front_matter(markdown):
    """The YAML front matter of a SKILL.md as a flat dict (enough of YAML for what is written)."""

    match = re.match(r"^---\n(.*?)\n---\n", markdown, re.S)
    assert match, "no front matter"

    found, section = {}, None
    for line in match.group(1).splitlines():
        top = re.match(r"^([\w-]+):\s*(.*)$", line)
        nested = re.match(r"^\s+([\w-]+):\s*(.*)$", line)
        if top and not line.startswith(" "):
            # A key with nothing after it holds a map of the indented lines that follow.
            found[top.group(1)] = ({} if top.group(2) == "" else
                                   json.loads(top.group(2)) if top.group(2).startswith('"') else top.group(2))
            section = top.group(1)
        elif nested and isinstance(found.get(section), dict):
            found[section][nested.group(1)] = nested.group(2)
    return found


class SkillMarkdown(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        previous_root = skills.ROOT
        self.addCleanup(setattr, skills, "ROOT", previous_root)
        skills.ROOT = Path(tmp.name)
        self.skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme", banner=True),
                                     demo("demo-20260914T101600-bbbbbb", "Globex")])
        self.skill["status"] = "ready"
        self.skill["done_when"] = "the page shows a line beginning Saved"
        self.text = skillmd.render(self.skill)

    def test_the_front_matter_is_what_agent_skills_requires(self):
        meta = front_matter(self.text)

        self.assertEqual(meta["name"], "add-a-customer")
        self.assertTrue(re.match(r"^[a-z0-9]+(-[a-z0-9]+)*$", meta["name"]))
        self.assertLessEqual(len(meta["name"]), 64)
        self.assertTrue(0 < len(meta["description"]) <= 1024)
        self.assertIn("Use when asked to add a customer", meta["description"])
        self.assertIn("customer_name", meta["description"])

    def test_it_has_understudys_three_layers(self):
        for heading in ("## Intent", "## Routes", "## GUI replay hints"):
            self.assertIn(heading, self.text)

    def test_the_intent_says_what_finishes_it_what_it_needs_and_what_only_a_person_does(self):
        self.assertIn("**Done when:** the page shows a line beginning Saved", self.text)
        self.assertIn("`customer_name` - Customer name, recorded as `Acme` (changes every run)", self.text)
        self.assertIn("**By a person:** step 3", self.text)
        self.assertIn("never in a run's inputs", self.text)

    def test_what_stops_for_approval_is_stated_as_a_boundary(self):
        self.assertIn("**Needs approval**", self.text)
        self.assertIn('- step 5: click button "Save"', self.text)

    def test_the_routes_are_what_this_can_actually_do(self):
        self.assertIn('`gui do add-a-customer --param customer_name="..."`', self.text)
        self.assertIn("**Fallback:**", self.text)
        self.assertIn("**Observed in the demonstration:**", self.text)

    def test_the_hints_say_how_each_step_is_found_and_never_where_it_is(self):
        hints = self.text.split("## GUI replay hints")[1]

        self.assertIn("by its role and name", hints)
        self.assertNotRegex(hints, r"\d+,\s*\d+\)")
        self.assertIn("not replayed: a person does it", hints)

    def test_a_click_known_only_by_a_picture_says_what_that_can_and_cannot_tell(self):
        skill = json.loads(json.dumps(self.skill))
        step = skill["steps"][0]
        step.pop("target")
        step.update(kind="click", point=[10, 20], look={"png": "x", "partial": True}, summary="click 10,20")

        self.assertIn("by a picture of what was around it", skillmd.render(skill))
        self.assertIn("cannot tell apart something that differs beyond it", skillmd.render(skill))

    def test_an_unnamed_control_found_by_place_is_named_so(self):
        skill = json.loads(json.dumps(self.skill))
        skill["steps"][1]["target"]["nth"] = 2

        self.assertIn("as the 2nd textbox with no name", skillmd.render(skill))

    def test_a_secret_never_appears(self):
        self.assertNotIn("hunter2", self.text)
        self.assertNotIn("Password", front_matter(self.text)["description"])

    def test_a_name_is_made_to_fit_the_standard(self):
        self.assertEqual(skillmd._name("Add--A  Customer_"), "add-a-customer")
        self.assertEqual(skillmd._name("x" * 100), "x" * 64)
        self.assertEqual(skillmd._name("---"), "decurix-skill")

    def test_the_metadata_is_a_flat_map_of_strings_as_the_spec_says(self):
        meta = front_matter(self.text)["metadata"]

        self.assertEqual(meta["decurix-artifact-kind"], '"skill"')
        self.assertEqual(json.loads(meta["decurix-version"]), "1")
        self.assertTrue(all(not v.startswith("{") for v in meta.values()))

    def test_angle_brackets_never_reach_the_front_matter(self):
        skill = json.loads(json.dumps(self.skill))
        skill["title"] = "Add <b>a</b> customer"
        skill["done_when"] = "the page says <Saved>"

        self.assertNotRegex(front_matter(skillmd.render(skill))["description"], r"[<>]")

    @unittest.skipUnless(importlib.util.find_spec("skills_ref"), "the Agent Skills reference validator is not installed")
    def test_the_official_validator_accepts_it_and_reads_its_properties(self):
        import skills_ref

        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp) / "add-a-customer"
            folder.mkdir()
            (folder / "SKILL.md").write_text(self.text)

            self.assertEqual(skills_ref.validate(folder), [])
            properties = skills_ref.read_properties(folder)
            self.assertEqual(properties.name, "add-a-customer")
            self.assertEqual(properties.metadata["decurix-version"], "1")

    def test_a_long_title_cannot_overrun_the_description(self):
        skill = json.loads(json.dumps(self.skill))
        skill["title"] = "Add a customer " * 200

        self.assertLessEqual(len(front_matter(skillmd.render(skill))["description"]), 1024)


class Routes(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.saved = skills.ROOT, recording.ROOT
        skills.ROOT, recording.ROOT = Path(self.tmp.name) / "skills", Path(self.tmp.name) / "recordings"

        skill = learning.build([demo("demo-20260914T101500-aaaaaa", "Acme")], name="add-a-customer")
        skill["status"] = skills.READY
        skills.save(skill)

        recording.ensure("demo-20260914T101500-aaaaaa", title="Add a customer")
        recording.append("demo-20260914T101500-aaaaaa", {"kind": "click", "outcome": "done", "actor": "human",
                                                          "element": {"role": "button", "name": "Save"}})

        # A desktop's agent serves only with its token (http_api.refusal); the control plane sends it.
        token = mock.patch.dict(os.environ, {"DECURIX_AGENT_TOKEN": "test-agent-token"})
        token.start()
        self.addCleanup(token.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        skills.ROOT, recording.ROOT = self.saved
        self.tmp.cleanup()

    def get(self, path):
        request = urllib.request.Request(self.base + path, headers={"Authorization": "Bearer test-agent-token"})
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, dict(response.headers), response.read().decode()
        except urllib.error.HTTPError as err:
            with err:
                return err.code, dict(err.headers), err.read().decode()

    def test_a_skill_is_downloaded_as_a_markdown_file(self):
        status, headers, body = self.get("/skills/add-a-customer/skill.md")

        self.assertEqual(status, 200)
        self.assertTrue(headers["Content-Type"].startswith("text/markdown"))
        self.assertIn('filename="add-a-customer-SKILL.md"', headers["Content-Disposition"])
        self.assertTrue(body.startswith("---\nname: add-a-customer\n"))

    def test_a_skill_that_is_not_there_is_a_404_and_a_bad_name_never_touches_a_file(self):
        self.assertEqual(self.get("/skills/nothing/skill.md")[0], 404)
        self.assertEqual(self.get("/skills/..%2Fetc/skill.md")[0], 404)

    def test_a_recordings_workflow_is_data_and_text(self):
        status, _, body = self.get("/recordings/demo-20260914T101500-aaaaaa/workflow")
        data = json.loads(body)

        self.assertEqual(status, 200)
        self.assertEqual(len(data["workflow"]), 1)
        self.assertIn("Workflow:", data["text"])


if __name__ == "__main__":
    unittest.main()
