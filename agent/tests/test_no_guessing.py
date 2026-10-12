import unittest
from unittest.mock import patch

from decurix_agent import guidance, skill_task
from decurix_agent.computer import core, experience


def option(name, box):
    return {"role": "option", "name": name, "box": box, "path": name, "source": "cdp"}


FIELD = {"role": "combobox", "name": "Product", "box": [390, 320, 215, 38], "path": "form > Product", "source": "cdp"}
ELSEWHERE = option("Series A", [310, 180, 270, 30])          # another dropdown's option, above the field
EMPTY_LIST = [option("Filtered by: Company equals 'X'", [396, 360, 240, 90]), option(" Create a new one", [396, 455, 240, 30]),
              option("Advanced Search", [396, 490, 240, 30])]


class TheListAFieldHasOpen(unittest.TestCase):
    def test_only_the_options_under_the_field_are_its_list(self):
        look = {"elements": [ELSEWHERE] + EMPTY_LIST + [option("Real One", [396, 400, 240, 30])]}
        self.assertEqual([e["name"] for e in core._offered(look, FIELD)], ["Real One"])

    def test_a_list_left_with_nothing_to_pick_is_seen_even_when_it_was_open_before(self):
        # Found live: the field had been clicked first, so the list's lines were on the page before the typing.
        look = {"elements": [ELSEWHERE] + EMPTY_LIST}
        self.assertTrue(core._offers_nothing(look, look, FIELD))
        self.assertFalse(core._offers_nothing(look, {"elements": look["elements"] + [option("Real One", [396, 400, 240, 30])]}, FIELD))
        self.assertFalse(core._offers_nothing({"elements": []}, {"elements": [ELSEWHERE]}, FIELD))


class StoppingATask(unittest.TestCase):
    def test_a_bare_stop_calls_the_task_off_and_other_words_do_not(self):
        with patch.object(guidance.task_context, "current", return_value="t1"):
            for said in ("stop", "Cancel.", "never mind", "stop it please"):
                guidance.note_reply(said, None)
                self.assertEqual(guidance.refused(), said, said)
            for said in ("stop and tell me what you see", "use the first one", "don't stop"):
                guidance.note_reply(said, None)
                self.assertIsNone(guidance.refused(), said)


def skill(chosen):
    steps = [{"kind": "type", "param": "name"}, {"kind": "click", "target": {"role": "option" if chosen else "button"}}]
    return {"name": "s", "steps": steps, "experience": {"verdicts": [
        {"ok": True, "checked_by": "model", "helped": False}] * experience.TRUST_AFTER}}


class WhoDoesATaughtTask(unittest.TestCase):
    def test_a_trusted_task_is_replayed_whether_or_not_it_picks_from_a_list(self):
        # It used to stay with the model for good: found live, trusted, the next document - one line where the
        # demonstration had two - was met with "I need item_2", because a replay needed every value the
        # demonstration typed. The program is now fitted to the run's rows and choices (skills.fit, test_fit.py).
        with patch.object(skill_task, "_engine_can_drive", return_value=True):
            self.assertFalse(skill_task._learning_by_model({"skill": skill(True)}))
            self.assertFalse(skill_task._learning_by_model({"skill": skill(False)}))
            self.assertFalse(skill_task._learning_by_model({"skill": skill(True), "replay_only": True}))

    def test_a_run_a_person_answered_is_not_a_clean_run(self):
        run = {"skill": skill(True), "values": {}}
        with patch.object(skill_task.guidance, "get", return_value={"run": run, "asked": "Which one?"}), \
                patch.object(skill_task.skills, "load", side_effect=FileNotFoundError):
            skill_task.learn_answer("the second one")
        self.assertTrue(run["asked_person"])


if __name__ == "__main__":
    unittest.main()


class LinesOfAListThatAreNotChoices(unittest.TestCase):
    def test_a_line_that_opens_with_an_icon_is_still_not_a_choice(self):
        # Found live: the list's "create" and "search" lines began with an icon character, were taken for
        # entries, and a list with nothing to pick was not seen to be empty.
        for line in ("\uf067 Create a new one", "\uf002 Advanced Search", "  Create X", "Filtered by: A equals B"):
            self.assertTrue(core._NOT_A_CHOICE.match(line), line)
        for entry in ("Creative Suite", "Newton Lamp", "Advanced Search Engine Kit"[9:]):
            self.assertFalse(core._NOT_A_CHOICE.match(entry), entry)


def taught(rows=2):
    """A skill whose demonstration filled the same three fields once per entry of its document."""
    P = lambda n, l, s: {"name": n, "label": l, "varies": True, "step": s, "example": "x"}
    params = [P("company", "Company", 2), P("reference", "Reference", 4)]
    for r in range(rows):
        tail = "" if r == 0 else f"_{r + 1}"
        params += [P("product" + tail, "Product", 10 + r * 6), P("quantity" + tail, "Quantity", 12 + r * 6), P("price" + tail, "Price", 14 + r * 6)]
    steps = [{"kind": "type", "param": "product"}, {"kind": "click", "target": {"role": "option"}}]
    return {"name": "enter-a-record", "params": params, "steps": steps}


