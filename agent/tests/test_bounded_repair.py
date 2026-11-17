"""
A stopped replay is repaired one step at a time and goes on as a replay (repairs.py): the model is given only the
stopped step, the harness checks its effect by code and resumes the person's steps; what fixed it is kept as a
candidate tool, tried by code before the model the next time the same stop comes, promoted after it worked in two
later tasks and demoted when it fails; after two repairs in a row that did not get past their step (or five in a run)
the model is given the rest, as before.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_bounded_repair.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import repairs
from decurix_agent.computer import branches, escalation, phase, replay

from test_branches import DOCS, create_ledger_events, taught


def program():
    """Two vouchers written out: the party chosen, the amount typed, Accept - per item."""

    steps = []
    for item, party in ((1, "Coastal Traders"), (2, "Brahmagiri Diagnostics LLP")):
        base = (item - 1) * 3
        steps += [
            {"n": base + 1, "id": "s1", "kind": "select", "param": f"party_name__{item}", "app": "books",
             "target": {"role": "combobox", "name": "Party A/c name"},
             "expect": [{"type": "value", "role": "combobox", "name": "Party A/c name", "param": f"party_name__{item}",
                         "choice": True}], "summary": f"choose Party A/c name (item {item} of 2)"},
            {"n": base + 2, "id": "s2", "kind": "type", "param": f"amount__{item}", "app": "books",
             "target": {"role": "textbox", "name": "Amount"}, "summary": f"type Amount (item {item} of 2)"},
            {"n": base + 3, "id": "s3", "kind": "click", "app": "books", "target": {"role": "button", "name": "Accept"},
             "summary": f"click Accept (item {item} of 2)",
             "expect": [{"type": "element", "role": "status", "name": "Voucher saved"}]},
        ]
    return {"steps": steps, "params": []}


VALUES = {"party_name__1": "Coastal Traders", "party_name__2": "Brahmagiri Diagnostics LLP", "gstin__2": "29ABCDE1234F1Z5",
          "amount__1": "800.00", "amount__2": "12,500.00"}


def halt(step=4, resume_from=4, outcome=replay.HALTED, reason="combobox Party A/c name has no option"):
    return {"outcome": outcome, "step": step, "resume_from": resume_from, "reason": reason, "text": reason,
            "evidence": "", "options": []}


class BoundedRepair(unittest.TestCase):

    def setUp(self):
        self.executed, self.by_model, self.takeovers = [], [], []
        self.task = "t-1"
        patches = [
            mock.patch.object(repairs, "_task", side_effect=lambda: self.task),
            mock.patch.object(repairs, "_events_now", return_value=10),
            mock.patch.object(repairs, "_events", side_effect=lambda: create_ledger_events(10)),
            mock.patch.object(repairs, "_inputs", return_value=DOCS),
            mock.patch.object(repairs.progress, "emit"),
            mock.patch("decurix_agent.computer.skills.load", side_effect=FileNotFoundError),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def new_run(self, skill=None):
        return {"skill": skill or taught(), "values": dict(VALUES), "start": 1, "ladder": escalation.Ladder(),
                "expanded": program(), "asked_ids": []}

    def execute(self, run):
        self.executed.append(run["start"])

    def model(self, run, **kw):
        self.by_model.append(kw)

    def takeover(self, run, n, note):
        self.takeovers.append((n, note))

    def halt_on(self, run, result):
        return repairs.on_halt(run, program(), VALUES, result, execute=self.execute, by_model=self.model,
                               observe=lambda: {"elements": []})

    def report(self, run, reply="DONE: made the ledger Brahmagiri Diagnostics LLP", kind="final", observe=None):
        return repairs.after_repair(run, reply, kind, execute=self.execute, by_model=self.model, takeover=self.takeover,
                                    observe=observe or (lambda: {"elements": []}))

    def passed_step(self, run, n):
        """The resumed replay got past step n, its result confirmed."""
        run["ladder"].confirm({"id": "s1", "n": n})
        return repairs.settle(run, {"outcome": replay.VERIFIED, "step": None})

    def test_a_halt_is_repaired_by_the_model_and_the_replay_resumes_at_that_step(self):
        run = self.new_run()
        self.assertTrue(self.halt_on(run, halt()))
        (asked,) = self.by_model
        self.assertEqual((asked["phase"], asked["from_step"]), (repairs.REPAIR_PHASE, 4))
        self.assertIn("Do NOT do step 4 itself", "\n".join(repairs.how_lines(run)))
        self.assertTrue(self.report(run))
        self.assertEqual(self.executed, [4])  # the replay does step 4 itself, with its checks
        self.assertTrue(run["resuming"])
        self.assertNotIn("taken_over", run)  # only the step is marked
        self.assertEqual(run["repaired_ids"], ["s1"])
        self.assertEqual(self.passed_step(run, 4), "passed")
        kept = branches.best(run["skill"], "before|select|combobox|Party A/c name|s1")
        self.assertEqual(kept["status"], branches.CANDIDATE)
        self.assertTrue(repairs.unverified(run))
        self.assertIn("Repairs during the replay", repairs.finished_text(run))

    def test_a_repair_that_did_not_get_past_its_step_keeps_nothing(self):
        run = self.new_run()
        self.halt_on(run, halt())
        self.report(run)
        self.assertEqual(repairs.settle(run, halt()), "failed")
        self.assertNotIn("branches", run["skill"])

    def test_two_repairs_in_a_row_that_fail_hand_the_rest_to_the_model(self):
        run = self.new_run()
        self.assertTrue(self.halt_on(run, halt()))
        self.report(run)
        repairs.settle(run, halt())
        self.assertTrue(self.halt_on(run, halt()))  # the second repair
        self.report(run)
        repairs.settle(run, halt())
        self.assertFalse(self.halt_on(run, halt()))  # the budget: the caller takes over the rest
        self.assertEqual(len(self.by_model), 2)

    def test_at_most_five_repairs_in_a_run(self):
        run = self.new_run()
        with mock.patch.object(repairs, "_events", return_value=[]):  # (slow screens: nothing to keep, each repaired)
            for _ in range(repairs.MAX_TOTAL):
                self.assertTrue(self.halt_on(run, halt()))
                self.report(run)
                self.passed_step(run, 4)
            self.assertFalse(self.halt_on(run, halt()))
        self.assertEqual(len(self.by_model), repairs.MAX_TOTAL)

    def test_a_result_that_never_came_must_be_on_the_screen_after_the_repair(self):
        run = self.new_run()
        self.halt_on(run, halt(step=3, resume_from=4, reason="expected status Voucher saved, which never appeared"))
        self.report(run, observe=lambda: {"elements": [{"role": "status", "name": "Voucher saved"}]})
        self.assertEqual(self.executed, [4])  # seen by code: on from the next step
        run2 = self.new_run()
        self.halt_on(run2, halt(step=3, resume_from=4))
        self.report(run2)  # still not there: a second repair, never a guess
        self.assertEqual(len([k for k in self.by_model if k["from_step"] == 3]), 3)

    def test_a_repair_that_ends_in_a_question_or_error_hands_over_the_rest(self):
        run = self.new_run()
        self.halt_on(run, halt())
        self.report(run, reply="HELP: which group?", kind="help")
        self.assertEqual([n for n, _ in self.takeovers], [4])
        self.assertEqual(self.executed, [])

    def test_a_kept_tool_is_tried_before_the_model_and_promoted_after_two_tasks(self):
        skill = taught()
        candidate, _ = branches.compile_candidate(skill, program()["steps"][3], VALUES, create_ledger_events(10),
                                                  inputs=DOCS, since=10)
        branches.add_candidate(skill, branches.shape(program()["steps"][3], halt()), candidate, task_id="t-0")
        key = "before|select|combobox|Party A/c name|s1"
        ran = []
        with mock.patch.object(repairs.replay, "run",
                               side_effect=lambda prog, **kw: ran.append((prog, kw["params"]))
                               or {"outcome": replay.VERIFIED, "text": "ok"}):
            for n, task in enumerate(("t-1", "t-2"), 1):
                self.task = task
                run = self.new_run(skill)
                self.assertTrue(self.halt_on(run, halt()))
                self.assertEqual(self.by_model, [])  # code, no model
                self.assertEqual(self.passed_step(run, 4), "passed")
                skill = run["skill"]
        self.assertEqual(ran[0][1]["b1"], "Brahmagiri Diagnostics LLP")
        self.assertEqual(branches.best(skill, key)["status"], branches.PROMOTED)
        self.assertEqual(self.executed, [4, 4])

    def test_a_kept_tool_that_fails_is_demoted_and_the_model_repairs(self):
        skill = taught()
        candidate, _ = branches.compile_candidate(skill, program()["steps"][3], VALUES, create_ledger_events(10),
                                                  inputs=DOCS, since=10)
        branches.add_candidate(skill, branches.shape(program()["steps"][3], halt()), candidate, task_id="t-0")
        with mock.patch.object(repairs.replay, "run", return_value={"outcome": replay.HALTED, "text": "no Create"}):
            run = self.new_run(skill)
            self.assertTrue(self.halt_on(run, halt()))
        self.assertIsNone(branches.best(run["skill"], "before|select|combobox|Party A/c name|s1"))
        (asked,) = self.by_model
        self.assertEqual(asked["phase"], repairs.REPAIR_PHASE)
        self.assertIn("A fix learned from an earlier repair was tried", "\n".join(repairs.how_lines(run)))

    def test_a_kept_tool_the_gate_holds_hands_that_press_to_the_model_uncounted(self):
        skill = taught()
        candidate, _ = branches.compile_candidate(skill, program()["steps"][3], VALUES, create_ledger_events(10),
                                                  inputs=DOCS, since=10)
        branches.add_candidate(skill, branches.shape(program()["steps"][3], halt()), candidate, task_id="t-0")
        with mock.patch.object(repairs.replay, "run", return_value={"outcome": replay.NEEDS_APPROVAL, "text": "ask"}):
            run = self.new_run(skill)
            self.halt_on(run, halt())
        version = branches.best(run["skill"], "before|select|combobox|Party A/c name|s1")
        self.assertEqual((version["status"], version["held"], version["uses"]), (branches.CANDIDATE, 1, 0))
        self.assertEqual(len(self.by_model), 1)

    def test_a_reconciliation_or_an_approval_is_never_repaired(self):
        run = self.new_run()
        for result in (halt(outcome=replay.RECONCILIATION_REQUIRED, resume_from=None),
                       halt(outcome=replay.NEEDS_APPROVAL)):
            self.assertFalse(self.halt_on(run, result))
        self.assertEqual(self.by_model, [])

    def test_the_gui_command_refuses_actions_past_the_repair_budget(self):
        noted = {"task": "t-1", "phase": repairs.REPAIR_PHASE, "events_from": 10, "budget": 5, "step": 4}
        with mock.patch.object(phase, "read", return_value=noted), \
                mock.patch("decurix_agent.computer.recording.meta", return_value={"event_count": 14}):
            self.assertIsNone(repairs.budget_refusal("t-1"))
        with mock.patch.object(phase, "read", return_value=noted), \
                mock.patch("decurix_agent.computer.recording.meta", return_value={"event_count": 15}):
            self.assertIn("used its 5 actions", repairs.budget_refusal("t-1"))
            self.assertIsNone(repairs.budget_refusal("another task"))


    def test_a_review_before_a_commit_has_its_own_budget(self):
        noted = {"task": "t-1", "phase": repairs.REVIEW_PHASE, "events_from": 0, "budget": 40, "step": 7}
        with mock.patch.object(phase, "read", return_value=noted),                 mock.patch("decurix_agent.computer.recording.meta", return_value={"event_count": 39}):
            self.assertIsNone(repairs.budget_refusal("t-1"))
        with mock.patch.object(phase, "read", return_value=noted),                 mock.patch("decurix_agent.computer.recording.meta", return_value={"event_count": 40}):
            self.assertIn("check before the commit has used its 40 actions", repairs.budget_refusal("t-1"))


if __name__ == "__main__":
    unittest.main()
