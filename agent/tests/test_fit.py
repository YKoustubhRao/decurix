import unittest
from unittest.mock import patch

from decurix_agent import skill_task
from decurix_agent.computer import experience, skills


def target(role, name, row=None):
    out = {"role": role, "name": name, "path": f"Form/{role}:{name}" + (f"/row {row}" if row else "")}
    if row:
        out["row"] = row
    return out


def a_bill_skill():
    """
    The shape of a vendor bill as a person demonstrated it: a supplier and two
    lines, each line an item searched for and picked from a list, a quantity
    and a rate; "Add row" between the lines; saved at the end.
    """

    steps = [
        {"kind": "click", "target": target("combobox", "Supplier")},
        {"kind": "type", "param": "supplier", "target": target("combobox", "Supplier")},
        {"kind": "click", "target": target("option", "TechCorp Cloud Solutions India Pvt Ltd"), "expect": [{"type": "element", "name": "Open Link"}]},
        {"kind": "type", "param": "invoice_no", "target": target("textbox", "Supplier Invoice No")},
        # line 1
        {"kind": "click", "target": target("combobox", "Item", 1)},
        {"kind": "type", "param": "item", "target": target("combobox", "Item", 1)},
        {"kind": "click", "target": target("option", "Cloud Compute Instances Cloud Compute Instances, Services")},
        {"kind": "type", "param": "qty", "target": target("textbox", "Accepted Qty", 1)},
        {"kind": "type", "param": "rate", "target": target("textbox", "Rate (INR)", 1)},
        # line 2
        {"kind": "click", "target": target("button", "Add row"), "expect": [{"type": "element", "name": "Cloud Compute Instances"}]},
        {"kind": "type", "param": "item_2", "target": target("combobox", "Item", 2)},
        {"kind": "click", "target": target("option", "Managed Database Hosting Managed Database Hosting, Services")},
        {"kind": "type", "param": "qty_2", "target": target("textbox", "Accepted Qty", 2)},
        {"kind": "type", "param": "rate_2", "target": target("textbox", "Rate (INR)", 2)},
        {"kind": "key", "keys": ["ctrl+s"]},
    ]
    examples = {"supplier": "Techcorp", "invoice_no": "TC/2026/0401", "item": "Cloud compute", "qty": "1", "rate": "40000",
                "item_2": "managed Database", "qty_2": "1", "rate_2": "10000"}
    labels = {"supplier": "Supplier", "invoice_no": "Supplier Invoice No", "item": "Item", "qty": "Accepted Qty",
              "rate": "Rate (INR)", "item_2": "Item", "qty_2": "Accepted Qty", "rate_2": "Rate (INR)"}
    params = []
    for n, step in enumerate(steps, 1):
        step.update(n=n, id=f"s{n}", summary=f"step {n}")
        step.setdefault("expect", [])
        if step.get("param"):
            name = step["param"]
            params.append({"name": name, "label": labels[name], "varies": True, "step": n, "example": examples[name],
                           "default": None})
    return {"name": "enter-a-vendor-bill", "title": "Enter a vendor bill", "status": "ready", "steps": steps,
            "params": params, "loops": []}


DEMO = {"supplier": "Techcorp", "invoice_no": "TC/2026/0401", "item": "Cloud compute", "qty": "1", "rate": "40000",
        "item_2": "managed Database", "qty_2": "1", "rate_2": "10000"}


def shape(program):
    return [(s["kind"], s.get("param") or (s.get("target") or {}).get("name") or " ".join(s.get("keys") or []))
            for s in program["steps"]]


class TheDemonstrationsOwnRunIsReplayedAsRecorded(unittest.TestCase):
    def test_the_same_values_change_nothing(self):
        skill = a_bill_skill()
        program, values = skills.expand(skill, dict(DEMO))
        self.assertNotIn("fitted", program)
        self.assertEqual(len(program["steps"]), 15)
        self.assertEqual([s["kind"] for s in program["steps"]].count("select"), 0)


class ThisRunsChoices(unittest.TestCase):
    def test_another_entry_is_searched_for_not_typed_and_then_the_demonstrations_clicked(self):
        skill = a_bill_skill()
        mine = dict(DEMO, supplier="NexGen Hardware Labs Pvt Ltd", item="AI Developer Workstations (Ryzen 9)")
        program, values = skills.expand(skill, mine)

        chosen = [s for s in program["steps"] if s["kind"] == "select"]
        self.assertEqual([s["param"] for s in chosen], ["supplier", "item"])
        self.assertEqual([s["target"]["name"] for s in chosen], ["Supplier", "Item"])
        # The demonstration's own entries are no longer clicked for them; line 2's, unchanged, still is.
        clicked = [s["target"]["name"] for s in program["steps"] if s["kind"] == "click" and s["target"]["role"] == "option"]
        self.assertEqual(clicked, ["Managed Database Hosting Managed Database Hosting, Services"])
        # What a choice leaves on the screen was the demonstration's: nothing of it is waited for.
        self.assertTrue(all(s["expect"] == [] for s in chosen))
        self.assertEqual([s["n"] for s in program["steps"]], list(range(1, len(program["steps"]) + 1)))
        self.assertEqual({p["name"]: p["step"] for p in program["params"]}["invoice_no"], 3)

    def test_a_typed_value_that_is_not_picked_from_a_list_is_still_typed(self):
        program, _ = skills.expand(a_bill_skill(), dict(DEMO, invoice_no="NXG-2026-8812", rate="65000"))
        self.assertNotIn("fitted", program)