class OneRowPerEntryOfTheDocument(unittest.TestCase):
    """Found live: taught on a document of two lines, one of three had no place for its third line."""

    def test_the_part_a_demonstration_repeated_is_recognised(self):
        from decurix_agent.computer import skills
        group = skills.repeated_rows(taught())
        self.assertEqual(group["fields"], ["Product", "Quantity", "Price"])
        self.assertEqual(group["rows"], [["product", "quantity", "price"], ["product_2", "quantity_2", "price_2"]])
        self.assertEqual(skills.row_names(group, 2), ["product_3", "quantity_3", "price_3"])
        self.assertIsNone(skills.repeated_rows(taught(rows=1)))

    def test_a_document_with_more_entries_gets_a_row_for_each_and_one_with_fewer_loses_the_rest(self):
        from decurix_agent import binding
        skill = taught()
        three = [{"product": p, "quantity": q, "price": c} for p, q, c in (("A", "1", "5"), ("B", "2", "6"), ("C", "3", "7"))]
        values, evidence = {}, {}
        rows = binding._rows(skill, skill["params"], {"rows": three}, [], values, evidence)
        self.assertEqual((rows["count"], rows["demo"]), (3, 2))
        self.assertEqual((values["product_3"], values["quantity_3"], values["price_3"]), ("C", "3", "7"))
        self.assertEqual(rows["base"]["product_3"], "product")

        values = {"product_2": "left over"}
        rows = binding._rows(skill, skill["params"], {"rows": three[:1]}, [], values, {})
        self.assertEqual(rows["count"], 1)
        self.assertNotIn("product_2", values)

    def test_the_engine_and_the_check_are_told_how_many_rows_belong_and_which_are_chosen(self):
        from decurix_agent import binding
        skill = taught()
        three = [{"product": p, "quantity": "1", "price": "5"} for p in "ABC"]
        values = {}
        rows = binding._rows(skill, skill["params"], {"rows": three}, [], values, {})
        run = {"skill": skill, "values": values, "rows": rows}
        self.assertIn("exactly 3 rows", skill_task._rows_note(run))
        self.assertIn("product_3", skill_task._chosen_for(run))
        self.assertNotIn("quantity_3", skill_task._chosen_for(run))
        self.assertEqual(skill_task._rows_note({"skill": skill, "values": {}}), "")


class NothingElseInItsPlace(unittest.TestCase):
    """Found live: a search found no record for a company, and the engine chose another company and saved."""

    def setUp(self):
        import pathlib, tempfile
        self.patch = patch.object(core, "FAILED_SEARCH_FILE", pathlib.Path(tempfile.mkdtemp()) / "failed.json")
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.field = {"role": "combobox", "name": "Company"}

    def test_an_entry_that_shares_no_word_with_what_was_searched_for_is_refused(self):
        core._note_failed_search(self.field, "Zenith Stationers & Printers")
        self.assertEqual(core._in_place_of(self.field, "Summit Traders Ltd. Summit Traders Ltd., Demo"), "Zenith Stationers & Printers")
        self.assertIsNone(core._in_place_of(self.field, "Zenith Stationers Zenith Stationers, All"))
        self.assertIsNone(core._in_place_of({"role": "combobox", "name": "Product"}, "Summit Traders Ltd."))   # another field
        with patch.object(core, "observe", return_value={"elements": []}), patch.object(core.cdp, "click") as click:
            with self.assertRaises(core.ComputerError) as ctx:
                core._select_custom(self.field, "Summit Traders Ltd.")
            click.assert_not_called()
        self.assertIn("ask the person", str(ctx.exception))

    def test_the_same_thing_under_a_shorter_name_is_allowed_and_a_persons_answer_lifts_it(self):
        core._note_failed_search(self.field, "High Back Ergonomic Lumbar Support Office Mesh Chairs")
        self.assertIsNone(core._in_place_of(self.field, "Ergonomic Office Chair Ergonomic Office Chair, Products"))
        self.assertIsNotNone(core._in_place_of(self.field, "Laptop Laptop, Products"))
        core.forget_failed_search()                      # the watcher does this when a person replies
        self.assertIsNone(core._in_place_of(self.field, "Laptop Laptop, Products"))

    def test_it_binds_the_task_it_happened_in_and_no_other(self):
        with patch.object(core, "_task_now", return_value="t1"):
            core._note_failed_search(self.field, "Zenith Stationers & Printers")
            self.assertIsNotNone(core._in_place_of(self.field, "Summit Traders Ltd."))
        with patch.object(core, "_task_now", return_value="t2"):
            self.assertIsNone(core._in_place_of(self.field, "Summit Traders Ltd."))

    def test_another_records_name_typed_into_the_field_is_refused_like_selecting_it(self):
        # Found live: select refused, the click in the list refused - then "Managed Database Hosting" was typed
        # into the Item field of a server chassis' line, Tab took it as the choice, and the draft was saved.
        item = {"role": "combobox", "name": "Item", "row": 2}
        core._note_failed_search(item, "2U Rack Server Chassis with Redundant Power Supply")
        self.assertEqual(core._typed_in_place_of(item, "Managed Database Hosting"),
                         "2U Rack Server Chassis with Redundant Power Supply")
        # Searching again for the thing itself, or part of it, is still what the field is for.
        self.assertIsNone(core._typed_in_place_of(item, "Rack Server"))
        self.assertIsNone(core._typed_in_place_of(item, "2U Rack Server Chassis with Redundant Power Supply"))
        # Another field, and a field that looks nothing up, are not bound by it.
        self.assertIsNone(core._typed_in_place_of({"role": "textbox", "name": "Rate (INR)"}, "48,000.00"))
        self.assertIsNone(core._typed_in_place_of({"role": "button", "name": "Item"}, "Managed Database Hosting"))
        self.assertIn("ask the person", core.SUBSTITUTE)

    def test_words_that_only_say_what_kind_of_body_it_is_do_not_count(self):
        core._note_failed_search(self.field, "Zenith Stationers Pvt Ltd")
        self.assertIsNotNone(core._in_place_of(self.field, "Summit Traders Pvt Ltd"))


