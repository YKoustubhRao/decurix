"""
A ref holds from one look to the next only where the next look shows the same control under it (elements.assign_refs;
the idea of Hermes' element tokens, valid for one snapshot): the same DOM node at the same address, or the same path with
the same role and name. A ref whose control changed - another document in the tab, a label that changed on the same
node, the next of several alike moving up into its place, rows numbered anew - is gone, and the engine is told to look
again instead of acting on another control. On a screen that did not change, every ref still works.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_refs_one_look.py
"""

import unittest

import config_stub  # noqa: F401

from decurix_agent.computer import core
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError

URL = "http://x.test/invoice/new"


def page(role, name, node, path=None, value="", row=None, group="cdp:t1"):
    e = {"role": role, "name": name, "value": value, "states": [], "box": [0, 0, 10, 10],
         "path": path or f'site x > {role} "{name}"', "source": "cdp", "group": group, "window_title": "Inv - Chromium",
         "handle": {"backend_id": node}}
    if row:
        e["row"] = row
    return e


def app(role, name, path=None, value=""):
    return {"role": role, "name": name, "value": value, "states": [], "box": [0, 0, 10, 10],
            "path": path or f'gedit window "notes" > {role} "{name}"', "source": "atspi", "group": "atspi:gedit:0:notes",
            "window_title": "notes", "handle": {"indices": [0, 1]}}


def look(elements, url=URL, previous=None):
    """Assign refs as core.observe does; return (elements, the state a next look reads)."""

    groups = [{"id": "cdp:t1", "url": url}]
    next_ref = el.assign_refs(elements, previous, groups)
    return elements, {"elements": elements, "groups": groups, "next_ref": next_ref}


def copy(elements):
    return [dict(e, handle=dict(e["handle"])) for e in elements]


class TheSameScreenKeepsItsRefs(unittest.TestCase):
    def test_nothing_changed(self):
        first, state = look([page("textbox", "Customer", 1), page("button", "Save", 2), app("button", "Find")])
        again, _ = look(copy(first), previous=state)
        self.assertEqual([e["ref"] for e in again], [e["ref"] for e in first])

    def test_a_value_typed_into_a_field_keeps_its_ref(self):
        first, state = look([page("textbox", "Customer", 1), app("textbox", "Search")])
        typed = copy(first)
        typed[0]["value"], typed[1]["value"] = "Acme", "colour"
        again, _ = look(typed, previous=state)
        self.assertEqual([e["ref"] for e in again], [e["ref"] for e in first])

    def test_a_row_inserted_in_front_renumbers_nothing(self):
        first, state = look([page("button", "Save", 2)])
        again, _ = look([page("row", "new", 9)] + copy(first), previous=state)
        self.assertEqual(again[1]["ref"], first[0]["ref"])
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_a_node_that_moved_keeps_its_ref(self):
        # Row 2 deleted: row 3's own field is now numbered 2, and it is still the same field.
        first, state = look([page("textbox", "Qty", 20, path='site x > row 2 > textbox "Qty"', row=2),
                             page("textbox", "Qty", 30, path='site x > row 3 > textbox "Qty"', row=3)])
        again, _ = look([page("textbox", "Qty", 30, path='site x > row 2 > textbox "Qty"', row=2)], previous=state)
        self.assertEqual(again[0]["ref"], first[1]["ref"])


