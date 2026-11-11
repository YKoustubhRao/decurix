"""
A person's own corrections in a demonstration are not part of the procedure: a field typed, then typed again (or its
item chosen again) before anything is saved, held only the last entry. Found live (2026-11-08, a supplier's bill taught
once): the bill number and date typed twice read as two "rows" of a repeated part, the real line rows were not found,
and every later bill would have been entered with the demonstration's lines. The same label in another row of a table,
or drawn elsewhere on the page, is another field and stays.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_demo_corrections.py
"""

import unittest

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import event, field

from decurix_agent.computer import learning, skills

FORM = "http://books.test/app/bill/new-bill-1"
META = {"id": "demo-20261108T151704-e5c4f2", "title": "Enter a supplier's bill", "kind": "demonstration",
        "started_at": 1794150000, "actors": ["human"], "screen": [1280, 720]}


def at(role, name, box, row=None, panel=True):
    """A control of the bill form at `box`; in table row `row` when given (the page names a grid's rows)."""

    path = ["site books.test"] + (["tabpanel \"Details\""] if panel else []) + ([f"row {row}"] if row else []) \
        + [f"{role} \"{name}\""]
    return field(role, name, box=box, path=" > ".join(path), window_title="New Bill - Chromium")


def on_form(i, kind, element=None, **extra):
    return event(i, kind, element, window="New Bill - Chromium", url=FORM, **extra)


NUMBER, DATE = [310, 541, 420, 28], [310, 600, 420, 28]
ITEM_1, QTY_1, RATE_1 = [390, 249, 216, 43], [620, 249, 90, 43], [720, 249, 110, 43]
ITEM_2, QTY_2, RATE_2 = [390, 293, 216, 43], [620, 293, 90, 43], [720, 293, 110, 43]


def bill_demo():
    """The live demonstration's shape: number and date typed twice, row 1's item typed then chosen again after Ctrl+A,
    then a second row, then Ctrl+S."""

    events, i = [], 0

    def add(kind, element=None, **extra):
        nonlocal i
        events.append(on_form(i, kind, element, **extra))
        i += 1

    add("click", at("textbox", "Supplier Invoice No", NUMBER))
    add("type", at("textbox", "Supplier Invoice No", NUMBER), text="TC/2026/0401")
    add("click", at("textbox", "Supplier Invoice Date", DATE))
    add("type", at("textbox", "Supplier Invoice Date", DATE), text="01-10-2026")
    add("key", at("textbox", "Supplier Invoice Date", DATE), keys=["Tab"])
    add("click", at("textbox", "Supplier Invoice No", NUMBER))
    add("type", at("textbox", "Supplier Invoice No", NUMBER), text="TC/2026/0401")
    add("click", at("textbox", "Supplier Invoice Date", DATE))
    add("type", at("textbox", "Supplier Invoice Date", DATE), text="01-10-2026")
    add("key", at("textbox", "Supplier Invoice Date", DATE), keys=["Tab"])
    # Row 1: the item typed (not chosen), the quantity, then the item again - chosen from the list this time.
    add("click", at("combobox", "Item", ITEM_1, panel=False))
    add("type", at("combobox", "Item", ITEM_1, panel=False), text="Cloud Compute", append=True)
    add("click", at("textbox", "Accepted Qty", QTY_1))
    add("type", at("textbox", "Accepted Qty", QTY_1), text="2")
    add("click", at("textbox", "Rate", RATE_1))
    add("type", at("textbox", "Rate", RATE_1), text="40000")
    add("click", at("combobox", "Item", ITEM_1))
    add("key", at("combobox", "Item", ITEM_1), keys=["ctrl+a"])
    add("type", at("combobox", "Item", ITEM_1), text="Cloud Compute", append=True)
    add("click", at("option", "Cloud Compute Instances", [390, 300, 216, 30]))
    add("click", at("button", "Add row", [310, 320, 71, 26]))
    # Row 2.
    add("type", at("combobox", "Item", ITEM_2, row=2), text="Managed Database")
    add("click", at("option", "Managed Database Hosting", [390, 340, 216, 30]))
    add("type", at("textbox", "Accepted Qty", QTY_2, row=2), text="1")
    add("type", at("textbox", "Rate", RATE_2, row=2), text="10000")
    add("key", None, keys=["ctrl+s"])
    return META, events


def typed(skill):
    return [(s["target"]["name"], (s["target"].get("path") or "").count("row 2") > 0)
            for s in skill["steps"] if s["kind"] == "type"]


