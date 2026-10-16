"""A reference a run reached by pressing the control named for it was used, not left out."""

import unittest
from unittest import mock

from decurix_agent import skill_task


RUN = {"skill": {"params": [{"name": "find_purchase_order"}, {"name": "received"}]},
       "values": {"find_purchase_order": "PO-4423", "received": "12"},
       "evidence": {"find_purchase_order": "the note", "received": "the note"}}


class APressedReference(unittest.TestCase):
    def check(self, pressed, typed=("NP-1",)):
        with mock.patch.object(skill_task.evidence, "entered", return_value=list(typed)), \
             mock.patch.object(skill_task.evidence, "read", return_value=""), \
             mock.patch.object(skill_task.evidence, "pressed", return_value=pressed), \
             mock.patch.object(skill_task, "_chosen_for", return_value=set()):
            return skill_task._never_entered(RUN)

    def test_a_link_named_for_the_reference_counts(self):
        # Found live (2026-10-15): PO-4423 opened by its link was refused as "never typed".
        self.assertEqual(self.check(["PO-4423"], typed=("12",)), {})
        self.assertEqual(self.check(["PO-4423 Delta Foods"], typed=("12",)), {})

    def test_a_quantity_is_not_reached_by_a_link(self):
        self.assertEqual(self.check(["PO-4423", "12"]), {"received": "12"})

    def test_another_reference_does_not_count(self):
        self.assertEqual(self.check(["PO-44231"], typed=("12",)), {"find_purchase_order": "PO-4423"})


if __name__ == "__main__":
    unittest.main()
