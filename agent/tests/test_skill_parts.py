"""
A taught task is reused part by part, never as one tool for the whole workflow (owner, 2026-11-16). Found in the
learning curves: a task taught across three applications was offered as one `gui do` that needed every value up front -
among them a file the run itself exports half way - so it was refused, and the model did every step by hand on every
case. skills.parts cuts the task where the work moves to another application and after each step that commits; a part
pressing a control named by the demonstration's own code is the model's by hand; `gui do --to N` replays one part.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import part_record, replay, skill_engine, skills


def step(n, kind, role=None, name="", url="", param=None, **extra):
    s = {"n": n, "kind": kind, "url": url, "target": {"role": role, "name": name} if role or name else {}}
    if param:
        s["param"] = param
    s.update(extra)
    return s


BOOKS, PREP, PORTAL = "http://books.test/c/1", "http://prep.test/c/1", "http://portal.test/r"

SKILL = {
    "name": "file-a-return", "title": "File a return", "status": skills.READY,
    "params": [
        {"name": "books_url", "label": "Address", "step": 2, "default": "http://books.test", "example": "http://books.test", "varies": True},
        {"name": "month", "label": "Month", "step": 4, "default": "October 2026", "example": "October 2026", "varies": True},
        {"name": "prep_url", "label": "Address", "step": 8, "default": "http://prep.test", "example": "http://prep.test", "varies": True},
        {"name": "client", "label": "Search", "step": 10, "default": "Acme Lifts", "example": "Acme Lifts", "varies": True},
        {"name": "export_file", "label": "File", "step": 12, "default": "AA12_OCT.json", "example": "AA12_OCT.json", "varies": True},
        {"name": "signatory", "label": "Signatory", "step": 20, "default": "R Rao", "example": "R Rao", "varies": True},
    ],
    "steps": [
        step(1, "click", "button", "Web Browser"),
        step(2, "type", param="books_url", url="about:blank"),
        step(3, "click", "link", "Returns", BOOKS),
        step(4, "select", "combobox", "Month", BOOKS, param="month"),
        step(5, "click", "button", "Show", BOOKS),
        step(6, "click", "button", "Export", BOOKS),
        step(7, "key", url=BOOKS, keys=["ctrl+l"]),
        step(8, "type", url=BOOKS, param="prep_url"),
        step(9, "key", url=BOOKS, keys=["Return"]),
        step(10, "type", "textbox", "Search", PREP, param="client"),
        step(11, "click", "button", "Search", PREP),
        step(12, "select", "radio", "AA12_OCT.json", PREP, param="export_file"),
        step(13, "click", "button", "Save to portal", PREP),
        step(14, "click", "checkbox", "Select 29AAQFC6610D1ZG", PREP),
        step(15, "click", "button", "Validate selected", PREP),
        step(16, "click", "button", "Update names", PREP),
        step(17, "click", "link", "Open the portal", PREP),
        step(18, "click", "link", "Returns list", PORTAL),
        step(19, "click", "button", "Generate summary", PORTAL),
        step(20, "select", "combobox", "Signatory", PORTAL, param="signatory"),
        step(21, "click", "button", "File with EVC", PORTAL, consequential=True),
        step(22, "manual", "textbox", "One-time password", PORTAL),
        step(23, "click", "button", "Verify", PORTAL),
    ],
}


class TheTaskInParts(unittest.TestCase):
    def setUp(self):
        self.parts = skills.parts(SKILL)

    def test_it_is_cut_where_the_application_changes_and_after_a_commit(self):
        spans = [(p["first"], p["last"]) for p in self.parts]
        self.assertEqual(spans[0], (1, 6))       # the books, up to the export
        self.assertEqual(spans[1][0], 7)          # the address typed for the next application goes with it
        self.assertIn(13, [p["last"] for p in self.parts])   # a save ends its part
        self.assertEqual(spans[-1][1], 23)
        self.assertEqual([p["k"] for p in self.parts], list(range(1, len(self.parts) + 1)))

    def test_each_part_needs_only_its_own_values(self):
        self.assertEqual(self.parts[0]["params"], ["books_url", "month"])
        self.assertIn("export_file", self.parts[1]["params"])
        self.assertNotIn("export_file", self.parts[0]["params"])
        self.assertEqual(self.parts[1]["where"], "prep.test")

    def test_the_demonstrations_own_case_is_a_part_by_hand(self):
        case = next(p for p in self.parts if p["first"] <= 14 <= p["last"])
        self.assertEqual(case["first"], 14)
        self.assertIn("29AAQFC6610D1ZG", case["by_hand"])
        before = next(p for p in self.parts if p["last"] == 13)
        self.assertEqual(before["by_hand"], "")   # the parts around it still replay

    def test_a_private_value_typed_by_hand_is_asked_not_a_part_by_hand(self):
        # (A one-time code typed into its field is asked of the person and typed for them: skill_task._type_given.)
        self.assertEqual(self.parts[-1]["by_hand"], "")
        self.assertTrue(skills.asks_value(next(s for s in SKILL["steps"] if s["n"] == 22)))

    def test_a_press_the_person_did_by_hand_is_said(self):
        steps = [dict(s, target={"role": "button", "name": "Approve"}) if s["n"] == 22 else s for s in SKILL["steps"]]
        self.assertIn("22", skills.parts(dict(SKILL, steps=steps))[-1]["by_hand"])

    def test_a_value_its_step_types_is_no_case_literal(self):
        # (the exported file's name is a parameter: this run's own value goes there)
        self.assertNotIn("AA12_OCT", " ".join(p["by_hand"] for p in self.parts))

    def test_a_skill_with_a_per_item_loop_has_no_parts(self):
        looped = dict(SKILL, loops=[{"enabled": True, "blocks": [["a"]]}])
        self.assertEqual(skills.parts(looped), [])
        self.assertEqual(skills.parts(dict(SKILL, steps=SKILL["steps"][:4])), [])

    def test_what_a_part_needs(self):
        self.assertEqual([p["name"] for p in skills.needed_from(SKILL, 7, 13)], ["prep_url", "client", "export_file"])
        self.assertEqual(len(skills.needed_from(SKILL, 7)), 4)


class OnePartReplayed(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        for p in (mock.patch.object(skill_engine, "REPLAYED_DIR", Path(self.tmp.name)),
                  mock.patch.object(part_record, "PATH", Path(self.tmp.name) / "skill-parts.json"),
                  mock.patch("decurix_agent.task_context.current", return_value="t00000000000000a1"),
                  mock.patch("decurix_agent.guidance.handed", return_value=True),
                  mock.patch("decurix_agent.learned.note_use"),
                  mock.patch.object(skill_engine.learning, "start_checks", return_value=[])):
            p.start()
            self.addCleanup(p.stop)
        self.ran = []

        def run(program, params=None, start=1, **_):
            self.ran.append([s["n"] for s in program["steps"] if s["n"] >= start])
            return {"outcome": replay.VERIFIED, "text": "Replayed.", "resume_from": None, "step": None}

        p = mock.patch.object(replay, "run", side_effect=run)
        p.start()
        self.addCleanup(p.stop)

    def test_only_its_steps_run_and_only_its_values_are_needed(self):
        result = skill_engine.run(SKILL, params={"prep_url": "http://prep.test", "client": "Beta Co",
                                                 "export_file": "BB_NOV.json"}, start=7, end=13)
        self.assertEqual(self.ran, [list(range(7, 14))])
        self.assertEqual(result["resume_from"], 14)
        self.assertIn("one part", result["text"])

    def test_a_part_that_ran_is_never_replayed_again_and_the_next_one_carries_on(self):
        values = {"books_url": "http://books.test", "month": "November 2026"}
        skill_engine.run(SKILL, params=values, start=1, end=6)
        again = skill_engine.run(SKILL, params=values, start=1, end=6)
        self.assertEqual(again["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertEqual(len(self.ran), 1)
        nxt = skill_engine.run(SKILL, params={"prep_url": "http://prep.test", "client": "Beta Co",
                                              "export_file": "BB_NOV.json"}, start=7, end=13)
        self.assertEqual(nxt["outcome"], replay.VERIFIED)

    def test_a_part_does_not_wait_for_a_later_parts_value(self):
        # The export's file name is not known before the export runs: the first part does not need it.
        result = skill_engine.run(SKILL, params={"books_url": "http://books.test", "month": "November 2026"},
                                  start=1, end=6)
        self.assertEqual(result["outcome"], replay.VERIFIED)
        whole = skill_engine.run(dict(SKILL, name="file-a-return-2"),
                                 params={"books_url": "http://books.test", "month": "November 2026"})
        self.assertEqual(whole["outcome"], replay.HALTED_BEFORE_EFFECT)
        self.assertIn("export_file", whole["text"])

    def test_a_backwards_range_is_refused(self):
        self.assertEqual(skill_engine.run(SKILL, params={}, start=9, end=7)["outcome"], replay.HALTED_BEFORE_EFFECT)


PREP_VALUES = {"prep_url": "http://prep.test", "client": "Beta Co", "export_file": "BB_NOV.json"}
ALL_VALUES = dict(PREP_VALUES, books_url="http://books.test", month="November 2026", signatory="R Rao")


class ProvenPartsRunOnByCode(OnePartReplayed):
    """A part that ran clean in a task that ended checked done is proven; after the part before it ends, it runs on
    by code. Owner (2026-11-16): "repeated sub parts logic can be codified"."""

    def task(self, task_id):
        return mock.patch("decurix_agent.task_context.current", return_value=task_id)

    def test_a_part_is_proven_only_by_a_clean_run_in_a_task_that_ended_done(self):
        part = skills.parts(SKILL)[0]
        with self.task("t00000000000000b1"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=part["last"])
        self.assertEqual(part_record.proven(SKILL, part), 0)        # (the task has not ended yet)
        part_record.settle("t00000000000000b1", False)
        self.assertEqual(part_record.proven(SKILL, part), 0)        # ended, not done
        with self.task("t00000000000000b2"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=part["last"])
        part_record.settle("t00000000000000b2", True)
        self.assertEqual(part_record.proven(SKILL, part), 1)

    def test_after_a_part_ends_the_proven_parts_after_it_run_on(self):
        found = skills.parts(SKILL)
        first, second = found[0], found[1]
        # An earlier task ran parts 1 and 2 clean and ended checked done.
        with self.task("t00000000000000c1"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=first["last"])
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=second["first"], end=second["last"])
        part_record.settle("t00000000000000c1", True)
        self.ran.clear()
        with self.task("t00000000000000c2"):
            result = skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=first["last"])
        self.assertEqual(self.ran[0], list(range(1, first["last"] + 1)))
        self.assertEqual(self.ran[1], list(range(second["first"], second["last"] + 1)))   # ran on, no model turn
        self.assertIn("Ran by code", result["text"])
        self.assertIn(f"part {second['k']}", result["text"])

    def test_a_part_that_stopped_is_the_models_again(self):
        found = skills.parts(SKILL)
        with self.task("t00000000000000d1"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=found[0]["last"])
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=found[1]["first"], end=found[1]["last"])
        part_record.settle("t00000000000000d1", True)
        self.assertEqual(part_record.proven(SKILL, found[1]), 1)
        stop = found[1]["first"] + 1

        def halted(program, params=None, start=1, **_):
            self.ran.append([s["n"] for s in program["steps"] if s["n"] >= start])
            return {"outcome": replay.HALTED, "text": "Stopped.", "resume_from": stop + 1, "step": stop}

        with self.task("t00000000000000d2"), mock.patch.object(replay, "run", side_effect=halted):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=found[1]["first"], end=found[1]["last"])
        self.assertEqual(part_record.proven(SKILL, found[1]), 0)

    def test_a_part_by_hand_or_without_its_values_is_never_run_on(self):
        found = skills.parts(SKILL)
        case = next(p for p in found if p["by_hand"])
        before = next(p for p in found if p["last"] == case["first"] - 1)
        with self.task("t00000000000000e1"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=before["first"], end=before["last"])
        part_record.settle("t00000000000000e1", True)
        self.ran.clear()
        with self.task("t00000000000000e2"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=before["first"], end=before["last"])
        self.assertEqual(len(self.ran), 1)     # the demonstration's own case is the model's

    def test_the_model_is_told_which_parts_are_proven(self):
        from decurix_agent import skill_task
        found = skills.parts(SKILL)
        with self.task("t00000000000000f1"):
            skill_engine.run(SKILL, params=dict(ALL_VALUES), start=1, end=found[0]["last"])
        part_record.settle("t00000000000000f1", True)
        how = skill_task._parts_how({"skill": SKILL, "values": dict(ALL_VALUES)})
        self.assertIn("[proven: ran clean in 1 checked run(s)", how)
        self.assertEqual(how.count("[proven"), 1)


class TheModelIsGivenTheParts(unittest.TestCase):
    def test_each_part_has_its_own_command_and_no_whole_workflow_command(self):
        from decurix_agent import skill_task
        run = {"skill": SKILL, "values": {"books_url": "http://books.test", "month": "November 2026",
                                          "prep_url": "http://prep.test", "client": "Beta Co", "export_file": ""}}
        how = skill_task._parts_how(run)
        self.assertIn("PART BY PART", how)
        self.assertIn("--to 6", how)
        self.assertIn("--from 7 --to", how)
        self.assertIn('export_file="<File, as the screen shows it by then>"', how)
        self.assertIn("by hand:", how)
        self.assertNotIn("signatory=", how.split("--to 6")[0])   # the first part's command holds only its values

    def test_no_parts_means_the_old_guidance(self):
        from decurix_agent import skill_task
        self.assertEqual(skill_task._parts_how({"skill": dict(SKILL, steps=SKILL["steps"][:4]), "values": {}}), "")


if __name__ == "__main__":
    unittest.main()
