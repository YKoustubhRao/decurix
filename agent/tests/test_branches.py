"""
The tools a worker builds from its own repairs (computer/branches.py): what the model pressed to get a stopped replay
going is compiled into a candidate branch keyed by the stop's shape, bound to the run's values (never one document's
text kept as a constant), confined to the applications the person's steps use, tried by code the next time, promoted
only after it worked in PROMOTE_AFTER later tasks and demoted on a failure; at most MAX_COMPILES versions per shape.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_branches.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import branches


def taught():
    """A voucher skill: the party is chosen from a list, then the amount typed and the voucher accepted."""

    steps = [
        {"n": 1, "id": "s1", "kind": "select", "param": "party_name", "app": "books",
         "target": {"role": "combobox", "name": "Party A/c name"}, "summary": "choose Party A/c name"},
        {"n": 2, "id": "s2", "kind": "type", "param": "amount", "app": "books",
         "target": {"role": "textbox", "name": "Amount"}, "summary": "type Amount"},
        {"n": 3, "id": "s3", "kind": "click", "app": "books", "target": {"role": "button", "name": "Accept"},
         "summary": "click Accept"},
    ]
    return {"name": "vouchers", "title": "Enter vouchers", "steps": steps, "params": []}


def halted_select(item=2):
    return {"n": 7, "id": "s1", "kind": "select", "param": f"party_name__{item}", "app": "books",
            "target": {"role": "combobox", "name": "Party A/c name"}, "summary": "choose Party A/c name (item 2 of 3)"}


VALUES = {"party_name__1": "Coastal Traders", "party_name__2": "Brahmagiri Diagnostics LLP",
          "gstin__2": "29ABCDE1234F1Z5", "amount__2": "12,500.00"}

DOCS = "Bill to: Brahmagiri Diagnostics LLP  GSTIN 29ABCDE1234F1Z5  Ship to: 14 Temple Road, Mysuru  Total 12,500.00"


def ev(i, kind, role=None, name=None, app="books", **extra):
    event = {"i": i, "kind": kind, "outcome": "done", "app": app, "window": "Books", "turn": i}
    if role:
        event["element"] = {"role": role, "name": name}
    event.update(extra)
    return event


def create_ledger_events(since=10, address="Sundry Debtors"):
    """What the model did: open the create screen, type the new party's name and tax number, pick its group, accept."""

    return [
        ev(since - 3, "click", "button", "Accept", replay_of={"step": 3}),  # (the replay's own press: not the model's)
        ev(since, "click", "button", "Create"),
        ev(since + 1, "type", "textbox", "Name", text="Brahmagiri Diagnostics LLP"),
        ev(since + 2, "type", "textbox", "GSTIN/UIN", text="29ABCDE1234F1Z5"),
        ev(since + 3, "select", "combobox", "Under", option=address),
        ev(since + 4, "click", "button", "Accept"),
    ]


class TheShapeOfAStop(unittest.TestCase):

    def test_where_it_stopped_decides_the_shape_never_the_reason_s_words(self):
        step = halted_select()
        before = branches.shape(step, {"resume_from": 7, "reason": "combobox Party A/c name has no option 'X'"})
        after = branches.shape(step, {"resume_from": 8, "reason": "expected a dialog"})
        self.assertEqual(before["when"], "before")
        self.assertEqual(after["when"], "after")
        self.assertEqual(branches.key(before), "before|select|combobox|Party A/c name|s1")
        other_words = branches.shape(step, {"resume_from": 7, "reason": "no element at path"})
        self.assertEqual(branches.key(before), branches.key(other_words))
        manual = branches.shape({"n": 4, "id": "m", "kind": "manual"}, {"resume_from": 5})
        self.assertEqual(manual["when"], "manual")


class CompilingARepair(unittest.TestCase):

    def test_the_model_s_actions_become_a_branch_bound_to_the_run_s_values(self):
        candidate, why = branches.compile_candidate(taught(), halted_select(), VALUES, create_ledger_events(),
                                                    inputs=DOCS, since=10)
        self.assertEqual(why, "")
        kinds = [s["kind"] for s in candidate["steps"]]
        self.assertEqual(kinds, ["click", "type", "type", "select", "click"])  # (the replay's own press left out)
        self.assertEqual(candidate["binds"], {"b1": {"halted": True}, "b2": {"item": "gstin"}})
        constant = next(p for p in candidate["params"] if p["name"] == "b3")
        self.assertEqual(constant["default"], "Sundry Debtors")  # in no document: a constant of the fix
        self.assertTrue(candidate["steps"][-1]["consequential"])  # a commit is never pressed twice
        self.assertEqual([s["id"] for s in candidate["steps"]], ["b1", "b2", "b3", "b4", "b5"])

    def test_a_literal_from_the_documents_that_is_no_value_of_the_run_refuses_the_candidate(self):
        events = create_ledger_events()
        events.insert(4, ev(13, "type", "textbox", "Address", text="14 Temple Road, Mysuru"))
        candidate, why = branches.compile_candidate(taught(), halted_select(), VALUES, events, inputs=DOCS, since=10)
        self.assertIsNone(candidate)
        self.assertIn("in this run's documents", why)

    def test_a_control_named_for_this_run_s_value_refuses_the_candidate(self):
        events = [ev(10, "click", "option", "Brahmagiri Diagnostics LLP")]
        candidate, why = branches.compile_candidate(taught(), halted_select(), VALUES, events, inputs=DOCS, since=10)
        self.assertIsNone(candidate)
        self.assertIn("named for this run's own value", why)

    def test_an_application_the_person_s_steps_never_use_refuses_the_candidate(self):
        events = [ev(10, "click", "button", "Create", app="mail")]
        candidate, why = branches.compile_candidate(taught(), halted_select(), VALUES, events, inputs=DOCS, since=10)
        self.assertIsNone(candidate)
        self.assertIn("never use", why)

    def test_a_bare_point_an_address_opened_or_a_withheld_value_refuses_the_candidate(self):
        point = [dict(ev(10, "click"), point=[10, 10])]
        opened = [dict(ev(10, "open"), target="https://elsewhere.example/")]
        secret = [ev(10, "type", "textbox", "Password", withheld={"text": "a password"})]
        for events in (point, opened, secret):
            candidate, _ = branches.compile_candidate(taught(), halted_select(), VALUES, events, inputs=DOCS, since=10)
            self.assertIsNone(candidate, events)

    def test_a_stop_that_cleared_by_itself_or_the_stopped_step_alone_keeps_nothing(self):
        none, why = branches.compile_candidate(taught(), halted_select(), VALUES, [], inputs=DOCS)
        self.assertIsNone(none)
        self.assertIn("cleared by itself", why)
        only = [ev(10, "select", "combobox", "Party A/c name", option="Brahmagiri Diagnostics LLP")]
        none, why = branches.compile_candidate(taught(), halted_select(), VALUES, only, inputs=DOCS, since=10)
        self.assertIsNone(none)
        self.assertIn("only did the stopped step", why)


