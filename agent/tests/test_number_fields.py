"""
Finding AR (2026-10-20, P2 on a portal): an order line's Quantity (an <input type=number>) went 24 -> 25 when the
worker clicked "option 24" of the browser's list of earlier entries by its picture - at a point on the field's
up/down arrows - and the order was saved with 60 where 24 was meant. Number fields are set whole and read back;
clicks on their arrows, Up/Down in them and the wheel over them are refused or moved; and a form with several filled
fields lists what it holds before it is sent. Stand-in pages only, never a browser.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first
from test_enter_in_forms import type_into

from decurix_agent.computer import cdp, core
from decurix_agent.computer.errors import ComputerError


class NumberPage:
    """A stand-in page whose focused field is a number input; `keeps` is what it holds after being set."""

    def __init__(self, keeps=None):
        self.keeps, self.value, self.keys = keeps, "", []

    def call(self, method, params=None):
        params = params or {}
        if method == "Input.dispatchKeyEvent" and params.get("type") == "keyDown":
            self.keys.append(params.get("key"))
        elif method == "Runtime.callFunctionOn":
            declaration = params.get("functionDeclaration", "")
            if declaration == cdp.SET_NUMBER:
                text = params["arguments"][0]["value"]
                self.value = self.keeps if self.keeps is not None else text
                return {"result": {"value": {"value": self.value}}}
            if "multiline" in declaration:
                return {"result": {"value": {"multiline": False, "input": "number"}}}
            return {"result": {"value": self.value}}
        return {}

    def close(self):
        pass


class ANumberFieldIsSetWhole(unittest.TestCase):
    def test_the_quantity_is_set_as_a_value_with_no_keys(self):
        page = NumberPage()
        type_into(page, "24")
        self.assertEqual((page.value, page.keys), ("24", []))  # no keys: no list of earlier entries opens

    def test_a_field_that_holds_something_else_is_said(self):
        page = NumberPage(keeps="25")
        with self.assertRaises(ComputerError) as caught:
            type_into(page, "24")
        self.assertIn("holds '25' after setting it to '24'", str(caught.exception))
        page = NumberPage(keeps="24.0")
        type_into(page, "24")  # the same number written another way is fine


class TheValueIsNeverSteppedByAccident(unittest.TestCase):
    def test_a_click_on_the_arrows_is_refused(self):
        arrows = {"label": "Quantity line 1", "value": "24", "arrows": True}
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "number_field_at", return_value=arrows):
            with self.assertRaises(ComputerError) as caught:
                core._refuse_number_arrows("click", (686, 584))
        said = str(caught.exception)
        for want in ("nothing was pressed", "up/down arrows of the number field \"Quantity line 1\"", "now '24'",
                     "gui type REF", "list of earlier entries"):
            self.assertIn(want, said)
        # The field's own text area, or no number field there: the click goes ahead.
        for found in ({"label": "Quantity line 1", "value": "24", "arrows": False}, None):
            with mock.patch.object(cdp, "available", return_value=True), \
                    mock.patch.object(cdp, "number_field_at", return_value=found):
                core._refuse_number_arrows("click", (650, 584))

    def test_up_and_down_in_a_number_field_press_nothing(self):
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "focused_number_field", return_value="Quantity line 1"), \
                mock.patch.object(core.xinput, "key") as key:
            with self.assertRaises(ComputerError) as caught:
                core._perform("key", None, None, None, ["Down"], None, None, None, False)
        key.assert_not_called()
        self.assertIn("steps its value", str(caught.exception))
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "focused_number_field", return_value=None), \
                mock.patch.object(cdp, "focused_enter_would_submit", return_value=None), \
                mock.patch.object(core.xinput, "key") as key:
            core._perform("key", None, None, None, ["Down"], None, None, None, False)
        key.assert_called_once()

    def test_the_wheel_over_a_number_field_scrolls_the_page_from_beside_it(self):
        over = lambda x, y: x > 600  # noqa: E731 - the number field is on the right
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "number_field_at", side_effect=over), \
                mock.patch.object(core.screen, "size", return_value=(1280, 720)), \
                mock.patch.object(core.xinput, "scroll") as scroll:
            core._perform("scroll", None, (686, 584), None, None, "down", 3, None, False)
        x, y = scroll.call_args.args[:2]
        self.assertLessEqual(x, 600)
        self.assertEqual(y, 584)


class AFormIsListedBeforeItIsSent(unittest.TestCase):
    FORM = {"form": "New order", "fields": [["Customer", "Cafe Bloom"], ["Product line 1", "MUG-12 - Ceramic mug 12oz"],
                                             ["Quantity line 1", "60"], ["Unit price line 1", "149"]]}

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        patch = mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "form-check.json")
        patch.start()
        self.addCleanup(patch.stop)
        self.save = {"ref": "e2081", "name": "Save order", "path": "/form/button", "source": "cdp"}

    def check(self, form, checked=False):
        with mock.patch.object(cdp, "submit_form_fields", return_value=form):
            return core._form_check(self.save, checked=checked)

    def test_the_first_click_lists_the_fields_and_checked_sends_it(self):
        said = self.check(self.FORM)
        for want in ("Not pressed yet", "\"New order\" would be sent holding", "Quantity line 1: 60",
                     "Product line 1: MUG-12 - Ceramic mug 12oz", "gui click e2081 --checked"):
            self.assertIn(want, said)
        self.assertEqual(self.check(self.FORM, checked=True), "")

    def test_checked_without_a_list_or_after_a_change_lists_again(self):
        self.assertIn("Not pressed yet", self.check(self.FORM, checked=True))  # no list was seen yet
        self.check(self.FORM)
        changed = {"form": "New order", "fields": [f if f[0] != "Quantity line 1" else ["Quantity line 1", "24"]
                                                    for f in self.FORM["fields"]]}
        self.assertIn("Quantity line 1: 24", self.check(changed, checked=True))

    def test_a_checked_click_the_gate_stops_is_still_checked_after_the_yes(self):
        # Found live (2026-10-24): the checked click the approval gate stopped used the note up; after the person's
        # yes the same click listed the form again instead of pressing, and the filing was asked for five more times.
        self.check(self.FORM)
        self.assertEqual(self.check(self.FORM, checked=True), "")   # passes; then the gate stops it for a yes
        self.assertEqual(self.check(self.FORM, checked=True), "")   # the approved repeat still passes
        self.assertTrue(core.FORM_CHECK_FILE.exists())               # used up only when the click is let through (act)

    def test_a_small_form_or_a_non_submit_click_is_sent_at_once(self):
        self.assertEqual(self.check(None), "")


if __name__ == "__main__":
    unittest.main()
