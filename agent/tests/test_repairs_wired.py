"""
A taught run's stopped replay goes to a bounded one-step repair, not to the model for the whole rest (repairs.py, wired
into skill_task._execute and _after_phase). Measured in the learning curves (2026-11-15): one click of a month's first
bill stopped the replay, the model was given everything after it, worked it click by click, and the case ran out of
tokens with three bills of five unentered.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_repairs_wired.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import repairs, skill_task
from decurix_agent.computer import replay


def _run():
    steps = [
        {"id": "s1", "n": 1, "kind": "click", "target": {"role": "link", "name": "Create voucher"},
         "summary": 'click link "Create voucher"'},
        {"id": "s2", "n": 2, "kind": "click", "point": [382, 402], "screen": [1280, 720], "summary": "click 382,402"},
        {"id": "s3", "n": 3, "kind": "click", "target": {"role": "button", "name": "Save voucher"},
         "summary": 'click button "Save voucher"'},
    ]
    skill = {"name": "enter-bills", "title": "Enter bills", "steps": steps, "params": []}
    return {"skill": skill, "values": {}, "start": 1, "checked": True, "patches": [], "asked": 0, "stuck": 0,
            "ladder": skill_task._ladder(skill), "asked_ids": [], "one_off": True}


class AStoppedReplayIsRepairedOneStepAtATime(unittest.TestCase):

    def test_the_stop_goes_to_a_repair_and_the_replay_resumes_after_it(self):
        run = _run()
        halted = {"outcome": replay.HALTED, "text": "what was around the click is not on the screen", "step": 2,
                  "resume_from": 2, "options": [], "trail": [], "reason": "what was around the click is not there"}
        done = {"outcome": replay.VERIFIED, "text": "Replayed: 3 steps", "step": 3, "resume_from": None,
                "options": [], "trail": []}
        results = [halted, done]
        starts, given, finished = [], [], []

        def fake_replay(program, params=None, start=1, ladder=None):
            starts.append(start)
            return results.pop(0)

        with mock.patch.object(replay, "run", side_effect=fake_replay), \
                mock.patch.object(skill_task, "_engine_can_drive", return_value=True), \
                mock.patch.object(skill_task, "_by_model", side_effect=lambda r, **kw: given.append(kw)), \
                mock.patch.object(skill_task, "_finish", side_effect=lambda r, res: finished.append(res)), \
                mock.patch.object(skill_task, "_settle"), \
                mock.patch.object(skill_task.phase_note, "write"), \
                mock.patch.object(repairs, "_observe", return_value={}):
            skill_task._execute(run)
            self.assertEqual(len(given), 1, given)
            self.assertEqual(given[0].get("phase"), repairs.REPAIR_PHASE)
            self.assertEqual(given[0].get("from_step"), 2)
            self.assertFalse(run.get("taken_over"), "a repair is not a takeover of the whole rest")

            # The model reports the one click made (a click kept only as a spot is the model's to make): the harness
            # resumes the person's steps after it, the click left unchecked by the replay.
            self.assertTrue(skill_task._after_phase(run, repairs.REPAIR_PHASE, "DONE: clicked the Party field", "final"))

        self.assertEqual(starts, [1, 3])
        self.assertEqual(run.get("unchecked_steps"), [2])
        self.assertEqual(len(finished), 1)
        self.assertTrue(repairs.unverified(run), "a repaired run is never verified by the replay alone")
        self.assertEqual(run["repaired_ids"], ["s2"])

    def test_a_spot_click_and_what_was_typed_into_what_it_focused_are_repaired_as_one(self):
        # Found live (2026-11-16, books round 4): the person clicked a spot (a list), typed the party into what it
        # focused and pressed Enter. The repair chose the party; the replay then typed it again into the field that had
        # the keyboard - the date - and the save was refused.
        run = _run()
        steps = run["skill"]["steps"]
        steps[2:2] = [{"id": "s2b", "n": 3, "kind": "type", "param": "party", "append": True,
                       "summary": "type {party} into the focused field"},
                      {"id": "s2c", "n": 4, "kind": "key", "keys": ["Return"], "summary": "press Return"}]
        steps[-1]["n"] = 5
        run["values"] = {"party": "Vasco Bay Hotels"}
        halted = {"outcome": replay.HALTED, "text": "not on the screen", "step": 2, "resume_from": 2, "options": [],
                  "trail": [], "reason": "what was around the click is not there"}
        done = {"outcome": replay.VERIFIED, "text": "Replayed", "step": 5, "resume_from": None, "options": [], "trail": []}
        results, starts, given = [halted, done], [], []

        def fake_replay(program, params=None, start=1, ladder=None):
            starts.append(start)
            return results.pop(0)

        with mock.patch.object(replay, "run", side_effect=fake_replay), \
                mock.patch.object(skill_task, "_engine_can_drive", return_value=True), \
                mock.patch.object(skill_task, "_by_model", side_effect=lambda r, **kw: given.append(kw)), \
                mock.patch.object(skill_task, "_finish"), mock.patch.object(skill_task, "_settle"), \
                mock.patch.object(skill_task.phase_note, "write"), mock.patch.object(repairs, "_observe", return_value={}), \
                mock.patch.object(skill_task, "_taught", return_value=""):
            skill_task._execute(run)
            told = skill_task._procedure(run, from_step=2, phase=repairs.REPAIR_PHASE)
            self.assertIn("Vasco Bay Hotels", told)
            self.assertIn("steps 3, 4 entered", told)
            skill_task._after_phase(run, repairs.REPAIR_PHASE, "DONE: chose Vasco Bay Hotels in Party", "final")
        self.assertEqual(starts, [1, 5])  # past the click and what it focused, never typed a second time
        self.assertEqual(run.get("unchecked_steps"), [2, 3, 4])

    def test_a_field_typed_after_a_spot_click_is_labelled_by_the_fix_learned_for_it(self):
        # Found live (2026-11-16, books round 6): the learned fix chose the party in "Party A/c name"; the declaration had
        # no such field and a correct month was refused as "a value no declaration backs".
        skill = {"steps": [{"n": 1, "kind": "click", "point": [1, 1]},
                           {"n": 2, "kind": "type", "param": "party_name", "summary": "type into the focused field"}],
                 "params": [{"name": "party_name", "step": 2, "list": True}],
                 "branches": {"manual|click|||s1": {"versions": [
                     {"v": 1, "binds": {"b1": {"item": "party_name"}},
                      "steps": [{"kind": "select", "param": "b1", "target": {"role": "combobox", "name": "Party A/c name"}}]}]}}}
        steps = {s["n"]: s for s in skill["steps"]}
        self.assertEqual(skill_task._value_field_label(skill, skill["params"][0], steps), "Party A/c name")
        with mock.patch.object(skill_task, "_taught_fields", return_value={}):
            fields = skill_task._item_fields({"skill": skill, "values": {"party_name": "Udyavara Apartments; Lotus Mall"}}, 1)
        self.assertEqual(fields, {"Party A/c name": "Lotus Mall"})

    def test_a_gathered_run_resumes_where_the_repair_left_it_not_at_the_items_first_step(self):
        # Found live (2026-11-15, books round 2): a gathered run begins at its per-item block, and that start was put
        # back on every replay - after a repair of step 43 the replay began the bill again, met step 43 and stopped.
        run = _run()
        run["gathered"], run["start"], run["resuming"] = "done", 3, True
        starts = []

        def fake_replay(program, params=None, start=1, ladder=None):
            starts.append(start)
            return {"outcome": replay.VERIFIED, "text": "ok", "step": 3, "resume_from": None, "options": [], "trail": []}

        loop = {"blocks": [["s1"]]}
        with mock.patch.object(replay, "run", side_effect=fake_replay), \
                mock.patch.object(skill_task, "_item_loop", return_value=loop), \
                mock.patch.object(skill_task, "_finish"), mock.patch.object(skill_task.phase_note, "write"):
            skill_task._execute(run)
            run.pop("resuming")
            run["start"] = 3
            skill_task._execute(run)
        self.assertEqual(starts, [3, 1])  # resumed where it was left; a first replay begins at the block

    def test_an_enter_that_submitted_nothing_then_and_would_submit_now_is_skipped(self):
        # Found live (2026-11-15, books round 3): the person's Enter picked an entry in an open list; chosen another way
        # this run, the same Enter would submit the half-filled form, and the guard held it back at every bill.
        from decurix_agent.computer import cdp, core
        url, window = "http://books.example.test/vouchers/new", "Voucher - Chromium"
        enter = {"n": 45, "id": "s45", "kind": "key", "keys": ["Return"], "url": url, "window": window}
        same_page = {"n": 46, "id": "s46", "kind": "select", "url": url, "window": window}
        new_page = {"n": 46, "id": "s46", "kind": "click", "url": "http://books.example.test/vouchers", "window": window}
        held = cdp.EnterWouldSubmit("Enter was not pressed: Enter would submit the form now, with 6 other field(s) still empty.")
        with mock.patch.object(core, "act", side_effect=held), mock.patch.object(replay, "_screen_now", return_value=None):
            done = replay._run_step(enter, {}, report=lambda text: None, next_step=same_page)
            self.assertEqual(done["status"], "done")
            with self.assertRaises(replay._Halt):
                replay._run_step(enter, {}, report=lambda text: None, next_step=new_page)

    def test_the_repair_phase_gets_its_own_short_request_and_guidance(self):
        run = _run()
        repairs.state(run)["job"] = {"n": 2, "step": run["skill"]["steps"][1], "shape": {"when": "before"},
                                     "values": {}, "result": {"reason": "not on the screen"}}
        run["expanded"] = {"steps": run["skill"]["steps"]}
        with mock.patch.object(skill_task, "_taught", return_value=""):
            request = skill_task._engine_request(run, repairs.REPAIR_PHASE)
            guidance = skill_task._procedure(run, from_step=2, phase=repairs.REPAIR_PHASE)
        self.assertIn("Fix ONE stopped step", request)
        self.assertIn("<- STOPPED HERE", guidance)
        self.assertIn("THIS REPAIR ONLY", guidance)


if __name__ == "__main__":
    unittest.main()
