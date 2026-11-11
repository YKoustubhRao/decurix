"""
A saved record's lines read back off its page (records.rows_shown): each declared line must stand on a line of the
page of its own, each value under its column. Found live (2026-11-09): a saved invoice shows its items as a grid of
plain text under a line of column names, and its lines were judged only by the model's look at the screen.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import records

BILL = """TAX INVOICE  No. RV-2207   Date 03-10-2026
Supplier: Lakeview Stationers
Description                                   Qty   Rate       Amount
Adjustable mesh-back task chair, grey          3    6,000.00   18,000.00
Wide 27-inch reading monitor                   2   15,000.00   30,000.00
Compact desk lamp                              3      900.00    2,700.00
Total                                                          50,700.00
"""

ROWS = [{"Item": "Adjustable mesh-back task chair, grey", "Accepted Qty": "3", "Rate (INR)": "6,000.00"},
        {"Item": "Wide 27-inch reading monitor", "Accepted Qty": "2", "Rate (INR)": "15,000.00"},
        {"Item": "Compact desk lamp", "Accepted Qty": "3", "Rate (INR)": "900.00"}]

# What the application's lists gave for the bill's descriptions (its own item names).
# (A list names an entry with more than the saved page keeps of it: the first as such a list showed it.)
PICKS = [{"label": "Item", "option": "Adjustable mesh-back task chair, grey", "taken": "Task Chair Task Chair, Products"},
         {"label": "Item", "option": "Wide 27-inch reading monitor", "taken": "Monitor 27"},
         {"label": "Item", "option": "Compact desk lamp", "taken": "Desk Lamp"}]


def piece(name, x, y, w, role="text", h=16):
    return {"role": role, "name": name, "box": [x, y, w, h], "states": ["offscreen"]}


def page(lines):
    """A saved record's page: a header field, the grid's column names, then one line per row."""

    elements = [{"role": "textbox", "name": "Supplier Invoice No", "value": "RV-2207", "box": [250, 580, 341, 28]},
                piece("Items", 250, 855, 36),
                {"role": "checkbox", "name": "", "box": [259, 888, 14, 14]},
                piece("No.", 295, 888, 21), piece("Item", 338, 888, 26), piece("Accepted Qty", 522, 888, 84),
                piece("Rate", 756, 888, 27), piece("Amount", 854, 888, 47)]
    for k, (item, qty, rate, amount) in enumerate(lines):
        y = 924 + 44 * k
        elements += [{"role": "checkbox", "name": "", "box": [259, y + 2, 14, 14]},
                     piece(str(k + 1), 302, y, 6), piece(item, 338, y, 140, role="link"), piece(qty, 608, y, 8),
                     piece(rate, 720, y, 73), piece(amount, 835, y, 77)]
    elements.append({"role": "group", "name": "Items table", "box": [240, 850, 730, 300]})  # (a box around them all)
    return {"elements": elements}


SAVED = [("Task Chair", "3", "₹ 6,000.00", "₹ 18,000.00"), ("Monitor 27", "2", "₹ 15,000.00", "₹ 30,000.00"),
         ("Desk Lamp", "3", "₹ 900.00", "₹ 2,700.00")]


class PageLines(unittest.TestCase):
    def test_pieces_side_by_side_are_one_line_and_a_box_around_them_is_none(self):
        lines = page_lines = records.page_lines(page(SAVED))
        texts = [[p[0] for p in line] for line in page_lines]
        self.assertIn(["No.", "Item", "Accepted Qty", "Rate", "Amount"], texts)
        self.assertIn(["1", "Task Chair", "3", "₹ 6,000.00", "₹ 18,000.00"], texts)
        self.assertFalse(any("Items table" in t for t in texts))
        self.assertEqual(len(lines), 6)  # the field, "Items", the column names, three rows


class RowsShown(unittest.TestCase):
    def test_every_line_saved_as_declared_is_shown_through_the_entries_its_picks_took(self):
        self.assertEqual(records.rows_shown(ROWS, page(SAVED), "day", PICKS), ([1, 2, 3], []))

    def test_without_the_picks_the_documents_words_are_not_on_the_page(self):
        self.assertEqual(records.rows_shown(ROWS, page(SAVED), "day", []), ([], [1, 2, 3]))

    def test_a_line_saved_with_another_quantity_is_not_shown_even_where_the_row_number_matches(self):
        # Line 3 saved with 2, not 3: its row number is 3, but the quantity is read in its own column.
        wrong = SAVED[:2] + [("Desk Lamp", "2", "₹ 900.00", "₹ 1,800.00")]
        self.assertEqual(records.rows_shown(ROWS, page(wrong), "day", PICKS), ([1, 2], [3]))

    def test_a_line_left_out_of_the_save_is_not_shown(self):
        self.assertEqual(records.rows_shown(ROWS, page(SAVED[:2]), "day", PICKS), ([1, 2], [3]))

    def test_two_lines_alike_need_two_lines_of_the_page(self):
        twice = [ROWS[2], dict(ROWS[2])]
        self.assertEqual(records.rows_shown(twice, page(SAVED), "day", PICKS), ([1], [2]))
        self.assertEqual(records.rows_shown(twice, page(SAVED + [SAVED[2]]), "day", PICKS), ([1, 2], []))

    def test_with_no_column_names_a_leading_row_number_holds_no_value(self):
        rows = [{"Count": "3", "Name": "Desk Lamp"}]
        bare = {"elements": [piece("3", 302, 924, 6), piece("Desk Lamp", 338, 924, 80), piece("2", 608, 924, 8)]}
        self.assertEqual(records.rows_shown(rows, bare, "day", []), ([], [1]))
        bare["elements"][2]["name"] = "3"
        self.assertEqual(records.rows_shown(rows, bare, "day", []), ([1], []))


class DeclaredAndReadBack(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bill = Path(self.tmp.name) / "RV-2207.txt"
        self.bill.write_text(BILL, encoding="utf-8")
        for p in (mock.patch.object(records, "LEDGER_DIR", Path(self.tmp.name) / "ledger"),
                  mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"),
                  mock.patch("decurix_agent.task_context.current_request", lambda: "Enter this bill"),
                  mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: ""),
                  mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []),
                  mock.patch.object(records, "_selects", lambda task_id=None, events=None: PICKS),
                  mock.patch.object(records, "_key_on_an_app_page", lambda key, source, task_id=None: ""),
                  mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day")):
            p.start()
            self.addCleanup(p.stop)

    def declare(self, rows):
        return records.declare("RV-2207", str(self.bill), {"Supplier Invoice No": "RV-2207",
                                                           "Supplier": "Lakeview Stationers"}, rows=rows)

    def test_only_values_the_document_writes_are_declared_and_a_row_needs_two(self):
        rows = [dict(ROWS[0], Warehouse="Stores - Main"), {"Item": "Compact desk lamp", "Accepted Qty": "7"}]
        rec, lines = self.declare(rows)
        self.assertEqual(rec["rows"], [ROWS[0]])
        self.assertIn("and 1 line(s)", lines[0])

    def test_a_record_whose_lines_all_stand_on_the_page_is_read_back(self):
        rec, _ = self.declare(ROWS)
        lines = records.readback(rec, "RV-2207", "RV-2207 Lakeview Stationers", "page is now /x/RV-2207",
                                 observation=page(SAVED))
        self.assertIn("all 3 stand on a line of the page", "\n".join(lines))
        self.assertTrue(records.all_read_back())
        self.assertIn("(3 line(s) declared from its source, read back", "\n".join(records.listing()))

    def test_a_line_not_shown_keeps_the_record_from_counting_as_read_back(self):
        rec, _ = self.declare(ROWS)
        lines = records.readback(rec, "RV-2207", "RV-2207 Lakeview Stationers", "page is now /x/RV-2207",
                                 observation=page(SAVED[:2]))
        self.assertIn("NOT shown: line 3 (Item=Compact desk lamp", "\n".join(lines))
        self.assertFalse(records.all_read_back())
        self.assertEqual(records.status_of(records.load()[0]), "saved; not read back: line 3")

    def test_lines_with_no_look_of_the_page_are_not_read_back(self):
        rec, _ = self.declare(ROWS)
        records.readback(rec, "RV-2207", "RV-2207 Lakeview Stationers", "saved")
        self.assertFalse(records.all_read_back())


if __name__ == "__main__":
    unittest.main()


class TaughtRows(unittest.TestCase):
    """The lines a taught run declares (skill_task._taught_rows)."""

    @staticmethod
    def skill(save_between=False, loop=False):
        steps, params = [], []

        def field(name, label, role="textbox", kind="type", list_=False):
            n = len(steps) + 1
            steps.append({"n": n, "id": f"s{n}", "kind": kind, "param": name, "target": {"role": role, "name": label}})
            params.append({"name": name, "label": label.lower(), "step": n, "varies": True, "list": list_})

        field("bill_no", "Supplier Invoice No")
        if loop:
            field("item", "Item", "combobox", "select", list_=True)
            field("qty", "Accepted Qty", list_=True)
            if save_between:
                steps.append({"n": len(steps) + 1, "id": f"s{len(steps) + 1}", "kind": "click",
                              "target": {"role": "button", "name": "Save"}})
            loops = [{"id": "l1", "enabled": True, "blocks": [[s["id"] for s in steps[1:]]]}]
            return {"name": "enter", "steps": steps, "params": params, "loops": loops}
        for k in (1, 2):
            field(f"item_{k}", "Item", "combobox", "select")
            field(f"qty_{k}", "Accepted Qty")
            if save_between and k == 1:
                steps.append({"n": len(steps) + 1, "id": f"s{len(steps) + 1}", "kind": "click",
                              "target": {"role": "button", "name": "Save"}})
        return {"name": "enter", "steps": steps, "params": params}

    def test_rows_the_demonstration_entered_one_after_another(self):
        from decurix_agent import skill_task
        run = {"skill": self.skill(), "values": {"bill_no": "RV-2207", "item_1": "Desk lamp", "qty_1": "3",
                                                 "item_2": "Task chair", "qty_2": "2"}}
        self.assertEqual(skill_task._taught_rows(run), [{"Item": "Desk lamp", "Accepted Qty": "3"},
                                                        {"Item": "Task chair", "Accepted Qty": "2"}])

    def test_a_loops_items_and_an_empty_item_left_out(self):
        from decurix_agent import binding, skill_task
        run = {"skill": self.skill(loop=True), "values": {"bill_no": "RV-2207", "item": "Desk lamp;Task chair",
                                                          "qty": f"3;{binding.EMPTY_ITEM}"}}
        self.assertEqual(skill_task._taught_rows(run), [{"Item": "Desk lamp", "Accepted Qty": "3"},
                                                        {"Item": "Task chair"}])

    def test_a_part_with_a_save_inside_makes_records_of_its_own_not_lines(self):
        from decurix_agent import skill_task
        run = {"skill": self.skill(save_between=True), "values": {"bill_no": "RV-2207", "item_1": "Desk lamp",
                                                                  "qty_1": "3", "item_2": "Task chair", "qty_2": "2"}}
        self.assertEqual(skill_task._taught_rows(run), [])
        run = {"skill": self.skill(save_between=True, loop=True), "values": {"item": "Desk lamp;Task chair",
                                                                             "qty": "3;2"}}
        self.assertEqual(skill_task._taught_rows(run), [])
