import unittest
import unittest.mock

from decurix_agent.computer import core


class NamesAnother(unittest.TestCase):
    def test_a_neighbour_with_another_name_is_refused(self):
        self.assertEqual(core._names_another("Recommended Charts in the Insert ribbon near the middle-left",
                                             "Recommended PivotTables"), "Recommended Charts")

    def test_the_named_control_passes(self):
        self.assertEqual(core._names_another("Browse in the Open page left-center area", "Browse"), "")
        self.assertEqual(core._names_another("New blank workbook near the upper left", "New blank workbook"), "")
        self.assertEqual(core._names_another("the OK button of the dialog", "OK"), "")
        self.assertEqual(core._names_another("Charts group", "Recommended Charts"), "")

    def test_a_description_that_names_nothing_decides_nothing(self):
        self.assertEqual(core._names_another("the small left arrow of the scrollbar", "Scroll Right"), "")
        self.assertEqual(core._names_another("Recommended Charts", ""), "")


if __name__ == "__main__":
    unittest.main()


class KeysWentTo(unittest.TestCase):
    def test_typing_with_no_ref_says_which_field_had_the_keys(self):
        box = {"ref": "e12", "role": "textbox", "name": "Name Box", "value": "A20", "states": ["focused"],
               "path": "Excel/textbox Name Box"}
        after_box = dict(box, value="A20Total")
        with unittest.mock.patch.object(core, "_same_field", return_value=after_box):
            said = core._keys_went_to({"elements": [box]}, {"elements": [after_box]})
        self.assertIn('Name Box', said)
        self.assertIn("A20Total", said)

    def test_nothing_is_said_when_no_listed_field_had_the_keys(self):
        self.assertEqual(core._keys_went_to({"elements": [{"role": "button", "name": "OK", "states": ["focused"]}]},
                                            {"elements": []}), "")


class CursorLine(unittest.TestCase):
    def test_a_vision_read_cursor_is_not_reported(self):
        # A vision model's "the cursor is in" is an estimate, and named the wrong field as often as the right one.
        look = {"elements": [{"role": "text", "name": "the cursor is in: Name", "states": []}]}
        self.assertEqual(core._keys_went_to(look, {"elements": []}), "")
