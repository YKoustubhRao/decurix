"""
A nameless field takes the name of the label beside it (atspi.name_fields_by_layout).

Found in GnuCash's New Invoice dialog, where Customer, Job and Billing ID are
nameless text boxes in one panel: a recorded "type 4501 into the Billing ID"
could only be found again by its order, which changed when a customer was chosen.
"""

import unittest

from test_recordings import FakeScreen  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import atspi


def label(name, x, y, w=60, h=17):
    return {"role": "label", "name": name, "box": [x, y, w, h], "path": f'dialog > label "{name}"'}


def box(x, y, w=254, h=34, role="textbox", name=""):
    return {"role": role, "name": name, "box": [x, y, w, h], "path": f'gnucash dialog "New Invoice" > panel "Billing Information" > {role} ""'}


class NameFieldsByLayout(unittest.TestCase):

    def gnucash(self):
        # Measured off the real dialog (gui observe --coords), after a customer was chosen.
        return [
            label("Customer", 244, 512), box(311, 504),
            label("Job", 280, 549, w=26), box(311, 541),
            label("Billing ID", 247, 586), box(311, 578),
            label("Notes", 236, 661), box(240, 690, w=330, h=60),
        ]

    def test_each_field_is_named_after_the_label_on_its_row(self):
        elements = self.gnucash()
        atspi.name_fields_by_layout(elements)

        fields = [e for e in elements if e["role"] == "textbox"]
        self.assertEqual([f["name"] for f in fields], ["Customer", "Job", "Billing ID", "Notes"])
        self.assertTrue(fields[2]["path"].endswith('textbox "Billing ID"'))
        self.assertEqual(fields[2]["label_from"], "layout")

    def test_a_field_with_its_own_name_or_no_label_near_it_is_left_alone(self):
        named = box(311, 504, name="Search for")
        far = box(900, 300)
        elements = [label("Customer", 244, 512), named, far]
        atspi.name_fields_by_layout(elements)

        self.assertEqual((named["name"], far["name"]), ("Search for", ""))

    def test_two_labels_equally_close_name_nothing(self):
        field = box(311, 504)
        elements = [label("Customer", 244, 512), label("Vendor", 244, 514), field]
        atspi.name_fields_by_layout(elements)

        self.assertEqual(field["name"], "")

    def test_a_label_on_another_row_does_not_name_a_field(self):
        field = box(311, 504)
        elements = [label("Customer", 244, 560), field]
        atspi.name_fields_by_layout(elements)

        self.assertEqual(field["name"], "")


if __name__ == "__main__":
    unittest.main()
