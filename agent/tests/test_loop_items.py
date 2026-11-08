"""
A demonstration that does the same thing once per document - with the person reading the next document, or making a
record it needed, between two of them - is one block to repeat, not three hundred steps to follow. Found live
(2026-11-01): taught on five bills (292 steps, 6-37 steps between bills), the learner's back-to-back search for blocks of
at most 12 steps found nothing, and the task was the model's step by step every month.

    PYTHONPATH=agent python3 -m unittest agent.tests.test_loop_items
"""

import unittest

from test_recordings import event, field

from decurix_agent.computer import learning, skills

SESSION = "demo-20260914T101500-bbbbbb"


def bills(numbers=("412", "413", "414"), parties=("Bhadra", "Tunga", "Malnad"), new_party_before=None, nav_first=False,
          report_after=False):
    """
    One demonstration: open the books, then for each bill enter its number and party and save. Before every bill
    after the first, the person opened the next bill in the mail to read it (a different address each time) and then
    brought up a fresh voucher form (the same address each time). Before `new_party_before` (an index), they also made
    the party's record first. nav_first: before the first bill they typed two addresses (the books, a report), the way
    the real recording began - a short repeat that must not be taken for the task's loop.
    """

    events = [event(0, "click", field("link", "Books"))]
    i = 1
    if nav_first:
        for address in ("http://books.test/company", "http://books.test/report/monthly"):
            events.append(event(i, "type", field("textbox", "Address"), text=address)); i += 1
            events.append(event(i, "key", field("textbox", "Address"), keys=["Return"])); i += 1
    for n, (number, party) in enumerate(zip(numbers, parties)):
        if n:
            events.append(event(i, "type", field("textbox", "Address"), text=f"http://mail.test/m?id={n}")); i += 1
            events.append(event(i, "key", field("textbox", "Address"), keys=["Return"])); i += 1
            if new_party_before == n:
                events.append(event(i, "click", field("link", "New ledger"))); i += 1
                events.append(event(i, "type", field("textbox", "Name"), text=party)); i += 1
                events.append(event(i, "click", field("button", "Save ledger"))); i += 1
            events.append(event(i, "type", field("textbox", "Address"), text="http://books.test/voucher/new")); i += 1
            events.append(event(i, "key", field("textbox", "Address"), keys=["Return"])); i += 1
        events.append(event(i, "click", field("textbox", "No."))); i += 1
        events.append(event(i, "type", field("textbox", "No."), text=number,
                            expect={"values": [{"role": "textbox", "name": "No.", "value": number}]})); i += 1
        events.append(event(i, "click", field("textbox", "Party"))); i += 1
        events.append(event(i, "type", field("textbox", "Party"), text=party,
                            expect={"values": [{"role": "textbox", "name": "Party", "value": party}]})); i += 1
        events.append(event(i, "click", field("button", "Accept"))); i += 1
    if report_after:
        # After the last bill the person opened the Day Book: typed like opening the next bill, so every repetition
        # ends the same way - and that ending is not part of an item.
        events.append(event(i, "type", field("textbox", "Address"), text="http://books.test/daybook?month=9")); i += 1
        events.append(event(i, "key", field("textbox", "Address"), keys=["Return"])); i += 1
    meta = {"id": SESSION, "title": "Enter the month's bills", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "screen": [1280, 720]}
    return meta, events


