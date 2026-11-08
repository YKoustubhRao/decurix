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


if __name__ == "__main__":
    unittest.main()