class ACellThatBecomesItsEditor(unittest.TestCase):
    """Found live: select aimed at a table cell (plain text until pressed) searched nothing and said the record was not there."""

    def test_the_field_that_takes_the_keyboard_is_the_one_searched(self):
        cell = {"role": "text", "name": "Product", "path": "site > table > text", "source": "cdp", "states": []}
        editor = {"role": "combobox", "name": "Product", "path": "site > table > combobox", "source": "cdp",
                  "states": ["focused"]}
        other = {"role": "textbox", "name": "Notes", "path": "site > textbox", "source": "cdp", "states": []}
        with patch.object(core, "observe", return_value={"elements": [cell, other, editor]}):
            self.assertIs(core._editor_opened_by(cell), editor)
        with patch.object(core, "observe", return_value={"elements": [cell, other]}), patch.object(core, "EDITOR_SECONDS", 0):
            self.assertIsNone(core._editor_opened_by(cell))

    def test_a_point_on_a_dialog_is_the_dialogs_and_never_the_page_behind_it(self):
        from decurix_agent.computer import elements as el
        behind = {"role": "link", "name": "Reports", "box": [400, 350, 80, 40], "group": "g", "states": []}
        dialog = {"role": "dialog", "name": "New record", "box": [345, 65, 575, 610], "group": "g", "states": []}
        field = {"role": "textbox", "name": "Name", "box": [360, 236, 540, 30], "group": "g", "states": []}
        outside = {"role": "link", "name": "Home", "box": [20, 180, 80, 30], "group": "g", "states": []}
        page = [behind, outside, dialog, field]
        self.assertIs(el.at_point(page, 420, 360), dialog)        # on the dialog, over the link behind: the dialog
        self.assertIs(el.at_point(page, 400, 250), field)
        self.assertIs(el.at_point(page, 40, 190), outside)        # beside the dialog: the page
        self.assertIs(el.at_point([behind, outside], 420, 360), behind)

    def test_a_pages_field_out_of_view_is_used_by_its_ref_not_scrolled_for(self):
        from decurix_agent.computer import elements as el
        field = {"ref": "e7", "role": "textbox", "name": "First Name", "states": ["offscreen"], "source": "cdp"}
        self.assertIn("use its ref", el.element_line(field))
        self.assertIn("scroll to reach", el.element_line(dict(field, source="atspi")))

    def test_a_look_keeps_the_words_a_saved_record_shows_outside_its_fields(self):
        from decurix_agent.computer import evidence
        page = [{"role": "heading", "name": "Record 42", "group": "g"},
                {"role": "link", "name": "Office Chair", "group": "g", "states": ["offscreen"]},
                {"role": "text", "name": "12,000.00", "group": "g", "states": ["offscreen"]},
                {"role": "text", "name": "12,000.00", "group": "g"},                 # said twice running: kept once
                {"role": "button", "name": "Save", "group": "g"},
                {"role": "text", "name": "another window", "group": "other"}]
        with patch.object(core, "active_group", return_value="g"):
            self.assertEqual(evidence.words_text({"elements": page}), "Record 42 | Office Chair | 12,000.00")
            self.assertEqual(evidence.words_text({"elements": []}), "")
        # The front window cannot be told: every window's words are kept, never none.
        with patch.object(core, "active_group", return_value=None):
            self.assertEqual(evidence.words_text({"elements": page}),
                             "Record 42 | Office Chair | 12,000.00 | another window")

    def test_an_entry_is_brought_back_by_its_own_name_as_the_list_writes_it(self):
        self.assertEqual(core._own_label("USB-C Monitor USB-C Monitor, Products"), "USB-C Monitor")
        self.assertEqual(core._own_label("Leased Line Leased Line, Services"), "Leased Line")
        self.assertEqual(core._own_label("Summit Traders Ltd."), "Summit Traders Ltd.")