class ARepeatedBlockWithStepsBetween(unittest.TestCase):

    def test_the_block_is_found_with_the_way_to_the_next_item_and_what_came_between(self):
        skill = learning.build([bills()])
        (loop,) = skill["loops"]
        by_id = {s["id"]: s for s in skill["steps"]}
        # Bringing up the fresh form (type the address, Return) is part of every item; the first item reached it
        # another way, so its block is two steps shorter.
        self.assertEqual((loop["length"], loop["reps"]), (7, 3))
        self.assertEqual([len(b) for b in loop["blocks"]], [5, 7, 7])
        self.assertEqual([by_id[i]["kind"] for i in loop["blocks"][1]], ["type", "key", "click", "type", "click", "type", "click"])
        # Opening the next bill in the mail is what came between: not part of any item.
        self.assertEqual([[by_id[i]["kind"] for i in gap] for gap in loop["between"]], [["type", "key"], ["type", "key"]])
        self.assertIn("Between two of them you did 4 other step(s)", learning.questions(skill)[2]["text"])

    def test_each_keeps_one_whole_item_and_lists_what_changes(self):
        skill = learning.build([bills(new_party_before=2)])
        (loop,) = skill["loops"]
        self.assertEqual([len(g) for g in loop["between"]], [2, 5])
        learning.answer(skill, learning.next_question(skill), "x")    # goal
        learning.answer(skill, learning.next_question(skill), "yes")  # start
        result = learning.answer(skill, learning.next_question(skill), "each")
        self.assertTrue(result["understood"])

        # Open the books, then one item of seven steps: the fresh form, number, party, save.
        self.assertEqual([s["kind"] for s in skill["steps"]], ["click", "type", "key", "click", "type", "click", "type", "click"])
        self.assertEqual([s["n"] for s in skill["steps"] if s.get("repeat")], [2, 3, 4, 5, 6, 7, 8])
        params = {p["name"]: p for p in skill["params"]}
        lists = {name: p["examples"] for name, p in params.items() if p.get("list")}
        self.assertEqual(sorted(lists.values()), [["412", "413", "414"], ["Bhadra", "Tunga", "Malnad"]])
        # The fresh form's address was the same every time: part of the procedure, not asked for.
        fixed = [p for p in params.values() if not p["varies"]]
        self.assertEqual([p["default"] for p in fixed], ["http://books.test/voucher/new"])
        # What came between is gone from the steps and kept in words.
        self.assertEqual(loop["between"], [])
        self.assertEqual(len(loop["between_summaries"]), 2)
        self.assertTrue(any("New ledger" in s for s in loop["between_summaries"][1]))
        self.assertNotIn("mail.test", str([s.get("samples") for s in skill["steps"]]))

    def test_a_value_typed_in_one_repetition_only_is_not_taken_for_the_same_every_time(self):
        # Found live (2026-11-08): a form of two lines. The first line's price, then a new line, its product, then its
        # price. Only the price repeats, the steps before the second price became the way to the next item - and the
        # product typed there once was kept as "typed the same into every repetition", a fixed value for every line.
        events = [event(0, "click", field("textbox", "Price")),
                  event(1, "type", field("textbox", "Price"), text="120"),
                  event(2, "click", field("button", "New line")),
                  event(3, "click", field("combobox", "Product")),
                  event(4, "type", field("combobox", "Product"), text="Blue widget"),
                  event(5, "click", field("textbox", "Price")),
                  event(6, "type", field("textbox", "Price"), text="75")]
        meta = {"id": SESSION, "title": "Enter an order", "kind": "demonstration", "status": "complete",
                "started_at": 1789380000, "screen": [1280, 720]}
        skill = learning.build([(meta, events)])
        (loop,) = skill["loops"]
        self.assertEqual([len(b) for b in loop["blocks"]], [2, 5])
        learning.answer(skill, learning.next_question(skill), "x")
        learning.answer(skill, learning.next_question(skill), "yes")
        self.assertTrue(learning.answer(skill, learning.next_question(skill), "each")["understood"])

        params = {p["name"]: p for p in skill["params"]}
        item = next(p for p in params.values() if p.get("example") == "Blue widget")
        self.assertTrue(item["varies"])
        self.assertIsNone(item["default"])
        self.assertTrue(item["list"])
        rate = next(p for p in params.values() if p.get("example") == "120")
        self.assertEqual(rate["examples"], ["120", "75"])

    def test_a_run_writes_the_item_out_once_per_document(self):
        skill = learning.build([bills()])
        learning.answer(skill, learning.next_question(skill), "x")
        learning.answer(skill, learning.next_question(skill), "yes")
        learning.answer(skill, learning.next_question(skill), "each")
        number = next(p["name"] for p in skill["params"] if p.get("list") and p["examples"][0] == "412")
        party = next(p["name"] for p in skill["params"] if p.get("list") and p["examples"][0] == "Bhadra")

        program, values = skills.expand(skill, {number: "1188; 1189", party: "Coastal; Hebri"})

        self.assertEqual(len(program["steps"]), 1 + 2 * 7)
        self.assertEqual(values, {f"{number}__1": "1188", f"{number}__2": "1189", f"{party}__1": "Coastal", f"{party}__2": "Hebri"})
        self.assertTrue(program["steps"][-1]["summary"].endswith("(item 2 of 2)"))

    def test_a_short_repeat_that_starts_earlier_does_not_take_the_bills_for_steps_between(self):
        # Found live (2026-11-01): "type an address, Enter" recurred seven times over a recording of five bills; it began
        # before the first bill, was taken for the loop, and the five bill blocks were dropped as "between".
        skill = learning.build([bills(nav_first=True)])
        by_id = {s["id"]: s for s in skill["steps"]}
        shapes = sorted((loop["length"], loop["reps"]) for loop in skill["loops"])
        self.assertEqual(shapes, [(2, 2), (7, 3)])
        nav, items = sorted(skill["loops"], key=lambda loop: loop["length"])
        # The address repeat keeps only the two before the bills; the ones between bills are the bills loop's own.
        self.assertEqual([by_id[i]["kind"] for i in nav["blocks"][0]], ["type", "key"])
        self.assertEqual([len(b) for b in items["blocks"]], [5, 7, 7])
        self.assertTrue(all(by_id[i]["kind"] in ("type", "key") for gap in items["between"] for i in gap))
        learning.answer(skill, learning.next_question(skill), "x")
        learning.answer(skill, learning.next_question(skill), "yes")
        learning.answer(skill, learning.next_question(skill), "each")  # the address repeat
        learning.answer(skill, learning.next_question(skill), "each")  # the bills
        self.assertEqual([s["kind"] for s in skill["steps"]], ["click", "type", "key", "type", "key", "click", "type", "click", "type", "click"])

    def test_an_item_does_not_end_with_the_next_document_opened_to_read_it(self):
        # Found live (2026-11-02): with the Day Book opened after the last bill, every bill ended with "type an address,
        # Enter" and the block kept it - the address of the NEXT bill became a list value read from the documents.
        skill = learning.build([bills(report_after=True)])
        by_id = {s["id"]: s for s in skill["steps"]}
        items = max(skill["loops"], key=lambda loop: loop["length"] * loop["reps"])
        self.assertEqual((items["length"], items["reps"]), (7, 3))
        self.assertEqual([by_id[i]["kind"] for i in items["blocks"][1]], ["type", "key", "click", "type", "click", "type", "click"])
        self.assertTrue(by_id[items["blocks"][1][-1]]["target"]["name"] == "Accept")

    def test_back_to_back_repeats_are_found_as_before(self):
        from test_skills import rows
        skill = learning.build([rows("demo-20260914T101500-aaaaaa")])
        (loop,) = skill["loops"]
        self.assertEqual((loop["length"], loop["reps"], loop["between"]), (2, 3, [[], []]))


if __name__ == "__main__":
    unittest.main()