class Corrections(unittest.TestCase):

    def setUp(self):
        self.skill = learning.build([bill_demo()])

    def test_a_field_entered_twice_keeps_only_the_last_entry(self):
        self.assertEqual(typed(self.skill), [
            ("Supplier Invoice No", False), ("Supplier Invoice Date", False),
            # Row 1's item, chosen again last, takes the place where the person first filled it.
            ("Item", False), ("Accepted Qty", False), ("Rate", False),
            ("Item", True), ("Accepted Qty", True), ("Rate", True)])
        numbers = [p for p in self.skill["params"] if p["label"] == "Supplier Invoice No"]
        self.assertEqual(len(numbers), 1)

    def test_the_superseded_entrys_press_and_list_pick_go_with_it(self):
        clicks = [s["target"]["name"] for s in self.skill["steps"] if s["kind"] == "click"]
        self.assertEqual(clicks.count("Supplier Invoice No"), 1)
        self.assertEqual(clicks.count("Item"), 1, "the press before the superseded item entry is gone")
        self.assertIn("Cloud Compute Instances", clicks, "the later entry's own pick stays")

    def test_the_person_is_told(self):
        self.assertTrue(any("twice; the last entry is kept" in note and "Supplier Invoice No" in note
                            for note in self.skill["tidied"]), self.skill["tidied"])

    def test_steps_and_params_are_numbered_in_order(self):
        self.assertEqual([s["n"] for s in self.skill["steps"]], list(range(1, len(self.skill["steps"]) + 1)))
        by_n = {s["n"]: s for s in self.skill["steps"]}
        for param in self.skill["params"]:
            self.assertEqual(by_n[param["step"]].get("param"), param["name"])

    def test_the_line_rows_are_found_not_the_retyped_header(self):
        for param in self.skill["params"]:
            param["varies"] = True  # what the lesson settles for a value read from each bill
        group = skills.repeated_rows(self.skill)
        self.assertIsNotNone(group)
        self.assertNotIn("Supplier Invoice No", group["fields"])
        self.assertEqual(len(group["rows"]), 2)
        self.assertEqual({f.lower() for f in group["fields"]}, {"item", "accepted qty", "rate"})