class KeepingProvingAndRetiring(unittest.TestCase):

    def setUp(self):
        self.skill = taught()
        self.shape = branches.shape(halted_select(), {"resume_from": 7})
        self.key = branches.key(self.shape)
        self.candidate, _ = branches.compile_candidate(self.skill, halted_select(), VALUES, create_ledger_events(),
                                                       inputs=DOCS, since=10)

    def test_a_candidate_is_promoted_only_after_it_worked_in_two_later_tasks(self):
        version = branches.add_candidate(self.skill, self.shape, self.candidate, task_id="t-source")
        self.assertEqual(branches.best(self.skill, self.key)["status"], branches.CANDIDATE)
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], True, "t-source"), "")
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], True, "t-2"), "")
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], True, "t-2"), "")  # (same task again)
        self.assertEqual(branches.best(self.skill, self.key)["status"], branches.CANDIDATE)
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], True, "t-3"), branches.PROMOTED)
        self.assertEqual(branches.best(self.skill, self.key)["status"], branches.PROMOTED)
        self.assertEqual(self.skill["steps"], taught()["steps"])  # the person's steps are never rewritten

    def test_a_failed_candidate_is_demoted_and_no_longer_tried(self):
        version = branches.add_candidate(self.skill, self.shape, self.candidate, task_id="t-source")
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], False, "t-2"), branches.DEMOTED)
        self.assertIsNone(branches.best(self.skill, self.key))

    def test_a_promoted_branch_is_demoted_after_two_failures_in_a_row(self):
        version = branches.add_candidate(self.skill, self.shape, self.candidate, task_id="t-source")
        for task in ("t-2", "t-3"):
            branches.note_trial(self.skill, self.key, version["v"], True, task)
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], False, "t-4"), "")
        self.assertEqual(branches.note_trial(self.skill, self.key, version["v"], False, "t-5"), branches.DEMOTED)

    def test_recompiles_are_capped_and_versions_kept(self):
        made = [branches.add_candidate(self.skill, self.shape, self.candidate, task_id=f"t{i}") for i in range(5)]
        self.assertEqual([m["v"] if m else None for m in made], [1, 2, 3, None, None])
        self.assertEqual(len(branches.entry(self.skill, self.key)["versions"]), branches.MAX_VERSIONS)

    def test_a_branch_runs_with_this_run_s_item_values_and_never_a_guess(self):
        version = branches.add_candidate(self.skill, self.shape, self.candidate)
        values = {"party_name__3": "Hebri Labs", "gstin__3": "29ZZZZZ9999Z1Z9"}
        program, params = branches.program_for(version, values, halted_select(item=3), "vouchers")
        self.assertEqual(params, {"b1": "Hebri Labs", "b2": "29ZZZZZ9999Z1Z9", "b3": "Sundry Debtors"})
        self.assertEqual(program["kind"], "branch")
        none, why = branches.program_for(version, {"party_name__3": "Hebri Labs"}, halted_select(item=3), "vouchers")
        self.assertIsNone(none)
        self.assertIn("no value", why)


class AFixLearnedAtAClickWithNoValueOfItsOwn(unittest.TestCase):
    """Found live (2026-11-16, books): the repair of a spot click (no parameter) chose bill 1's party; the fix kept was
    bound to bill 1's own value and chose it again for bill 2. Which item a stop is in is the step's, not its value's."""

    def test_the_value_chosen_is_bound_to_the_item_and_follows_it(self):
        values = {"party_name__1": "Vasco Bay Hotels", "party_name__2": "Hampi Residency"}
        how = branches._bind("Vasco Bay Hotels", values, None, item=1)
        self.assertEqual(how, {"item": "party_name"})
        self.assertEqual(branches._resolve_for_compile(how, values, None, item=2), "Hampi Residency")

    def test_another_item_s_value_at_a_stop_in_no_item_is_never_bound_to_that_item(self):
        values = {"party_name__1": "Vasco Bay Hotels", "party_name__2": "Hampi Residency", "month": "November 2026"}
        self.assertIsNone(branches._bind("Vasco Bay Hotels", values, None))
        self.assertEqual(branches._bind("November 2026", values, None), {"name": "month"})


if __name__ == "__main__":
    unittest.main()