class ThisRunsRows(unittest.TestCase):
    def test_a_document_of_one_line_enters_one_row(self):
        skill = a_bill_skill()
        one = {k: v for k, v in DEMO.items() if not k.endswith("_2")}
        self.assertEqual(skills.run_rows(skill, one), (1, [["item", "qty", "rate"]]))

        program, _ = skills.expand(skill, one)
        self.assertEqual(shape(program), [
            ("click", "Supplier"), ("type", "supplier"), ("click", "TechCorp Cloud Solutions India Pvt Ltd"),
            ("type", "invoice_no"), ("click", "Item"), ("type", "item"),
            ("click", "Cloud Compute Instances Cloud Compute Instances, Services"), ("type", "qty"), ("type", "rate"),
            ("key", "ctrl+s")])
        self.assertNotIn("item_2", [p["name"] for p in program["params"]])

    def test_a_document_of_four_lines_enters_four_each_found_by_what_it_is(self):
        skill = a_bill_skill()
        four = dict(DEMO, item_3="Monitor", qty_3="2", rate_3="15000", item_4="Chair", qty_4="4", rate_4="6200")
        self.assertEqual(skills.run_rows(skill, four)[0], 4)

        program, values = skills.expand(skill, four)
        tail = shape(program)[14:]
        self.assertEqual(tail, [("click", "Add row"), ("select", "item_3"), ("type", "qty_3"), ("type", "rate_3"),
                                ("click", "Add row"), ("select", "item_4"), ("type", "qty_4"), ("type", "rate_4"),
                                ("key", "ctrl+s")])
        # A written-out row's controls carry no row of the demonstration's and no result of its.
        added = program["steps"][14:22]
        self.assertTrue(all("path" not in s["target"] and "row" not in s["target"] and s["expect"] == [] for s in added))
        self.assertIn("(row 3)", added[1]["summary"])
        by_name = {p["name"]: p for p in program["params"]}
        self.assertEqual((by_name["rate_4"]["label"], by_name["rate_4"]["step"]), ("Rate (INR)", 22))
        self.assertEqual(values["item_4"], "Chair")

    def test_a_row_given_in_part_is_not_guessed_at(self):
        skill = a_bill_skill()
        # The second line's rate is missing: not "one row", and not a row with a hole - the demonstration's
        # program, which then says what it lacks.
        partial = {k: v for k, v in DEMO.items() if k != "rate_2"}
        self.assertEqual(skills.run_rows(skill, partial), (None, None))
        program, _ = skills.expand(skill, partial)
        self.assertEqual(len(program["steps"]), 15)
        # Nor a third row given with the second missing.
        gap = {k: v for k, v in DEMO.items() if not k.endswith("_2")}
        gap.update(item_3="x", qty_3="1", rate_3="2")
        self.assertEqual(skills.run_rows(skill, gap), (None, None))

    def test_a_run_started_by_an_engine_keeps_the_rows_it_was_given(self):
        skill = a_bill_skill()
        given = dict(DEMO, item_3="Monitor", qty_3="2", rate_3="15000", not_a_value="x")
        values = skills.initial_values(skill, given)
        self.assertEqual(values["item_3"], "Monitor")
        self.assertNotIn("not_a_value", values)


