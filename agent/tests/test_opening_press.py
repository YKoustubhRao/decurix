import unittest

from decurix_agent.computer import replay

ITEM = {"role": "combobox", "name": "Item"}
STEPS = [
    {"n": 1, "kind": "click", "target": {"role": "combobox", "name": "Supplier"}},
    {"n": 2, "kind": "select", "target": {"role": "combobox", "name": "Supplier"}, "param": "supplier"},
    {"n": 3, "kind": "click", "target": dict(ITEM)},
    {"n": 4, "kind": "select", "target": dict(ITEM), "param": "item"},
    {"n": 5, "kind": "click", "target": {"role": "textbox", "name": "Qty"}},
    {"n": 6, "kind": "type", "target": {"role": "textbox", "name": "Rate"}, "param": "rate"},
]


class ResumingAtAFieldItsPressOpens(unittest.TestCase):
    def test_the_press_on_the_same_control_before_it_is_run_too(self):
        self.assertEqual(replay._with_opening_press(STEPS, 4), 3)
        self.assertEqual(replay._with_opening_press(STEPS, 2), 1)

    def test_anything_else_starts_where_it_was_asked(self):
        # The step before pressed another control.
        self.assertEqual(replay._with_opening_press(STEPS, 6), 6)
        # A press is not itself extended, and the first and out-of-range steps are left alone.
        for start in (1, 3, 5, 7):
            self.assertEqual(replay._with_opening_press(STEPS, start), start)


if __name__ == "__main__":
    unittest.main()