class NotCorrections(unittest.TestCase):

    def build(self, events):
        return learning.build([(META, events)])

    def test_an_entry_saved_in_between_stays(self):
        """Typed, saved, typed again: two records' values (a search, then another), not a correction."""

        number = at("textbox", "Supplier Invoice No", NUMBER)
        skill = self.build([on_form(0, "type", number, text="A1"), on_form(1, "key", number, keys=["Return"]),
                            on_form(2, "type", number, text="A2")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 2)

    def test_text_added_at_the_end_supersedes_nothing(self):
        item = at("combobox", "Item", ITEM_1)
        skill = self.build([on_form(0, "type", item, text="Cloud"), on_form(1, "click", at("textbox", "Rate", RATE_1)),
                            on_form(2, "type", item, text=" Compute", append=True)])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 2)

    def test_the_same_label_elsewhere_on_the_page_is_another_field(self):
        skill = self.build([on_form(0, "type", at("textbox", "Amount", [100, 100, 100, 20]), text="5"),
                            on_form(1, "type", at("textbox", "Amount", [100, 400, 100, 20]), text="6")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 2)

    def test_another_row_is_another_field_even_at_a_close_place(self):
        skill = self.build([on_form(0, "type", at("textbox", "Qty", QTY_1, row=1), text="5"),
                            on_form(1, "type", at("textbox", "Qty", QTY_1, row=2), text="6")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 2)

    def test_a_button_pressed_in_between_makes_the_next_entry_a_repetition(self):
        """Add row, type the item, Add row, type the next: the field is drawn in one place each time, but a button press
        between the two entries can make a new row - a repetition, not a correction."""

        item, add = at("combobox", "Item", ITEM_1), at("button", "Add row", [310, 320, 71, 26])
        skill = self.build([on_form(0, "click", add), on_form(1, "type", item, text="Pens"),
                            on_form(2, "click", add), on_form(3, "type", item, text="Pens")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 2)

    def test_another_value_simply_typed_again_stays(self):
        """A register's next line is entered in the same place with no button: without selecting what the field held,
        another value is not a correction."""

        number = at("textbox", "Description", NUMBER)
        skill = self.build([on_form(0, "type", number, text="Bolts"),
                            on_form(1, "type", at("textbox", "Quantity", DATE), text="12"),
                            on_form(2, "type", number, text="Washers")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 3)

    def test_a_field_entered_the_same_after_another_field_changed_is_a_new_line(self):
        """Description, Account, Description (another value), Account (the same): the second Account is the new line's."""

        desc, account = at("textbox", "Description", NUMBER), at("textbox", "Account", DATE)
        skill = self.build([on_form(0, "type", desc, text="Bolts"), on_form(1, "type", account, text="Income:Sales"),
                            on_form(2, "type", desc, text="Washers"), on_form(3, "type", account, text="Income:Sales")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 4)

    def test_a_click_on_blank_space_in_between_does_not_hide_a_correction(self):
        number = at("textbox", "Supplier Invoice No", NUMBER)
        skill = self.build([on_form(0, "type", number, text="A1"), on_form(1, "click", None, point=[900, 650]),
                            on_form(2, "type", number, text="A1")])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "type"]), 1)

    def test_a_secret_is_never_dropped(self):
        password = at("textbox", "Password", NUMBER)
        password["secret"] = "password field"
        skill = self.build([on_form(0, "type", password, text=None, withheld={"text": "password field"}),
                            on_form(1, "type", password, text=None, withheld={"text": "password field"})])
        self.assertEqual(len(skill["steps"]), 2, "a secret entry is a step of its own, kept (made manual)")


ITEM_3, RATE_3, CHECK_3 = [390, 337, 216, 43], [720, 337, 110, 43], [319, 350, 14, 14]


def row_3_controls():
    """Row 3's controls as a recorded result lists them (role, name, path): what appears when it is added and
    disappears when it is deleted."""

    return [{k: c[k] for k in ("role", "name", "path")}
            for c in (at("combobox", "Item", ITEM_3, row=3), at("textbox", "Rate", RATE_3, row=3))]


def empty_row_demo(entered_in_row_3=False, delete=True, notice=False, added_seen=True, deleted_seen=True):
    """Two lines entered; a Tab out of the last field makes a third, empty row (controls appear); Escape in it; a save;
    then the person ticks row 3 and deletes it, and saves again. The live shape of 2026-11-08. `added_seen` and
    `deleted_seen` say whether the recording kept the row's controls appearing after the Tab and disappearing after the
    delete."""

    events, i = [], 0

    def add(kind, element=None, **extra):
        nonlocal i
        events.append(on_form(i, kind, element, **extra))
        i += 1

    add("type", at("combobox", "Item", ITEM_1), text="Pens")
    add("type", at("textbox", "Rate", RATE_1), text="10")
    add("click", at("button", "Add row", [310, 320, 71, 26]),
        expect={"appeared": [{"role": "combobox", "name": "Item"}]})
    add("type", at("combobox", "Item", ITEM_2, row=2), text="Ink")
    add("type", at("textbox", "Rate", RATE_2, row=2), text="20")
    add("key", at("textbox", "Rate", RATE_2, row=2), keys=["Tab"],
        expect={"appeared": row_3_controls() if added_seen else [{"role": "textbox", "name": "Rate"}]})
    add("key", at("combobox", "Item", ITEM_3, row=3), keys=["Escape"])
    if entered_in_row_3:
        add("type", at("combobox", "Item", ITEM_3, row=3), text="Paper")
    if notice:
        # The save is refused: a notice appears, and the person closes it with an unnamed button.
        add("key", at("combobox", "Item", ITEM_3, row=3), keys=["ctrl+s"],
            expect={"appeared": [{"role": "heading", "name": "Missing Fields"}]})
        add("click", field("button", "", box=[900, 120, 20, 20], path='site books.test > button ""[4]',
                           window_title="New Bill - Chromium"))
    else:
        add("key", at("combobox", "Item", ITEM_3, row=3), keys=["ctrl+s"])
    if delete:
        add("click", at("checkbox", "", CHECK_3, row=3), expect={"appeared": [{"role": "button", "name": "Delete row"}]})
        add("click", at("button", "Delete row", [310, 395, 87, 26]),
            expect={"disappeared": row_3_controls()} if deleted_seen else {})
    add("key", None, keys=["ctrl+s"])
    return META, events


def rows_in(skill):
    return [(s["kind"], (s.get("target") or {}).get("name"), _row(s)) for s in skill["steps"]]


def _row(step):
    path = str((step.get("target") or {}).get("path") or "")
    return next((seg for seg in path.split(" > ") if seg.startswith("row ")), None)


class EmptyRows(unittest.TestCase):

    def test_a_row_added_by_the_way_and_deleted_is_left_out(self):
        skill = learning.build([empty_row_demo()])
        steps = rows_in(skill)
        self.assertFalse([s for s in steps if s[2] == "row 3"], steps)
        self.assertNotIn(("click", "Delete row", None), steps)
        self.assertNotIn(("key", "Rate", "row 2"), steps, "the Tab that made the row goes too")
        self.assertEqual([s[0] for s in steps].count("key"), 2, "both saves stay")
        self.assertTrue(any("deleted it again with nothing entered" in n for n in skill["tidied"]), skill["tidied"])

    def test_the_save_pressed_in_that_row_keeps_its_keys(self):
        skill = learning.build([empty_row_demo()])
        saves = [s for s in skill["steps"] if s["kind"] == "key"]
        self.assertEqual([s["keys"] for s in saves], [["ctrl+s"], ["ctrl+s"]])
        self.assertTrue(all("target" not in s for s in saves))

    def test_a_row_something_was_entered_into_stays(self):
        skill = learning.build([empty_row_demo(entered_in_row_3=True)])
        steps = rows_in(skill)
        self.assertIn(("click", "Delete row", None), steps)
        self.assertTrue([s for s in steps if s[2] == "row 3"])

    def test_an_empty_row_never_deleted_is_not_touched(self):
        """Not selected and deleted: nothing says the person threw it away."""

        skill = learning.build([empty_row_demo(delete=False)])
        self.assertTrue([s for s in rows_in(skill) if s[2] == "row 3"])
        self.assertFalse(any("deleted it again" in n for n in skill["tidied"]))

    def test_the_rows_of_the_lines_are_still_found(self):
        skill = learning.build([empty_row_demo()])
        for param in skill["params"]:
            param["varies"] = True
        group = skills.repeated_rows(skill)
        self.assertEqual(len(group["rows"]), 2)

    def test_a_row_not_seen_added_is_not_taken_for_an_added_one(self):
        """The Tab's result kept no control of row 3: nothing shows the row was made in this recording."""

        skill = learning.build([empty_row_demo(added_seen=False)])
        self.assertTrue([s for s in rows_in(skill) if s[2] == "row 3"])
        self.assertIn(("click", "Delete row", None), rows_in(skill))
        self.assertFalse(any("deleted it again" in n for n in skill["tidied"]))

    def test_a_press_that_showed_no_row_go_is_not_taken_for_its_delete(self):
        skill = learning.build([empty_row_demo(deleted_seen=False)])
        self.assertTrue([s for s in rows_in(skill) if s[2] == "row 3"])
        self.assertIn(("click", "Delete row", None), rows_in(skill))
        self.assertFalse(any("deleted it again" in n for n in skill["tidied"]))


LIST = "http://books.test/app/parties"


def on_list(i, kind, element=None, **extra):
    return event(i, kind, element, window="Check parties - Chromium", url=LIST, **extra)


def party(role, name, row=None):
    path = ["site books.test", "table \"Parties\""] + ([f"row {row}"] if row else []) + [f"{role} \"{name}\""]
    return field(role, name, box=[300, 200 + 30 * (row or 0), 14, 14], path=" > ".join(path),
                 window_title="Check parties - Chromium")


def brief(element):
    return {k: element[k] for k in ("role", "name", "path")}


def bulk_action_demo(pages=True):
    """
    The live shape of 2026-11-10: a link opens a list of parties (rows 1-4 there at once); the person ticks the party
    in row 4 and presses a bulk button outside the table, which draws row 4 again with what it found; ticks it again
    and presses a second bulk button; then leaves. `pages`: each press moves to another address (a page that answers
    with a reload), else everything happens in place.
    """

    ticked = party("checkbox", "Select P-4", row=4)

    def result(url, **seen):
        return dict(seen, url=url) if pages else seen

    return META, [
        on_form(0, "click", at("link", "Check parties", [900, 80, 90, 20]),
                expect=result(LIST, appeared=[{"role": "heading", "name": "Check parties", "path": "site books.test"}]
                              + [brief(party("checkbox", f"Select P-{n}", row=n)) for n in (1, 2, 3, 4)],
                              active_window="Check parties - Chromium" if pages else None)),
        on_list(1, "click", ticked),
        on_list(2, "click", party("button", "Validate selected"),
                expect=result(LIST + "?done=validated",
                              appeared=[{"role": "status", "name": "Validated.", "path": "site books.test"},
                                        brief(ticked)], disappeared=[brief(ticked)])),
        on_list(3, "click", ticked),
        on_list(4, "click", party("button", "Update names from the registry"),
                expect=result(LIST + "?done=names",
                              appeared=[{"role": "status", "name": "Names updated.", "path": "site books.test"},
                                        brief(ticked)], disappeared=[brief(ticked)])),
        on_list(5, "click", party("link", "Back to the bill")),
    ]


class BulkActionOnARow(unittest.TestCase):
    """A tick in a row that was already there, then a button outside the table, is a bulk action on that row: kept."""

    def test_the_ticks_the_bulk_buttons_and_the_link_before_them_stay(self):
        for pages in (True, False):
            with self.subTest(pages=pages):
                skill = learning.build([bulk_action_demo(pages=pages)])
                self.assertEqual([(s["kind"], s["target"]["name"]) for s in skill["steps"]], [
                    ("click", "Check parties"), ("click", "Select P-4"), ("click", "Validate selected"),
                    ("click", "Select P-4"), ("click", "Update names from the registry"),
                    ("click", "Back to the bill")])
                self.assertFalse(any("deleted it again" in n for n in skill["tidied"]), skill["tidied"])


class RetriedCommits(unittest.TestCase):
    """A save the person had to make again (its notice closed, nothing entered between) is kept once, the last."""

    def test_a_refused_save_and_the_press_that_closed_its_notice_are_left_out(self):
        skill = learning.build([empty_row_demo(notice=True)])
        keys = [s for s in skill["steps"] if s["kind"] == "key" and s.get("keys") == ["ctrl+s"]]
        self.assertEqual(len(keys), 1, rows_in(skill))
        self.assertFalse([s for s in skill["steps"] if s["kind"] == "click" and not (s.get("target") or {}).get("name")
                          and (s.get("target") or {}).get("role") == "button"], rows_in(skill))
        self.assertEqual(skill["steps"][-1]["keys"], ["ctrl+s"])
        self.assertTrue(any("twice" in n for n in skill["tidied"]), skill["tidied"])

    def test_a_save_that_showed_nothing_is_not_taken_for_a_refused_one(self):
        skill = learning.build([empty_row_demo()])
        self.assertEqual(len([s for s in skill["steps"] if s["kind"] == "key" and s.get("keys") == ["ctrl+s"]]), 2)

    def build(self, between):
        events = [on_form(0, "type", at("textbox", "Supplier Invoice No", NUMBER), text="A1"),
                  on_form(1, "key", at("textbox", "Supplier Invoice No", NUMBER), keys=["ctrl+s"],
                          expect={"appeared": [{"role": "heading", "name": "Missing Fields"}]})]
        events += [on_form(2 + k, *e[:2], **e[2]) for k, e in enumerate(between)]
        events.append(on_form(2 + len(between), "key", None, keys=["ctrl+s"]))
        return learning.build([(META, events)])

    def test_anything_done_between_the_two_keeps_both(self):
        named = ("click", at("button", "Add row", [310, 320, 71, 26]), {})
        entry = ("type", at("textbox", "Supplier Invoice Date", DATE), {"text": "01-10-2026"})
        for between in ([named], [entry]):
            skill = self.build(between)
            self.assertEqual(len([s for s in skill["steps"] if s.get("keys") == ["ctrl+s"]]), 2, between)

    def test_a_different_commit_after_is_no_retry(self):
        skill = self.build([("key", None, {"keys": ["Return"]})])
        self.assertEqual(len([s for s in skill["steps"] if s.get("keys") == ["ctrl+s"]]), 2)


class TableRowsAreNoLoop(unittest.TestCase):
    """The lines of one table (each repetition's entries in one row, no two in the same row) are the rows of the form,
    written out per line of the document (skills.repeated_rows / fit), never a loop to fold: found live (2026-11-08),
    line 2 began with "Add row" and line 1 did not, and folding them dropped line 1's quantity and rate."""

    def test_two_lines_of_a_bill_are_no_loop_and_stay_two_rows(self):
        skill = learning.build([empty_row_demo()])
        self.assertEqual(skill["loops"], [])
        for param in skill["params"]:
            param["varies"] = True
        self.assertEqual(len(skills.repeated_rows(skill)["rows"]), 2)
        self.assertEqual(len([s for s in skill["steps"] if s.get("param")]), 4, "both lines keep every field")


if __name__ == "__main__":
    unittest.main()
