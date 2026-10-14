"""Regression from live Writer/Impress: visible sibling labels lacked relations."""
import unittest
from unittest.mock import patch

from decurix_agent.computer import atspi


class Node:
    def __init__(self, role, name="", children=(), editable=False, showing=True):
        self.role, self.name, self.children = role, name, list(children)
        self.flags = {"editable": editable, "showing": showing}
        self.parent = None
        for child in self.children:
            child.parent = self

    def get_parent(self): return self.parent
    def get_role_name(self): return self.role
    def get_name(self): return self.name
    def get_child_count(self): return len(self.children)
    def get_child_at_index(self, index): return self.children[index]


class NativeFieldLabels(unittest.TestCase):
    def setUp(self):
        self.patch = patch.object(atspi, "_states", side_effect=lambda api, node: (node.flags, []))
        self.patch.start()
        self.addCleanup(self.patch.stop)

    def test_live_find_and_replace_panel_shapes_expose_distinct_visible_labels(self):
        find, replace = Node("text", editable=True), Node("text", editable=True)
        Node("dialog", "Find and Replace", [Node("panel", "Search For", [Node("label", "Find:"), Node("combo box", children=[find])]),
                                               Node("panel", "Replace With", [Node("label", "Replace:"), Node("combo box", children=[replace])])])
        self.assertEqual(atspi._container_label(None, find), "Find")
        self.assertEqual(atspi._container_label(None, replace), "Replace")

    def test_two_fields_or_two_labels_cannot_be_assigned_a_guessed_label(self):
        for children in ([Node("label", "Name:"), Node("text", editable=True), Node("text", editable=True)],
                         [Node("label", "First:"), Node("label", "Second:"), Node("text", editable=True)]):
            panel = Node("panel", children=children)
            field = next(child for child in panel.children if child.flags["editable"])
            self.assertEqual(atspi._container_label(None, field), "")

    def test_hidden_labels_do_not_override_visible_field_label(self):
        field = Node("text", editable=True)
        Node("panel", children=[Node("label", "Hidden:", showing=False), Node("label", "Visible:"), field])
        self.assertEqual(atspi._container_label(None, field), "Visible")

    def test_label_from_entire_dialog_is_not_guessed_for_unscoped_field(self):
        field = Node("text", editable=True)
        Node("dialog", children=[Node("label", "Group title:"), field])
        self.assertEqual(atspi._container_label(None, field), "")


if __name__ == "__main__":
    unittest.main()