class AChangedControlGetsANewRef(unittest.TestCase):
    def test_another_address_in_the_same_tab_is_another_document(self):
        first, state = look([page("button", "Save", 2)])
        again, _ = look(copy(first), url="http://x.test/invoice/INV-7", previous=state)
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_a_label_that_changed_on_the_same_node(self):
        first, state = look([page("button", "Edit", 2)])
        again, _ = look([page("button", "Save", 2)], previous=state)
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_the_next_of_several_alike_does_not_take_the_gone_ones_ref(self):
        # Two "Delete" buttons an app tells apart only by their order ([2]); the first goes, the second becomes the
        # only one, at the first's path.
        first, state = look([app("button", "Delete"), app("button", "Delete", path='gedit window "notes" > '
                                                                                  'button "Delete"[2]')])
        again, _ = look([app("button", "Delete")], previous=state)
        self.assertNotIn(again[0]["ref"], (first[0]["ref"],))

    def test_a_redrawn_page_control_holding_another_value_is_another(self):
        first, state = look([page("textbox", "Qty", 20, path='site x > row 2 > textbox "Qty"', value="5", row=2)])
        again, _ = look([page("textbox", "Qty", 77, path='site x > row 2 > textbox "Qty"', value="7", row=2)],
                        previous=state)
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_rows_numbered_anew_after_one_went(self):
        first, state = look([page("textbox", "Qty", 20, path='site x > row 2 > textbox "Qty"', value="", row=2),
                             page("textbox", "Qty", 30, path='site x > row 3 > textbox "Qty"', value="", row=3)])
        # Every row drawn anew, row 3 now numbered 2: the old row 2's ref must not name it.
        again, _ = look([page("textbox", "Qty", 31, path='site x > row 2 > textbox "Qty"', value="", row=2)],
                        previous=state)
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_a_control_drawn_anew_unchanged_keeps_its_ref(self):
        first, state = look([page("textbox", "Customer", 1, value="Acme")])
        again, _ = look([page("textbox", "Customer", 2, value="Acme")], previous=state)
        self.assertEqual(again[0]["ref"], first[0]["ref"])


class AGoneRefIsNotReboundToAnother(unittest.TestCase):
    def gone(self, previous, now):
        groups = [{"id": "cdp:t1", "url": URL}]
        return core._gone(dict(previous, groups=groups), now, groups)

    def test_not_to_the_same_named_control_of_another_row(self):
        then = [page("button", "Delete", 1, path='site x > table "Bills" > row "B-1 | 100" > button "Delete"',
                     ) | {"ref": "e4"}]
        now = [page("button", "Delete", 2, path='site x > table "Bills" > row "B-2 | 200" > button "Delete"',
                    ) | {"ref": "e9"}]
        where = self.gone({"elements": then}, now)
        with self.assertRaises(ComputerError) as caught:
            el.find(now, ref="e4", gone=where)
        self.assertIn("observe again", str(caught.exception).lower())

    def test_not_to_the_label_left_beside_a_gone_field(self):
        # Found live (2026-11-15): a page removed its "Supplier name" textbox and kept the label's static text; the gone
        # textbox's ref was re-bound to the label (both roles normalised twice to "textbox") and typed into.
        path = 'site x > form "Supplier" > textbox "Supplier name"'
        then = [page("textbox", "Supplier name", 1, path=path) | {"ref": "e2"}]
        now = [page("text", "Supplier name", 7, path='site x > form "Supplier" > text "Supplier name"') | {"ref": "e10"}]
        where = self.gone({"elements": then}, now)
        with self.assertRaises(ComputerError) as caught:
            el.find(now, ref="e2", gone=where)
        self.assertIn("no longer on the screen", str(caught.exception))

    def test_a_field_does_not_pass_its_ref_to_a_label_on_the_same_node(self):
        first, state = look([page("textbox", "Supplier name", 5)])
        again, _ = look([page("text", "Supplier name", 5)], previous=state)
        self.assertNotEqual(again[0]["ref"], first[0]["ref"])

    def test_still_to_the_same_control_drawn_anew_in_its_place(self):
        path = 'site x > form "Return" > button "Proceed"'
        then = [page("button", "Proceed", 1, path=path) | {"ref": "e3"}]
        now = [page("button", "Proceed", 2, path=path) | {"ref": "e8"}]
        where = self.gone({"elements": then}, now)
        self.assertEqual(el.find(now, ref="e3", gone=where)["ref"], "e8")


class TypingNeedsAField(unittest.TestCase):
    def test_a_pages_static_text_is_never_typed_into(self):
        # Found live (2026-11-15): typed "into" a label, the keys went to the focused button.
        from decurix_agent.computer import selecting
        for role in ("text", "paragraph", "label"):
            self.assertIn(role, selecting.NOT_A_FIELD)
        self.assertFalse(selecting.NOT_A_FIELD & selecting.TYPING_ROLES)


if __name__ == "__main__":
    unittest.main()