class WhoDoesItOnceItIsTrusted(unittest.TestCase):
    def test_a_trusted_task_is_replayed_and_a_new_one_is_the_models(self):
        skill = a_bill_skill()
        with patch.object(skill_task, "_engine_can_drive", return_value=True):
            self.assertTrue(skill_task._learning_by_model({"skill": skill}))
            skill["experience"] = {"verdicts": [{"ok": True, "checked_by": "model", "helped": False, "basis": ["model", "ledger"]}] * experience.TRUST_AFTER}
            self.assertFalse(skill_task._learning_by_model({"skill": skill}))
            # ...unless its rules ask for more than its steps: then it stays the model's, with the replay as its tool.
            skill["supervised"] = "nothing shows the search for an earlier copy was made"
            self.assertTrue(skill_task._learning_by_model({"skill": skill}))

    def test_a_replay_the_check_cannot_confirm_is_the_models_to_finish_and_marks_the_skill(self):
        skill = a_bill_skill()
        run = {"skill": skill, "values": dict(DEMO), "ladder": None, "asked_ids": []}
        handed, saved = [], []
        verdict = {"verdict": skill_task.verify.UNSURE, "seen": "the draft is saved", "why": "no proof the search for an earlier copy was made"}
        with patch.object(skill_task, "_engine_can_drive", return_value=True), \
                patch.object(skill_task, "_judge", return_value=verdict), \
                patch.object(skill_task.verify, "available", return_value=True), \
                patch.object(skill_task, "_by_model", side_effect=lambda r, **kw: handed.append(dict(r))), \
                patch.object(skill_task.skills, "load", return_value=dict(skill)), \
                patch.object(skill_task.skills, "save", side_effect=saved.append), \
                patch.object(skill_task.progress, "emit"), patch.object(skill_task, "_pause") as paused:
            skill_task._finish(run, {"outcome": "completed_unverified", "text": "Replayed: all 15 steps ran."})

        paused.assert_not_called()                       # the model looks first; a person only after that
        self.assertEqual(handed[0]["attempts"], 1)       # as a second attempt: what was entered exists, do not enter again
        self.assertIn("no proof the search", handed[0]["attempt_note"])
        self.assertIn("no proof the search", saved[0]["supervised"])

    def test_a_task_the_person_said_to_check_before_starting_is_the_models_from_the_first_run(self):
        skill = a_bill_skill()
        skill["experience"] = {"verdicts": [{"ok": True, "checked_by": "model", "helped": False, "basis": ["model", "ledger"]}] * experience.TRUST_AFTER}
        with patch.object(skill_task, "_engine_can_drive", return_value=True):
            for said in ("Before you start, search ERPNext for the bill's supplier invoice number.",
                         "If a Purchase Invoice with that number already exists, do not enter it again - tell me.",
                         "First check that the customer is active.", "Enter it only if the total is under 50,000."):
                skill["explanation"] = said
                self.assertTrue(skill_task._learning_by_model({"skill": skill}), said)
            for said in ("Dates are typed as DD-MM-YYYY. Save as a draft only, never submit.",
                         "Pick the existing record that is the same thing. Enter one row for every line of the bill."):
                skill["explanation"] = said
                self.assertFalse(skill_task._learning_by_model({"skill": skill}), said)

    def test_the_engine_is_handed_the_command_with_this_runs_values(self):
        run = {"skill": a_bill_skill(), "values": {"supplier": "Sharma & Co", "item": "Professional Fees", "qty": "1", "rate": ""}}
        line = skill_task._do_line(run)
        self.assertTrue(line.startswith("gui do enter-a-vendor-bill --param "))
        self.assertIn("'supplier=Sharma & Co'", line)
        self.assertNotIn("rate=", line)
        self.assertTrue(skill_task._do_line(run, start=7).endswith("--from 7"))

    def test_an_application_left_open_elsewhere_is_put_back_on_the_page_the_steps_begin_on(self):
        skill = a_bill_skill()
        opening = {"n": 0, "id": "s0", "kind": "double_click", "point": [70, 262], "summary": "double click 70,262",
                   "expect": [{"type": "url", "value": "http://erp:8000/desk"}, {"type": "window", "value": "Desktop"}]}
        skill["steps"] = [opening] + skill["steps"]
        for n, step in enumerate(skill["steps"], 1):
            step["n"] = n
        run = {"skill": skill, "values": {"supplier": "Sharma & Co"}}
        self.assertEqual(skill_task._first_step_inside(run), 2)
        self.assertEqual(skill_task._back_to_start(run), "gui open http://erp:8000/desk && ")
        # A task that does not begin by opening an application has nowhere to be put back to.
        self.assertEqual(skill_task._back_to_start({"skill": a_bill_skill(), "values": {}}), "")


if __name__ == "__main__":
    unittest.main()


class TypingOverWhatWasSelected(unittest.TestCase):
    """Found live: Ctrl+A then a rate, recorded as text added to a pre-filled field, was replayed as an addition."""

    def test_typing_straight_after_select_all_in_the_same_field_replaces(self):
        from decurix_agent.computer import replay
        field = target("textbox", "Rate (INR)", 1)
        select_all = {"kind": "key", "keys": ["ctrl+a"], "target": field}
        typed = {"kind": "type", "param": "rate", "append": True, "target": field}

        self.assertTrue(replay._replaces_selection(select_all, typed))
        # Not after another key, in another field, or when it was never an addition.
        self.assertFalse(replay._replaces_selection({"kind": "key", "keys": ["End"], "target": field}, typed))
        self.assertFalse(replay._replaces_selection(dict(select_all, target=target("textbox", "Accepted Qty", 1)), typed))
        self.assertFalse(replay._replaces_selection(select_all, dict(typed, append=False)))
        self.assertFalse(replay._replaces_selection(None, typed))
