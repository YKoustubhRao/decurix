import unittest
from unittest.mock import patch

from decurix_agent.computer import core, replay, skills

from test_fit import DEMO, a_bill_skill, target


class AFieldThatAppearsWhenItsCellIsPressed(unittest.TestCase):
    """Found live: a table's cell is plain text until pressed, and the recorder named the press by the field that then appeared."""

    def step(self, **changed):
        base = {"n": 14, "kind": "click", "target": target("combobox", "Item"), "point": [411, 338], "screen": [1280, 720],
                "summary": 'click combobox "Item"', "expect": []}
        base.update(changed)
        return base

    def look(self, size=(1280, 720)):
        return {"size": list(size), "elements": []}

    MISSING = 'combobox "Item" is not on the screen'

    def test_the_recorded_spot_is_pressed_only_for_a_plain_click_on_a_field_that_is_simply_not_there(self):
        self.assertTrue(replay._appears_when_pressed(self.step(), self.look(), self.MISSING))

        for not_this, why in (
                (self.step(target=target("button", "Save")), "a button is not a cell's field"),
                (self.step(consequential=True), "a step that could do harm is never pressed blind"),
                (self.step(optional=True), "an optional step that is not there is left out, not pressed"),
                (self.step(kind="type"), "typing is not a press"),
                (self.step(point=None), "nothing recorded to press")):
            self.assertFalse(replay._appears_when_pressed(not_this, self.look(), self.MISSING), why)
        self.assertFalse(replay._appears_when_pressed(self.step(), self.look((1920, 1080)), self.MISSING))
        self.assertFalse(replay._appears_when_pressed(
            self.step(), self.look(), '2 elements match combobox "Item", and a replay does not guess which'))

    def test_healing_is_tried_before_the_spot_is_pressed(self):
        # Found live: the field was there all along, called "Rate" where the recording said "Rate (INR)".
        renamed = {"role": "textbox", "name": "Rate", "path": "Form/textbox:Rate"}

        class Ladder:
            asked = 0

            def recover(self, step, observation, reason):
                Ladder.asked += 1
                return {"element": renamed, "options": [], "how": "the same field, renamed", "confidence": 0.9}

        step = self.step(target=target("textbox", "Rate (INR)"), summary='click textbox "Rate (INR)"')
        with patch.object(replay.core, "observe", return_value=self.look()), patch.object(replay, "FIND_SECONDS", 0), \
                patch.object(replay.time, "sleep"):
            element, point, seen, repair = replay._find(step, Ladder())
        self.assertEqual((element, point, Ladder.asked), (renamed, None, 1))
        self.assertNotIn("pressed_cell", seen)

    def test_with_nothing_to_heal_to_the_spot_is_pressed_and_said(self):
        said = []
        with patch.object(replay.core, "observe", return_value=self.look()), patch.object(replay, "FIND_SECONDS", 0), \
                patch.object(replay.time, "sleep"):
            element, point, seen, repair = replay._find(self.step(), None, said.append)
        self.assertEqual((element, point), (None, (411, 338)))
        self.assertTrue(seen["pressed_cell"])
        self.assertIn("pressing where the person pressed", said[0])


class TitlesThatAreNotResults(unittest.TestCase):
    def test_an_unsaved_records_id_is_not_part_of_where_a_step_was(self):
        # Found live: "New Purchase Invoice - new-purchase-invoice-sumgovbtbp" one day, "...-iilddgjxmx" the next.
        for title, kept in (("New Purchase Invoice - new-purchase-invoice-sumgovbtbp - Chromium", "New Purchase Invoice - Chromium"),
                            ("New Purchase Invoice - new-purchase-invoice-sumgovbtbp", "New Purchase Invoice"),
                            ("Sales Order - sales-order-abc - Chromium", "Sales Order - Chromium"),
                            ("Suppliers - Chromium", "Suppliers - Chromium"),
                            ("my-notes - Mousepad", "my-notes - Mousepad"),
                            ("Koustubh - Google Search - Chromium", "Koustubh - Google Search - Chromium")):
            self.assertEqual(replay._stable_title(title), kept, title)
        self.assertTrue(replay.title_matches(
            replay._stable_title("New Purchase Invoice - new-purchase-invoice-sumgovbtbp"),
            "New Purchase Invoice - new-purchase-invoice-iilddgjxmx"))


class APageThatDied(unittest.TestCase):
    def test_the_browsers_own_words_for_a_dead_page_are_recognised(self):
        for shown in ("Aw, Snap!\nSomething went wrong while displaying this webpage.\nError code: 11", "Aw Snap",
                      "This page isn't responding", "Out of Memory"):
            self.assertTrue(core._CRASHED_PAGE.search(shown), shown)
        for shown in ("Purchase Invoice", "Something went wrong with the upload", "Snap to grid"):
            self.assertFalse(core._CRASHED_PAGE.search(shown), shown)
        self.assertIn("gui key F5", core.CRASHED_PAGE_NOTE)
        self.assertIn("check what was saved", core.CRASHED_PAGE_NOTE)

    def test_a_page_that_is_merely_slow_is_not_called_dead(self):
        class Frame:
            width, height, rgb = 4, 4, bytes(48)

        with patch("decurix_agent.computer.ocr.read", return_value=[{"text": "Purchase Invoice", "box": [0, 0, 1, 1]}]):
            self.assertIsNone(core._dead_page_note(Frame()))
        with patch("decurix_agent.computer.ocr.read", return_value=[{"text": "Aw, Snap!", "box": [0, 0, 1, 1]}]):
            self.assertEqual(core._dead_page_note(Frame()), core.CRASHED_PAGE_NOTE)
        with patch("decurix_agent.computer.ocr.read", side_effect=RuntimeError("no tesseract")):
            self.assertIsNone(core._dead_page_note(Frame()))


class WhatAFittedRunStillLooksFor(unittest.TestCase):
    def test_results_that_were_the_demonstrations_are_not_waited_for_after_the_first_fitted_step(self):
        skill = a_bill_skill()
        # As recorded: the row's own field holding what was typed is checked; "Add row" led to the demo's item.
        skill["steps"][7]["expect"] = [{"type": "value", "role": "textbox", "name": "Accepted Qty", "param": "qty"}]
        skill["steps"][9]["expect"] = [{"type": "element", "role": "link", "name": "{item} Instances"},
                                       {"type": "element", "role": "textbox", "name": "Accepted Qty"}]
        skill["steps"][14]["expect"] = [{"type": "element", "role": "link", "name": "{item_2} Hosting"}]

        mine = dict(DEMO, supplier="NexGen Hardware Labs Pvt Ltd", item="AI Developer Workstations")
        program, _ = skills.expand(skill, mine)
        by_summary = {s.get("param") or (s.get("target") or {}).get("name") or "save": s for s in program["steps"]}

        self.assertEqual(by_summary["qty"]["expect"], [{"type": "value", "role": "textbox", "name": "Accepted Qty", "param": "qty"}])
        self.assertEqual(by_summary["Add row"]["expect"], [])
        self.assertEqual(program["steps"][-1]["expect"], [])


if __name__ == "__main__":
    unittest.main()
